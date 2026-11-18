package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"harness-go/pkg/toolapi"
)

// stubPlugin 测试用插件。
type stubPlugin struct {
	tools  []toolapi.Tool
	health toolapi.Health
}

func (p *stubPlugin) Start(context.Context) error { return nil }
func (p *stubPlugin) Stop(context.Context) error  { return nil }
func (p *stubPlugin) Tools() []toolapi.Tool       { return p.tools }
func (p *stubPlugin) Health() toolapi.Health      { return p.health }

func newTool(name string, tier toolapi.Tier, risk toolapi.Risk) toolapi.Tool {
	return toolapi.ToolFunc{S: toolapi.Spec{
		Name: name, Description: "测试工具 " + name, Tier: tier, Risk: risk,
		SchemaJSON: []byte(`{"type":"object"}`),
	}, InvokeF: func(context.Context, *toolapi.Call) (*toolapi.Result, error) {
		return &toolapi.Result{Content: "ok"}, nil
	}}
}

func manifestFor(id string, tier toolapi.Tier, tools ...string) toolapi.Manifest {
	specs := make([]toolapi.Spec, 0, len(tools))
	for _, n := range tools {
		specs = append(specs, toolapi.Spec{Name: n, Description: "desc", Tier: tier})
	}
	return toolapi.Manifest{ID: id, Name: id, Version: "0.1.0", Kind: toolapi.KindBuiltin,
		DefaultTier: tier, ProtocolVersion: toolapi.ProtocolVersion, Tools: specs}
}

func TestRegisterPluginAndResolve(t *testing.T) {
	r := New(Options{})
	p := &stubPlugin{tools: []toolapi.Tool{newTool("echo", toolapi.TierCore, toolapi.RiskReadonly)}}
	if err := r.RegisterPlugin(context.Background(), manifestFor("builtin.echo", toolapi.TierCore, "echo"), p); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	tool, spec, err := r.Resolve("echo")
	if err != nil || tool == nil {
		t.Fatalf("解析失败: %v", err)
	}
	if spec.Tier != toolapi.TierCore {
		t.Fatalf("tier 应为 core，得到 %q", spec.Tier)
	}
	if spec.CostTokens <= 0 {
		t.Fatalf("成本应自动估算，得到 %d", spec.CostTokens)
	}
	if _, _, err := r.Resolve("nope"); !errors.Is(err, toolapi.ErrNotFound) {
		t.Fatalf("未注册工具应返回 ErrNotFound，得到 %v", err)
	}
	if risk, ok := r.Risk("echo"); !ok || risk != toolapi.RiskReadonly {
		t.Fatalf("Risk 解析错误: %q %v", risk, ok)
	}
}

// TestDisableToolSemantics 校验停用语义（核心不变量）：
//   - 停用 = 移出注入集合 + Resolve 直接失败（不注入也不可调用）；
//   - 停用不改 spec.tier（重新启用后原档位原样恢复）；
//   - 未注册工具名写入时静默忽略（不能因配置残留让启动失败）。
func TestDisableToolSemantics(t *testing.T) {
	r := New(Options{})
	p := &stubPlugin{tools: []toolapi.Tool{
		newTool("echo", toolapi.TierCore, toolapi.RiskReadonly),
		newTool("webfetch", toolapi.TierDeferred, toolapi.RiskReadonly),
	}}
	if err := r.RegisterPlugin(context.Background(), manifestFor("builtin.mix", toolapi.TierDeferred, "echo", "webfetch"), p); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	r.SetDisabled([]string{"echo", "ghost-not-registered"})
	if got := r.Disabled(); len(got) != 1 || got[0] != "echo" {
		t.Fatalf("未注册名应被静默忽略，得到 %v", got)
	}

	// 注入集合：只剩 webfetch
	injected := r.InjectedSpecs()
	if len(injected) != 1 || injected[0].Name != "webfetch" {
		t.Fatalf("停用工具不得注入，得到 %+v", injected)
	}
	// 可调用性：停用工具必须报 ErrUnavailable（而非 ErrNotFound——注册还在，只是不可用）
	if _, _, err := r.Resolve("echo"); !errors.Is(err, toolapi.ErrUnavailable) {
		t.Fatalf("停用工具应返回 ErrUnavailable，得到 %v", err)
	}
	// 档位不受影响：重新启用即恢复 core
	snap := r.Snapshot()
	if len(snap.Disabled) != 1 || snap.Disabled[0] != "echo" {
		t.Fatalf("快照 disabled 清单异常: %v", snap.Disabled)
	}
	for _, s := range snap.Core {
		if s.Name == "echo" && s.Enabled {
			t.Fatalf("core 档内停用工具 enabled 应为 false: %+v", s)
		}
	}

	r.SetDisabled(nil)
	if _, spec, err := r.Resolve("echo"); err != nil || spec.Tier != toolapi.TierCore {
		t.Fatalf("重新启用后应恢复可解析且档位为 core，得到 err=%v tier=%q", err, spec.Tier)
	}
	if len(r.Snapshot().Disabled) != 0 {
		t.Fatal("清空后 disabled 应为空")
	}
}

func TestDuplicateToolNameRejected(t *testing.T) {
	r := New(Options{})
	p1 := &stubPlugin{tools: []toolapi.Tool{newTool("dup", toolapi.TierCore, toolapi.RiskReadonly)}}
	if err := r.RegisterPlugin(context.Background(), manifestFor("p1", toolapi.TierCore, "dup"), p1); err != nil {
		t.Fatal(err)
	}
	p2 := &stubPlugin{tools: []toolapi.Tool{newTool("dup", toolapi.TierCore, toolapi.RiskReadonly)}}
	err := r.RegisterPlugin(context.Background(), manifestFor("p2", toolapi.TierCore, "dup"), p2)
	if !errors.Is(err, toolapi.ErrDuplicate) {
		t.Fatalf("重名应返回 ErrDuplicate，得到 %v", err)
	}
	if snap := r.Snapshot(); snap.Total != 1 {
		t.Fatalf("冲突后不应半注册，Total=%d", snap.Total)
	}
}

func TestTierOverrideLayering(t *testing.T) {
	r := New(Options{})
	p := &stubPlugin{tools: []toolapi.Tool{
		newTool("a", toolapi.TierCore, toolapi.RiskReadonly),
		newTool("b", toolapi.TierCore, toolapi.RiskReadonly),
	}}
	if err := r.RegisterPlugin(context.Background(), manifestFor("p", toolapi.TierCore, "a", "b"), p); err != nil {
		t.Fatal(err)
	}
	// 全局覆盖优先于插件声明
	r.SetOverrides(map[string]toolapi.Tier{"a": toolapi.TierOnDemand})
	snap := r.Snapshot()
	if len(snap.Core) != 1 || snap.Core[0].Name != "b" {
		t.Fatalf("core 应只剩 b，得到 %+v", snap.Core)
	}
	if len(snap.OnDemand) != 1 || snap.OnDemand[0].Name != "a" {
		t.Fatalf("a 应被覆盖为 on_demand，得到 %+v", snap.OnDemand)
	}
	if snap.OnDemand[0].DefaultTier != toolapi.TierCore {
		t.Fatalf("default_tier 必须保留插件声明值，得到 %q", snap.OnDemand[0].DefaultTier)
	}
	// 未注册工具名与非法规格忽略
	r.SetOverrides(map[string]toolapi.Tier{"ghost": toolapi.TierCore, "b": "bogus"})
	if _, _, err := r.Resolve("b"); err != nil {
		t.Fatal(err)
	}
	if len(r.Overrides()) != 0 {
		t.Fatalf("非法/未注册覆盖应被丢弃，得到 %+v", r.Overrides())
	}
}

func TestSoftLimitWarnOnly(t *testing.T) {
	r := New(Options{CoreSoftLimit: 1})
	p := &stubPlugin{tools: []toolapi.Tool{
		newTool("a", toolapi.TierCore, toolapi.RiskReadonly),
		newTool("b", toolapi.TierCore, toolapi.RiskReadonly),
	}}
	if err := r.RegisterPlugin(context.Background(), manifestFor("p", toolapi.TierCore, "a", "b"), p); err != nil {
		t.Fatalf("超软上限不应阻断注册: %v", err)
	}
	warns := r.Warnings()
	if len(warns) == 0 || !strings.Contains(warns[0], "软上限") {
		t.Fatalf("应产生软上限告警，得到 %v", warns)
	}
	// 宽进严出：告警后仍可执行
	if _, _, err := r.Resolve("a"); err != nil {
		t.Fatalf("告警不能影响可用性: %v", err)
	}
}

func TestManifestToolsMismatchRejected(t *testing.T) {
	r := New(Options{})
	p := &stubPlugin{tools: []toolapi.Tool{newTool("real", toolapi.TierCore, toolapi.RiskReadonly)}}
	err := r.RegisterPlugin(context.Background(), manifestFor("p", toolapi.TierCore, "declared"), p)
	if err == nil {
		t.Fatal("清单声明与运行时工具不一致应报错")
	}
}

func TestRegisterFactory(t *testing.T) {
	r := New(Options{})
	f := toolapi.FactoryFunc{
		M: manifestFor("p", toolapi.TierDeferred, "x"),
		NewF: func(toolapi.Host, toolapi.Config) (toolapi.Plugin, error) {
			return &stubPlugin{tools: []toolapi.Tool{newTool("x", toolapi.TierDeferred, toolapi.RiskWrite)}}, nil
		},
	}
	if err := r.RegisterFactory(context.Background(), f, nil, nil); err != nil {
		t.Fatalf("工厂装配失败: %v", err)
	}
	if snap := r.Snapshot(); len(snap.Deferred) != 1 || len(snap.Plugins) != 1 {
		t.Fatalf("快照异常: %+v", snap)
	}
	// 健康态缺省填充
	if snap := r.Snapshot(); snap.Deferred[0].Health.Status != toolapi.HealthAvailable {
		t.Fatalf("健康态应默认 available，得到 %q", snap.Deferred[0].Health.Status)
	}
}
