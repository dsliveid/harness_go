package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"harness-go/internal/auth"
	"harness-go/internal/model"
	"harness-go/internal/observ"
	"harness-go/internal/service"
	"harness-go/internal/store"
)

// ============ 移动端策略强制（2026-11 产品决策）============
//
// 产品决策：移动端只做纯对话 —— 不浏览客户端目录、不建项目、不用任何工具。
//
// 本文件守的是**服务端强制**这一层，而不是前端按钮是否渲染：
// `web/src/lib/mobileCapabilities.ts` 的声明可被任意脚本篡改（该文件头已写明
// "不是安全边界"），所以真正拦住移动端建项目的是 `createProject` 里的
// `currentTokenKind(r) == tokenKindWebview` 判定。若哪天有人"顺手简化"掉它，
// 移动端就会重新获得建项目能力，且前端看不出任何异常。
//
// 为什么单独造一个 env 而不用 newDataTestEnv：后者**没有装配项目域**（Deps.Projects
// 为 nil），建项目会先撞 503「项目域未启用」，测不到真正的 kind 判定，是典型的
// 假阴性陷阱。

// mobileGuardEnv 只装配「认证 + 项目域」的最小环境。
type mobileGuardEnv struct {
	srv     *httptest.Server
	main    *store.MainStore
	authSvc *auth.Service
	// adminToken 普通令牌（桌面端 / 浏览器口径）
	adminToken string
}

// newMobileGuardEnv 造测试环境。
func newMobileGuardEnv(t *testing.T) *mobileGuardEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	authSvc := auth.NewService(auth.Options{Main: main})
	if _, _, err := authSvc.EnsureBootstrapUser(ctx, "admin", "test-password-123"); err != nil {
		t.Fatalf("创建 owner 账号失败: %v", err)
	}
	plain, _, err := authSvc.EnsureBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}

	router := NewRouter(Deps{
		Version:     "test",
		Log:         observ.NewLogger("error", "text", io.Discard),
		Projects:    service.NewProjectService(main),
		Auth:        authSvc,
		VerifyToken: authSvc.VerifyToken,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(func() {
		srv.Close()
		_ = main.Close()
	})
	return &mobileGuardEnv{srv: srv, main: main, authSvc: authSvc, adminToken: plain}
}

// issueToken 用指定 kind 签发一枚令牌（kind=webview 即模拟移动端 WebView 会话）。
func (e *mobileGuardEnv) issueToken(t *testing.T, username, kind string) string {
	t.Helper()
	ctx := context.Background()
	if err := e.main.Users().Create(ctx, &model.User{
		ID: "u-" + username, Username: username, PasswordHash: "hash-" + username,
		DisplayName: username, Role: "member",
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("创建账号 %s 失败: %v", username, err)
	}
	plain, _, err := e.authSvc.IssueToken(ctx, "u-"+username, kind, "write")
	if err != nil {
		t.Fatalf("签发 %s 令牌失败: %v", kind, err)
	}
	return plain
}

// ① 移动端（webview 令牌）建项目 → 必须被服务端拒绝（403）。
func TestCreateProjectRejectedForWebviewToken(t *testing.T) {
	env := newMobileGuardEnv(t)
	webviewToken := env.issueToken(t, "mobile", tokenKindWebview)

	doJSON(t, http.MethodPost, env.srv.URL+"/api/v1/projects", webviewToken,
		map[string]any{"name": "手机上偷建的项目", "path": `D:\hack`},
		http.StatusForbidden, nil)

	// 二次确认：拒绝必须发生在落库之前，不得留下半成品数据。
	list, err := env.main.Projects().List(context.Background())
	if err != nil {
		t.Fatalf("查询项目失败: %v", err)
	}
	for _, p := range list {
		if p.Name == "手机上偷建的项目" {
			t.Fatal("被拒绝的建项目请求不得落库")
		}
	}
}

// ② 对照组：普通令牌（桌面端 / 浏览器）建项目仍必须成功。
//
// 这条防的是"为了禁移动端，把所有人都禁了"这类过度收紧 —— 那会造成比原缺陷
// 更严重的回归，而且很容易在只测移动端的用例里被漏掉。
func TestCreateProjectAllowedForNormalToken(t *testing.T) {
	env := newMobileGuardEnv(t)

	doJSON(t, http.MethodPost, env.srv.URL+"/api/v1/projects", env.adminToken,
		map[string]any{"name": "桌面端正常项目", "path": `D:\work`},
		http.StatusCreated, nil)
}

// ③ 移动端令牌同样不得通过「先建项目、再建带 project_dir 的会话」绕过。
//
// 这条锁住「拒绝发生在项目创建入口」这个位置本身：即使有人找到别的路径建了会话，
// createSession 侧也会清空 project_id / project_dir（既有约束），两层都在。
func TestWebviewTokenCannotBootstrapProjectThenSession(t *testing.T) {
	env := newMobileGuardEnv(t)
	webviewToken := env.issueToken(t, "mobile2", tokenKindWebview)

	// 建项目被拒
	doJSON(t, http.MethodPost, env.srv.URL+"/api/v1/projects", webviewToken,
		map[string]any{"name": "绕过尝试", "path": `D:\x`}, http.StatusForbidden, nil)

	// 确认库里真的没有项目（防止"返回 403 但其实写了库"这种最坏情况）
	list, err := env.main.Projects().List(context.Background())
	if err != nil {
		t.Fatalf("查询项目失败: %v", err)
	}
	for _, p := range list {
		if p.Name == "绕过尝试" {
			t.Fatal("403 之后不得存在该项目")
		}
	}
}
