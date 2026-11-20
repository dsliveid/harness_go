package service

import (
	"context"
	"testing"

	"harness-go/internal/model"
)

// 本文件锁定客户端插件安装清单（plugin: 命名空间）的三个不变量。
//
// 之所以必须有测试：本功能复用 capabilities 表，而那张表同时承载设备的**协议能力**
// （workspace.list / fs.list 等，目录列举能力协商直接依赖）。一旦 SetPluginKeys
// 误删协议能力，桌面端选目录会立刻报「该设备未申报目录列举能力」—— 这是线上
// 级故障，但静态检查与契约自检都发现不了，只能靠测试钉住。

// TestPluginCapabilityPrefixNoCollisionWithProtocolCaps 命名空间不得与协议能力重名。
//
// 这条断言看起来朴素，但它是「复用 capabilities 表」这个设计成立的前提：
// internal/api/fs_handler.go 的 hasFsListCapability 只精确比对 workspace.list / fs.list，
// 一旦 plugin: 前缀哪天被改成 fs. 或 workspace. 开头，能力协商会被静默污染。
func TestPluginCapabilityPrefixNoCollisionWithProtocolCaps(t *testing.T) {
	protocolNames := []string{
		"workspace.list", "workspace.read", "workspace.write",
		"fs.list", "fs.read", "fs.write",
	}
	for _, n := range protocolNames {
		if len(n) >= len(pluginCapabilityPrefix) && n[:len(pluginCapabilityPrefix)] == pluginCapabilityPrefix {
			t.Fatalf("协议能力名 %q 撞上插件清单前缀 %q（会污染能力协商）", n, pluginCapabilityPrefix)
		}
	}
	if pluginCapabilityPrefix != "plugin:" {
		t.Fatalf("前缀口径必须为 plugin:（与 web/src/stores/pluginStore.ts 的 KEY_PREFIX 对齐），实际 %q", pluginCapabilityPrefix)
	}
}

// TestPartitionPluginCaps 名单切分：只挑 plugin: 项，且剥掉前缀。
//
// 这是 PluginKeys 的纯函数化核心，独立于 store 校验，避免「端口抖动导致测不到」。
func TestPartitionPluginCaps(t *testing.T) {
	caps := []model.Capability{
		{Name: "workspace.list", Version: "1.0"},
		{Name: "plugin:tool:read_file"},
		{Name: "fs.list"},
		{Name: "plugin:mcp:github"},
		{Name: "plugin:skill:deploy"},
	}
	got := partitionPluginKeys(caps)
	want := []string{"tool:read_file", "mcp:github", "skill:deploy"}
	if len(got) != len(want) {
		t.Fatalf("插件 key 数量不符：got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项不符：got=%q want=%q（顺序应与原始能力一致）", i, got[i], want[i])
		}
	}
}

// TestMergePluginCapsPreservesProtocolCaps 合并新清单时必须原样保留协议能力。
//
// 覆盖三种边界：
//   - 入参自带 plugin: 前缀 → 不重复加前缀；
//   - 重复 key → 去重（否则 capabilities 表的 (device_id,name) 唯一约束会插入失败）；
//   - 空串 / 纯空白 → 跳过（避免写出 name='' 的脏行）。
func TestMergePluginCapsPreservesProtocolCaps(t *testing.T) {
	existing := []model.Capability{
		{Name: "workspace.list", Version: "1.0"},
		{Name: "plugin:tool:stale_tool"}, // 旧清单：必须被剔除
	}
	keys := []string{"tool:read_file", "plugin:mcp:github", "tool:read_file", "", "   "}
	caps, written := mergePluginCaps(existing, keys)

	// 协议能力必须保留，且只剩一份
	proto := 0
	stale := 0
	plugins := make(map[string]bool, len(caps))
	for _, c := range caps {
		switch c.Name {
		case "workspace.list":
			proto++
		case "plugin:tool:stale_tool":
			stale++
		default:
			plugins[c.Name] = true
		}
	}
	if proto != 1 {
		t.Fatalf("协议能力 workspace.list 必须保留恰好 1 份，实际 %d 份", proto)
	}
	if stale != 0 {
		t.Fatalf("旧插件清单 plugin:tool:stale_tool 必须被剔除，实际残留 %d 份", stale)
	}
	if !plugins["plugin:tool:read_file"] || !plugins["plugin:mcp:github"] {
		t.Fatalf("新清单未按 plugin: 前缀写入：%v", plugins)
	}
	if len(plugins) != 2 {
		t.Fatalf("去重/跳空失败，插件项应为 2 项，实际 %d 项：%v", len(plugins), plugins)
	}
	// 回写给前端的裸 key 同样要去重、跳空
	if len(written) != 2 || written[0] != "tool:read_file" || written[1] != "mcp:github" {
		t.Fatalf("回写裸 key 不符：%v", written)
	}
}

// TestSetPluginKeysRejectsEmptyDeviceID 空 device_id 必须在触库前拒绝。
//
// store 层 Replace 只报 "能力替换缺少 device_id"，若不提前拦，前端会拿到
// 500 而不是可读的 400；且空 ID 拼出的 URL 也毫无意义。
func TestSetPluginKeysRejectsEmptyDeviceID(t *testing.T) {
	s := &DeviceService{}
	if _, err := s.SetPluginKeys(context.Background(), "  ", []string{"tool:read_file"}); err == nil {
		t.Fatal("空设备 ID 必须返回错误")
	}
	if _, err := s.PluginKeys(context.Background(), ""); err == nil {
		t.Fatal("空设备 ID 读取同样必须返回错误")
	}
}
