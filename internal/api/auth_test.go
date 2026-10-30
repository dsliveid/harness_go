package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

const (
	testUsername = "admin"
	testPassword = "test-password-123"
)

// noRedirectClient 不自动跟随 302，便于断言重定向目标。
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// 登录成功：返回令牌 + 写 HttpOnly Cookie。
func TestLoginSuccess(t *testing.T) {
	srv, _ := newTestServer(t)

	resp := postJSON(t, noRedirectClient(), srv.URL+"/api/v1/auth/login", "",
		map[string]any{"username": testUsername, "password": testPassword})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("登录应成功，得到 %d: %s", resp.StatusCode, raw)
	}

	var body struct {
		User struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.User.Username != testUsername || body.User.Role != "owner" {
		t.Fatalf("用户信息错误: %+v", body.User)
	}
	if !strings.HasPrefix(body.Token, "hgt_") {
		t.Fatalf("令牌格式错误: %s", body.Token)
	}

	// Cookie 必须 HttpOnly + SameSite=Lax
	var sess *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("应设置会话 Cookie")
	}
	if !sess.HttpOnly {
		t.Fatal("会话 Cookie 必须 HttpOnly")
	}
	if sess.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite 应为 Lax，得到 %v", sess.SameSite)
	}
}

// 登录失败统一 401，且「用户不存在」与「口令错误」返回完全相同的错误码（防枚举）。
func TestLoginFailureIsIndistinguishable(t *testing.T) {
	srv, _ := newTestServer(t)

	readCode := func(username, password string) (int, string) {
		resp := postJSON(t, noRedirectClient(), srv.URL+"/api/v1/auth/login", "",
			map[string]any{"username": username, "password": password})
		defer resp.Body.Close()
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Error.Code
	}

	st1, code1 := readCode(testUsername, "wrong-password")
	st2, code2 := readCode("no-such-user", "whatever")
	if st1 != http.StatusUnauthorized || st2 != http.StatusUnauthorized {
		t.Fatalf("两种情况都应 401，得到 %d / %d", st1, st2)
	}
	if code1 != code2 {
		t.Fatalf("错误码必须一致（防枚举），得到 %q vs %q", code1, code2)
	}
	if code1 != "auth.invalid_credentials" {
		t.Fatalf("错误码应为 auth.invalid_credentials，得到 %q", code1)
	}
}

// Cookie 可作为鉴权凭据（SSO 与网页登录的默认通道）。
func TestCookieAuthWorks(t *testing.T) {
	srv, _ := newTestServer(t)
	client := noRedirectClient()

	resp := postJSON(t, client, srv.URL+"/api/v1/auth/login", "",
		map[string]any{"username": testUsername, "password": testPassword})
	defer resp.Body.Close()
	var sess *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("登录未返回 Cookie")
	}

	// 带 Cookie 访问 me
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/auth/me", nil)
	req.AddCookie(sess)
	meResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("Cookie 鉴权应成功，得到 %d", meResp.StatusCode)
	}
	var me struct {
		Username string `json:"username"`
	}
	_ = json.NewDecoder(meResp.Body).Decode(&me)
	if me.Username != testUsername {
		t.Fatalf("me 返回用户错误: %+v", me)
	}

	// 登出后 Cookie 失效（登出请求需手动带 Cookie：client 无 cookie jar）
	logoutReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/auth/logout", nil)
	logoutReq.AddCookie(sess)
	logoutResp, err := client.Do(logoutReq)
	if err != nil {
		t.Fatal(err)
	}
	logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("登出应 204，得到 %d", logoutResp.StatusCode)
	}
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/auth/me", nil)
	req2.AddCookie(sess)
	meResp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	meResp2.Body.Close()
	if meResp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登出后应 401，得到 %d", meResp2.StatusCode)
	}
}

// 改口令：旧口令错 401、新口令过短 400、成功 204。
func TestChangePassword(t *testing.T) {
	srv, token := newTestServer(t)

	resp := postJSON(t, noRedirectClient(), srv.URL+"/api/v1/auth/password", token,
		map[string]any{"old_password": "wrong", "new_password": "new-password-123"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("旧口令错误应 401，得到 %d", resp.StatusCode)
	}

	resp = postJSON(t, noRedirectClient(), srv.URL+"/api/v1/auth/password", token,
		map[string]any{"old_password": testPassword, "new_password": "short"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("新口令过短应 400，得到 %d", resp.StatusCode)
	}

	resp = postJSON(t, noRedirectClient(), srv.URL+"/api/v1/auth/password", token,
		map[string]any{"old_password": testPassword, "new_password": "new-password-123"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("改密应 204，得到 %d", resp.StatusCode)
	}

	// 新口令可登录
	resp = postJSON(t, noRedirectClient(), srv.URL+"/api/v1/auth/login", "",
		map[string]any{"username": testUsername, "password": "new-password-123"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("新口令应可登录，得到 %d", resp.StatusCode)
	}
}

// ============ SSO 免登 ============

// 核心铁律：带凭据的 URL 必须 302 到**不含凭据的干净地址**，并写 HttpOnly Cookie。
func TestSSOLoginRedirectsAndStripsCredentials(t *testing.T) {
	srv, _ := newTestServer(t)
	client := noRedirectClient()

	url := srv.URL + "/?user_name=" + testUsername + "&pwd=" + testPassword
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("应 302，得到 %d: %s", resp.StatusCode, raw)
	}
	loc := resp.Header.Get("Location")
	if loc != "/" {
		t.Fatalf("应重定向到 /，得到 %q", loc)
	}
	if strings.Contains(loc, "pwd") || strings.Contains(loc, testPassword) {
		t.Fatalf("重定向地址不得包含凭据: %q", loc)
	}
	// 防 Referer 泄露
	if resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("应设置 Referrer-Policy: no-referrer，得到 %q", resp.Header.Get("Referrer-Policy"))
	}
	// Cookie
	var sess *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			sess = c
		}
	}
	if sess == nil || !sess.HttpOnly {
		t.Fatal("SSO 必须写 HttpOnly 会话 Cookie")
	}

	// 用该 Cookie 直接访问 me（免登生效）
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/auth/me", nil)
	req.AddCookie(sess)
	meResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("SSO Cookie 应可直接访问 me，得到 %d", meResp.StatusCode)
	}
}

// 免登失败：也 302 到干净地址（地址栏立刻干净），且 URL 不含失败原因。
func TestSSOLoginFailureStripsCredentials(t *testing.T) {
	srv, _ := newTestServer(t)
	client := noRedirectClient()

	resp, err := client.Get(srv.URL + "/?user_name=" + testUsername + "&pwd=wrong-password")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("失败也应 302（清地址栏），得到 %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/?sso_error=1" {
		t.Fatalf("应重定向到 /?sso_error=1，得到 %q", loc)
	}
	if strings.Contains(loc, "wrong-password") || strings.Contains(loc, "invalid") {
		t.Fatalf("不得在 URL 中泄露失败原因: %q", loc)
	}
	// 失败不应写会话 Cookie
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			t.Fatal("失败不应设置有效会话 Cookie")
		}
	}
}

// 站内 redirect 生效，站外被拒（防开放重定向）。
func TestSSORedirectIsRestrictedToInternal(t *testing.T) {
	srv, _ := newTestServer(t)
	client := noRedirectClient()

	resp, _ := client.Get(srv.URL + "/?user_name=" + testUsername + "&pwd=" + testPassword + "&redirect=/sessions/1")
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/sessions/1" {
		t.Fatalf("站内 redirect 应生效，得到 %q", loc)
	}

	for _, bad := range []string{"//evil.com", "https://evil.com", "/\\evil.com"} {
		resp, err := client.Get(srv.URL + "/?user_name=" + testUsername + "&pwd=" + testPassword + "&redirect=" + bad)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); loc != "/" {
			t.Fatalf("站外 redirect 应被拒（%s），得到 %q", bad, loc)
		}
	}
}

// 无凭据访问 / 返回提示页（A 阶段前端未迁入）。
func TestRootWithoutCredentials(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得到 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("应为 HTML，得到 %s", ct)
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("HTML 响应应设置 Referrer-Policy")
	}
}

// 前端兜底路径：POST /auth/sso 同样写 Cookie。
func TestSSOLoginEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	client := noRedirectClient()

	resp := postJSON(t, client, srv.URL+"/api/v1/auth/sso", "",
		map[string]any{"user_name": testUsername, "pwd": testPassword})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("兜底 SSO 应成功，得到 %d: %s", resp.StatusCode, raw)
	}
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("兜底 SSO 也应写会话 Cookie")
	}
}

// ============ 辅助 ============

func postJSON(t *testing.T, client *http.Client, url, token string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(http.MethodPost, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s 失败: %v", url, err)
	}
	return resp
}
