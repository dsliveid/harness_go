package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 铁律：pwd / sign 绝不出现在日志中（含键名）。
func TestRedactQueryDropsCredentials(t *testing.T) {
	got := RedactQuery("user_name=admin&pwd=supersecret&sign=deadbeef&ts=1700000000")
	if strings.Contains(got, "supersecret") {
		t.Fatalf("泄露了 pwd: %s", got)
	}
	if strings.Contains(got, "pwd") {
		t.Fatalf("不应出现 pwd 键名: %s", got)
	}
	if strings.Contains(got, "sign") || strings.Contains(got, "deadbeef") {
		t.Fatalf("不应出现 sign: %s", got)
	}
	if !strings.Contains(got, "user_name=a***n") {
		t.Fatalf("user_name 应打码保留: %s", got)
	}
}

func TestRedactQueryEdgeCases(t *testing.T) {
	if got := RedactQuery(""); got != "" {
		t.Fatalf("空 query 应返回空: %q", got)
	}
	if got := RedactQuery("pwd=only"); got != "" {
		t.Fatalf("仅含敏感键应返回空: %q", got)
	}
	// 非法转义 → 宁可返回空也不泄露原文
	if got := RedactQuery("user_name=%zz"); got != "" {
		t.Fatalf("非法 query 应返回空: %q", got)
	}
	// redirect 属白名单
	if got := RedactQuery("redirect=/session/1&pwd=x"); got != "redirect=%2Fsession%2F1" {
		t.Fatalf("redirect 应保留: %q", got)
	}
}

func TestMaskName(t *testing.T) {
	cases := map[string]string{
		"":      "",
		"a":     "***",
		"ab":    "***",
		"abc":   "a***c",
		"admin": "a***n",
		"张三丰":   "张***丰",
	}
	for in, want := range cases {
		if got := MaskName(in); got != want {
			t.Fatalf("MaskName(%q) 期望 %q，得到 %q", in, want, got)
		}
	}
}

func TestTraceIDRoundTrip(t *testing.T) {
	id := NewTraceID()
	if len(id) != 16 {
		t.Fatalf("trace_id 应为 16 位十六进制，得到 %q", id)
	}
	if NewTraceID() == id {
		t.Fatal("两次生成的 trace_id 不应相同")
	}
	ctx := WithTrace(context.Background(), id)
	if got := TraceFrom(ctx); got != id {
		t.Fatalf("trace_id 未正确回读: %s", got)
	}
	if got := TraceFrom(context.Background()); got != "" {
		t.Fatalf("缺失时应为空串，得到 %q", got)
	}
}

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(Healthz("9.9.9"))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，得到 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type 错误: %s", ct)
	}
	var body healthBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Version != "9.9.9" {
		t.Fatalf("响应体错误: %+v", body)
	}
	if body.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol_version 应为 %s，得到 %s", ProtocolVersion, body.ProtocolVersion)
	}

	// 非 GET 应 405
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应返回 405，得到 %d", resp2.StatusCode)
	}
}

func TestNewLoggerLevels(t *testing.T) {
	var sb strings.Builder
	l := NewLogger("error", "json", &sb)
	l.Info("should be filtered")
	if sb.Len() != 0 {
		t.Fatalf("error 级别不应输出 info 日志: %s", sb.String())
	}
	l.Error("boom")
	if !strings.Contains(sb.String(), "boom") {
		t.Fatalf("error 日志未输出: %s", sb.String())
	}
}
