package service

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"harness-go/internal/mcp"
	"harness-go/internal/secret"
	"harness-go/internal/store"
	"harness-go/internal/tools/registry"
)

// newMCPTestService 测试环境（复用 fake-mcp fixture 的构建产物路径规则）。
func newMCPTestService(t *testing.T) (*MCPService, *registry.Registry, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	main, err := store.OpenMain(ctx, dataDir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	cipher, err := secret.LoadOrCreate(filepath.Join(dataDir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(registry.Options{CoreSoftLimit: 8, DeferredSoftLimit: 60})
	mgr := mcp.NewManager(mcp.Options{ClientName: "svc-test", ClientVersion: "0"}, mcp.DeploymentPolicy{Mode: "local"})
	t.Cleanup(mgr.StopAll)
	bin := fakeMCPPkgBin // 同一 fixture（package_e2e_test.go TestMain 构建）
	svc := NewMCPService(MCPServiceOptions{
		Main:     main,
		Manager:  mgr,
		Registry: reg,
		Cipher:   cipher,
		Dep:      mcp.DeploymentPolicy{Mode: "local"},
	})
	return svc, reg, bin
}

func mkIn(name, command string) MCPCreateInput {
	enabled := true
	return MCPCreateInput{
		Name: name, Transport: "stdio", Command: command,
		Env: map[string]string{"TOKEN": "sekrit"}, Enabled: &enabled,
	}
}

// TestMCPServiceCreateMasksSecrets 配置落库 + API 视图脱敏（env 只回键名）。
func TestMCPServiceCreateMasksSecrets(t *testing.T) {
	svc, _, _ := newMCPTestService(t)
	ctx := context.Background()
	v, err := svc.Create(ctx, mkIn("脱敏服务", "no-such-cmd"), false)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if len(v.EnvKeys) != 1 || v.EnvKeys[0] != "TOKEN" {
		t.Fatalf("env 键名脱敏错误: %v", v.EnvKeys)
	}
	// 全列表同样脱敏
	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("列表错误: %v", err)
	}
	if list[0].EnvKeys[0] != "TOKEN" {
		t.Fatalf("列表脱敏错误")
	}
	// 原始值绝不出现
	for _, s := range []string{"sekrit"} {
		if mcpViewContains(v, s) {
			t.Fatalf("密文泄漏: %q", s)
		}
	}
}

func mcpViewContains(v *ServerView, sub string) bool {
	b := []byte(v.Name + v.Command + v.LastError)
	for _, k := range v.EnvKeys {
		b = append(b, []byte(k)...)
	}
	return indexOfStr(string(b), sub) >= 0
}

// TestMCPServiceCloudRejectsStdio cloud 形态 stdio 配置被拒（M5）。
func TestMCPServiceCloudRejectsStdio(t *testing.T) {
	svc, _, _ := newMCPTestService(t)
	svc.dep = mcp.DeploymentPolicy{Mode: "cloud"}
	_, err := svc.Create(context.Background(), mkIn("c1", "x"), false)
	if err == nil {
		t.Fatalf("cloud 形态必须拒绝 stdio")
	}
}

// TestMCPServiceLifecycle 启动 → 注册联动 → 联通测试 → 停用注销 → 删除。
func TestMCPServiceLifecycle(t *testing.T) {
	svc, reg, bin := newMCPTestService(t)
	ctx := context.Background()

	// ① 创建并立即启动
	v, err := svc.Create(ctx, mkIn("生命周期", bin), true)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if v.State != string(mcp.StateReady) {
		t.Fatalf("启动后应 ready: %+v", v)
	}
	// ② 注册表联动：外部工具与内置同构
	want := "mcp__" + mcp.ServerKey("生命周期") + "__echo"
	if _, _, err := reg.Resolve(want); err != nil {
		t.Fatalf("注册表缺少 %s: %v", want, err)
	}
	// ③ 联通测试（未落库的配置也可测）
	tr := mkIn("临时", bin)
	res, err := svc.Test(ctx, tr)
	if err != nil || len(res.ToolNames) != 3 {
		t.Fatalf("联通测试失败: %v %+v", err, res)
	}
	// ④ 停用 → 工具移出注册表
	if err := svc.Disable(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Resolve(want); err == nil {
		t.Fatalf("停用后工具必须注销")
	}
	// ⑤ 启用 → 恢复
	if err := svc.Enable(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Resolve(want); err != nil {
		t.Fatalf("启用后工具必须恢复: %v", err)
	}
	// ⑥ 热重载
	if err := svc.Reload(ctx, v.ID); err != nil {
		t.Fatalf("重载失败: %v", err)
	}
	// ⑦ 删除
	if err := svc.Delete(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Resolve(want); err == nil {
		t.Fatalf("删除后工具必须注销")
	}
	if list, _ := svc.List(ctx); len(list) != 0 {
		t.Fatalf("删除后列表应为空")
	}
}

// TestMCPServiceStartAllIsolated 启动期批量拉起：坏配置只隔离自身。
func TestMCPServiceStartAllIsolated(t *testing.T) {
	svc, reg, bin := newMCPTestService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, mkIn("坏", "no-such-cmd-"+string(rune(65))), false); err != nil {
		t.Fatalf("坏配置应可保存（启动期才失败）: %v", err)
	}
	good := mkIn("好", bin)
	if _, err := svc.Create(ctx, good, false); err != nil {
		t.Fatal(err)
	}
	svc.StartAll(ctx)
	// 好 Server 注册成功，坏 Server 只记录状态
	_, _, err := reg.Resolve("mcp__" + mcp.ServerKey("好") + "__echo")
	if err != nil {
		t.Fatalf("好 Server 必须正常注册: %v", err)
	}
	if runtime.GOOS == "windows" {
		_ = runtime.GOOS // 占位：坏 Server 的状态断言与平台相关，此处只验证隔离不验证失败文案
	}
}
