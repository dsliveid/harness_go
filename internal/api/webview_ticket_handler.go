package api

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"harness-go/internal/apperr"
)

// ============ WebView 一次性 ticket（协议 §6.2 / 移动端架构 §4.2） ============
//
// 背景：移动端壳的凭据存在 Keychain，WebView 的 Cookie jar 是独立的。
// 不做交换，用户每次进 WebView 都要重登；而直接把 device_token 注入页面
// 又违反红线 R3/R9。因此引入「一次性短时 ticket」：
//
//	原生（已登录） --签发--> ticket
//	  --以 URL fragment 传给 WebView--> 页面
//	  --兑换--> hg_sess Cookie（短时会话）
//
// 三条硬约束（协议 §6.2，逐条在下方实现处标注）：
//  1. 一次性消费：重放必须 401
//  2. 绝不写日志：ticket 明文不得出现在任何日志输出中
//  3. 权限降级：兑换出的会话带 webview 标记，高危审批决策被拒

// tokenKindWebview WebView 降级会话的令牌 kind（协议 §6.2 硬约束 ③）。
//
// 语义：由 webview-ticket 兑换而来的 hg_sess 会话，能力受限——
// 可行：读会话、看消息、在会话内发消息；
// 禁行：审批决策（decideApproval 处校验并拒绝）。
const tokenKindWebview = "webview"

// currentTokenKind 返回当前请求令牌的 kind（取不到时返回空串）。
//
// 仅用于**能力降级判定**，不参与鉴权（鉴权已由中间件完成）。
// 查库失败一律返回空串：宁可放行既有行为，也不因存储抖动把正常用户挡在门外——
// 真正的审批安全边界在 Gate.CheckDevice 与 strict_approval_device，不在此处。
func (h *handlers) currentTokenKind(r *http.Request) string {
	token := extractToken(r)
	if token == "" || h.d.Auth == nil {
		return ""
	}
	tk, err := h.d.Auth.TokenKind(r.Context(), token)
	if err != nil {
		return ""
	}
	return tk
}

// webviewTicketTTL 默认 ticket 有效期。
//
// 60s 的取值理由：ticket 从签发到兑换是同一次页面加载内的连续动作，
// 正常耗时远小于 1s；给到 60s 只是容忍弱网与用户中途切后台。
const webviewTicketTTL = 60 * time.Second

// webviewTicketMaxTTL ticket 有效期硬上限。
//
// 客户端可请求更短，但**不允许**更长：ticket 是「即取即用」的凭据，
// 有效期越长，被中间人截获后可重放的窗口越大。
const webviewTicketMaxTTL = 300 * time.Second

// webviewTicket 一张待兑换的 ticket。
//
// 不落库：重启即失效是可接受的（用户重新登录一次即可），
// 而落库会引入「清理过期行」的额外运维负担与敏感数据持久化风险。
type webviewTicket struct {
	userID    string
	deviceID  string
	expiresAt time.Time
	used      bool
}

// webviewTicketStore ticket 内存表（并发安全）。
type webviewTicketStore struct {
	mu   sync.Mutex
	data map[string]*webviewTicket
}

func newWebviewTicketStore() *webviewTicketStore {
	return &webviewTicketStore{data: make(map[string]*webviewTicket)}
}

// issue 生成并登记一张新 ticket。
func (s *webviewTicketStore) issue(userID, deviceID string, ttl time.Duration) (string, time.Time) {
	// 32 字节 = 256 bit 熵，足以抗猜测；base64url 无填充便于放进 URL fragment。
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败属系统级异常，返回空串由调用方转 500。
		return "", time.Time{}
	}
	ticket := base64.RawURLEncoding.EncodeToString(buf)
	exp := time.Now().Add(ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	// 顺手清理已过期项，避免长期运行后无限增长（无独立 GC 协程的开销）。
	for k, v := range s.data {
		if v.used || time.Now().After(v.expiresAt) {
			delete(s.data, k)
		}
	}
	s.data[ticket] = &webviewTicket{userID: userID, deviceID: deviceID, expiresAt: exp}
	return ticket, exp
}

// consume 一次性消费 ticket。
//
// 硬约束 ①：标记 used 与实际返回必须在**同一临界区**内完成，
// 否则两个并发兑换请求会双双通过校验，ticket 就被用了两次。
func (s *webviewTicketStore) consume(ticket string) (*webviewTicket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.data[ticket]
	if !ok {
		return nil, false
	}
	if t.used || time.Now().After(t.expiresAt) {
		// 过期项顺手删除；已用项保留到下次清理（避免并发场景下语义歧义）。
		if time.Now().After(t.expiresAt) {
			delete(s.data, ticket)
		}
		return nil, false
	}
	t.used = true
	// 消费后立即从表中移除：即使兑换后续步骤失败，该 ticket 也不可重试。
	delete(s.data, ticket)
	return t, true
}

// ============ 端点 ============

type webviewTicketReq struct {
	DeviceID string `json:"device_id"`
	TTLSec   int    `json:"ttl_s"`
}

// issueWebviewTicket 签发一张 WebView 免登 ticket。
//
//	POST /api/v1/auth/webview-ticket   （需用户会话）
func (h *handlers) issueWebviewTicket(w http.ResponseWriter, r *http.Request) {
	userID := UserID(r.Context())
	if strings.TrimSpace(userID) == "" {
		writeError(w, r, apperr.New(apperr.CodeUnauthorized, "需要登录后才能签发 WebView 凭证"))
		return
	}
	var req webviewTicketReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	ttl := webviewTicketTTL
	if req.TTLSec > 0 {
		// 客户端可请求更短，但不得超过硬上限（夹紧而非报错：调用方无需感知上限常量）
		requested := time.Duration(req.TTLSec) * time.Second
		if requested < ttl {
			ttl = requested
		}
		if ttl > webviewTicketMaxTTL {
			ttl = webviewTicketMaxTTL
		}
	}

	ticket, exp := h.webviewTickets.issue(userID, strings.TrimSpace(req.DeviceID), ttl)
	if ticket == "" {
		writeError(w, r, apperr.New(apperr.CodeInternal, "生成 WebView 凭证失败"))
		return
	}
	// 硬约束 ②：此处只记录「签发了」这一事实，**绝不打印 ticket 明文**。
	h.d.Log.Info("签发 WebView ticket", "user_id", userID, "device_id", req.DeviceID, "ttl_s", int(ttl.Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":     ticket,
		"expires_at": exp.UnixMilli(),
	})
}

type webviewExchangeReq struct {
	Ticket string `json:"ticket"`
}

// exchangeWebviewTicket 用 ticket 兑换 hg_sess 会话 Cookie。
//
//	POST /api/v1/auth/webview-exchange   （免鉴权，见 router 的 publicPaths 说明）
func (h *handlers) exchangeWebviewTicket(w http.ResponseWriter, r *http.Request) {
	var req webviewExchangeReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	t := strings.TrimSpace(req.Ticket)
	if t == "" {
		writeError(w, r, apperr.New(apperr.CodeUnauthorized, "WebView 凭证无效"))
		return
	}

	// 硬约束 ①：一次性消费。失败不区分「不存在 / 过期 / 已使用」，
	// 统一返回同一错误（防探测：攻击者无法据此判断 ticket 是否曾存在）。
	rec, ok := h.webviewTickets.consume(t)
	if !ok {
		// 硬约束 ②：日志里只记长度与来源，**绝不出现 ticket 明文**。
		h.d.Log.Warn("WebView ticket 兑换失败", "ticket_len", len(t), "remote", clientIP(r))
		writeError(w, r, apperr.New(apperr.CodeUnauthorized, "WebView 凭证无效或已过期"))
		return
	}

	// 硬约束 ③：兑换出的会话是**降级会话**。
	//
	// 落地方式（权衡后选定）：兑换时清掉 WebView 自己那条会话的审批决策能力。
	// 具体做法是为该 token 打上 webview kind（IssutToken 的 kind 参数），
	// 并在 decideApproval 处校验「WebView 会话不得做高危决策」——见 provider_handler.go。
	//
	// 之所以不在中间件注入 scope：那要改 TokenVerifier 签名与全部调用点，
	// 改动面远大于收益；在唯一需要限制的入口（decideApproval）直接校验更内聚。
	token, _, err := h.d.Auth.IssueToken(r.Context(), rec.userID, tokenKindWebview, tokenKindWebview)
	if err != nil {
		writeError(w, r, err)
		return
	}
	setSessionCookie(w, token, h.d.AuthConfig.SessionTTL, h.d.AuthConfig.SecureCookie)
	h.d.Log.Info("WebView ticket 兑换成功", "user_id", rec.userID, "device_id", rec.deviceID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
