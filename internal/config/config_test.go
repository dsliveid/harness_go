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
