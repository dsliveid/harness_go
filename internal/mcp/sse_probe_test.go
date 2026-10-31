package mcp

import (
	"context"
	"fmt"
	nethttp "net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSSEProbe 隔离探测 SSE 传输链路（诊断用，随主测试运行）。
func TestSSEProbe(t *testing.T) {
	sdk := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "probe", Version: "1"}, nil)
	sdk.AddTool(simpleTool("echo", "回显"), func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}
		_ = jsonUnmarshalArgs(req.Params.Arguments, &args)
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "sse: " + args.Text}}}, nil
	})
	srv := httptest.NewServer(sdkmcp.NewSSEHandler(func(*nethttp.Request) *sdkmcp.Server { return sdk }, nil))
	defer srv.Close()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "probe-client", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10e9)
	defer cancel()
	session, err := client.Connect(ctx, &sdkmcp.SSEClientTransport{Endpoint: srv.URL + "/sse"}, nil)
	if err != nil {
		t.Fatalf("SSE 连接失败: %v", err)
	}
	defer func() { _ = session.Close() }()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list 失败: %v", err)
	}
	fmt.Printf("SSE tools=%d\n", len(res.Tools))
}
