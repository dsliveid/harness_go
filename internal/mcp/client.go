package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// conn 一个 MCP Server 的活动连接（session + stdio 进程句柄）。
//
// 并发语义：SDK ClientSession 的 CallTool/ListTools 并发安全；
// 本结构只负责生命周期（建立/关闭/进程退出感知）。
//
// 会话上下文：SSE 传输把事件流绑定到 Connect 的 ctx —— 因此会话 ctx 必须与
// 连接同生命周期（close 时取消），握手超时由竞速计时器实现，绝不用
// defer cancel 的短命 ctx 传给 Connect（否则 dial 返回即断流）。
type conn struct {
	cfg     ServerConfig
	ctx     context.Context
	cancel  context.CancelFunc
	session *sdkmcp.ClientSession
	cmd     *exec.Cmd // 仅 stdio；Close 后由 Wait 复收
}

// sdkContent MCP 内容块接口别名（TextContent / ImageContent / …）。
type sdkContent = sdkmcp.Content

// clientCapabilities 客户端自述（握手用）。
func clientCapabilities(name, version string) *sdkmcp.Implementation {
	return &sdkmcp.Implementation{Name: name, Version: version}
}

// childEnv 构造子进程环境：仅传入配置项 + 必要系统变量（设计 §2.3「限制 env」）。
// 绝不透传宿主全量环境（防密钥/代理变量泄漏进不可信子进程）。
func childEnv(cfg ServerConfig) []string {
	keep := map[string]bool{
		"PATH": true, "PATHEXT": true, "SYSTEMROOT": true, "COMSPEC": true,
		"HOME": true, "USERPROFILE": true, "TEMP": true, "TMP": true,
		"WINDIR": true, "APPDATA": true, "LOCALAPPDATA": true, "PROGRAMFILES": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && keep[kv[:i]] {
			out = append(out, kv)
		}
	}
	for k, v := range cfg.Env {
		out = append(out, k+"="+v)
	}
	return out
}

// dial 建立连接并完成握手（握手超时 HandshakeTimeout，超限放弃连接）。
//
// 失败统一归一为 MCP_CONNECT_FAILED（含命令不存在/端口不可达/协议错误）。
// 会话 ctx 与连接同生命周期（conn.close 取消），保证 SSE 长流不断。
func dial(ctx context.Context, cfg ServerConfig, clientName, clientVersion string) (*conn, error) {
	cfg = cfg.Normalize()
	cctx, cancel := context.WithCancel(ctx)

	client := sdkmcp.NewClient(clientCapabilities(clientName, clientVersion), nil)
	c := &conn{cfg: cfg, ctx: cctx, cancel: cancel}

	var transport sdkmcp.Transport
	switch cfg.Transport {
	case TransportStdio:
		cmd := exec.Command(cfg.Command, cfg.Args...)
		cmd.Env = childEnv(cfg)
		if cwd := strings.TrimSpace(cfg.Cwd); cwd != "" {
			cmd.Dir = cwd
		}
		c.cmd = cmd
		transport = &sdkmcp.CommandTransport{Command: cmd}
	case TransportSSE:
		transport = &sdkmcp.SSEClientTransport{Endpoint: cfg.URL}
	case TransportHTTP:
		transport = &sdkmcp.StreamableClientTransport{Endpoint: cfg.URL}
	default:
		cancel()
		return nil, newError(ErrCodeConnectFailed, cfg.ID, "未知传输 %q", string(cfg.Transport))
	}

	// 握手竞速：Connect 完成 or 超时（超时取消会话 ctx 促使 Connect 返回）
	type connectResult struct {
		session *sdkmcp.ClientSession
		err     error
	}
	done := make(chan connectResult, 1)
	go func() {
		s, e := client.Connect(cctx, transport, nil)
		done <- connectResult{s, e}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			cancel()
			return nil, newError(ErrCodeConnectFailed, cfg.ID, "连接/握手失败: %v", res.err)
		}
		c.session = res.session
		return c, nil
	case <-time.After(HandshakeTimeout):
		cancel()
		<-done // 等待 goroutine 收尾，防泄漏
		return nil, newError(ErrCodeConnectFailed, cfg.ID, "握手超时（>%s）", HandshakeTimeout)
	}
}

// ToolList 连接上的工具清单（MCP 原始形状）。
type ToolList struct {
	Tools []*sdkmcp.Tool
}

// listTools 分页拉全工具清单（超时 ListTimeout）。
func (c *conn) listTools(ctx context.Context) ([]*sdkmcp.Tool, error) {
	lctx, cancel := context.WithTimeout(ctx, ListTimeout)
	defer cancel()
	var out []*sdkmcp.Tool
	cursor := ""
	for {
		res, err := c.session.ListTools(lctx, &sdkmcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, newError(ErrCodeProtocolError, c.cfg.ID, "tools/list 失败: %v", err)
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" || res.NextCursor == cursor {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// callTool 执行单次工具调用（超时 cfg.TimeoutMS）。
//
// 返回 (文本, isError, error)：isError=true 时按工具错误处理（不中断 Run）；
// error 仅表示传输/协议级失败。
func (c *conn) callTool(ctx context.Context, rawName string, args []byte) (string, bool, error) {
	cfg := c.cfg.Normalize()
	cctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMS)*time.Millisecond)
	defer cancel()
	params := &sdkmcp.CallToolParams{Name: rawName}
	if len(args) > 0 {
		params.Arguments = jsonRaw(args)
	}
	res, err := c.session.CallTool(cctx, params)
	if err != nil {
		// SDK 把协议错误与超时都归为 error：按超时口径区分（ctx 截止）
		if cctx.Err() != nil {
			return "", false, newError(ErrCodeTimeout, cfg.ID,
				"调用超时（>%dms），可尝试重试或改用其他方式", cfg.TimeoutMS)
		}
		return "", false, newError(ErrCodeProtocolError, cfg.ID, "调用失败: %v", err)
	}
	text := extractText(res.Content)
	if len([]byte(text)) > cfg.MaxOutput {
		text = string([]byte(text)[:cfg.MaxOutput]) + fmt.Sprintf("\n…[输出超限，已截断到 %d 字节]", cfg.MaxOutput)
	}
	return text, res.IsError, nil
}

// ping 主动健康探测。
func (c *conn) ping(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.session.Ping(pctx, nil)
}

// close 关闭连接：先关 session（stdio 路径 SDK 会关 stdin → 进程退出），
// 再取消会话 ctx（SSE 长流终止）。幂等。
func (c *conn) close() error {
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
	c.cancel()
	return nil
}

// exited stdio 进程是否已退出（连接级故障的感知来源）。
func (c *conn) exited() bool {
	if c.cmd == nil || c.cmd.Process == nil {
		return false // 远程传输无进程概念
	}
	return c.cmd.ProcessState != nil
}
