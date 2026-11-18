package service

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/sop"
)

// 自动标题口径（对齐桌面端 commands.rs：首条消息前 24 字，仅默认标题时生效）。
func TestDeriveSessionTitle(t *testing.T) {
	cases := []struct {
		name    string
		current string
		input   string
		want    string
	}{
		{"默认标题+普通输入", "新对话", "帮我看看这段代码为什么报错", "帮我看看这段代码为什么报错"},
		{"空标题+普通输入", "", "短输入", "短输入"},
		{"超长输入截断 24 字", "新对话", "这是一段非常非常非常非常非常非常非常非常长的输入内容已经超过二十四个字符了", "这是一段非常非常非常非常非常非常非常非常长的输入"},
		{"自定义标题不覆盖", "我的重要讨论", "新消息", ""},
		{"空白输入不生成", "新对话", "   \n\t ", ""},
		{"首尾空白被修剪", "新对话", "  带空白的输入  ", "带空白的输入"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveSessionTitle(c.current, c.input)
			if got != c.want {
				t.Fatalf("deriveSessionTitle(%q, %q) = %q, want %q", c.current, c.input, got, c.want)
			}
		})
	}
}

// ============ 原生 SOP 规则段装配（RunService 侧）============

// TestRunServiceAppendRules 主 Run 与子 Run 必须走同一条装配口径，仅作用域不同。
//
// 这是「补全的 SOP 到底有没有进模型视野」的服务层契约：
// 只要装配点漏调一次，模型就完全看不到这些规则，界面上的开关也就变成了摆设。
func TestRunServiceAppendRules(t *testing.T) {
	svc := NewSOPService(t.TempDir(), 800)
	svc.SetDisabledProvider(func(context.Context) []string { return nil })
	svc.SetPlanModeProvider(func(context.Context) string { return sop.PlanModeStandard })

	rs := NewRunService(RunServiceOptions{
		RulesFunc: svc.BindRules(),
	})
	ctx := context.Background()
	base := "人格\n\n## 工作区\n本会话已绑定工作区：F:\\proj"

	mainOut := rs.appendRules(ctx, base, "sess-1", false)
	subOut := rs.appendRules(ctx, base, "sess-1", true)

	// ① 两处都必须真的追加了规则段（漏装配即失败）
	for name, out := range map[string]string{"主会话": mainOut, "子会话": subOut} {
		if !strings.Contains(out, "## 工作规则") {
			t.Fatalf("%s 系统提示缺少规则段（装配点漏调）", name)
		}
		if !strings.HasPrefix(out, "人格") {
			t.Fatalf("%s 人格段必须保持在开头", name)
		}
		if strings.Index(out, "## 工作区") > strings.Index(out, "## 工作规则") {
			t.Fatalf("%s 规则段必须在工作区段之后", name)
		}
	}

	// ② 作用域隔离：main_only 只在主会话、sub_only 只在子会话
	for _, def := range sop.Definitions() {
		title := "【" + def.Variants[0].Title + "】"
		switch def.Item.Scope {
		case sop.ScopeMainOnly:
			if !strings.Contains(mainOut, title) {
				t.Fatalf("main_only 规则 %s 未进主会话系统提示", def.Item.ID)
			}
			if strings.Contains(subOut, title) {
				t.Fatalf("main_only 规则 %s 泄漏进子会话系统提示", def.Item.ID)
			}
		case sop.ScopeSubOnly:
			if !strings.Contains(subOut, title) {
				t.Fatalf("sub_only 规则 %s 未进子会话系统提示", def.Item.ID)
			}
			if strings.Contains(mainOut, title) {
				t.Fatalf("sub_only 规则 %s 泄漏进主会话系统提示", def.Item.ID)
			}
		}
	}
}

// TestRunServiceAppendRulesDisabled 未装配规则源时必须原样返回（降级不注入空段落）。
func TestRunServiceAppendRulesDisabled(t *testing.T) {
	base := "人格"
	if got := NewRunService(RunServiceOptions{}).appendRules(context.Background(), base, "s", false); got != base {
		t.Fatalf("未装配规则源时应原样返回，实际: %q", got)
	}
	var nilSvc *RunService
	if got := nilSvc.appendRules(context.Background(), base, "s", false); got != base {
		t.Fatalf("nil 服务应原样返回，实际: %q", got)
	}
}
