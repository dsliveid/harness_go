package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"harness-go/internal/api"
	"harness-go/internal/apperr"
	"harness-go/internal/device"
)

// ============ 端侧扮演者（脚本视角） ============
//
// 本文件所有代码都站在「桌面端 / 移动端」的角度：只依赖 HTTP + WS 公开契约，
// 不 import 服务端内部装配（除协议帧结构 device.Envelope 与 apperr 错误体，
// 这两个是跨仓共用的线上契约，复用可保证字段名不写错）。

// maskToken 把长期凭证打码为「前 8 位 + ***」。
// 安全红线：device_token 是长期凭证，任何时候都不得完整出现在日志/输出里。
func maskToken(tok string) string {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "(空)"
	}
	if len(tok) <= 8 {
		return "***"
	}
	return tok[:8] + "***"
}

// apiError 是服务端统一错误体（apperr.Body）。失败路径解读全靠它。
type apiError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("HTTP %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// doJSON 发一个 JSON 请求并按需解出响应体；非 2xx 一律转成 *apiError。
func doJSON(ctx context.Context, cli *http.Client, method, urlStr, bearer string,
	body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("序列化请求体失败: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, rdr)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("请求 %s %s 失败: %w", method, urlStr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &apiError{StatusCode: resp.StatusCode}
		var b apperr.Body
		if jErr := json.Unmarshal(raw, &b); jErr == nil && b.Error.Code != "" {
			ae.Code, ae.Message = b.Error.Code, b.Error.Message
		} else {
			ae.Code = "unknown"
			ae.Message = strings.TrimSpace(string(raw))
		}
		return ae
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("解析响应 JSON 失败: %w（原文：%s）", err, truncate(string(raw), 200))
		}
	}
	return nil
}

// explainHTTP 把 HTTP 侧失败翻译成人话（含可执行建议）。
//
// 这是任务要求「失败路径要给人话」的落点：每个错误码都对应一句「你该去改什么」。
func explainHTTP(err error) string {
	ae := &apiError{}
	if !asAPIError(err, ae) {
		return err.Error()
	}
	switch ae.Code {
	case string(apperr.CodeDeviceDisabled):
		return "设备域未启用（503 device.disabled）→ 请在 config.yaml 中设置 devices.enabled: true 后重启服务端"
	case string(apperr.CodeDevicePairCodeInvalid):
		return "配对码无效/已消费/已过期（401 device.pair_code_invalid）→ 配对码 TTL 默认 120s，且只能用一次；请重新签发并在窗口内兑换"
	case string(apperr.CodeDeviceRevoked):
		return "设备令牌已被吊销（401 device.revoked）→ 该设备在管理端被删除，需要重新配对"
	case string(apperr.CodeUnauthorized):
		return "未授权（401）→ 令牌缺失/无效/过期。管理端接口需要用户令牌，WS 需要设备令牌（且必须经 ?token= 传入）"
	case string(apperr.CodeRateLimited), string(apperr.CodeAuthRateLimited):
		return "触发限流（429）→ 配对失败限流 6 次/分钟；稍等再试，或检查是否重复使用同一个配对码"
	case string(apperr.CodeAuthInvalidCredentials):
		return "账号或口令错误（401 auth.invalid_credentials）→ 检查 -user / -password"
	default:
		return fmt.Sprintf("%s（HTTP %d）→ %s", ae.Code, ae.StatusCode, ae.Message)
	}
}

// asAPIError 用 errors.As 语义取出 *apiError（避免本文件依赖 errors 包外扩）。
func asAPIError(err error, out *apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*out = *ae
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ============ HTTP 侧：登录 / 签发配对码 / 配对 ============

// login 用真实 /api/v1/auth/login 换用户令牌。
//
// 为什么不直接 EnsureBootstrapToken：走 HTTP 才能顺带验证「鉴权中间件 + 登录端点」这一段。
func login(ctx context.Context, cli *http.Client, baseURL, user, password string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := doJSON(ctx, cli, http.MethodPost, baseURL+"/api/v1/auth/login", "",
		map[string]any{"username": user, "password": password}, &out)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Token) == "" {
		return "", fmt.Errorf("登录成功但响应里没有 token 字段")
	}
	return out.Token, nil
}

// pingHealth 外部模式的前置探活（/healthz 免鉴权）。
//
// 提前探活的价值：把「忘了起服务端」「端口不对」「用了 localhost 结果解析到 ::1」这三类
// 环境问题，从后面某一步的莫名超时前移到第一步，并直接给出修复命令。
func pingHealth(ctx context.Context, cli *http.Client, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("构造探活请求失败: %w", err)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz 返回 HTTP %d（应 200）", resp.StatusCode)
	}
	return nil
}

// issuePairCode 签发 6 位配对码（管理端接口，需用户令牌）。
func issuePairCode(ctx context.Context, cli *http.Client, baseURL, userToken, name, kind string) (*servicePairCode, error) {
	var out servicePairCode
	err := doJSON(ctx, cli, http.MethodPost, baseURL+"/api/v1/devices/pair-codes", userToken,
		map[string]any{"name": name, "kind": kind}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// servicePairCode 对应 service.PairCodeView 的线上字段（code/expires_at/ttl_s/...）。
type servicePairCode struct {
	Code       string `json:"code"`
	ExpiresAt  int64  `json:"expires_at"`
	TTLSec     int    `json:"ttl_s"`
	DeviceName string `json:"device_name"`
	DeviceKind string `json:"device_kind"`
}

// pairResult 对应 service.PairResult（device_token 明文仅此一次）。
//
// 另含「配对后免二次登录」下发的用户会话三字段：UserToken/User/SessionTTLS。
// 它们是**可选**的（历史配对码或签发失败时整体缺失），故用零值 + 显式判空处理，
// 不能因为服务端没下发就让 e2e 失败。
type pairResult struct {
	DeviceID        string `json:"device_id"`
	DeviceToken     string `json:"device_token"`
	WSPath          string `json:"ws_path"`
	ProtocolVersion int    `json:"protocol_version"`

	UserToken   string `json:"user_token"`
	SessionTTLS int    `json:"session_ttl_s"`
	User        struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	} `json:"user"`
}

// pairDevice 凭码配对（免登录接口），端侧名固定为「脚本扮演」。
func pairDevice(ctx context.Context, cli *http.Client, baseURL, code string) (*pairResult, error) {
	return pairDeviceNamed(ctx, cli, baseURL, code, "device-e2e 端侧（脚本扮演）")
}

// pairDeviceNamed 与 pairDevice 相同，但可指定端侧上报的名字。
//
// 为什么需要区分名字：-mode runner 时真正连上来的是 hd-runner 进程，
// 设备表里若仍写「脚本扮演」会让事后排查（管理端设备列表）看不出这次是哪种模式。
func pairDeviceNamed(ctx context.Context, cli *http.Client, baseURL, code, name string) (*pairResult, error) {
	var out pairResult
	err := doJSON(ctx, cli, http.MethodPost, baseURL+"/api/v1/devices/pair", "",
		map[string]any{
			"code":        code,
			"name":        name,
			"kind":        "desktop",
			"platform":    "windows",
			"app_version": "0.0.1-e2e",
			"capabilities": []map[string]string{
				{"name": "fs.list", "version": "1.0"},
				{"name": "fs.read", "version": "1.0"},
			},
		}, &out)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.DeviceToken) == "" {
		return nil, fmt.Errorf("配对成功但响应里没有 device_token")
	}
	return &out, nil
}

// fetchMe 用给定令牌调 /api/v1/auth/me（用于验证配对下发的 user_token 真能用）。
//
// 刻意复用 doJSON：非 2xx 会转成 *apiError，调用方据此判断「令牌不可用」。
// 红线：本函数**绝不**打印令牌明文，错误信息里只出现状态码与错误码。
func fetchMe(ctx context.Context, cli *http.Client, baseURL, token string) (*meResult, error) {
	var out meResult
	if err := doJSON(ctx, cli, http.MethodGet, baseURL+"/api/v1/auth/me", token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// renewDeviceSession 用设备令牌换取新的用户会话（设备与账号持久绑定的续期入口）。
//
// 实测要点：本端点**只认设备令牌**，不接受 Cookie。故这里显式把 device_token
// 放进 Authorization: Bearer —— 若误传 user_token，服务端会返回 401。
func renewDeviceSession(ctx context.Context, cli *http.Client, baseURL, deviceToken string) (*meResult, string, error) {
	var out struct {
		UserToken   string   `json:"user_token"`
		SessionTTLS int      `json:"session_ttl_s"`
		User        meResult `json:"user"`
	}
	if err := doJSON(ctx, cli, http.MethodPost, baseURL+"/api/v1/devices/session", deviceToken,
		nil, &out); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(out.UserToken) == "" {
		return nil, "", fmt.Errorf("续期成功但响应里没有 user_token")
	}
	return &out.User, out.UserToken, nil
}

// meResult 对应 /auth/me 的响应（不包 user 字段，直接是 User 对象）。
type meResult struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// wsURL 由 baseURL 推导 WS 端点（http→ws / https→wss），令牌经 query 传入。
func wsURL(baseURL, deviceToken string, protocolVersion int) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("解析 -base-url 失败: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("-base-url 必须是 http:// 或 https://，当前为 %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1/devices/ws"
	q := url.Values{}
	q.Set("token", deviceToken)
	if protocolVersion > 0 {
		q.Set("protocol_version", fmt.Sprint(protocolVersion))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ============ WS 侧：端点侧扮演 ============

// deviceConn 端侧连接封装：读一帧 + 带类型断言的等待。
type deviceConn struct {
	ws  *websocket.Conn
	ctx context.Context
}

// readFrame 读一帧并解成 device.Envelope（客户端视角只认协议帧结构）。
func (c *deviceConn) readFrame(timeout time.Duration) (*device.Envelope, error) {
	rctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	typ, data, err := c.ws.Read(rctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, fmt.Errorf("收到非文本帧（type=%v），设备协议只允许 JSON 文本帧", typ)
	}
	var env device.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("帧不是合法 JSON: %w（原文：%s）", err, truncate(string(data), 200))
	}
	return &env, nil
}

// waitFrame 等指定类型的帧；途中的其它帧按 skipLog 记录后跳过。
//
// want 为空表示「任意帧」。超时报错文本必须写明「在等哪一帧」，否则联调时无法定位。
func (c *deviceConn) waitFrame(want device.MessageType, timeout time.Duration,
	skipLog func(*device.Envelope)) (*device.Envelope, error) {
	deadline := time.Now().Add(timeout)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return nil, fmt.Errorf("等待 %q 帧超时（%s）", frameLabel(want), timeout)
		}
		env, err := c.readFrame(remain)
		if err != nil {
			if want == "" {
				return nil, fmt.Errorf("等待任意帧时连接异常: %w", err)
			}
			return nil, fmt.Errorf("等待 %q 帧期间连接异常: %w", frameLabel(want), err)
		}
		if want == "" || env.Type == want {
			return env, nil
		}
		if env.Type == device.MsgError {
			// 服务端错误帧是「等待失败」的最强证据，直接上抛
			var ep device.ErrorPayload
			_ = env.Decode(&ep)
			return nil, fmt.Errorf("在等 %q 时收到错误帧 %s: %s（fatal=%v）",
				frameLabel(want), ep.Code, ep.Message, ep.Fatal)
		}
		if skipLog != nil {
			skipLog(env)
		}
	}
}

func frameLabel(t device.MessageType) string {
	if t == "" {
		return "任意"
	}
	return string(t)
}

// sendEnvelope 以服务端同一套信封结构上行一帧（保证 v/type/id/payload 形状一致）。
func (c *deviceConn) sendEnvelope(t device.MessageType, id string, payload any) error {
	env, err := device.NewEnvelope(t, id, payload)
	if err != nil {
		return fmt.Errorf("构造 %s 帧失败: %w", t, err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("序列化 %s 帧失败: %w", t, err)
	}
	wctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, raw)
}

// dialDevice 建立 WS 连接；返回连接与「是否 101」的事实。
//
// 鉴权在**升级前**完成：失败时 coder/websocket 会把 *http.Response 交回来，
// 我们据此区分 401（令牌）与 503（设备域关闭），而不是笼统报「升级失败」。
func dialDevice(ctx context.Context, baseURL, deviceToken string, protocolVersion int) (*deviceConn, int, error) {
	target, err := wsURL(baseURL, deviceToken, protocolVersion)
	if err != nil {
		return nil, 0, err
	}
	ws, resp, err := websocket.Dial(ctx, target, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		switch status {
		case http.StatusUnauthorized:
			return nil, status, fmt.Errorf("WS 升级被拒 401：设备令牌无效/已吊销（令牌前缀 %s…）", maskToken(deviceToken))
		case http.StatusServiceUnavailable:
			return nil, status, fmt.Errorf("WS 升级被拒 503：设备域未启用 → 请在 config.yaml 中设置 devices.enabled: true")
		case 0:
			return nil, status, fmt.Errorf("WS 连接失败（未拿到 HTTP 响应，检查 -base-url 是否可达）: %w", err)
		default:
			return nil, status, fmt.Errorf("WS 升级失败（HTTP %d）: %w", status, err)
		}
	}
	// coder/websocket 的 Dial 成功即代表 101 Switching Protocols
	return &deviceConn{ws: ws, ctx: ctx}, http.StatusSwitchingProtocols, nil
}

// closeNormally 正常关闭（1000），避免服务端日志出现「连接中断」噪音。
func (c *deviceConn) closeNormally() {
	_ = c.ws.Close(websocket.StatusNormalClosure, "e2e 完成")
}

// 编译期断言：确认脚本引用的 api 包类型可用（防止 internal 契约漂移时静默失配）。
var _ = api.SessionCookieName
