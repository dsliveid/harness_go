package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"harness-go/internal/market"
	"harness-go/internal/mcp"
	"harness-go/internal/store"
	"harness-go/internal/tools/registry"
	"harness-go/pkg/toolapi"
)

// fakeMCPPkgBin TestMain 构建的 fake-mcp 可执行文件（作为「工具包」二进制）。
var fakeMCPPkgBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pkg-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "fake-mcp")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "harness-go/cmd/fake-mcp")
	build.Dir = ".." + string(filepath.Separator) + ".."
	if out, berr := build.CombinedOutput(); berr != nil {
		fmt.Fprintf(os.Stderr, "构建 fake-mcp 失败: %v\n%s", berr, out)
		os.Exit(1)
	}
	fakeMCPPkgBin = bin
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// pkgTestEnv 测试环境：主库 + Manager + Registry + PackageService。
type pkgTestEnv struct {
	main    *store.MainStore
	reg     *registry.Registry
	mgr     *mcp.Manager
	svc     *PackageService
	priv    ed25519.PrivateKey
	dataDir string
}

func newPkgTestEnv(t *testing.T) *pkgTestEnv {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	main, err := store.OpenMain(ctx, dataDir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	reg := registry.New(registry.Options{CoreSoftLimit: 8, DeferredSoftLimit: 60})
	mgr := mcp.NewManager(mcp.Options{ClientName: "pkg-test", ClientVersion: "0"}, mcp.DeploymentPolicy{Mode: "local"})
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("公钥断言失败")
	}
	svc := NewPackageService(PackageServiceOptions{
		Main:        main,
		Manager:     mgr,
		Registry:    reg,
		DataDir:     dataDir,
		TrustedKeys: []ed25519.PublicKey{pub},
	})
	// Windows 上子进程持有安装目录 exe 的文件锁：测试结束必须停掉全部连接，
	// 否则 t.TempDir 清理报 unlinkat Access is denied。
	t.Cleanup(mgr.StopAll)
	return &pkgTestEnv{main: main, reg: reg, mgr: mgr, svc: svc, priv: priv, dataDir: dataDir}
}

// buildPkg 把 fake-mcp 打包为 zip（signed=false 产出未签名包）。
func (e *pkgTestEnv) buildPkg(t *testing.T, id, version string, signed bool) string {
	t.Helper()
	m := toolapi.Manifest{
		ID:              id,
		Name:            "Fake 包 " + id,
		Version:         version,
		Kind:            toolapi.KindPackage,
		Description:     "测试工具包",
		DefaultTier:     toolapi.TierDeferred,
		ProtocolVersion: toolapi.ProtocolVersion,
		Tools: []toolapi.Spec{
			{Name: "echo", Description: "回显", Tier: toolapi.TierDeferred, Risk: toolapi.RiskReadonly},
		},
		Meta: map[string]any{},
	}
	var priv ed25519.PrivateKey
	if signed {
		priv = e.priv
	}
	raw, err := market.BuildPackage(m, fakeMCPPkgBin, priv)
	if err != nil {
		t.Fatalf("打包失败: %v", err)
	}
	p := filepath.Join(t.TempDir(), id+"-"+version+".zip")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPackageUnsignedRejected 出口标准①：装未签名包被拒。
func TestPackageUnsignedRejected(t *testing.T) {
	e := newPkgTestEnv(t)
	zipPath := e.buildPkg(t, "unsigned-pkg", "1.0.0", false)
	_, err := e.svc.Install(context.Background(), zipPath)
	if err == nil {
		t.Fatalf("未签名包必须被拒（出口标准）")
	}
	// 错误必须机器可判（ErrUnsigned）
	if !containsErrText(err, "未签名") {
		t.Fatalf("错误文案不可读: %v", err)
	}
	// 注册表与库都必须零残留
	if got := len(e.reg.Specs()); got != 0 {
		t.Fatalf("拒装后注册表必须为空，实际 %d", got)
	}
}

// TestPackageBadSignatureRejected 签名非法（换钥）→ 硬拒，即使 allow_unsigned。
func TestPackageBadSignatureRejected(t *testing.T) {
	e := newPkgTestEnv(t)
	e.svc.allowUnsigned = true
	// 用另一把私钥签名
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	saved := e.priv
	e.priv = other
	zipPath := e.buildPkg(t, "bad-sig", "1.0.0", true)
	e.priv = saved
	if _, err := e.svc.Install(context.Background(), zipPath); err == nil {
		t.Fatalf("签名校验失败的包必须硬拒（不受 allow_unsigned 影响）")
	}
}

// TestPackageInstallCallUninstall 出口标准②③：子进程隔离生效 + 卸载后索引一致。
func TestPackageInstallCallUninstall(t *testing.T) {
	ctx := context.Background()
	e := newPkgTestEnv(t)
	zipPath := e.buildPkg(t, "demo", "1.0.0", true)

	// ① 安装：注册表出现 pkg__demo__echo
	res, err := e.svc.Install(ctx, zipPath)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if !res.Signed || res.Tools != 1 {
		t.Fatalf("安装结果错误: %+v", res)
	}
	toolName := "pkg__demo__echo"
	tool, spec, err := e.reg.Resolve(toolName)
	if err != nil {
		t.Fatalf("安装后注册表缺少 %s: %v（现有 %v）", toolName, err, specNames(e.reg.Specs()))
	}
	if spec.Namespace != "pkg:demo" {
		t.Fatalf("命名空间错误: %q", spec.Namespace)
	}

	// ② 调用（子进程 stdio 全链路）
	callRes, err := tool.Invoke(ctx, &toolapi.Call{Args: []byte(`{"text":"hello"}`)})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if callRes.IsError || callRes.Content != "echo: hello" {
		t.Fatalf("调用结果错误: %+v", callRes)
	}

	// ③ 注入索引包含该工具（PromptRefs）
	if !containsSpecName(e.reg.InjectedSpecs(), toolName) {
		t.Fatalf("注入索引缺少 %s", toolName)
	}

	// ④ 落库
	row, err := e.main.Packages().Get(ctx, "demo")
	if err != nil || row == nil || row.Trust != "signed" || row.Version != "1.0.0" {
		t.Fatalf("落库错误: %+v %v", row, err)
	}

	// ⑤ 卸载：索引一致（注册表、库、目录三清）
	if err := e.svc.Uninstall(ctx, "demo"); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if _, _, err := e.reg.Resolve(toolName); err == nil {
		t.Fatalf("卸载后工具必须从注册表移除")
	}
	if containsSpecName(e.reg.InjectedSpecs(), toolName) {
		t.Fatalf("卸载后注入索引必须一致（不含已卸载工具）")
	}
	if row2, _ := e.main.Packages().Get(ctx, "demo"); row2 != nil {
		t.Fatalf("卸载后库行必须删除")
	}
	if _, err := os.Stat(filepath.Join(e.svc.PackagesDir(), "demo")); !os.IsNotExist(err) {
		t.Fatalf("卸载后安装目录必须清理")
	}
}

// TestPackageUpgradeRollback 升级保留旧版本 + 回滚换回。
func TestPackageUpgradeRollback(t *testing.T) {
	ctx := context.Background()
	e := newPkgTestEnv(t)
	v1 := e.buildPkg(t, "up", "1.0.0", true)
	if _, err := e.svc.Install(ctx, v1); err != nil {
		t.Fatalf("安装 v1 失败: %v", err)
	}
	v2 := e.buildPkg(t, "up", "2.0.0", true)
	if _, err := e.svc.Install(ctx, v2); err != nil {
		t.Fatalf("升级 v2 失败: %v", err)
	}
	row, _ := e.main.Packages().Get(ctx, "up")
	if row == nil || row.Version != "2.0.0" || row.PreviousVersion != "1.0.0" {
		t.Fatalf("升级后行错误: %+v", row)
	}
	// 旧版本目录保留
	if _, err := os.Stat(filepath.Join(e.svc.PackagesDir(), "up", "1.0.0")); err != nil {
		t.Fatalf("旧版本目录必须保留（回滚依赖）")
	}
	// 回滚 → 1.0.0
	v, err := e.svc.Rollback(ctx, "up")
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if v.Version != "1.0.0" {
		t.Fatalf("回滚后版本错误: %+v", v)
	}
	// 回滚后工具可调用
	tool, _, err := e.reg.Resolve("pkg__up__echo")
	if err != nil {
		t.Fatalf("回滚后工具缺失: %v", err)
	}
	if res, err := tool.Invoke(ctx, &toolapi.Call{Args: []byte(`{"text":"v1"}`)}); err != nil || res.Content != "echo: v1" {
		t.Fatalf("回滚后调用失败: %v %+v", err, res)
	}
}

// TestPackageDisableEnable 启停：禁用移出注册表（保留安装），启用恢复。
func TestPackageDisableEnable(t *testing.T) {
	ctx := context.Background()
	e := newPkgTestEnv(t)
	if _, err := e.svc.Install(ctx, e.buildPkg(t, "tog", "1.0.0", true)); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetEnabled(ctx, "tog", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.reg.Resolve("pkg__tog__echo"); err == nil {
		t.Fatalf("禁用后工具必须移出注册表")
	}
	if err := e.svc.SetEnabled(ctx, "tog", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.reg.Resolve("pkg__tog__echo"); err != nil {
		t.Fatalf("启用后工具必须恢复: %v", err)
	}
}

// TestPackageRestoreAll 重启恢复：重新连接已启用包。
func TestPackageRestoreAll(t *testing.T) {
	ctx := context.Background()
	e := newPkgTestEnv(t)
	if _, err := e.svc.Install(ctx, e.buildPkg(t, "boot", "1.0.0", true)); err != nil {
		t.Fatal(err)
	}
	// 模拟重启：注销 + 停连接（行与目录仍在）
	_ = e.reg.UnregisterPlugin("pkg:boot")
	_ = e.mgr.Stop("boot")
	if _, _, err := e.reg.Resolve("pkg__boot__echo"); err == nil {
		t.Fatal("注销后不应可解析")
	}
	// RestoreAll → 恢复
	e.svc.RestoreAll(ctx)
	if _, _, err := e.reg.Resolve("pkg__boot__echo"); err != nil {
		t.Fatalf("重启恢复失败: %v", err)
	}
}

func containsErrText(err error, sub string) bool {
	return err != nil && len(sub) > 0 && indexOfStr(err.Error(), sub) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func containsSpecName(specs []toolapi.Spec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

func specNames(specs []toolapi.Spec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}
