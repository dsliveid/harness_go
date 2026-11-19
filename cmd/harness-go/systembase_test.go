package main

import (
	"strings"
	"testing"

	"harness-go/internal/sop"
)

// TestDefaultSystemBaseIsSingleSource 兜底系统提示必须与 embed 出厂真源同源。
//
// # 守的是什么
//
// 改造前 cmd/harness-go/main.go 里有一份与 internal/sop/builtin/agent-core.md
// **逐字重复**的常量副本，靠人工保持一致。这种双真源必然漂移：
// 改了 embed 忘了改常量（或反之），两条路径行为分叉，而文本没有编译器
// 守护 —— 任何测试都发现不了。
//
// 本测试把"两份文本必须逐字相同"变成可执行断言。
// 现在两者本就是同一个值（defaultSystemBase = sop.BuiltinCoreText()），
// 因此它同时是一道**防止重新内联副本**的护栏：若有人再写一份字面量，
// 这条断言就会在文本出现差异时立刻失败。
func TestDefaultSystemBaseIsSingleSource(t *testing.T) {
	want := sop.BuiltinCoreText()
	if strings.TrimSpace(want) == "" {
		t.Fatal("embed 出厂正文为空：go:embed 可能失效")
	}
	if defaultSystemBase != want {
		t.Fatalf("兜底系统提示与出厂真源不一致（双真源漂移）：\n"+
			"请勿在 main.go 重新内联副本，改用 sop.BuiltinCoreText()。\n"+
			"兜底长度=%d，真源长度=%d", len(defaultSystemBase), len(want))
	}
}

// TestDefaultSystemBaseHasNoToolInstructionDrift 兜底文本里提到的工具名必须真实存在。
//
// 与 toolref_guard_test.go 同一口径，但单独覆盖"默认兜底路径"：
// 即使 SOP 服务未装配（测试/降级），这段文本也必须是可兑现的。
func TestDefaultSystemBaseHasNoToolInstructionDrift(t *testing.T) {
	registered := registeredToolNames()
	refs := sop.ExtractToolRefs(defaultSystemBase)
	var missing []string
	for _, ref := range refs {
		if _, ok := registered[ref]; !ok {
			missing = append(missing, ref)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("兜底系统提示引用了不存在的工具：%v", missing)
	}
}
