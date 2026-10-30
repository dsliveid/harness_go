package echo

import (
	"context"
	"encoding/json"
	"testing"

	"harness-go/pkg/toolapi"
)

func TestManifestIsValid(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("清单必须合法: %v", err)
	}
	if m.ID != "builtin.echo" || m.Kind != toolapi.KindBuiltin || m.ProtocolVersion != toolapi.ProtocolVersion {
		t.Fatalf("清单元信息异常: %+v", m)
	}
	if len(m.Tools) != 1 {
		t.Fatalf("应暴露 1 个工具，得到 %d", len(m.Tools))
	}
	s := m.Tools[0]
	if s.Name != ToolName || s.Tier != toolapi.TierCore || s.Risk != toolapi.RiskReadonly {
		t.Fatalf("fixture 工具规格异常: %+v", s)
	}
	if !json.Valid(s.SchemaJSON) {
		t.Fatal("schema 必须是合法 JSON")
	}
}

func TestFactoryExposesTool(t *testing.T) {
	p, err := Factory().New(nil, nil)
	if err != nil {
		t.Fatalf("插件初始化失败: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	tools := p.Tools()
	if len(tools) != 1 || tools[0].Spec().Name != ToolName {
		t.Fatalf("工具列表异常: %+v", tools)
	}
	if p.Health().Status != toolapi.HealthAvailable {
		t.Fatalf("健康态应为 available，得到 %q", p.Health().Status)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
}

func TestInvokeEchoesText(t *testing.T) {
	tool := &echoTool{spec: toolSpec()}
	res, err := tool.Invoke(context.Background(), &toolapi.Call{
		Name: ToolName, Args: json.RawMessage(`{"text":"你好"}`), ToolEventID: "ev-1",
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if res.IsError || res.Content != "echo: 你好" {
		t.Fatalf("回显结果异常: %+v", res)
	}
	if res.Meta["tool_event_id"] != "ev-1" {
		t.Fatalf("应回传 tool_event_id，得到 %+v", res.Meta)
	}
}

func TestInvokeRejectsBadArgs(t *testing.T) {
	tool := &echoTool{spec: toolSpec()}
	cases := []string{`{}`, `{"text":""}`, `not-json`}
	for _, args := range cases {
		res, err := tool.Invoke(context.Background(), &toolapi.Call{Args: json.RawMessage(args)})
		if err != nil {
			t.Fatalf("参数错误不应返回 error（要给模型看的失败原因）: %v", err)
		}
		if res == nil || !res.IsError || res.Content == "" {
			t.Fatalf("参数 %q 应返回 IsError 结果，得到 %+v", args, res)
		}
	}
}
