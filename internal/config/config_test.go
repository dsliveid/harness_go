package config

import (
	"os"
	"path/filepath"
	"testing"
)

// envMap 构造注入用的环境变量读取函数。
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaultValues(t *testing.T) {
	c := Default()
	if c.Server.Addr != "127.0.0.1:8080" {
		t.Fatalf("默认监听地址错误: %s", c.Server.Addr)
	}
	if c.Deployment.Mode != ModeLocal {
		t.Fatalf("默认部署形态错误: %s", c.Deployment.Mode)
	}
	if c.Storage.MaxOpenSessions != 8 {
		t.Fatalf("max_open_sessions 默认应为 8，得到 %d", c.Storage.MaxOpenSessions)
	}
	if c.Storage.MaxBlobMB != 64 {
		t.Fatalf("max_blob_mb 默认应为 64，得到 %d", c.Storage.MaxBlobMB)
	}
	if c.Tools.CoreSoftLimit != 8 || c.Tools.DeferredSoftLimit != 60 {
		t.Fatalf("工具软上限默认错误: %+v", c.Tools)
	}
	// 阶段 D-2 动态注入默认值（D11 推荐 4~8 取中位 6；降级下限 2 不得为 0）
	if c.Tools.DynamicTopK != 6 || c.Tools.DynamicMinTopK != 2 {
		t.Fatalf("动态工具注入默认值错误（应为 top_k=6 / min_top_k=2）: %+v", c.Tools)
	}
	if !c.Auth.SSOEnabled || c.Auth.SSOMode != SSOPlain {
		t.Fatalf("local 形态下 SSO 应默认开启且为 plain: %+v", c.Auth)
	}
	// 阶段 C 设备域默认值（铁律：配对 TTL 120s / 心跳 15s / 离线 45s / 任务 600s）
	if !c.Devices.Enabled {
		t.Fatalf("设备域应默认启用: %+v", c.Devices)
	}
	if c.Devices.PairTTLS != 120 || c.Devices.HeartbeatS != 15 ||
		c.Devices.OfflineAfterS != 45 || c.Devices.TaskTTLS != 600 {
		t.Fatalf("设备域默认值错误: %+v", c.Devices)
	}
	if c.Devices.StrictApprovalDevice {
		t.Fatal("strict_approval_device 应默认 false（只审计不阻断）")
	}
	// 阶段 D-4 预算闸门默认值：默认不限预算（闸门直通），历史占比 0.6
	if c.Budget.MaxTotal != 0 {
		t.Fatalf("max_total 默认应为 0（不限），得到 %d", c.Budget.MaxTotal)
	}
	if c.Budget.HistoryRatio != 0.6 {
		t.Fatalf("history_ratio 默认应为 0.6，得到 %v", c.Budget.HistoryRatio)
	}
}

// 优先级：flag > env > 文件 > 默认
func TestPriorityChain(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := `
server:
  addr: 10.0.0.1:9000
storage:
  max_open_sessions: 4
log:
  level: warn
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	env := envMap(map[string]string{
		"HARNESS_SERVER_ADDR":       "10.0.0.2:9000", // 覆盖文件
		"HARNESS_MAX_OPEN_SESSIONS": "16",            // 覆盖文件
	})

	// 无 flag：env 覆盖文件
	c, err := Load(Options{Args: []string{"-config", cfgPath}, Env: env})
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if c.Server.Addr != "10.0.0.2:9000" {
		t.Fatalf("env 应覆盖文件，得到 %s", c.Server.Addr)
	}
	if c.Storage.MaxOpenSessions != 16 {
		t.Fatalf("env 应覆盖文件，得到 %d", c.Storage.MaxOpenSessions)
	}
	if c.Log.Level != "warn" {
		t.Fatalf("文件值应保留，得到 %s", c.Log.Level)
	}
	if c.ConfigFile != cfgPath {
		t.Fatalf("ConfigFile 未记录: %s", c.ConfigFile)
	}

	// 带 flag：flag 覆盖 env
	c2, err := Load(Options{
		Args: []string{"-config", cfgPath, "-addr", "127.0.0.1:1234"},
		Env:  env,
	})
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if c2.Server.Addr != "127.0.0.1:1234" {
		t.Fatalf("flag 应覆盖 env，得到 %s", c2.Server.Addr)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"非法 mode", func(c *Config) { c.Deployment.Mode = "k8s" }},
		{"非法 sso_mode", func(c *Config) { c.Auth.SSOMode = "magic" }},
		{"max_open_sessions=0", func(c *Config) { c.Storage.MaxOpenSessions = 0 }},
		{"max_blob_mb=0", func(c *Config) { c.Storage.MaxBlobMB = 0 }},
		{"非法日志级别", func(c *Config) { c.Log.Level = "verbose" }},
		{"空 addr", func(c *Config) { c.Server.Addr = "" }},
		// 阶段 D-2：动态工具注入参数（0 或自相矛盾一律拒绝）
		{"dynamic_top_k=0", func(c *Config) { c.Tools.DynamicTopK = 0 }},
		{"dynamic_top_k 负数", func(c *Config) { c.Tools.DynamicTopK = -1 }},
		{"dynamic_min_top_k=0", func(c *Config) { c.Tools.DynamicMinTopK = 0 }},
		{"dynamic_min > dynamic_top", func(c *Config) {
			c.Tools.DynamicTopK = 4
			c.Tools.DynamicMinTopK = 6
		}},
		// 阶段 C 设备域：离线阈值必须大于心跳间隔，否则端侧反复上下线
		{"offline_after == heartbeat", func(c *Config) {
			c.Devices.HeartbeatS = 15
			c.Devices.OfflineAfterS = 15
		}},
		{"offline_after < heartbeat", func(c *Config) {
			c.Devices.HeartbeatS = 30
			c.Devices.OfflineAfterS = 10
		}},
		{"devices.pair_ttl_s=0", func(c *Config) { c.Devices.PairTTLS = 0 }},
		{"devices.heartbeat_s=0", func(c *Config) { c.Devices.HeartbeatS = 0 }},
		{"devices.task_ttl_s=0", func(c *Config) { c.Devices.TaskTTLS = 0 }},
		// 阶段 D-4 预算闸门：负预算 = 配了假闸门；比例越界 = 历史段挤占或失控
		{"budget.max_total 负数", func(c *Config) { c.Budget.MaxTotal = -1 }},
		{"budget.history_ratio=0", func(c *Config) { c.Budget.HistoryRatio = 0 }},
		{"budget.history_ratio>1", func(c *Config) { c.Budget.HistoryRatio = 1.5 }},
		{"budget.history_ratio 负数", func(c *Config) { c.Budget.HistoryRatio = -0.1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("期望校验失败，实际通过")
			}
		})
	}
}

// server/cloud 形态下不得使用明文密码免登。
func TestNonLocalForcesSigned(t *testing.T) {
	c := Default()
	c.Deployment.Mode = ModeServer
	c.Auth.SSOEnabled = true
	c.Auth.SSOMode = SSOPlain
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	if c.Auth.SSOMode != SSOSigned {
		t.Fatalf("server 形态应强制 signed，得到 %s", c.Auth.SSOMode)
	}
}

func TestConfigFlagMissingValue(t *testing.T) {
	if _, err := Load(Options{Args: []string{"-config"}, Env: envMap(nil)}); err == nil {
		t.Fatal("期望 -config 缺值报错")
	}
}

func TestEnsureDataDir(t *testing.T) {
	c := Default()
	c.Data.Dir = filepath.Join(t.TempDir(), "nested", "data")
	abs, err := c.EnsureDataDir()
	if err != nil {
		t.Fatalf("EnsureDataDir 失败: %v", err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		t.Fatalf("数据目录未创建: %v", err)
	}
}

// 阶段 D-2 动态工具注入参数：环境变量须能覆盖默认值并生效（契约可配）。
func TestDynamicTopKEnvOverride(t *testing.T) {
	cfg, err := Load(Options{
		Args: []string{},
		Env: envMap(map[string]string{
			"HARNESS_DYNAMIC_TOPK":     "8",
			"HARNESS_DYNAMIC_MIN_TOPK": "4",
		}),
	})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Tools.DynamicTopK != 8 {
		t.Fatalf("HARNESS_DYNAMIC_TOPK 未生效，得到 %d", cfg.Tools.DynamicTopK)
	}
	if cfg.Tools.DynamicMinTopK != 4 {
		t.Fatalf("HARNESS_DYNAMIC_MIN_TOPK 未生效，得到 %d", cfg.Tools.DynamicMinTopK)
	}
}

// 阶段 C/D 契约：出厂默认值必须能通过 Validate（防止把校验条件写反，
// 出现「默认配置自己都加载不了」的低级故障）。
func TestDefaultConfigValidates(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("默认配置应当合法，实际被拒: %v", err)
	}
}

// 合法边界：offline_after_s 刚好比 heartbeat_s 大 1 必须通过
// （校验只要求「大于」，不该隐含 3× 之类的强假设）。
func TestDevicesOfflineAfterBoundaryAccepted(t *testing.T) {
	c := Default()
	c.Devices.HeartbeatS = 15
	c.Devices.OfflineAfterS = 16
	if err := c.Validate(); err != nil {
		t.Fatalf("offline_after_s 仅比 heartbeat_s 大 1 应被接受，实际被拒: %v", err)
	}
}

// 阶段 D-2：YAML 中把 dynamic_min_top_k 写成 0 必须被拒绝，
// 而不是被 selector.normalize() 静默改写（静默兜底会掩盖配置错误）。
func TestDynamicTopKYamlZeroRejected(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := `
tools:
  dynamic_top_k: 6
  dynamic_min_top_k: 0
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Options{Args: []string{"-config", cfgPath}, Env: envMap(nil)}); err == nil {
		t.Fatal("期望 dynamic_min_top_k=0 被拒绝，实际加载成功")
	}
}

// 阶段 D-4 预算闸门：环境变量覆盖 + 合法边界。
func TestBudgetEnvOverride(t *testing.T) {
	cfg, err := Load(Options{
		Args: []string{},
		Env: envMap(map[string]string{
			"HARNESS_BUDGET_MAX_TOTAL":     "32000",
			"HARNESS_BUDGET_HISTORY_RATIO": "0.5",
		}),
	})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Budget.MaxTotal != 32000 {
		t.Fatalf("HARNESS_BUDGET_MAX_TOTAL 未生效，得到 %d", cfg.Budget.MaxTotal)
	}
	if cfg.Budget.HistoryRatio != 0.5 {
		t.Fatalf("HARNESS_BUDGET_HISTORY_RATIO 未生效，得到 %v", cfg.Budget.HistoryRatio)
	}
}

// 合法边界：max_total=0（不限）与 history_ratio 恰好 1.0 都必须通过。
func TestBudgetBoundaryAccepted(t *testing.T) {
	c := Default()
	c.Budget.MaxTotal = 0
	c.Budget.HistoryRatio = 1
	if err := c.Validate(); err != nil {
		t.Fatalf("history_ratio=1.0 应被接受（历史段可占满预算），实际被拒: %v", err)
	}
}
