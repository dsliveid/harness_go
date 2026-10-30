package toolapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestParseTierFallback(t *testing.T) {
	cases := map[string]Tier{
		"core":      TierCore,
		"CORE":      TierCore,
		" core ":    TierCore,
		"deferred":  TierDeferred,
		"on_demand": TierOnDemand,
		"":          TierDeferred,
		"unknown":   TierDeferred,
	}
	for in, want := range cases {
		if got := ParseTier(in); got != want {
			t.Fatalf("ParseTier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpecNormalizedDefaults(t *testing.T) {
	s := Spec{Name: "  echo  ", Description: "回显输入"}
	n := s.Normalized()
	if n.Name != "echo" {
		t.Fatalf("Name 未 trim: %q", n.Name)
	}
	if n.Tier != TierDeferred {
		t.Fatalf("空 tier 应回落 deferred，得到 %q", n.Tier)
	}
	if n.Executor != ExecutorServer {
		t.Fatalf("空 executor 应回落 server，得到 %q", n.Executor)
	}
	if n.Risk != RiskWrite {
		t.Fatalf("空 risk 应保守回落 write，得到 %q", n.Risk)
	}
	if n.Namespace != "builtin" {
		t.Fatalf("空 namespace 应为 builtin，得到 %q", n.Namespace)
	}
	if n.CostTokens <= 0 {
		t.Fatalf("成本应自动估算，得到 %d", n.CostTokens)
	}
	if s.Tier != "" {
		t.Fatalf("Normalized 不允许修改接收者")
	}
}

func TestSpecValidate(t *testing.T) {
	ok := Spec{Name: "echo", Tier: TierCore, SchemaJSON: json.RawMessage(`{"type":"object"}`)}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法规格不应报错: %v", err)
	}
	if err := (Spec{Name: ""}).Validate(); err == nil {
		t.Fatal("空工具名应报错")
	}
	if err := (Spec{Name: "x", Tier: "bogus"}).Validate(); err == nil {
		t.Fatal("非法 tier 应报错")
	}
	if err := (Spec{Name: "x", Tier: TierCore, Executor: "moon"}).Validate(); err == nil {
		t.Fatal("非法 executor 应报错")
	}
	if err := (Spec{Name: "x", Tier: TierCore, SchemaJSON: json.RawMessage(`{`)}).Validate(); err == nil {
		t.Fatal("非法 schema 应报错")
	}
}

func TestEstimateCostTokensPositive(t *testing.T) {
	if got := EstimateCostTokens("", "", nil); got <= 0 {
		t.Fatalf("空定义也需要正成本，得到 %d", got)
	}
	long := EstimateCostTokens("fs", "读取文件", json.RawMessage(`{"type":"object","properties":{}}`))
	if long <= 1 {
		t.Fatalf("带 schema 的工具成本应大于 1，得到 %d", long)
	}
}

func TestChildStatusTerminal(t *testing.T) {
	terminal := []ChildStatus{ChildCompleted, ChildFailed, ChildCancelled, ChildInterrupted}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Fatalf("%q 应为终态", s)
		}
	}
	nonTerminal := []ChildStatus{ChildPending, ChildRunning, ChildWaitingApproval}
	for _, s := range nonTerminal {
		if s.Terminal() {
			t.Fatalf("%q 不应为终态（禁止用「没在跑」推断成功）", s)
		}
	}
}

func TestManifestValidate(t *testing.T) {
	m := Manifest{ID: "echo", Name: "Echo", Version: "0.1.0", Kind: KindBuiltin, ProtocolVersion: ProtocolVersion,
		Tools: []Spec{{Name: "echo", Tier: TierCore}}}
	if err := m.Validate(); err != nil {
		t.Fatalf("合法清单不应报错: %v", err)
	}
	bad := []Manifest{
		{Name: "x", Kind: KindBuiltin, ProtocolVersion: 1},
		{ID: "x", Kind: KindBuiltin, ProtocolVersion: 1},
		{ID: "x", Name: "x", Kind: KindBuiltin},
		{ID: "x", Name: "x", Kind: "magic", ProtocolVersion: 1},
	}
	for i, b := range bad {
		if err := b.Validate(); err == nil {
			t.Fatalf("第 %d 个非法清单应报错", i)
		}
	}
}

func TestToolFuncNotImplemented(t *testing.T) {
	tf := ToolFunc{S: Spec{Name: "echo"}}
	if _, err := tf.Invoke(context.Background(), &Call{Name: "echo"}); !IsNotImplemented(err) {
		t.Fatalf("未提供实现应返回 ErrNotImplemented，得到 %v", err)
	}
	if errors.Is(nil, ErrNotImplemented) {
		t.Fatal("nil 不应被判为未实现")
	}
}
