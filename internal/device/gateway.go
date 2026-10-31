package device

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
)

// ============ WS 网关：升级 + 读写循环（阶段 C-3） ============
//
// 铁律：
//   - **鉴权在升级前完成**：先校验 device_token（查 devices.token_hash + 未吊销），
//     失败直接 401 JSON，绝不返回 101。升级后不再信任任何客户端自述的身份
//   - 一连接两协程：读循环（唯一读取者）+ 写循环（唯一写入者，经 writeMu 串行）
//   - 端侧只产事件，永不直写 DB（不变量 1）；落库一律经 Hub 回调 → service
//   - 协议不兼容 → 回 error{device.protocol_mismatch, fatal} 后关闭，**不静默降级执行**

// 读/写超时与帧上限。
const (
	// wsReadLimit 单帧上限（任务结果可能较大，但端侧不该回传巨型 blob）。
	wsReadLimit = 1 << 20 // 1 MiB
	// wsWriteTimeout 单帧写超时。
	wsWriteTimeout = 10 * time.Second
	// wsCloseGrace 关闭握手的等待上限。
	wsCloseGrace = 2 * time.Second
)

// FrameHandler 处理端→服的上行帧（task.progress / task.result 等由 C-4 注入）。
//
// 返回非 nil 错误时网关回 error 帧；返回 fatal=true 则关闭连接。
type FrameHandler func(ctx context.Context, c *Conn, env Envelope) (fatal bool, err error)

// GatewayOptions 网关依赖。
type GatewayOptions struct {
	Log *slog.Logger
	Hub *Hub

	// Authenticate 升级前鉴权（必填）：用 device_token 换取设备。
	// 失败必须返回 apperr（401 unauthorized / device.revoked），绝不返回 101。
	Authenticate func(ctx context.Context, token string) (*model.Device, error)

	// Heartbeat 端侧心跳间隔（下发 welcome 用）。
	Heartbeat time.Duration
	// OfflineAfter 无帧判离线阈值（下发 welcome 用）。
	OfflineAfter time.Duration

	// Handler 上行帧处理器（C-4 任务族；nil 时任务族帧回 device.task_unsupported）。
	Handler FrameHandler
	// PendingCount 待派发任务数（填充 pong.pending；nil 恒 0）。
	PendingCount func(deviceID string) int
	// AcceptOptions 覆盖 WS 升级参数（测试用）。
	AcceptOptions *websocket.AcceptOptions
}

// Gateway WS 网关（HTTP 升级入口 + 连接生命周期）。
type Gateway struct {
	log          *slog.Logger
	hub          *Hub
	authenticate func(ctx context.Context, token string) (*model.Device, error)
	heartbeat    time.Duration
	offlineAfter time.Duration
	handler      FrameHandler
	pendingCount func(deviceID string) int
	acceptOpts   *websocket.AcceptOptions

	now    func() time.Time
	connID atomic.Int64
}

// NewGateway 创建网关。
func NewGateway(opts GatewayOptions) *Gateway {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	hb := opts.Heartbeat
	if hb <= 0 {
		hb = 15 * time.Second
	}
	offline := opts.OfflineAfter
	if offline <= 0 {
		offline = 3 * hb
	}
	return &Gateway{
		log:          log,
		hub:          opts.Hub,
		authenticate: opts.Authenticate,
		heartbeat:    hb,
		offlineAfter: offline,
		handler:      opts.Handler,
		pendingCount: opts.PendingCount,
		acceptOpts:   opts.AcceptOptions,
		now:          time.Now,
	}
}

// SetClock 注入时钟（测试用）。
func (g *Gateway) SetClock(fn func() time.Time) {
	if fn != nil {
		g.now = fn
	}
}

// TokenFromRequest 从请求提取设备令牌。
//
// WS 无法设置自定义请求头（浏览器/多数客户端的限制），故走 query `token`；
// 同时兼容 `Authorization: Bearer`（CLI/服务端 Runner 可用，更安全）。
// **日志脱敏**：access log 的 query 白名单不含 token，故 token 永不落日志（见 observ.RedactQuery）。
func TokenFromRequest(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	if h := r.Header.Get("Authorization"); len(h) > 7 && (h[:7] == "Bearer " || h[:7] == "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// atoiSafe 解析整数，失败返回 0（用于 query 版本号，容错优先）。
func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// ServeHTTP 处理 WS 升级。
//
// 响应语义：
//   - 设备域未启用 → 503（由上层 handler 拦截，这里兜底 503 device.disabled）
//   - 令牌缺失/无效/已吊销 → **401 JSON**（升级前完成，绝不 101）
//   - 协议版本不兼容 → 101 后回 error{device.protocol_mismatch} 并关闭（版本在 hello/ping 中才知道）
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.hub == nil || g.authenticate == nil {
		writeGatewayError(w, apperr.New(apperr.CodeDeviceDisabled, "设备功能未配置"))
		return
	}
	token := TokenFromRequest(r)
	if token == "" {
		writeGatewayError(w, apperr.New(apperr.CodeUnauthorized, "缺少设备令牌"))
		return
	}
	dev, err := g.authenticate(r.Context(), token)
	if err != nil {
		// 令牌无效/已吊销 → 401/401，均在 Upgrade 之前完成
		writeGatewayError(w, err)
		return
	}

	// 版本协商：端侧可在 query 声明 protocol_version（首帧 hello/ping 亦可）。
	clientVer := 0
	if v := r.URL.Query().Get("protocol_version"); v != "" {
		clientVer = atoiSafe(v)
	}

	conn, err := websocket.Accept(w, r, g.acceptOpts)
	if err != nil {
		g.log.Warn("WS 升级失败", "device_id", dev.ID, "err", err)
		return
	}
	conn.SetReadLimit(wsReadLimit)

	c := newConn(conn, dev, g, r.RemoteAddr)
	g.log.Info("设备长连接已建立",
		"device_id", dev.ID, "device_name", dev.Name, "remote", r.RemoteAddr,
		"protocol_version", clientVer)

	if clientVer != 0 {
		if vErr := CompatibleVersion(clientVer); vErr != nil {
			// 升级成功但版本不兼容：CloseWith 会先发一帧 fatal error 再关闭，
			// 避免端侧误以为连接可用（不静默降级执行）
			c.CloseWith(FrameCodeProtocolMismatch, apperr.From(vErr).Message)
			g.log.Warn("拒绝不兼容的端侧协议版本",
				"device_id", dev.ID, "client_v", clientVer,
				"supported", SupportedVersions())
			return
		}
	}

	g.serve(c)
}

// serve 驱动单连接的生命周期：注册 → welcome → 读循环 → 清理。
func (g *Gateway) serve(c *Conn) {
	defer c.CloseWith(FrameCodeServerClosing, "连接处理结束")

	g.hub.Register(c)
	defer g.hub.Unregister(c)

	// 握手确认：下发心跳节奏与协议版本，端侧据此驱动 ping
	c.Send(MustEnvelope(MsgWelcome, "", WelcomePayload{
		DeviceID:        c.DeviceID(),
		ServerTime:      g.now().UnixMilli(),
		ProtocolVersion: CurrentProtocolVersion,
		HeartbeatS:      int(g.heartbeat.Seconds()),
		OfflineAfterS:   int(g.offlineAfter.Seconds()),
		Pending:         g.pending(c.DeviceID()),
	}))

	// 读循环阻塞直到连接关闭
	g.readLoop(c)
}

// readLoop 单协程独占读取（coder/websocket 要求同一连接只有一个读协程）。
func (g *Gateway) readLoop(c *Conn) {
	ctx := c.ctx
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		readCtx, cancel := context.WithTimeout(ctx, g.offlineAfter+g.heartbeat)
		typ, data, err := c.ws.Read(readCtx)
		cancel()
		if err != nil {
			status := websocket.CloseStatus(err)
			switch {
			case errors.Is(err, context.Canceled), status == websocket.StatusNormalClosure,
				status == websocket.StatusGoingAway:
				g.log.Info("设备长连接正常关闭", "device_id", c.DeviceID())
			case status == websocket.StatusPolicyViolation:
				g.log.Warn("设备长连接被策略关闭", "device_id", c.DeviceID(), "code", int(status))
			default:
				g.log.Info("设备长连接中断", "device_id", c.DeviceID(), "err", err)
			}
			return
		}
		if typ != websocket.MessageText {
			g.log.Debug("忽略非文本帧", "device_id", c.DeviceID(), "type", typ)
			continue
		}

		c.touch()

		var env Envelope
		if uErr := json.Unmarshal(data, &env); uErr != nil {
			c.Send(MustEnvelope(MsgError, "", ErrorPayload{
				Code:    FrameCodeBadFrame,
				Message: "帧不是合法 JSON",
			}))
			continue
		}
		if vErr := env.Validate(); vErr != nil {
			c.Send(MustEnvelope(MsgError, env.ID, ErrorPayload{
				Code:    FrameCodeBadFrame,
				Message: vErr.Error(),
			}))
			continue
		}

		fatal, hErr := g.handleFrame(c, env)
		if hErr != nil {
			ae := apperr.From(hErr)
			c.Send(MustEnvelope(MsgError, env.ID, ErrorPayload{
				Code:    string(ae.Code),
				Message: ae.Message,
				Fatal:   fatal,
			}))
		}
		if fatal {
			return
		}
	}
}

// handleFrame 分发单个上行帧；返回 fatal 表示随后关闭连接。
func (g *Gateway) handleFrame(c *Conn, env Envelope) (fatal bool, err error) {
	// 帧级版本协商：每帧都可携带 v，不兼容立即终止（宽进严出）
	if vErr := CompatibleVersion(env.V); vErr != nil {
		return true, vErr
	}

	switch env.Type {
	case MsgPing:
		var p PingPayload
		if dErr := env.Decode(&p); dErr != nil {
			return false, dErr
		}
		if vErr := CompatibleVersion(p.ProtocolVersion); vErr != nil {
			return true, vErr
		}
		g.hub.Beat(c.DeviceID(), p.Load, p.CapabilitiesHash, p.ProtocolVersion)
		c.Send(MustEnvelope(MsgPong, env.ID, PongPayload{
			ServerTime:      g.now().UnixMilli(),
			Pending:         g.pending(c.DeviceID()),
			ProtocolVersion: CurrentProtocolVersion,
		}))
		return false, nil

	case MsgHello:
		var h HelloPayload
		if dErr := env.Decode(&h); dErr != nil {
			return false, dErr
		}
		if vErr := CompatibleVersion(h.ProtocolVersion); vErr != nil {
			return true, vErr
		}
		g.hub.Beat(c.DeviceID(), nil, h.CapabilitiesHash, h.ProtocolVersion)
		c.Send(MustEnvelope(MsgWelcome, env.ID, WelcomePayload{
			DeviceID:        c.DeviceID(),
			ServerTime:      g.now().UnixMilli(),
			ProtocolVersion: CurrentProtocolVersion,
			HeartbeatS:      int(g.heartbeat.Seconds()),
			OfflineAfterS:   int(g.offlineAfter.Seconds()),
			Pending:         g.pending(c.DeviceID()),
		}))
		return false, nil

	case MsgPong, MsgWelcome:
		// 端侧不应发送这两个方向相反的帧，但容忍（不视为错误，避免老端被踢）
		return false, nil

	case MsgTaskProgress, MsgTaskResult:
		if g.handler == nil {
			return false, apperr.New(apperr.CodeToolUnavailable, "服务端未启用端侧任务执行")
		}
		return g.handler(context.Background(), c, env)

	case MsgTaskDispatch, MsgLeaseRevoked:
		// 服→端方向的帧被端侧上行发送 → 协议误用
		return false, apperr.New(apperr.CodeInternal, "该帧类型不应由端侧发送")

	case MsgError:
		var ep ErrorPayload
		_ = env.Decode(&ep)
		g.log.Warn("端侧回报错误帧", "device_id", c.DeviceID(), "code", ep.Code, "msg", ep.Message)
		return ep.Fatal, nil
	}

	// 未知类型（Validate 已拦，这里兜底前向兼容）
	return false, nil
}

// pending 待派发任务数。
func (g *Gateway) pending(deviceID string) int {
	if g.pendingCount == nil {
		return 0
	}
	return g.pendingCount(deviceID)
}

// ============ Conn：单连接抽象（实现 Connection + 串行写） ============

// Conn 已升级的 WS 连接。
//
// 并发约定：**所有写操作经 Send 串行化**（writeMu），读只由网关读循环发起。
type Conn struct {
	ws        *websocket.Conn
	device    *model.Device
	remote    string
	createdAt time.Time

	ctx    context.Context
	cancel context.CancelFunc

	writeMu  sync.Mutex
	lastSeen atomic.Int64 // ms 时间戳
	closed   atomic.Bool
}

// newConn 构造连接（ctx 从 background 派生，与 http.Request ctx 解耦：
// coder/websocket Accept 后 request ctx 不可再依赖）。
func newConn(ws *websocket.Conn, dev *model.Device, g *Gateway, remote string) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		ws:        ws,
		device:    dev,
		remote:    remote,
		createdAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
	}
	c.touch()
	_ = g
	return c
}

// DeviceID 所属设备 ID。
func (c *Conn) DeviceID() string { return c.device.ID }

// Device 所属设备快照（升级时鉴权结果）。
func (c *Conn) Device() *model.Device { return c.device }

// RemoteAddr 端侧地址。
func (c *Conn) RemoteAddr() string { return c.remote }

// LastSeen 最近一次收到端侧帧的时间。
func (c *Conn) LastSeen() time.Time { return time.UnixMilli(c.lastSeen.Load()) }

// Closed 是否已关闭。
func (c *Conn) Closed() bool { return c.closed.Load() }

// touch 刷新最后活跃时间。
func (c *Conn) touch() { c.lastSeen.Store(time.Now().UnixMilli()) }

// Send 线程安全写出一个信封（写超时 + 已关闭短路）。
func (c *Conn) Send(env Envelope) error {
	if c.closed.Load() {
		return errConnClosed
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return errConnClosed
	}
	ctx, cancel := context.WithTimeout(c.ctx, wsWriteTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, raw)
}

// SendJSON 便捷方法：以 MustEnvelope 语义发送指定类型帧。
func (c *Conn) SendJSON(t MessageType, id string, payload any) error {
	return c.Send(MustEnvelope(t, id, payload))
}

// CloseWith 以指定错误码关闭连接（幂等；不阻塞读循环清理）。
//
// 顺序铁律：**先发错误帧 → 再 cancel → 最后关闭握手**。
// 若先 cancel，coder/websocket 会因读被取消而把连接标记为已失败，导致后续写直接报错，
// 端侧就收不到 device.replaced / device.revoked 这类关键原因码。
func (c *Conn) CloseWith(code, reason string) {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}

	// ① 尽力发一帧错误说明（端侧据此决定是否重连）
	if code != "" && code != FrameCodeServerClosing {
		_ = c.sendBestEffort(MsgError, ErrorPayload{Code: code, Message: reason, Fatal: true})
	}

	// ② 取消连接上下文，通知读循环退出
	c.cancel()

	// ③ 关闭握手（coder/websocket 内部 5s 上限）
	status := websocket.StatusNormalClosure
	if code != "" && code != FrameCodeServerClosing {
		status = websocket.StatusPolicyViolation
	}
	_ = c.ws.Close(status, truncateReason(reason))
}

// sendBestEffort 用独立 ctx 写一帧（CloseWith 内部使用，无 closed 短路）。
func (c *Conn) sendBestEffort(t MessageType, payload any) error {
	raw, err := json.Marshal(MustEnvelope(t, "", payload))
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, raw)
}

var errConnClosed = errors.New("device: 连接已关闭")

// writeGatewayError 把 apperr 写成 JSON 错误响应（升级前，仍是普通 HTTP 语义）。
func writeGatewayError(w http.ResponseWriter, err error) {
	ae := apperr.From(err)
	if ae == nil {
		ae = apperr.New(apperr.CodeInternal, "服务内部错误")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(ae.Status())
	_ = json.NewEncoder(w).Encode(ae.Body())
}

// truncateReason WS 关闭原因上限 123 字节（RFC 6455），超出会握手失败。
func truncateReason(s string) string {
	const maxLen = 120
	if len(s) <= maxLen {
		return s
	}
	// 按 rune 边界截断，避免切碎 UTF-8
	b := []byte(s)[:maxLen]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
