// Package config 负责配置加载、默认值与校验。
//
// 优先级（高 → 低，见 docs/架构设计文档.md §9.1）：
//
//	命令行 flag > 环境变量 HARNESS_* > data/config.yaml > 内置默认值
//
// 配置对象一经 Load 返回即为只读，运行期不再修改（热更新走 settings 表，不走本包）。
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DeploymentMode 部署形态，决定内置工具开关与鉴权强度。
type DeploymentMode string

const (
	ModeLocal  DeploymentMode = "local"  // 本地单机（一等公民）
	ModeServer DeploymentMode = "server" // 内网自托管
	ModeCloud  DeploymentMode = "cloud"  // 云端多租户（预留）
)

// SSOMode 免登入口的凭据校验方式。
type SSOMode string

const (
	SSOPlain  SSOMode = "plain"  // 直接校验 user_name + pwd（local 默认）
	SSOSigned SSOMode = "signed" // HMAC 签名 + ts 防重放（server/cloud 推荐）
	SSOBoth   SSOMode = "both"   // 两者都接受（迁移期）
)

// ServerConfig HTTP 服务参数。
type ServerConfig struct {
	Addr          string `yaml:"addr"`            // 默认 127.0.0.1:8080
	ReadTimeoutS  int    `yaml:"read_timeout_s"`  // 秒
	WriteTimeoutS int    `yaml:"write_timeout_s"` // 秒（SSE 需为 0 或足够大）
}

// DeploymentConfig 部署形态。
type DeploymentConfig struct {
	Mode DeploymentMode `yaml:"mode"`
}

// DataConfig 数据目录（主库/会话库/blob 均在其下）。
type DataConfig struct {
	Dir string `yaml:"dir"`
}

// AuthConfig 账号与单点登录。
type AuthConfig struct {
	SSOEnabled  bool    `yaml:"sso_enabled"`
	SSOMode     SSOMode `yaml:"sso_mode"`
	SSOSecret   string  `yaml:"sso_secret"`    // signed 模式的 HMAC 密钥
	SessionTTLH int     `yaml:"session_ttl_h"` // web 会话有效期（小时）
	InitialUser string  `yaml:"initial_user"`  // 首启引导账号（仅无用户时生效）
	InitialPwd  string  `yaml:"initial_pwd"`   // 首启引导口令；空则随机生成
}

// StorageConfig 存储与 CAS。
type StorageConfig struct {
	MaxOpenSessions int `yaml:"max_open_sessions"` // 同时打开的会话库上限（LRU）
	MaxBlobMB       int `yaml:"max_blob_mb"`       // 单 blob 上限
	DataQuotaGB     int `yaml:"data_quota_gb"`     // data 目录配额告警阈值
	IdleEvictMin    int `yaml:"idle_evict_min"`    // 会话库闲置淘汰（分钟）
	BlobGCGraceH    int `yaml:"blob_gc_grace_h"`   // 全局快照 blob GC 宽限期（小时）
}

// ToolsConfig 工具注入（软上限仅告警，不阻断启动）。
//
// 契约（已冻结，后续并行开发以此签名为准）：
//   - CoreSoftLimit / DeferredSoftLimit 是**软上限**，仅告警不阻断启动（D11 修定）。
//   - DynamicTopK 是每轮动态检索注入全量定义的 deferred 工具条数（D11 推荐 4~8）。
//   - DynamicMinTopK 是预算降级（topK 减半）时允许的最小条数，**必须 > 0**：
//     砍到 0 会让按需工具完全不可见，退化成比全量注入更糟的形态。
//
// 两者均在 Validate 中硬校验，非法值直接拒绝加载，不做静默兜底
// （静默兜底会让 `config.yaml` 里写错的值看起来"生效了"，实际却被改写）。
type ToolsConfig struct {
	CoreSoftLimit     int `yaml:"core_soft_limit"`     // 默认 8
	DeferredSoftLimit int `yaml:"deferred_soft_limit"` // 默认 60
	// DynamicTopK 动态检索每轮注入全量定义的 deferred 工具条数（D-2/D11 推荐 4~8）。
	DynamicTopK int `yaml:"dynamic_top_k"` // 默认 6
	// DynamicMinTopK 预算降级（topK 减半）时允许的最小条数。
	// 不得为 0：砍到 0 会让按需工具完全不可见，退化成比全量注入更糟的形态。
	DynamicMinTopK int `yaml:"dynamic_min_top_k"` // 默认 2
}

// DevicesConfig 多端协同（HALO）设备域参数。
//
// 默认值对齐 docs/开发实施计划.md 阶段 C：配对码 TTL 120s、心跳 15s、
// 45s（3×心跳）无帧判离线、任务派发默认 600s 超时。
type DevicesConfig struct {
	Enabled              bool `yaml:"enabled"`                // 设备域总开关（关闭时端点返回 unavailable）
	PairTTLS             int  `yaml:"pair_ttl_s"`             // 配对码有效期（秒）
	HeartbeatS           int  `yaml:"heartbeat_s"`            // 端侧 ping 间隔（秒）
	OfflineAfterS        int  `yaml:"offline_after_s"`        // 无帧判离线阈值（秒）
	TaskTTLS             int  `yaml:"task_ttl_s"`             // 任务派发超时（秒）
	StrictApprovalDevice bool `yaml:"strict_approval_device"` // 高风险审批是否仅限执行端设备确认
}

// BudgetConfig 预算闸门（阶段 D-4）参数。
//
// 契约：
//   - MaxTotal = 0 表示不限预算，闸门直通（零开销，行为与 D-4 之前完全一致）；
//   - MaxTotal > 0 时超出即触发五级降级（省记忆 → topK 减半 → 常驻工具降到 4
//     → 旧工具结果转引用 → 强制压缩），五级用尽仍超则 Run 明确失败
//     （budget_exhausted），**严禁静默截断**；
//   - HistoryRatio 是历史段占总预算的比例上限（映射 prompt.Limits.MaxHistory），
//     超出部分由 Builder 从最旧开始裁剪（这是预算内的正常裁剪，有 DroppedHistory 计数，
//     与「降级用尽后的明确报错」是两条路径）。
//
// 两者均在 Validate 中硬校验，非法值直接拒绝加载，不做静默兜底。
type BudgetConfig struct {
	MaxTotal     int     `yaml:"max_total"`     // 总 token 预算，0 = 不限（默认）
	HistoryRatio float64 `yaml:"history_ratio"` // 历史段占比上限 (0,1]，默认 0.6
}

// MCPConfig 外部插件接入（阶段 E / MCP + 工具市场）参数。
//
// 安全边界（docs/MCP接入设计.md §10）：
//   - cloud 形态恒禁止 stdio（服务端不起子进程，Validate 硬约束）；
//   - server 形态使用 stdio 需显式 AllowStdio；
//   - TrustedKeys 是受信 ed25519 公钥（hex）：安装签名包的信任根，为空时
//     任何签名都无法通过 → 签名包一律硬拒（无法证明来源）；
//   - AllowUnsigned 默认 false：未签名包拒绝安装（出口标准「装未签名包被拒」）。
type MCPConfig struct {
	Enabled       bool     `yaml:"enabled"`        // 外部插件总开关（关闭时相关端点返回 unavailable）
	AllowStdio    bool     `yaml:"allow_stdio"`    // server 形态显式放行 stdio
	TrustedKeys   []string `yaml:"trusted_keys"`   // 受信公钥（64 位 hex ed25519）
	AllowUnsigned bool     `yaml:"allow_unsigned"` // 允许安装未签名包（默认拒绝）
}

// LogConfig 日志。
type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | text
}

// Config 聚合配置。
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Deployment DeploymentConfig `yaml:"deployment"`
	Data       DataConfig       `yaml:"data"`
	Auth       AuthConfig       `yaml:"auth"`
	Storage    StorageConfig    `yaml:"storage"`
	Tools      ToolsConfig      `yaml:"tools"`
	Devices    DevicesConfig    `yaml:"devices"`
	Budget     BudgetConfig     `yaml:"budget"`
	MCP        MCPConfig        `yaml:"mcp"`
	Log        LogConfig        `yaml:"log"`

	// ConfigFile 实际加载的配置文件路径（空 = 未使用文件）
	ConfigFile string `yaml:"-"`
}

// Default 返回内置默认值（local 形态、本机监听）。
func Default() Config {
	return Config{
		Server:     ServerConfig{Addr: "127.0.0.1:8080", ReadTimeoutS: 30, WriteTimeoutS: 0},
		Deployment: DeploymentConfig{Mode: ModeLocal},
		Data:       DataConfig{Dir: "./data"},
		Auth: AuthConfig{
			SSOEnabled:  true, // local 默认开启；server/cloud 在 Validate 中强制关闭
			SSOMode:     SSOPlain,
			SessionTTLH: 168, // 7d
			InitialUser: "admin",
		},
		Storage: StorageConfig{
			MaxOpenSessions: 8,
			MaxBlobMB:       64,
			DataQuotaGB:     8,
			IdleEvictMin:    10,
			BlobGCGraceH:    24,
		},
		Tools: ToolsConfig{
			CoreSoftLimit:     8,
			DeferredSoftLimit: 60,
			DynamicTopK:       6, // D11 推荐 4~8 的中位
			DynamicMinTopK:    2,
		},
		Devices: DevicesConfig{
			Enabled:              true,
			PairTTLS:             120, // 铁律：配对码 TTL 120s
			HeartbeatS:           15,
			OfflineAfterS:        45, // 3 × heartbeat
			TaskTTLS:             600,
			StrictApprovalDevice: false, // 默认只审计不阻断
		},
		Budget: BudgetConfig{
			MaxTotal:     0,   // 默认不限预算（闸门直通）；配置后超限走五级降级
			HistoryRatio: 0.6, // 历史段占比上限，仅 MaxTotal>0 时生效
		},
		MCP: MCPConfig{
			Enabled:       true,  // 外部插件默认启用（未配置 Server/包时零副作用）
			AllowStdio:    false, // server 形态需显式放行 stdio
			TrustedKeys:   nil,   // 受信公钥（空 = 签名包无法安装）
			AllowUnsigned: false, // 出口铁律：未签名包默认被拒
		},
		Log: LogConfig{Level: "info", Format: "text"},
	}
}

// Options 注入点，便于测试。
type Options struct {
	Args []string            // 命令行参数（不含程序名）
	Env  func(string) string // 环境变量读取（nil 用 os.Getenv）
}

// Load 按优先级加载并校验配置。
func Load(opts Options) (Config, error) {
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}

	cfg := Default()

	// ① 定位配置文件：flag > env > ./data/config.yaml
	explicitPath, err := scanConfigPath(opts.Args, env)
	if err != nil {
		return cfg, err
	}
	path := explicitPath
	if path == "" {
		path = filepath.Join(cfg.Data.Dir, "config.yaml")
	}
	if err := loadFile(path, &cfg); err != nil {
		return cfg, err
	}

	// ② 环境变量覆盖
	if err := applyEnv(&cfg, env); err != nil {
		return cfg, err
	}

	// ③ flag 覆盖（只覆盖显式传入的）
	if err := applyFlags(&cfg, opts.Args); err != nil {
		return cfg, err
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// scanConfigPath 提前解析 -config / --config / HARNESS_CONFIG。
func scanConfigPath(args []string, env func(string) string) (string, error) {
	for i, a := range args {
		switch {
		case a == "-config" || a == "--config":
			if i+1 >= len(args) {
				return "", errors.New("config: -config 缺少参数值")
			}
			return args[i+1], nil
		case strings.HasPrefix(a, "-config="):
			return strings.TrimPrefix(a, "-config="), nil
		case strings.HasPrefix(a, "--config="):
			return strings.TrimPrefix(a, "--config="), nil
		}
	}
	return env("HARNESS_CONFIG"), nil
}

func loadFile(path string, cfg *Config) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 无配置文件是正常情况
		}
		return fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return fmt.Errorf("config: 解析 %s 失败: %w", path, err)
	}
	cfg.ConfigFile = path
	return nil
}

func applyEnv(cfg *Config, env func(string) string) error {
	type binding struct {
		key string
		set func(string) error
	}
	boolVal := func(dst *bool) func(string) error {
		return func(v string) error {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("需要布尔值，得到 %q", v)
			}
			*dst = b
			return nil
		}
	}
	intVal := func(dst *int) func(string) error {
		return func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("需要整数，得到 %q", v)
			}
			*dst = n
			return nil
		}
	}
	floatVal := func(dst *float64) func(string) error {
		return func(v string) error {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("需要数值，得到 %q", v)
			}
			*dst = f
			return nil
		}
	}
	strVal := func(dst *string) func(string) error {
		return func(v string) error { *dst = v; return nil }
	}

	bindings := []binding{
		{"HARNESS_SERVER_ADDR", strVal(&cfg.Server.Addr)},
		{"HARNESS_DEPLOYMENT_MODE", func(v string) error { cfg.Deployment.Mode = DeploymentMode(v); return nil }},
		{"HARNESS_DATA_DIR", strVal(&cfg.Data.Dir)},
		{"HARNESS_LOG_LEVEL", strVal(&cfg.Log.Level)},
		{"HARNESS_LOG_FORMAT", strVal(&cfg.Log.Format)},
		{"HARNESS_SSO_ENABLED", boolVal(&cfg.Auth.SSOEnabled)},
		{"HARNESS_SSO_MODE", func(v string) error { cfg.Auth.SSOMode = SSOMode(v); return nil }},
		{"HARNESS_SSO_SECRET", strVal(&cfg.Auth.SSOSecret)},
		{"HARNESS_SESSION_TTL_H", intVal(&cfg.Auth.SessionTTLH)},
		{"HARNESS_USER", strVal(&cfg.Auth.InitialUser)},
		{"HARNESS_PWD", strVal(&cfg.Auth.InitialPwd)},
		{"HARNESS_MAX_OPEN_SESSIONS", intVal(&cfg.Storage.MaxOpenSessions)},
		{"HARNESS_MAX_BLOB_MB", intVal(&cfg.Storage.MaxBlobMB)},
		{"HARNESS_DATA_QUOTA_GB", intVal(&cfg.Storage.DataQuotaGB)},
		{"HARNESS_CORE_SOFT_LIMIT", intVal(&cfg.Tools.CoreSoftLimit)},
		{"HARNESS_DEFERRED_SOFT_LIMIT", intVal(&cfg.Tools.DeferredSoftLimit)},
		{"HARNESS_DYNAMIC_TOPK", intVal(&cfg.Tools.DynamicTopK)},
		{"HARNESS_DYNAMIC_MIN_TOPK", intVal(&cfg.Tools.DynamicMinTopK)},
		{"HARNESS_DEVICES_ENABLED", boolVal(&cfg.Devices.Enabled)},
		{"HARNESS_DEVICES_PAIR_TTL_S", intVal(&cfg.Devices.PairTTLS)},
		{"HARNESS_DEVICES_HEARTBEAT_S", intVal(&cfg.Devices.HeartbeatS)},
		{"HARNESS_DEVICES_OFFLINE_AFTER_S", intVal(&cfg.Devices.OfflineAfterS)},
		{"HARNESS_DEVICES_TASK_TTL_S", intVal(&cfg.Devices.TaskTTLS)},
		{"HARNESS_DEVICES_STRICT_APPROVAL", boolVal(&cfg.Devices.StrictApprovalDevice)},
		{"HARNESS_BUDGET_MAX_TOTAL", intVal(&cfg.Budget.MaxTotal)},
		{"HARNESS_BUDGET_HISTORY_RATIO", floatVal(&cfg.Budget.HistoryRatio)},
	}
	for _, b := range bindings {
		v := env(b.key)
		if v == "" {
			continue
		}
		if err := b.set(v); err != nil {
			return fmt.Errorf("config: 环境变量 %s 非法: %w", b.key, err)
		}
	}
	return nil
}

func applyFlags(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("harness-go", flag.ContinueOnError)
	addr := fs.String("addr", "", "HTTP 监听地址（覆盖配置文件/环境变量）")
	dataDir := fs.String("data", "", "数据目录")
	mode := fs.String("mode", "", "部署形态 local|server|cloud")
	logLevel := fs.String("log-level", "", "日志级别 debug|info|warn|error")
	_ = fs.String("config", "", "配置文件路径（在 Load 早期已解析）")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("config: 参数解析失败: %w", err)
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			cfg.Server.Addr = *addr
		case "data":
			cfg.Data.Dir = *dataDir
		case "mode":
			cfg.Deployment.Mode = DeploymentMode(*mode)
		case "log-level":
			cfg.Log.Level = *logLevel
		}
	})
	return nil
}

// Validate 校验取值合法性并填充依赖默认值。
func (c *Config) Validate() error {
	switch c.Deployment.Mode {
	case ModeLocal, ModeServer, ModeCloud:
	default:
		return fmt.Errorf("config: deployment.mode 非法: %q（可选 local|server|cloud）", c.Deployment.Mode)
	}

	switch c.Auth.SSOMode {
	case SSOPlain, SSOSigned, SSOBoth:
	default:
		return fmt.Errorf("config: auth.sso_mode 非法: %q（可选 plain|signed|both）", c.Auth.SSOMode)
	}

	// 非本地形态禁止明文密码免登（见 docs/开发实施计划.md 附录 A）
	if c.Deployment.Mode != ModeLocal {
		if c.Auth.SSOMode == SSOPlain {
			c.Auth.SSOMode = SSOSigned
		}
		if c.Auth.SSOEnabled && c.Auth.SSOMode == SSOPlain {
			c.Auth.SSOEnabled = false
		}
	}

	if c.Server.Addr == "" {
		return errors.New("config: server.addr 不能为空")
	}
	if c.Data.Dir == "" {
		return errors.New("config: data.dir 不能为空")
	}
	if c.Storage.MaxOpenSessions <= 0 {
		return errors.New("config: storage.max_open_sessions 必须 > 0")
	}
	if c.Storage.MaxBlobMB <= 0 {
		return errors.New("config: storage.max_blob_mb 必须 > 0")
	}
	if c.Auth.SessionTTLH <= 0 {
		return errors.New("config: auth.session_ttl_h 必须 > 0")
	}
	// 阶段 D-2 动态工具注入：两个参数直接决定「模型每轮看得见多少工具」，
	// 取值非法会让 Token 治理静默失效，故一律硬拒绝而非兜底。
	if c.Tools.DynamicTopK <= 0 {
		return errors.New("config: tools.dynamic_top_k 必须 > 0（0 会让按需工具完全不可见）")
	}
	if c.Tools.DynamicMinTopK <= 0 {
		return errors.New("config: tools.dynamic_min_top_k 必须 > 0（降级不得把按需工具砍到 0）")
	}
	if c.Tools.DynamicMinTopK > c.Tools.DynamicTopK {
		// 归一化会把它压成 DynamicTopK，但那是掩盖配置错误：用户写了自相矛盾的值。
		return fmt.Errorf("config: tools.dynamic_min_top_k(%d) 不能大于 tools.dynamic_top_k(%d)",
			c.Tools.DynamicMinTopK, c.Tools.DynamicTopK)
	}
	if c.Devices.PairTTLS <= 0 {
		return errors.New("config: devices.pair_ttl_s 必须 > 0")
	}
	if c.Devices.HeartbeatS <= 0 {
		return errors.New("config: devices.heartbeat_s 必须 > 0")
	}
	if c.Devices.OfflineAfterS <= 0 {
		return errors.New("config: devices.offline_after_s 必须 > 0")
	}
	// offline_after_s 是「多久没收到帧就判离线」的阈值，heartbeat_s 是端侧主动 ping 的间隔。
	// 若前者不大于后者，端侧在两次心跳的正常间隙里就会被判离线，表现为反复上下线；
	// 推荐值 3×heartbeat（与默认 15s/45s 一致），这里只硬性要求必须留出余量。
	if c.Devices.OfflineAfterS <= c.Devices.HeartbeatS {
		return fmt.Errorf("config: devices.offline_after_s(%d) 必须大于 devices.heartbeat_s(%d)，否则端侧会在两次心跳间被判反复上下线",
			c.Devices.OfflineAfterS, c.Devices.HeartbeatS)
	}
	if c.Devices.TaskTTLS <= 0 {
		return errors.New("config: devices.task_ttl_s 必须 > 0")
	}
	// 阶段 D-4 预算闸门：负预算会让「超限判定」永远不触发，等于配置了假闸门；
	// 比例越界同理（<=0 历史段无上限吃光预算，>1 历史段挤掉系统提示与工具）。
	if c.Budget.MaxTotal < 0 {
		return errors.New("config: budget.max_total 不能为负（0 = 不限预算）")
	}
	if c.Budget.HistoryRatio <= 0 || c.Budget.HistoryRatio > 1 {
		return fmt.Errorf("config: budget.history_ratio 必须在 (0,1] 区间，得到 %v", c.Budget.HistoryRatio)
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
		c.Log.Level = strings.ToLower(c.Log.Level)
	default:
		return fmt.Errorf("config: log.level 非法: %q", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "json", "text":
		c.Log.Format = strings.ToLower(c.Log.Format)
	default:
		return fmt.Errorf("config: log.format 非法: %q", c.Log.Format)
	}
	return nil
}

// DataDir 返回数据目录的绝对路径（不存在则创建）。
func (c *Config) EnsureDataDir() (string, error) {
	abs, err := filepath.Abs(c.Data.Dir)
	if err != nil {
		return "", fmt.Errorf("config: 解析 data.dir 失败: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("config: 创建数据目录 %s 失败: %w", abs, err)
	}
	return abs, nil
}
