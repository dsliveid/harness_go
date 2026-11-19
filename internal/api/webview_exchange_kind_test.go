package api

import (
	"bytes"
	"context"
	"encoding/json"
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

// ============ ticket 兑换的令牌档位必须按「设备形态」裁决（2026-11 回归守卫）============
//
// ## 这组测试守的是什么缺陷
//
// 实测现象（用户报告）：桌面端在「服务端工作空间」建项目，弹出「已创建 + 创建失败」
// 两条矛盾提示，且服务端最终没有这个项目；同一操作在浏览器里完全正常。
//
// 根因：桌面端进 Web UI 走的是 ticket 免登（harness_desktop gui.rs
// `prepare_web_ui_entry` → `#ticket=...`），而服务端 `exchangeWebviewTicket` 原先
// **无条件**签发 kind=webview 的降级令牌。于是桌面端在 WebView 里的每个请求都被
// `handler_project.go:createProject` 判成「移动端」而 403；浏览器走表单登录拿
// kind=web_session，故不受影响 —— 这就是两端表现差异的全部来源。
//
// ## 为什么不能只测移动端（本文件的存在理由）
//
// 既有的 `project_mobile_guard_test.go` 只断言「webview 令牌建项目必须 403」。
// 若把兑换逻辑改成「一律放行」，那条测试**依然全绿**（它自己手工签的 webview 令牌
// 不经过兑换端点），缺陷与回归都测不到。因此必须双向都测：
//   - 桌面端设备兑换 → 令牌必须是 web_session 且能建项目（本文件 ①）；
//   - 移动端设备兑换 → 令牌必须仍是 webview 且建项目 403（本文件 ②，防过度放宽）；
//   - 判定依据失效时（设备查不到 / deviceID 为空 / 设备域未装配）→ 保守降级（③④⑤）。

// exchangeKindEnv ticket 兑换测试环境：认证 + 项目域 + 设备域。
type exchangeKindEnv struct {
	srv     *httptest.Server
	main    *store.MainStore
	authSvc *auth.Service
	userID  string
}

// newExchangeKindEnv 造环境。withDevices=false 用于验证「设备域未装配时保守降级」。
func newExchangeKindEnv(t *testing.T, withDevices bool) *exchangeKindEnv {
	t.Helper()
	ctx := context.Background()

	// 主库与会话库必须共用同一 dataDir（与生产一致：sessions/<id>/session.db 落在 data 下）。
	dir := t.TempDir()
	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	authSvc := auth.NewService(auth.Options{Main: main})
	if _, _, err := authSvc.EnsureBootstrapUser(ctx, "admin", "test-password-123"); err != nil {
		t.Fatalf("创建 owner 账号失败: %v", err)
	}
	user, err := main.Users().GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("查询 owner 账号失败: %v", err)
	}

	// 会话分库：①「建项目」用例不依赖它，但 ①' 要验证「项目内建会话继承 full」，
	// 而该继承逻辑正是被 webview 降级抹掉的那一环（project_id 被清空 → 继承分支进不去）。
	// Create 只写主库，但既有签名要求传入会话库，且后续 run/消息链路也依赖它。
	sessionStore, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 4})
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })

	deps := Deps{
		Version:     "test",
		Log:         observ.NewLogger("error", "text", io.Discard),
		Projects:    service.NewProjectService(main),
		Auth:        authSvc,
		VerifyToken: authSvc.VerifyToken,
		Sessions:    service.NewSessionService(main, sessionStore),
	}
	if withDevices {
		deps.Devices = service.NewDeviceService(service.DeviceServiceOptions{
			Main:    main,
			Enabled: true,
		})
	}

	srv := httptest.NewServer(NewRouter(deps))
	t.Cleanup(func() {
		srv.Close()
		_ = main.Close()
	})
	return &exchangeKindEnv{srv: srv, main: main, authSvc: authSvc, userID: user.ID}
}

// createDevice 直接落一条设备记录（绕开配对码流程，只关心 kind 字段）。
func (e *exchangeKindEnv) createDevice(t *testing.T, id string, kind model.DeviceKind) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := e.main.Devices().Create(context.Background(), &model.Device{
		ID:              id,
		Name:            id,
		Kind:            kind,
		Platform:        string(kind),
		Status:          model.DeviceOnline,
		TokenHash:       "hash-" + id,
		ProtocolVersion: 1,
		UserID:          e.userID,
		PairedAt:        now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("创建设备 %s 失败: %v", id, err)
	}
}

// userToken 取一枚可用的用户会话令牌（签到 ticket 需要已登录用户）。
//
// 用 EnsureBootstrapToken 而非手工 IssueToken：它保证与生产 bootstrap 同源，
// 且 kind 是 api（不受 webview 降级判定影响，正合本文件用途）。
func (e *exchangeKindEnv) userToken(t *testing.T) string {
	t.Helper()
	plain, _, err := e.authSvc.EnsureBootstrapToken(context.Background())
	if err != nil {
		t.Fatalf("签发用户令牌失败: %v", err)
	}
	return plain
}

// issueTicket 用用户会话签发一张 ticket（等价于桌面端/移动端原生侧的动作）。
func (e *exchangeKindEnv) issueTicket(t *testing.T, userToken, deviceID string) string {
	t.Helper()
	rec := struct {
		Ticket string `json:"ticket"`
	}{}
	doJSON(t, http.MethodPost, e.srv.URL+"/api/v1/auth/webview-ticket", userToken,
		map[string]any{"device_id": deviceID}, http.StatusOK, &rec)
	if rec.Ticket == "" {
		t.Fatal("签发 ticket 返回空串")
	}
	return rec.Ticket
}

// exchange 用 ticket 兑换会话 Cookie，返回 Cookie 中的令牌明文。
func (e *exchangeKindEnv) exchange(t *testing.T, ticket string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"ticket": ticket})
	if err != nil {
		t.Fatalf("序列化兑换请求失败: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/auth/webview-exchange",
		bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("构造兑换请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("兑换请求失败: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("兑换应成功，实得 %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			return c.Value
		}
	}
	t.Fatal("兑换响应未下发 hg_sess Cookie")
	return ""
}

// tokenKindOf 查令牌 kind（直接读库，避免再用 HTTP 绕一圈）。
func (e *exchangeKindEnv) tokenKindOf(t *testing.T, plain string) string {
	t.Helper()
	kind, err := e.authSvc.TokenKind(context.Background(), plain)
	if err != nil {
		t.Fatalf("查询令牌 kind 失败: %v", err)
	}
	return kind
}

// ① 桌面端设备兑换 ticket → 令牌必须是 web_session，且能用它建项目成功。
//
// 这是「桌面端建项目 403」缺陷的直接回归守卫：改回无条件 webview 时本用例必红。
func TestExchangeTicketForDesktopDeviceGrantsWebSession(t *testing.T) {
	env := newExchangeKindEnv(t, true)
	env.createDevice(t, "dev-desktop", model.DeviceKindDesktop)

	userToken := env.userToken(t)
	ticket := env.issueTicket(t, userToken, "dev-desktop")
	session := env.exchange(t, ticket)

	if kind := env.tokenKindOf(t, session); kind != model.TokenKindWebSession {
		t.Fatalf("桌面端兑换后令牌 kind 应为 %q，实得 %q（桌面端被误降级成移动端 WebView）",
			model.TokenKindWebSession, kind)
	}

	// 端到端：用兑换到的会话令牌建项目，必须 201（浏览器同款能力）。
	doJSON(t, http.MethodPost, env.srv.URL+"/api/v1/projects", session,
		map[string]any{"name": "桌面端建的项目", "path": `D:\work`}, http.StatusCreated, nil)

	list, err := env.main.Projects().List(context.Background())
	if err != nil {
		t.Fatalf("查询项目失败: %v", err)
	}
	found := false
	for _, p := range list {
		if p.Name == "桌面端建的项目" {
			found = true
		}
	}
	if !found {
		t.Fatal("桌面端建项目必须真正落库（不能只返回 201 不写库）")
	}
}

// ①' 桌面端兑换后的令牌在「项目内新建会话」时必须保留项目归属并继承 full。
//
// ============ 这是用户实际报告的那一环（2026-11 实测）============
//
// 现象：「项目设置 → 审批规则」里已设为完全访问，但该项目下新建对话仍是
// 「变更前确认」，顶栏锁图标，发消息真的弹审批条。
//
// 根因链（实测 data/main.db 得到，非推测）：
//
//	桌面端 ticket 兑换 → 令牌被误签发成 kind=webview
//	→ createSession 命中 `if currentTokenKind(r) == tokenKindWebview`
//	→ req.ProjectID = ""（项目归属被清空）+ req.AccessMode = "confirm"（权限锁死）
//	→ SessionService.Create 的「继承项目 default_access_mode」分支**只在
//	   ProjectID 非空时执行** → 分支根本进不去 → access_mode 兜底 confirm。
//
// 库里的铁证：真正挂上项目的会话 access_mode 全是 full，
// 唯独那条「新对话」project_id='' 且 access_mode=confirm —— 项目归属在建会话
// 那一刻就被抹掉了，与项目设置里的 full 无关。
//
// 为什么必须单独测这一环：① 只验证了「能建项目」，若 createSession 的
// project_id 仍被清空，① 依然全绿而用户缺陷完好无损地存活。
func TestExchangeTicketForDesktopDeviceKeepsProjectOnSessionCreate(t *testing.T) {
	env := newExchangeKindEnv(t, true)
	env.createDevice(t, "dev-desktop", model.DeviceKindDesktop)

	session := env.exchange(t, env.issueTicket(t, env.userToken(t), "dev-desktop"))

	// ① 先建一个 default_access_mode=full 的项目（模拟用户在项目设置里设了完全访问）
	var proj struct {
		ID                string `json:"id"`
		DefaultAccessMode string `json:"default_access_mode"`
	}
	doJSON(t, http.MethodPost, env.srv.URL+"/api/v1/projects", session,
		map[string]any{"name": "全权项目", "path": `D:\full`, "default_access_mode": "full"},
		http.StatusCreated, &proj)
	if proj.ID == "" || proj.DefaultAccessMode != "full" {
		t.Fatalf("项目创建异常: %+v", proj)
	}

	// ② 在该项目内新建会话（**不传 access_mode**，走服务端继承）
	var sess struct {
		ID         string `json:"id"`
		AccessMode string `json:"access_mode"`
		ProjectID  string `json:"project_id"`
	}
	doJSON(t, http.MethodPost, env.srv.URL+"/api/v1/sessions", session,
		map[string]any{"title": "项目内新对话", "project_id": proj.ID},
		http.StatusCreated, &sess)

	// ③ 项目归属必须保留（被清空就是本缺陷的直接症状）
	if sess.ProjectID != proj.ID {
		t.Fatalf("桌面端建会话的项目归属被丢弃：want %q got %q（"+
			"这正是 webview 降级把 project_id 清空的症状）", proj.ID, sess.ProjectID)
	}
	// ④ 必须继承项目的 full，而不是回落 confirm
	if sess.AccessMode != "full" {
		t.Fatalf("项目内新建会话应继承项目默认 full，实得 %q（"+
			"用户看到的「顶栏锁图标 + 弹审批条」即源于此）", sess.AccessMode)
	}
}

// ② 移动端设备兑换 ticket → 令牌必须仍是 webview，建项目必须 403。
//
// 防的是「为了修桌面端而顺手把降级整段删掉」这类过度放宽 —— 那会让移动端
// 重新获得建项目能力，且前端（只隐藏按钮）看不出异常。
func TestExchangeTicketForMobileDeviceStaysWebview(t *testing.T) {
	env := newExchangeKindEnv(t, true)
	env.createDevice(t, "dev-phone", model.DeviceKindMobile)

	userToken := env.userToken(t)
	ticket := env.issueTicket(t, userToken, "dev-phone")
	session := env.exchange(t, ticket)

	if kind := env.tokenKindOf(t, session); kind != tokenKindWebview {
		t.Fatalf("移动端兑换后令牌 kind 应仍为 %q，实得 %q（移动端降级被误放开）",
			tokenKindWebview, kind)
	}
	doJSON(t, http.MethodPost, env.srv.URL+"/api/v1/projects", session,
		map[string]any{"name": "手机上偷建的项目", "path": `D:\hack`}, http.StatusForbidden, nil)
}

// ③ 设备查不到（如已被吊销/删除）→ 保守降级为 webview。
func TestExchangeTicketForUnknownDeviceFallsBackToWebview(t *testing.T) {
	env := newExchangeKindEnv(t, true)

	userToken := env.userToken(t)
	ticket := env.issueTicket(t, userToken, "dev-does-not-exist")
	session := env.exchange(t, ticket)

	if kind := env.tokenKindOf(t, session); kind != tokenKindWebview {
		t.Fatalf("设备查不到时必须保守降级，kind 应为 %q，实得 %q", tokenKindWebview, kind)
	}
}

// ④ 签票时未带 deviceID（空串）→ 保守降级为 webview。
func TestExchangeTicketWithoutDeviceIDFallsBackToWebview(t *testing.T) {
	env := newExchangeKindEnv(t, true)

	userToken := env.userToken(t)
	ticket := env.issueTicket(t, userToken, "")
	session := env.exchange(t, ticket)

	if kind := env.tokenKindOf(t, session); kind != tokenKindWebview {
		t.Fatalf("无 deviceID 时必须保守降级，kind 应为 %q，实得 %q", tokenKindWebview, kind)
	}
}

// ⑤ 设备域未装配（Deps.Devices == nil）→ 保守降级为 webview，且兑换本身不能 500。
//
// 这条守的是「可选子系统缺失不得让核心免登流程崩」的既有约定。
func TestExchangeTicketWithoutDeviceDomainFallsBackToWebview(t *testing.T) {
	env := newExchangeKindEnv(t, false)

	userToken := env.userToken(t)
	ticket := env.issueTicket(t, userToken, "dev-desktop")
	session := env.exchange(t, ticket)

	if kind := env.tokenKindOf(t, session); kind != tokenKindWebview {
		t.Fatalf("设备域未装配时必须保守降级，kind 应为 %q，实得 %q", tokenKindWebview, kind)
	}
}

// ⑥ sessionKindForDevice 的纯函数分支表（无需 HTTP，最快定位回归）。
func TestSessionKindForDevice(t *testing.T) {
	revoked := int64(1)
	cases := []struct {
		name  string
		dev   *model.Device
		wantK string
		wantS string
	}{
		{"桌面端", &model.Device{Kind: model.DeviceKindDesktop}, model.TokenKindWebSession, "write"},
		{"移动端", &model.Device{Kind: model.DeviceKindMobile}, tokenKindWebview, tokenKindWebview},
		{"平板", &model.Device{Kind: model.DeviceKindTablet}, tokenKindWebview, tokenKindWebview},
		{"浏览器设备", &model.Device{Kind: model.DeviceKindBrowser}, tokenKindWebview, tokenKindWebview},
		{"CLI", &model.Device{Kind: model.DeviceKindCLI}, tokenKindWebview, tokenKindWebview},
		{"kind 为空", &model.Device{}, tokenKindWebview, tokenKindWebview},
		{"nil 设备", nil, tokenKindWebview, tokenKindWebview},
		// 已吊销的桌面端设备不得再换取全能力会话（吊销即失效）。
		{"已吊销的桌面端", &model.Device{Kind: model.DeviceKindDesktop, RevokedAt: &revoked},
			tokenKindWebview, tokenKindWebview},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotK, gotS := sessionKindForDevice(c.dev)
			if gotK != c.wantK || gotS != c.wantS {
				t.Fatalf("sessionKindForDevice = (%q,%q)，期望 (%q,%q)",
					gotK, gotS, c.wantK, c.wantS)
			}
		})
	}
}
