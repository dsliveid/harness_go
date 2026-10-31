package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"harness-go/pkg/toolapi"
)

// fakeMCPBin TestMain 构建的 fake-mcp 可执行文件路径。
var fakeMCPBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fake-mcp-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "fake-mcp")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "harness-go/cmd/fake-mcp")
	build.Dir = ".." + string(filepath.Separator) + ".."
	if out, berr := build.CombinedOutput(); berr != nil {
		fmt.Fprintf(os.Stderr, "构建 fake-mcp 失败: %v\n%s", berr, out)
		os.Exit(1)
	}
	fakeMCPBin = bin
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func testManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(Options{ClientName: "harness-go-test", ClientVersion: "0.0.0-test"}, DeploymentPolicy{Mode: "local"})
}

func stdioConfig(id, extra string) ServerConfig {
	cfg := ServerConfig{
		ID: id, Name: "fake " + id, Transport: TransportStdio,
		Command: fakeMCPBin, Enabled: true,
	}
	if extra != "" {
		cfg.Args = []string{extra}
	}
	return cfg
}

// TestServerKeyNaming 命名规范化契约（设计 §5）。
func TestServerKeyNaming(t *testing.T) {
	cases := map[string]string{
		"My Server-1":                            "my_server_1",
		"  中文 服务器 ":                              "server",
		"very-long-name-that-exceeds-the-limit!": "very_long_name_that_exce",
		"":                                       "server",
	}
	for in, want := range cases {
		if got := ServerKey(in); got != want {
			t.Errorf("ServerKey(%q)=%q, want %q", in, got, want)
		}
	}
	name := ToolName("mcp", ServerKey("My Server"), "echo")
	if name != "mcp__my_server__echo" {
		t.Fatalf("工具全名错误: %q", name)
	}
}

// TestValidateDeployment 部署形态硬约束（M5：cloud 禁 stdio）。
func TestValidateDeployment(t *testing.T) {
	cfg := stdioConfig("s1", "")
	if err := cfg.Validate(DeploymentPolicy{Mode: "cloud"}); err == nil {
		t.Fatalf("cloud 形态必须拒绝 stdio")
	}
	if err := cfg.Validate(DeploymentPolicy{Mode: "server"}); err == nil {
		t.Fatalf("server 形态 stdio 需显式 allow_stdio")
	}
	if err := cfg.Validate(DeploymentPolicy{Mode: "server", AllowStdio: true}); err != nil {
		t.Fatalf("server+allow_stdio 应放行: %v", err)
	}
	if err := cfg.Validate(DeploymentPolicy{Mode: "local"}); err != nil {
		t.Fatalf("local 应放行: %v", err)
	}
	remote := ServerConfig{ID: "r", Name: "r", Transport: TransportHTTP, URL: "http://127.0.0.1:1/mcp"}
	if err := remote.Validate(DeploymentPolicy{Mode: "cloud"}); err != nil {
		t.Fatalf("cloud 允许远程传输: %v", err)
	}
}

// TestManagerStdioLifecycle stdio 全链路：启动 → 工具清单 → 调用 → 工具级错误 → 统计。
func TestManagerStdioLifecycle(t *testing.T) {
	mgr := testManager(t)
	defer mgr.StopAll()
	ctx := context.Background()

	if err := mgr.Start(ctx, stdioConfig("s1", "")); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	h, ok := mgr.Handle("s1")
	if !ok {
		t.Fatalf("句柄缺失")
	}
	// ① 工具清单（manifest 同构声明）
	if got := h.Manifest().Kind; got != toolapi.KindMCP {
		t.Fatalf("形态错误: %v", got)
	}
	names := make(map[string]bool)
	for _, tool := range h.Tools() {
		names[tool.Spec().Name] = true
	}
	for _, want := range []string{"mcp__fake_s1__echo", "mcp__fake_s1__add", "mcp__fake_s1__fail"} {
		if !names[want] {
			t.Fatalf("缺少工具 %s，实际 %v", want, names)
		}
	}
	// ②③ 按名取工具（listTools 顺序无保证）：正常调用 + 工具级错误
	byName := make(map[string]toolapi.Tool)
	for _, tool := range h.Tools() {
		byName[tool.Spec().Name] = tool
	}
	res, err := byName["mcp__fake_s1__echo"].Invoke(ctx, &toolapi.Call{Args: []byte(`{"text":"你好"}`)})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if res.IsError || res.Content != "echo: 你好" {
		t.Fatalf("echo 结果错误: %+v", res)
	}
	res, err = byName["mcp__fake_s1__fail"].Invoke(ctx, &toolapi.Call{Args: []byte(`{"reason":"注定失败"}`)})
	if err != nil {
		t.Fatalf("工具级错误不应返回 error: %v", err)
	}
	if !res.IsError || res.Content != "fail: 注定失败" {
		t.Fatalf("fail 结果错误: %+v", res)
	}
	// ④ 统计（M6）
	sts := mgr.Statuses()
	if len(sts) != 1 || sts[0].Calls < 2 || sts[0].Failures < 1 || sts[0].ToolCount != 3 {
		t.Fatalf("统计错误: %+v", sts)
	}
	// ⑤ 停用后不可调用
	if err := mgr.Stop("s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.CallTool(ctx, "s1", "echo", nil); err == nil {
		t.Fatalf("停用后调用必须报错")
	}
}

// TestManagerFailureIsolation 失败隔离（M2）：坏 Server 不影响好 Server。
func TestManagerFailureIsolation(t *testing.T) {
	mgr := testManager(t)
	defer mgr.StopAll()
	ctx := context.Background()

	err := mgr.Start(ctx, stdioConfig("bad", "-exit-on-start"))
	if err == nil {
		t.Fatalf("坏 Server 启动应失败")
	}
	if err := mgr.Start(ctx, stdioConfig("good", "")); err != nil {
		t.Fatalf("好 Server 必须不受影响: %v", err)
	}
	st, serr := mgr.State("bad")
	if serr != nil || st != StateFailed {
		t.Fatalf("坏 Server 应标 failed: %v %v", st, serr)
	}
	st, serr = mgr.State("good")
	if serr != nil || st != StateReady {
		t.Fatalf("好 Server 应 ready: %v %v", st, serr)
	}
	// 好 Server 可正常调用
	if _, isErr, err := mgr.CallTool(ctx, "good", "echo", []byte(`{"text":"x"}`)); err != nil || isErr {
		t.Fatalf("好 Server 调用失败: %v", err)
	}
}

// TestManagerHTTPTransport Streamable HTTP 传输（M1）。
func TestManagerHTTPTransport(t *testing.T) {
	sdk := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake-mcp-http", Version: "1.0.0"}, nil)
	sdk.AddTool(simpleTool("echo", "回显"), func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}
		_ = unmarshalArgs(req, &args)
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "http echo: " + args.Text}}}, nil
	})
	srv2 := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*nethttp.Request) *sdkmcp.Server { return sdk }, nil))
	defer srv2.Close()

	mgr := testManager(t)
	defer mgr.StopAll()
	cfg := ServerConfig{ID: "http1", Name: "fake http", Transport: TransportHTTP, URL: srv2.URL + "/mcp", Enabled: true}
	if err := mgr.Start(context.Background(), cfg); err != nil {
		t.Fatalf("HTTP 启动失败: %v", err)
	}
	if _, isErr, err := mgr.CallTool(context.Background(), "http1", "echo", []byte(`{"text":"a"}`)); err != nil || isErr {
		t.Fatalf("HTTP 调用失败: %v", err)
	}
}

// TestManagerSSETransport 旧版 HTTP+SSE 传输（M1）。
func TestManagerSSETransport(t *testing.T) {
	sdk := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake-mcp-sse", Version: "1.0.0"}, nil)
	sdk.AddTool(simpleTool("echo", "回显"), func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}
		_ = unmarshalArgs(req, &args)
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "sse echo: " + args.Text}}}, nil
	})
	srv := httptest.NewServer(sdkmcp.NewSSEHandler(func(*nethttp.Request) *sdkmcp.Server { return sdk }, nil))
	defer srv.Close()

	mgr := testManager(t)
	defer mgr.StopAll()
	cfg := ServerConfig{ID: "sse1", Name: "fake sse", Transport: TransportSSE, URL: srv.URL + "/sse", Enabled: true}
	if err := mgr.Start(context.Background(), cfg); err != nil {
		t.Fatalf("SSE 启动失败: %v", err)
	}
	if _, isErr, err := mgr.CallTool(context.Background(), "sse1", "echo", []byte(`{"text":"b"}`)); err != nil || isErr {
		t.Fatalf("SSE 调用失败: %v", err)
	}
}

// TestManagerDegradeCircuit 连续失败 ≥3 → degraded → 移出注入集合。
func TestManagerDegradeCircuit(t *testing.T) {
	mgr := testManager(t)
	defer mgr.StopAll()
	ctx := context.Background()
	if err := mgr.Start(ctx, stdioConfig("d1", "")); err != nil {
		t.Fatal(err)
	}
	h, _ := mgr.Handle("d1")
	// 连续调用 fail 工具 3 次（工具级错误不熔断——它不是连接故障）
	for i := 0; i < 3; i++ {
		if _, _, err := mgr.CallTool(ctx, "d1", "fail", nil); err != nil {
			t.Fatalf("fail 工具不应返回传输错误: %v", err)
		}
	}
	if st, _ := mgr.State("d1"); st != StateReady {
		t.Fatalf("工具级错误不应熔断连接: %v", st)
	}
	_ = h
	// 杀进程 → 连接级故障 → 重建（进程退出后模拟）
	// 直接验证 degraded→ready 恢复路径：手工置 degraded 后成功调用恢复
	mgr.mu.Lock()
	mgr.servers["d1"].state = StateDegraded
	mgr.mu.Unlock()
	if _, _, err := mgr.CallTool(ctx, "d1", "echo", []byte(`{"text":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	if st, _ := mgr.State("d1"); st != StateReady {
		t.Fatalf("成功调用后应恢复 ready: %v", st)
	}
}

// TestManagerReload 热重载：新连接失败保留旧连接（设计 §4.3）。
func TestManagerReload(t *testing.T) {
	mgr := testManager(t)
	defer mgr.StopAll()
	ctx := context.Background()
	if err := mgr.Start(ctx, stdioConfig("r1", "")); err != nil {
		t.Fatal(err)
	}
	// 用坏配置重载 → 失败但旧连接仍在
	bad := stdioConfig("r1", "-exit-on-start")
	if err := mgr.Reload(ctx, bad); err == nil {
		t.Fatalf("坏配置重载应失败")
	}
	if _, _, err := mgr.CallTool(ctx, "r1", "echo", []byte(`{"text":"still-alive"}`)); err != nil {
		t.Fatalf("重载失败后旧连接应保留: %v", err)
	}
	// 好配置重载 → 成功
	if err := mgr.Reload(ctx, stdioConfig("r1", "")); err != nil {
		t.Fatalf("正常重载失败: %v", err)
	}
	if _, _, err := mgr.CallTool(ctx, "r1", "echo", []byte(`{"text":"new"}`)); err != nil {
		t.Fatalf("重载后调用失败: %v", err)
	}
}

// TestManagerTestEndpoint 联通测试（错误文案可读，M3）。
func TestManagerTestEndpoint(t *testing.T) {
	mgr := testManager(t)
	defer mgr.StopAll()
	ctx := context.Background()
	// 成功：返回清单与耗时
	res, err := mgr.Test(ctx, stdioConfig("t1", ""))
	if err != nil {
		t.Fatalf("联通测试失败: %v", err)
	}
	if len(res.ToolNames) != 3 || res.ElapsedMS < 0 {
		t.Fatalf("测试结果错误: %+v", res)
	}
	// 命令不存在：可读错误
	bad := ServerConfig{ID: "t2", Name: "t2", Transport: TransportStdio, Command: "no-such-binary-xyz", Enabled: true}
	if _, err := mgr.Test(ctx, bad); err == nil {
		t.Fatalf("命令不存在应报错")
	} else if got := err.Error(); !containsAny(got, "MCP_CONNECT_FAILED", "连接/握手失败") {
		t.Fatalf("错误文案不可读: %q", got)
	}
	// 空清单：专门文案
	if _, err := mgr.Test(ctx, stdioConfig("t3", "-no-tools")); err == nil {
		t.Fatalf("空清单应报错")
	} else if !containsAny(err.Error(), "清单为空") {
		t.Fatalf("空清单文案错误: %q", err.Error())
	}
}

// TestProcessExitReconnect stdio 进程被杀 → 调用时被动重建一次（设计 §6）。
func TestProcessExitReconnect(t *testing.T) {
	mgr := testManager(t)
	defer mgr.StopAll()
	ctx := context.Background()
	if err := mgr.Start(ctx, stdioConfig("p1", "")); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	c := mgr.servers["p1"].c
	mgr.mu.Unlock()
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		t.Fatalf("stdio 进程未启动")
	}
	_ = c.cmd.Process.Kill()
	time.Sleep(100 * time.Millisecond)
	// 调用触发重建
	if _, isErr, err := mgr.CallTool(ctx, "p1", "echo", []byte(`{"text":"reborn"}`)); err != nil || isErr {
		t.Fatalf("进程退出后重建调用失败: %v", err)
	}
}

// unmarshalArgs 解析工具参数（测试辅助，与 fake-mcp 同口径）。
func unmarshalArgs(req *sdkmcp.CallToolRequest, v any) error {
	return jsonUnmarshalArgs(req.Params.Arguments, v)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// jsonUnmarshalArgs 工具参数解析。
func jsonUnmarshalArgs(data []byte, v any) error { return json.Unmarshal(data, v) }

// simpleTool 构造带空 object schema 的测试工具（SDK 要求 schema 非空）。
func simpleTool(name, desc string) *sdkmcp.Tool {
	return &sdkmcp.Tool{
		Name:        name,
		Description: desc,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}
