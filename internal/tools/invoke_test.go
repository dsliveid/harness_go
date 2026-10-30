package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"harness-go/internal/agent"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
	"harness-go/internal/tools/loader"
	"harness-go/internal/tools/registry"
	"harness-go/internal/tools/router"
	"harness-go/pkg/toolapi"
)

func newTestRunner(t *testing.T, specs ...toolapi.Spec) *Runner {
	t.Helper()
	reg := registry.New(registry.Options{})
	for _, s := range specs {
		s := s
		tool := toolapi.ToolFunc{S: s, InvokeF: func(_ context.Context, call *toolapi.Call) (*toolapi.Result, error) {
			switch s.Name {
			case "boom":
				return &toolapi.Result{Content: "执行失败：磁盘只读", IsError: true}, nil
			case "panic":
				return nil, errors.New("底层爆炸")
			case "denied":
				return nil, toolapi.ErrDenied
			default:
				return &toolapi.Result{Content: "echo: " + string(call.Args)}, nil
			}
		}}
		if err := reg.RegisterTool(tool, "test"); err != nil {
			t.Fatalf("注册测试工具失败: %v", err)
		}
	}
	rtr := router.New(router.Options{
		Executor: toolapi.ExecutorServer,
		Capabilities: map[string]bool{
			toolapi.CapWorkspaceRead: true,
		},
	})
	return NewRunner(reg, rtr, loader.Limits{CoreSoft: 8, DeferredSoft: 60})
}

func specOf(name string, tier toolapi.Tier, risk toolapi.Risk, caps ...string) toolapi.Spec {
	return toolapi.Spec{Name: name, Description: name + " 描述", Tier: tier, Risk: risk,
		Capabilities: caps, SchemaJSON: []byte(`{"type":"object"}`)}
}

// TestRunSuccess 校验成功路径：结果必须是**合法 JSON**（json.RawMessage 落库前提）。
func TestRunSuccess(t *testing.T) {
	r := newTestRunner(t, specOf("echo", toolapi.TierCore, toolapi.RiskReadonly))
	res, err := r.Run(context.Background(), agent.ToolRunRequest{
		RunID: "run-1", SessionID: "s-1", ToolEventID: "ev-1", ToolName: "echo",
		ArgsJSON: `{"text":"hi"}`, AccessMode: "confirm",
	})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Status != model.ToolOK {
		t.Fatalf("状态应为 ok，得到 %s", res.Status)
	}
	if !json.Valid([]byte(res.ResultJSON)) {
		t.Fatalf("工具结果必须是合法 JSON（否则 tool_events 序列化会失败）: %q", res.ResultJSON)
	}
	var payload struct {
		Content string `json:"content"`
	}
	if uErr := json.Unmarshal([]byte(res.ResultJSON), &payload); uErr != nil {
		t.Fatalf("结果解析失败: %v", uErr)
	}
	if payload.Content != `echo: {"text":"hi"}` {
		t.Fatalf("结果内容异常: %q", payload.Content)
	}
}

func TestRunFailuresAreResults(t *testing.T) {
	r := newTestRunner(t,
		specOf("boom", toolapi.TierCore, toolapi.RiskWrite),
		specOf("panic", toolapi.TierCore, toolapi.RiskWrite),
		specOf("denied", toolapi.TierCore, toolapi.RiskWrite),
		specOf("device-only", toolapi.TierCore, toolapi.RiskWrite, toolapi.CapDevice),
	)
	cases := []struct {
		name       string
		wantStatus model.ToolEventStatus
	}{
		{"boom", model.ToolError},
		{"panic", model.ToolError},
		{"denied", model.ToolDenied},
		{"device-only", model.ToolError},
		{"not-registered", model.ToolError},
	}
	for _, c := range cases {
		res, err := r.Run(context.Background(), agent.ToolRunRequest{ToolName: c.name, ArgsJSON: `{}`})
		if err != nil {
			t.Fatalf("%s 不应返回 error（失败必须走 Status，避免重复落库）: %v", c.name, err)
		}
		if res.Status != c.wantStatus || res.Error == "" {
			t.Fatalf("%s 期望 %s，得到 %+v", c.name, c.wantStatus, res)
		}
	}
}

func TestRiskAndPromptRefs(t *testing.T) {
	r := newTestRunner(t,
		specOf("a", toolapi.TierCore, toolapi.RiskReadonly),
		specOf("b", toolapi.TierDeferred, toolapi.RiskWrite),
		specOf("c", toolapi.TierOnDemand, toolapi.RiskDanger),
	)
	if risk, ok := r.Risk("a"); !ok || risk != model.RiskReadonly {
		t.Fatalf("风险级映射错误: %q %v", risk, ok)
	}
	if _, ok := r.Risk("ghost"); ok {
		t.Fatal("未注册工具不应返回风险级")
	}

	refs := r.PromptRefs()
	byName := map[string]prompt.Tier{}
	for _, ref := range refs {
		byName[ref.Name] = ref.Tier
	}
	if byName["a"] != prompt.TierCore || byName["b"] != prompt.TierDeferred || byName["c"] != prompt.TierOnDemand {
		t.Fatalf("tier 映射漂移: %+v", byName)
	}

	inj := r.Injection()
	if len(inj.Core) != 1 || len(inj.DeferredIndex) != 1 || len(inj.OnDemand) != 1 {
		t.Fatalf("注入分层异常: %+v", inj)
	}
}

func TestOverridesAffectInjection(t *testing.T) {
	r := newTestRunner(t, specOf("a", toolapi.TierCore, toolapi.RiskReadonly))
	r.SetOverrides(map[string]toolapi.Tier{"a": toolapi.TierOnDemand})
	inj := r.Injection()
	if len(inj.Core) != 0 || len(inj.OnDemand) != 1 {
		t.Fatalf("覆盖后应立即重新分档: %+v", inj)
	}
	if snap := r.Snapshot(); snap.OnDemand[0].DefaultTier != toolapi.TierCore {
		t.Fatalf("default_tier 不应被覆盖改写: %+v", snap.OnDemand[0])
	}
}
