package mcp

import (
	"context"
	nethttp "net/http"
	"net/http/httptest"
	"testing"

	"harness-go/pkg/toolapi"
)

// ============ HTTP 插件协议（manifest 发现 + tools/call） ============

// TestHTTPPluginDiscovery 联通：manifest 发现 → 工具注册形状 → 调用。
func TestHTTPPluginDiscovery(t *testing.T) {
	mux := nethttp.NewServeMux()
	mux.HandleFunc("/manifest.json", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "demo-http",
			"name": "演示 HTTP 插件",
			"version": "1.0.0",
			"kind": "http",
			"protocol_version": 1,
			"default_tier": "deferred",
			"tools": [
				{"name": "greet", "description": "问候", "schema_json": {"type": "object", "properties": {"who": {"type": "string"}}}, "risk": "readonly"}
			]
		}`))
	})
	mux.HandleFunc("/tools/call", func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"你好，HTTP 插件"}],"is_error":false}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, err := NewHTTPPlugin(context.Background(), "demo-http", "演示 HTTP 插件", srv.URL, nil)
	if err != nil {
		t.Fatalf("发现失败: %v", err)
	}
	// manifest 形状
	m := p.Manifest()
	if m.Kind != toolapi.KindHTTP || m.ID != "demo-http" || len(m.Tools) != 1 {
		t.Fatalf("manifest 错误: %+v", m)
	}
	// 工具命名空间与命名
	spec := p.Tools()[0].Spec()
	if spec.Name != "http__demo_http__greet" || spec.Namespace != "http:demo-http" {
		t.Fatalf("工具形状错误: %+v", spec)
	}
	// 调用（含鉴权头透传）
	res, err := p.Tools()[0].Invoke(context.Background(), &toolapi.Call{Args: []byte(`{"who":"HTTP"}`)})
	if err != nil || res.IsError || res.Content != "你好，HTTP 插件" {
		t.Fatalf("调用错误: %v %+v", err, res)
	}
	// 编译期契约
	var _ toolapi.Plugin = p
}

// TestHTTPPluginReadableErrors 可读失败：404 manifest / 非法 JSON。
func TestHTTPPluginReadableErrors(t *testing.T) {
	mux := nethttp.NewServeMux()
	mux.HandleFunc("/manifest.json", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		w.WriteHeader(nethttp.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, err := NewHTTPPlugin(context.Background(), "x", "x", srv.URL, nil); err == nil {
		t.Fatalf("404 必须报错")
	}
}

// TestHTTPPluginEmptyManifestRejected 空 manifest 拒绝注册。
func TestHTTPPluginEmptyManifestRejected(t *testing.T) {
	mux := nethttp.NewServeMux()
	mux.HandleFunc("/manifest.json", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		_, _ = w.Write([]byte(`{"id":"empty","name":"空","version":"1.0.0","kind":"http","protocol_version":1,"tools":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, err := NewHTTPPlugin(context.Background(), "empty", "空", srv.URL, nil); err == nil {
		t.Fatalf("无工具 manifest 必须拒绝（与 MCP 空清单同口径）")
	}
}
