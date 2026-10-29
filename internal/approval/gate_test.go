package approval

import (
	"context"
	"testing"
	"time"

	"harness-go/internal/model"
)

func TestReadonlyAlwaysAllowed(t *testing.T) {
	g := NewGate(time.Second)
	for _, mode := range []AccessMode{ModeConfirm, ModeFull} {
		v := g.Evaluate(Request{ToolName: "read_file", Risk: model.RiskReadonly, AccessMode: mode})
		if v.NeedApproval {
			t.Fatalf("%s 模式下只读工具不应打扰用户", mode)
		}
		if v.Auto != AllowOnce {
			t.Fatalf("应自动放行，得到 %s", v.Auto)
		}
	}
}

func TestConfirmModeRequiresApproval(t *testing.T) {
	g := NewGate(time.Second)
	for _, risk := range []model.Risk{model.RiskWrite, model.RiskCommand, model.RiskDanger} {
		v := g.Evaluate(Request{ToolName: "x", Risk: risk, AccessMode: ModeConfirm})
		if !v.NeedApproval {
			t.Fatalf("confirm 模式下 %s 应需审批", risk)
		}
	}
}

func TestFullModeAutoAllowsExceptDanger(t *testing.T) {
	g := NewGate(time.Second)
	for _, risk := range []model.Risk{model.RiskWrite, model.RiskCommand} {
		v := g.Evaluate(Request{ToolName: "x", Risk: risk, AccessMode: ModeFull})
		if v.NeedApproval {
			t.Fatalf("full 模式下 %s 应自动放行", risk)
		}
	}
	// danger 必须逐次确认，不可永久放行
	v := g.Evaluate(Request{ToolName: "x", Risk: model.RiskDanger, AccessMode: ModeFull})
	if !v.NeedApproval {
		t.Fatal("full 模式下 danger 仍须审批")
	}
}

func TestSessionRuleSkipsApproval(t *testing.T) {
	g := NewGate(time.Second)
	rules := []Rule{{ToolName: "write_file", Pattern: ""}}
	v := g.Evaluate(Request{
		ToolName: "write_file", Risk: model.RiskWrite,
		AccessMode: ModeConfirm, Rules: rules,
	})
	if v.NeedApproval {
		t.Fatal("命中会话规则应自动放行")
	}

	// 规则只对同名工具生效
	v2 := g.Evaluate(Request{
		ToolName: "run_command", Risk: model.RiskCommand,
		AccessMode: ModeConfirm, Rules: rules,
	})
	if !v2.NeedApproval {
		t.Fatal("规则不应作用于其他工具")
	}
}

func TestRulePatternMatching(t *testing.T) {
	g := NewGate(time.Second)
	rules := []Rule{{ToolName: "run_command", Pattern: "go test"}}
	// 参数包含前缀 → 放行
	if v := g.Evaluate(Request{
		ToolName: "run_command", Risk: model.RiskCommand, AccessMode: ModeConfirm,
		ArgsJSON: `{"cmd":"go test ./..."}`, Rules: rules,
	}); v.NeedApproval {
		t.Fatal("参数命中规则前缀应放行")
	}
	// 参数不含前缀 → 仍需审批
	if v := g.Evaluate(Request{
		ToolName: "run_command", Risk: model.RiskCommand, AccessMode: ModeConfirm,
		ArgsJSON: `{"cmd":"rm -rf /"}`, Rules: rules,
	}); !v.NeedApproval {
		t.Fatal("参数不含规则前缀应需审批")
	}
}

// 审批超时视为拒绝（原因要能回传模型）。
func TestAwaitTimeoutDenies(t *testing.T) {
	g := NewGate(80 * time.Millisecond)
	const id = "ap-timeout"
	if _, err := g.Register(id); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	d, reason := g.Await(context.Background(), id)
	if d != Deny {
		t.Fatalf("超时应视为拒绝，得到 %s", d)
	}
	if reason == "" {
		t.Fatal("拒绝原因不能为空（需回传模型）")
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("不应立即返回")
	}
	if g.PendingCount() != 0 {
		t.Fatalf("超时后应清理待决项，剩余 %d", g.PendingCount())
	}
}

func TestResolveDeliversDecision(t *testing.T) {
	g := NewGate(time.Second)
	const id = "ap-ok"
	if _, err := g.Register(id); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		if !g.Resolve(id, AllowSession) {
			t.Error("Resolve 应命中待决项")
		}
	}()
	d, reason := g.Await(context.Background(), id)
	if d != AllowSession {
		t.Fatalf("应收到 allow_session，得到 %s", d)
	}
	if reason != "" {
		t.Fatalf("正常决策不应有拒绝原因: %s", reason)
	}
	if g.PendingCount() != 0 {
		t.Fatal("决策后应清理待决项")
	}
}

func TestAwaitContextCancelDenies(t *testing.T) {
	g := NewGate(5 * time.Second)
	const id = "ap-cancel"
	if _, err := g.Register(id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	d, reason := g.Await(ctx, id)
	if d != Deny {
		t.Fatalf("取消应视为拒绝，得到 %s", d)
	}
	if reason == "" {
		t.Fatal("应给出取消原因")
	}
}

func TestRegisterRejectsDuplicateAndEmpty(t *testing.T) {
	g := NewGate(time.Second)
	if _, err := g.Register(""); err == nil {
		t.Fatal("空 approvalID 应报错")
	}
	if _, err := g.Register("dup"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Register("dup"); err == nil {
		t.Fatal("重复登记应报错")
	}
	if g.Resolve("nonexistent", AllowOnce) {
		t.Fatal("不存在的审批项 Resolve 应返回 false")
	}
}

func TestAwaitUnknownApprovalDenies(t *testing.T) {
	g := NewGate(time.Second)
	d, reason := g.Await(context.Background(), "never-registered")
	if d != Deny || reason == "" {
		t.Fatalf("未知审批应拒绝并给原因: %s / %s", d, reason)
	}
}
