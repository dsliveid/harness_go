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
		{"HARNESS_DEVICES_ENABLED", boolVal(&cfg.Devices.Enabled)},
		{"HARNESS_DEVICES_PAIR_TTL_S", intVal(&cfg.Devices.PairTTLS)},
		{"HARNESS_DEVICES_HEARTBEAT_S", intVal(&cfg.Devices.HeartbeatS)},
		{"HARNESS_DEVICES_OFFLINE_AFTER_S", intVal(&cfg.Devices.OfflineAfterS)},
		{"HARNESS_DEVICES_TASK_TTL_S", intVal(&cfg.Devices.TaskTTLS)},
		{"HARNESS_DEVICES_STRICT_APPROVAL", boolVal(&cfg.Devices.StrictApprovalDevice)},
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
	if c.Devices.PairTTLS <= 0 {
		return errors.New("config: devices.pair_ttl_s 必须 > 0")
	}
	if c.Devices.HeartbeatS <= 0 {
		return errors.New("config: devices.heartbeat_s 必须 > 0")
	}
	if c.Devices.OfflineAfterS <= 0 {
		return errors.New("config: devices.offline_after_s 必须 > 0")
	}
	if c.Devices.TaskTTLS <= 0 {
		return errors.New("config: devices.task_ttl_s 必须 > 0")
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
