// Command fake-mcp 是插件协议的假 MCP Server fixture（阶段 E / 协议清单 #7）。
//
// 用途：
//  1. internal/mcp 三传输（stdio / SSE / Streamable HTTP）的集成测试与 e2e；
//  2. 打进 pkg.zip 作为「本地工具包」被安装（market.Install → 子进程 stdio → 握手），
//     验证出口标准「工具包子进程隔离生效」；
//  3. 联通测试的错误路径验证（-exit-on-start / -no-tools 变体）。
//
// 工具集（覆盖三种返回形态）：
//
//	echo(text)           — 回显（正常返回）
//	add(a, b)            — 求和（结构化参数解析）
//	fail(reason?)        — isError=true（工具级错误，不中断 Run）
//
// 传输：
//
//	fake-mcp -transport stdio                      （默认；stdin/stdout 换行 JSON）
//	fake-mcp -transport http -addr 127.0.0.1:0     （/mcp 端点，Streamable HTTP）
//	fake-mcp -transport sse  -addr 127.0.0.1:0     （/sse 端点，旧版 HTTP+SSE）
//
// 故障变体（测试用）：
//
//	-exit-on-start   启动即退出（模拟坏包：握手必失败）
//	-no-tools        不暴露任何工具（模拟空清单）
//	-print-addr      http/sse 模式下把实际监听地址打印到 stderr（测试读端口）
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	transport := flag.String("transport", "stdio", "stdio | http | sse")
	addr := flag.String("addr", "127.0.0.1:0", "http/sse 监听地址")
	exitOnStart := flag.Bool("exit-on-start", false, "启动即退出（坏包 fixture）")
	noTools := flag.Bool("no-tools", false, "不暴露任何工具（空清单 fixture）")
	printAddr := flag.Bool("print-addr", false, "打印实际监听地址到 stderr")
	flag.Parse()

	if *exitOnStart {
		fmt.Fprintln(os.Stderr, "fake-mcp: -exit-on-start，立即退出")
		os.Exit(3)
	}

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake-mcp", Version: "1.0.0"}, nil)
	if !*noTools {
		srv.AddTool(objectTool("echo", "回显输入文本（fixture）"), echoHandler)
		srv.AddTool(objectTool("add", "两数求和（fixture）"), addHandler)
		srv.AddTool(objectTool("fail", "总是返回工具错误（fixture）"), failHandler)
	}

	ctx := context.Background()
	switch *transport {
	case "stdio":
		if err := srv.Run(ctx, &sdkmcp.StdioTransport{}); err != nil {
			log.Fatal(err)
		}
	case "http", "sse":
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			log.Fatalf("监听失败: %v", err)
		}
		if *printAddr {
			fmt.Fprintf(os.Stderr, "FAKE_MCP_ADDR=%s\n", ln.Addr().String())
		}
		var h http.Handler
		if *transport == "http" {
			h = sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
		} else {
			h = sdkmcp.NewSSEHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
		}
		mux := http.NewServeMux()
		if *transport == "http" {
			mux.Handle("/mcp", h)
		} else {
			mux.Handle("/sse", h)
		}
		if err := http.Serve(ln, mux); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("未知 transport: %q", *transport)
	}
}

// objectTool 构造带空 object schema 的工具（SDK 要求 schema 非空）。
func objectTool(name, desc string) *sdkmcp.Tool {
	return &sdkmcp.Tool{
		Name:        name,
		Description: desc,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// echoHandler 回显工具。
func echoHandler(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
	var args struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(req.Params.Arguments, &args)
	if args.Text == "" {
		args.Text = "(空)"
	}
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
		&sdkmcp.TextContent{Text: "echo: " + args.Text},
	}}, nil
}

// addHandler 结构化参数工具。
func addHandler(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
	var args struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	_ = json.Unmarshal(req.Params.Arguments, &args)
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
		&sdkmcp.TextContent{Text: fmt.Sprintf("sum=%d", args.A+args.B)},
	}}, nil
}

// failHandler 工具级错误（isError=true，模型可自我纠正）。
func failHandler(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
	var args struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(req.Params.Arguments, &args)
	reason := args.Reason
	if reason == "" {
		reason = "fixture 注定的失败"
	}
	return &sdkmcp.CallToolResult{
		IsError: true,
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "fail: " + reason}},
	}, nil
}
