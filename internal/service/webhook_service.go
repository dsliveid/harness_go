// WebhookService 实现 P1 Webhook 事件订阅（需求 §4.4）：
//
//   - 订阅配置：url + secret（HMAC-SHA256 签名，AES-256-GCM 密文落库）+ 事件白名单
//     （空 = 全部一期事件：run.completed / run.failed / run.interrupted /
//     approval.requested / tool.failed）；
//   - 分发：经 events.Bus 全局订阅（SubscribeAll）消费新事件 → 白名单匹配 →
//     POST JSON（X-Harness-Signature: sha256=<hmac>），单次超时 5s，
//     失败重试 3 次（指数退避 1s/2s/4s）；
//   - **绝不阻塞主链路**：分发在独立 goroutine（信号量限流 32），
//     慢消费者被总线断开后自动重连。
package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/events"
	"harness-go/internal/model"
	"harness-go/internal/secret"
	"harness-go/internal/store"
)

// 一期 Webhook 事件名（需求 §4.4）。
const (
	WebhookRunCompleted   = "run.completed"
	WebhookRunFailed      = "run.failed"
	WebhookRunInterrupted = "run.interrupted"
	WebhookApprovalReq    = "approval.requested"
	WebhookToolFailed     = "tool.failed"
	// WebhookTest 测试事件（TestFire 专用，不受白名单约束）。
	WebhookTest = "webhook.test"
)

// WebhookEvents 一期全部事件（创建时校验白名单取值）。
var WebhookEvents = []string{
	WebhookRunCompleted, WebhookRunFailed, WebhookRunInterrupted,
	WebhookApprovalReq, WebhookToolFailed,
}

// Webhook 分发边界。
const (
	WebhookTimeout    = 5 * time.Second
	WebhookMaxRetries = 3
	webhookBackoff    = time.Second
	webhookMaxWorkers = 32
)

// WebhookServiceOptions Webhook 用例依赖。
type WebhookServiceOptions struct {
	Main   *store.MainStore
	Cipher *secret.Cipher
	Bus    *events.Bus
	Logger *slog.Logger
	// Now 时钟（测试注入）。
	Now func() time.Time
	// HTTPClient 可注入（测试用 httptest server）。
	HTTPClient *http.Client
}

// WebhookService Webhook 订阅用例 + 分发器。
type WebhookService struct {
	main    *store.MainStore
	cipher  *secret.Cipher
	bus     *events.Bus
	logger  *slog.Logger
	now     func() time.Time
	client  *http.Client
	sem     chan struct{}
	startMu sync.Mutex
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewWebhookService 创建 Webhook 用例。
func NewWebhookService(opts WebhookServiceOptions) *WebhookService {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: WebhookTimeout}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookService{
		main:   opts.Main,
		cipher: opts.Cipher,
		bus:    opts.Bus,
		logger: logger,
		now:    now,
		client: client,
		sem:    make(chan struct{}, webhookMaxWorkers),
	}
}

// —— CRUD ——_

// WebhookCreateInput 创建入参。
type WebhookCreateInput struct {
	URL    string   `json:"url"`
	Secret string   `json:"secret"`
	Events []string `json:"events"`
}

// Create 新增订阅（secret 明文只出现一次，密文落库；返回脱敏视图）。
func (s *WebhookService) Create(ctx context.Context, in WebhookCreateInput) (*WebhookView, error) {
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, invalidArg("url 必须是合法的 http/https 地址")
	}
	allowed := map[string]struct{}{}
	for _, e := range WebhookEvents {
		allowed[e] = struct{}{}
	}
	for _, e := range in.Events {
		if _, ok := allowed[e]; !ok {
			return nil, invalidArg("未知 Webhook 事件 %q（可选：%v）", e, WebhookEvents)
		}
	}
	secretEnc, hasSecret, err := store.EncryptSecret(s.cipher, in.Secret)
	if err != nil {
		return nil, err
	}
	w := &store.Webhook{
		ID:        "wh-" + newID(),
		URL:       u.String(),
		SecretEnc: secretEnc,
		HasSecret: hasSecret,
		Events:    in.Events,
		Enabled:   true,
	}
	if err := s.main.Webhooks().Create(ctx, w); err != nil {
		return nil, wrapStoreErr(err, apperr.CodeInternal, "写入 Webhook 失败")
	}
	v := webhookViewOf(w)
	return &v, nil
}

// WebhookView 订阅视图（secret 脱敏：只回 has_secret；api 层契约形状）。
type WebhookView struct {
	ID        string   `json:"id"`
	URL       string   `json:"url"`
	HasSecret bool     `json:"has_secret"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
	CreatedAt int64    `json:"created_at"`
}

func webhookViewOf(w *store.Webhook) WebhookView {
	return WebhookView{
		ID: w.ID, URL: w.URL, HasSecret: w.HasSecret,
		Events: w.Events, Enabled: w.Enabled, CreatedAt: w.CreatedAt,
	}
}

// List 全部订阅（内部形状；分发器与测试用）。
func (s *WebhookService) List(ctx context.Context) ([]store.Webhook, error) {
	return s.main.Webhooks().List(ctx)
}

// ListView 全部订阅（api 契约形状，secret 脱敏）。
func (s *WebhookService) ListView(ctx context.Context) ([]WebhookView, error) {
	items, err := s.main.Webhooks().List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WebhookView, 0, len(items))
	for i := range items {
		out = append(out, webhookViewOf(&items[i]))
	}
	return out, nil
}

// GetView 单条订阅（api 契约形状）。
func (s *WebhookService) GetView(ctx context.Context, id string) (*WebhookView, error) {
	w, err := s.main.Webhooks().Get(ctx, id)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "Webhook 不存在")
	}
	v := webhookViewOf(w)
	return &v, nil
}

// Get 单条订阅。
func (s *WebhookService) Get(ctx context.Context, id string) (*store.Webhook, error) {
	w, err := s.main.Webhooks().Get(ctx, id)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "Webhook 不存在")
	}
	return w, nil
}

// Delete 删除订阅。
func (s *WebhookService) Delete(ctx context.Context, id string) error {
	if err := s.main.Webhooks().Delete(ctx, id); err != nil {
		return wrapStoreErr(err, apperr.CodeNotFound, "Webhook 不存在")
	}
	return nil
}

// SetEnabled 启用/停用。
func (s *WebhookService) SetEnabled(ctx context.Context, id string, enabled bool) error {
	if err := s.main.Webhooks().SetEnabled(ctx, id, enabled); err != nil {
		return wrapStoreErr(err, apperr.CodeNotFound, "Webhook 不存在")
	}
	return nil
}

// TestFire 向指定订阅同步投递一条测试事件（不重试）。
// 返回（HTTP 状态码（连接失败=0），失败详情，配置类错误——订阅不存在/解密失败）。
// 投递失败（网络/远端非 2xx）是**结果**不是错误：返回 ok=false 供 UI 展示。
func (s *WebhookService) TestFire(ctx context.Context, id string) (int, string, error) {
	w, err := s.Get(ctx, id)
	if err != nil {
		return 0, "", err
	}
	secretPlain, err := w.DecryptSecret(s.cipher)
	if err != nil {
		return 0, "", err
	}
	body, err := s.buildDelivery(WebhookTest, w.ID, "", "", map[string]any{"note": "TestFire from harness-go"})
	if err != nil {
		return 0, "", err
	}
	status, _, perr := s.post(ctx, w.URL, WebhookTest, body, secretPlain)
	if perr != nil {
		return 0, perr.Error(), nil
	}
	return status, "", nil
}

// —— 分发器 ——_

// Start 启动全局订阅消费循环（幂等；进程退出经 Stop 收敛）。
// 订阅在本方法内**同步建立**——Start 返回后的 Publish 保证可达
// （若订阅在 goroutine 里才建立，测试/调用方随即 Publish 会竞态丢事件）。
func (s *WebhookService) Start(ctx context.Context) {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started || s.bus == nil {
		return
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sub := s.bus.SubscribeAll()
	s.cancel = cancel
	s.started = true
	s.wg.Add(1)
	go s.consume(runCtx, sub)
}

// Stop 停止消费循环。
func (s *WebhookService) Stop() {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if !s.started {
		return
	}
	s.cancel()
	s.wg.Wait()
	s.started = false
}

// consume 消费全局事件流；慢消费者被断开后自动重连（指数退避封顶 10s）。
// 退出路径必须显式 Unsubscribe——否则 Stop 的 wg.Wait 会等一个永远阻塞在
// 事件通道上的 goroutine（range 不会因 ctx 取消而退出）。
func (s *WebhookService) consume(ctx context.Context, sub *events.Subscriber) {
	defer s.wg.Done()
	backoff := time.Second
	for ctx.Err() == nil {
		for {
			select {
			case <-ctx.Done():
				s.bus.Unsubscribe(sub)
				return
			case ev, open := <-sub.C():
				if !open {
					goto reconnect // 慢消费者被断开：退避后重连
				}
				backoff = time.Second // 正常消费即重置退避
				s.handleEvent(ctx, ev)
			}
		}
	reconnect:
		// 重连窗口内的新事件会丢失（慢消费者本就是降级路径，可容忍）
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
		sub = s.bus.SubscribeAll()
	}
}

// handleEvent 事件映射 + 白名单匹配 + 异步分发（不阻塞消费循环）。
func (s *WebhookService) handleEvent(ctx context.Context, ev events.Event) {
	name, ok := mapWebhookEvent(ev)
	if !ok || ctx.Err() != nil {
		return
	}
	hooks, err := s.main.Webhooks().List(ctx)
	if err != nil {
		s.logger.Warn("Webhook: 读取订阅失败", "err", err)
		return
	}
	var enabled []*store.Webhook
	for i := range hooks {
		w := &hooks[i]
		if !w.Enabled || !webhookWants(w.Events, name) {
			continue
		}
		enabled = append(enabled, w)
	}
	if len(enabled) == 0 {
		return
	}
	sessionID := extractSessionID(ev.Payload)
	data := map[string]any{}
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &data)
	}
	for _, w := range enabled {
		w := w
		deliveryID := newDeliveryID()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			select {
			case s.sem <- struct{}{}:
				defer func() { <-s.sem }()
			case <-ctx.Done():
				return
			}
			s.deliver(ctx, w, deliveryID, name, ev.RunID, sessionID, data)
		}()
	}
}

// deliver 投递 + 重试（指数退避 1s/2s/4s，单次 5s 超时）。
func (s *WebhookService) deliver(ctx context.Context, w *store.Webhook, deliveryID, event, runID, sessionID string, data map[string]any) {
	secretPlain, err := w.DecryptSecret(s.cipher)
	if err != nil {
		s.logger.Warn("Webhook: 解密 secret 失败", "webhook", w.ID, "err", err)
		return
	}
	body, err := s.buildDelivery(event, deliveryID, runID, sessionID, data)
	if err != nil {
		return
	}
	backoff := webhookBackoff
	for attempt := 0; ; attempt++ {
		dctx, cancel := context.WithTimeout(ctx, WebhookTimeout)
		status, respBody, perr := s.post(dctx, w.URL, event, body, secretPlain)
		cancel()
		if perr == nil && status >= 200 && status < 300 {
			return // 成功
		}
		if attempt >= WebhookMaxRetries || ctx.Err() != nil {
			s.logger.Warn("Webhook: 投递失败（已放弃）",
				"webhook", w.ID, "event", event, "status", status,
				"attempts", attempt+1, "err", perr)
			_ = respBody
			return
		}
		s.logger.Warn("Webhook: 投递失败（将重试）",
			"webhook", w.ID, "event", event, "status", status,
			"attempt", attempt+1, "err", perr)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// post 发送一次签名 POST。
func (s *WebhookService) post(ctx context.Context, urlVal, event string, body []byte, secretPlain string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlVal, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Harness-Event", event)
	req.Header.Set("X-Harness-Signature", "sha256="+hmacSHA256Hex(secretPlain, body))
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode, respBody, nil
}

// buildDelivery 构造投递报文（shape 与需求 §4.4 一致，body 参与签名）。
func (s *WebhookService) buildDelivery(event, deliveryID, runID, sessionID string, data map[string]any) ([]byte, error) {
	payload := map[string]any{
		"id":          deliveryID,
		"event":       event,
		"occurred_at": s.now().UnixMilli(),
		"data":        data,
	}
	if runID != "" {
		payload["run_id"] = runID
	}
	if sessionID != "" {
		payload["session_id"] = sessionID
	}
	return json.Marshal(payload)
}

// mapWebhookEvent 内部事件 → Webhook 一期事件名（需求 §4.4）。
func mapWebhookEvent(ev events.Event) (string, bool) {
	switch ev.Type {
	case events.TypeRunStatus:
		var p struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			return "", false
		}
		switch model.RunStatus(p.Status) {
		case model.RunCompleted:
			return WebhookRunCompleted, true
		case model.RunFailed:
			return WebhookRunFailed, true
		case model.RunInterrupted:
			return WebhookRunInterrupted, true
		}
		return "", false
	case events.TypeApprovalRequest:
		return WebhookApprovalReq, true
	case events.TypeToolResult:
		var p struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || p.Status != string(model.ToolError) {
			return "", false
		}
		return WebhookToolFailed, true
	}
	return "", false
}

// webhookWants 白名单匹配（空 = 全部一期事件）。
func webhookWants(whitelist []string, event string) bool {
	if len(whitelist) == 0 {
		return true
	}
	for _, e := range whitelist {
		if e == event {
			return true
		}
	}
	return false
}

// extractSessionID 从事件 payload 提取 session_id（引擎 P1 起随事件携带）。
func extractSessionID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(raw, &p) == nil {
		return p.SessionID
	}
	return ""
}

func hmacSHA256Hex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func newDeliveryID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("whd-%d", store.NowMillis())
	}
	return "whd-" + hex.EncodeToString(b[:])
}
