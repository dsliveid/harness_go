package prompt

import (
	"strings"
	"testing"
)

// ============ 记忆注入（D-3）：位置铁律 ============

func TestBuildMemoryCoreInStablePrefix(t *testing.T) {
	b := NewBuilder(nil, DefaultLimits())
	res := b.Build(Input{
		SystemBase:     "BASE",
		ProjectSummary: "约束",
		MemoryCore:     "用户偏好深色主题",
		Tools: []ToolRef{
			{Name: "d1", Description: "按需工具", Tier: TierDeferred},
		},
	})
	sys := res.Request.System
	// 顺序：基础提示 → 项目约束 → 核心记忆 → deferred 索引（记忆在索引前 = 可缓存前缀更长）
	iBase := strings.Index(sys, "BASE")
	iMem := strings.Index(sys, "核心记忆")
	iIdx := strings.Index(sys, "可按需调用的工具")
	if iBase < 0 || iMem < 0 || iIdx < 0 {
		t.Fatalf("缺少必需段: %q", sys)
	}
	if !(iBase < iMem && iMem < iIdx) {
		t.Fatalf("记忆块必须位于基础提示之后、deferred 索引之前: %q", sys)
	}
	if !strings.Contains(sys, "用户偏好深色主题") {
		t.Fatalf("记忆正文缺失: %q", sys)
	}
}

func TestBuildMemoryTailAtEnd(t *testing.T) {
	b := NewBuilder(nil, DefaultLimits())
	res := b.Build(Input{
		MemoryTail:  "## 相关记忆\n- [L2] 结论",
		DynamicTail: "## 检索结果",
		UserInput:   "本轮问题",
	})
	msgs := res.Request.Messages
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("应恰有一条 user 消息: %+v", msgs)
	}
	c := msgs[0].Content
	iMem := strings.Index(c, "## 相关记忆")
	iDyn := strings.Index(c, "## 检索结果")
	iQ := strings.Index(c, "本轮问题")
	if !(iMem < iDyn && iDyn < iQ) {
		t.Fatalf("动态段必须按序置尾（记忆→运行时→输入）: %q", c)
	}
	// 记忆块绝不能进 system（否则前缀缓存全废）
	if strings.Contains(res.Request.System, "相关记忆") {
		t.Fatalf("动态记忆块泄漏进了 system")
	}
}

func TestBuildMemoryEmptyZeroRegression(t *testing.T) {
	// 两个字段都为空时，行为与 D-3 之前完全一致（无「核心记忆」段、无空尾拼接）
	b := NewBuilder(nil, DefaultLimits())
	res := b.Build(Input{SystemBase: "BASE", UserInput: "问题"})
	if strings.Contains(res.Request.System, "核心记忆") {
		t.Fatalf("无记忆时不得输出空段")
	}
	msgs := res.Request.Messages
	if len(msgs) != 1 || msgs[0].Content != "问题" {
		t.Fatalf("无尾段时 user 消息应为纯输入: %+v", msgs)
	}
}

func TestBuildMemoryTailOnlyNoInput(t *testing.T) {
	// 续轮（UserInput 为空）仍有记忆尾段：产出仅含记忆块的 user 提醒消息
	b := NewBuilder(nil, DefaultLimits())
	res := b.Build(Input{MemoryTail: "## 相关记忆\n- [L2] 结论", UserInput: ""})
	msgs := res.Request.Messages
	if len(msgs) != 1 {
		t.Fatalf("应产出 1 条消息: %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "相关记忆") {
		t.Fatalf("续轮应保留记忆尾段: %q", msgs[0].Content)
	}
}
