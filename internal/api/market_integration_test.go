package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"harness-go/internal/agent"
	"harness-go/internal/approval"
	"harness-go/internal/auth"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/market"
	"harness-go/internal/mcp"
	"harness-go/internal/observ"
	"harness-go/internal/plugins/echo"
	"harness-go/internal/prompt"
	"harness-go/internal/secret"
	"harness-go/internal/service"
	"harness-go/internal/store"
	"harness-go/internal/tokenizer"
	"harness-go/internal/tools"
	"harness-go/internal/tools/hostimpl"
	"harness-go/internal/tools/loader"
	"harness-go/internal/tools/registry"
	"harness-go/internal/tools/router"
	"harness-go/pkg/toolapi"
)

// marketFakeMCPBin TestMain 构建的 fake-mcp 可执行文件（协议 fixture）。
var marketFakeMCPBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "api-mcp-*")
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
	marketFakeMCPBin = bin
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// marketEnv 集成环境（注册表 + 受信签名私钥，供测试构造合法签名包）。
type marketEnv struct {
	reg        *registry.Registry
	signingKey ed25519.PrivateKey
}

// newMarketServerFull 组装带外部插件子系统 + 工具子系统的完整 HTTP 服务。
func newMarketServerFull(t *testing.T) (*httptest.Server, string, *marketEnv) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	mainStore, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	sessionStore, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 8})
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	bus := events.NewBus(1000, 256)
	gate := approval.NewGate(2 * time.Second)
	provider := llm.NewFakeProvider()
	provider.Delay = 0

	// 工具子系统（真实注册表 + echo 内置插件）
	toolReg := registry.New(registry.Options{CoreSoftLimit: 8, DeferredSoftLimit: 60})
	toolHost := hostimpl.New(hostimpl.Options{WorkDir: dir})
	if _, fail := registerBuiltinsForTest(ctx, toolReg, toolHost); fail != 0 {
		t.Fatalf("内置插件装配失败")
	}
	toolRouter := router.New(router.Options{Executor: toolapi.ExecutorServer})
	toolRunner := tools.NewRunner(toolReg, toolRouter, loader.Limits{CoreSoft: 8, DeferredSoft: 60})

	engine, err := agent.New(agent.Deps{
		Sessions: service.NewSessionProvider(sessionStore),
		Summary:  service.NewSummaryWriter(mainStore),
		Events:   bus,
		Models:   &testResolver{provider: provider},
		Builder:  prompt.NewBuilder(tokenizer.NewHeuristic(), prompt.DefaultLimits()),
		Tools:    toolRunner,
		Catalog:  toolRunner,
		Gate:     gate,
		MaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("创建引擎失败: %v", err)
	}

	tokenSvc := auth.NewService(auth.Options{Main: mainStore})
	if _, _, err := tokenSvc.EnsureBootstrapUser(ctx, "admin", "test-password-123"); err != nil {
		t.Fatal(err)
	}
	plain, _, err := tokenSvc.EnsureBootstrapToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	cipher, err := secret.New(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}

	// 外部插件子系统（受信密钥对由测试环境持有：pub 注入 svc，priv 供测试签名）
	signPub, signPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mcpMgr := mcp.NewManager(mcp.Options{ClientName: "api-test", ClientVersion: "0"},
		mcp.DeploymentPolicy{Mode: "local"})
	mcpSvc := service.NewMCPService(service.MCPServiceOptions{
		Main: mainStore, Manager: mcpMgr, Registry: toolReg, Cipher: cipher,
		Dep: mcp.DeploymentPolicy{Mode: "local"},
	})
	pkgSvc := service.NewPackageService(service.PackageServiceOptions{
		Main: mainStore, Manager: mcpMgr, Registry: toolReg,
		DataDir: dir, TrustedKeys: []ed25519.PublicKey{signPub},
	})
	sessionSvc := service.NewSessionService(mainStore, sessionStore)
	runSvc := service.NewRunService(service.RunServiceOptions{
		Main: mainStore, Sessions: sessionStore, Engine: engine,
		SystemBase: "你是测试助手", Gate: gate,
		ToolRefs: toolRunner.PromptRefs,
	})
	maintenanceSvc := service.NewMaintenanceService(mainStore, sessionStore, runSvc, gate.PendingCount)
	toolSvc := service.NewToolService(toolRunner, mainStore)
	providerSvc := service.NewProviderService(service.ProviderServiceOptions{Main: mainStore, Cipher: cipher})
	settingsSvc := service.NewSettingsService(mainStore)
	logSvc := service.NewLogService("", 100)

	logger := observ.NewLogger("error", "text", io.Discard)
	router := NewRouter(Deps{
		Version: "test", Log: logger,
		Sessions: sessionSvc, Runs: runSvc, Maintenance: maintenanceSvc,
		Tools: toolSvc, Providers: providerSvc, Settings: settingsSvc, Logs: logSvc,
		MCP: mcpSvc, Packages: pkgSvc,
		Bus: bus, Auth: tokenSvc,
		AuthConfig: AuthConfig{
			SSOEnabled: true, SSOMode: "plain", SessionTTL: time.Hour,
			SSOLimiter: auth.NewLimiter(5, time.Minute),
		},
		VerifyToken: tokenSvc.VerifyToken,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(func() {
		srv.Close()
		mcpMgr.StopAll()
		bus.Close()
		_ = sessionStore.Close()
		_ = mainStore.Close()
	})
	return srv, plain, &marketEnv{reg: toolReg, signingKey: signPriv}
}

// registerBuiltinsForTest 装配内置 echo 插件（避免依赖 main 包的装配点）。
func registerBuiltinsForTest(ctx context.Context, reg *registry.Registry, host toolapi.Host) (int, int) {
	okCount, failCount := 0, 0
	for _, f := range []toolapi.Factory{echo.Factory()} {
		if err := reg.RegisterFactory(ctx, f, host, nil); err != nil {
			failCount++
			continue
		}
		okCount++
	}
	return okCount, failCount
}

// TestMCPServerEndpointsLifecycle 端到端：创建启动 → 脱敏列表 → 健康 → 停用 → 删除。
func TestMCPServerEndpointsLifecycle(t *testing.T) {
	srv, token, _ := newMarketServerFull(t)

	var created struct {
		ID      string   `json:"id"`
		State   string   `json:"state"`
		EnvKeys []string `json:"env_keys"`
	}
	body := map[string]any{
		"name": "E2E 服务", "transport": "stdio",
		"command": marketFakeMCPBin,
		"env":     map[string]string{"SECRET_TOKEN": "sekrit-value"},
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/mcp/servers?start=true", token, body, http.StatusCreated, &created)
	if created.ID == "" || created.State != "ready" {
		t.Fatalf("创建启动失败: %+v", created)
	}
	if len(created.EnvKeys) != 1 || created.EnvKeys[0] != "SECRET_TOKEN" {
		t.Fatalf("env 必须只回键名: %+v", created.EnvKeys)
	}

	var list struct {
		Items []map[string]any `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/mcp/servers", token, nil, http.StatusOK, &list)
	if len(list.Items) != 1 {
		t.Fatalf("列表错误: %+v", list)
	}
	raw, _ := json.Marshal(list)
	if containsStr(string(raw), "sekrit-value") {
		t.Fatalf("密钥泄漏进列表响应")
	}

	var health struct {
		State     string `json:"state"`
		ToolCount int    `json:"tool_count"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/mcp/servers/"+created.ID+"/health", token, nil, http.StatusOK, &health)
	if health.State != "ready" || health.ToolCount != 3 {
		t.Fatalf("健康详情错误: %+v", health)
	}

	doJSON(t, http.MethodPost, srv.URL+"/api/v1/mcp/servers/"+created.ID+"/disable", token, nil, http.StatusOK, nil)
	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/mcp/servers/"+created.ID, token, nil, http.StatusNoContent, nil)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/mcp/servers/"+created.ID, token, nil, http.StatusNotFound, nil)
}

// TestPackageEndpointsLifecycle 出口标准端到端：
// 装未签名包被拒 → 安装签名包 → 工具进注入索引 → 卸载 → 索引一致。
func TestPackageEndpointsLifecycle(t *testing.T) {
	srv, token, env := newMarketServerFull(t)

	// 未签名包：构建不带签名 → 安装必须 422（装未签名包被拒）
	unsignedZip := filepath.Join(t.TempDir(), "unsigned.zip")
	rawUnsigned, err := market.BuildPackage(pkgManifestForAPI(), marketFakeMCPBin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unsignedZip, rawUnsigned, 0o600); err != nil {
		t.Fatal(err)
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/packages/install", token,
		map[string]any{"path": unsignedZip}, http.StatusBadRequest, nil)

	// 签名包：用 env 暴露的受信私钥签名（newMarketServerFull 提供）
	signedZip := filepath.Join(t.TempDir(), "signed.zip")
	rawSigned, err := market.BuildPackage(pkgManifestForAPI(), marketFakeMCPBin, env.signingKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signedZip, rawSigned, 0o600); err != nil {
		t.Fatal(err)
	}
	var installed struct {
		View   map[string]any `json:"view"`
		Signed bool           `json:"signed"`
		Tools  int            `json:"tools"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/packages/install", token,
		map[string]any{"path": signedZip}, http.StatusCreated, &installed)
	if !installed.Signed || installed.Tools != 1 {
		t.Fatalf("安装结果错误: %+v", installed)
	}

	// 注入索引包含包工具
	before := countTools(t, srv, token)
	if before < 2 {
		t.Fatalf("工具列表应含包工具（builtin echo + 包 echo）: %d", before)
	}

	// 卸载 → 索引一致
	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/packages/e2e-pkg", token, nil, http.StatusNoContent, nil)
	after := countTools(t, srv, token)
	if after >= before {
		t.Fatalf("卸载后工具数必须回落: %d -> %d", before, after)
	}
}

// pkgManifestForAPI 集成测试包清单。
func pkgManifestForAPI() toolapi.Manifest {
	return toolapi.Manifest{
		ID: "e2e-pkg", Name: "E2E 包", Version: "1.0.0", Kind: toolapi.KindPackage,
		Description: "端到端测试包", DefaultTier: toolapi.TierDeferred,
		ProtocolVersion: toolapi.ProtocolVersion,
		Tools: []toolapi.Spec{
			{Name: "echo", Description: "回显", Tier: toolapi.TierDeferred, Risk: toolapi.RiskReadonly},
		},
	}
}

func countTools(t *testing.T, srv *httptest.Server, token string) int {
	t.Helper()
	var out struct {
		Total int `json:"total"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/tools", token, nil, http.StatusOK, &out)
	return out.Total
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
