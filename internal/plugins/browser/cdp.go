package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// cdp 最小 DevTools 协议客户端（JSON-RPC 风格 id/method/params，逐请求应答）。
type cdp struct {
	conn    *websocket.Conn
	nextID  atomic.Int64
	pending chan []byte
}

// dialCDP 连接调试 WebSocket。
func dialCDP(ctx context.Context, wsURL string) (*cdp, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("连接 CDP 失败: %w", err)
	}
	c := &cdp{conn: conn, pending: make(chan []byte, 64)}
	// 后台读循环：应答按 id 分发（本客户端串行调用，单缓冲足够）
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				close(c.pending)
				return
			}
			select {
			case c.pending <- data:
			default:
			}
		}
	}()
	return c, nil
}

// call 执行一次 CDP 方法调用（串行语义：请求发出后取下一条应答）。
func (c *cdp) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	req := map[string]any{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return nil, fmt.Errorf("CDP 写失败: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		rctx, cancel := context.WithDeadline(ctx, deadline)
		select {
		case <-rctx.Done():
			cancel()
			return nil, fmt.Errorf("CDP 应答超时（%s）", method)
		case data, ok := <-c.pending:
			cancel()
			if !ok {
				return nil, fmt.Errorf("CDP 连接已关闭")
			}
			var msg struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				continue // 事件帧跳过
			}
			if msg.ID != int(id) {
				continue // 非本次应答（理论上串行不会发生）
			}
			if msg.Error != nil {
				return nil, fmt.Errorf("CDP %s 失败: %s", method, msg.Error.Message)
			}
			return msg.Result, nil
		}
	}
}

// Close 关闭连接。
func (c *cdp) Close() error { return c.conn.CloseNow() }
