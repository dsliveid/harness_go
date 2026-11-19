package sop

import (
	"reflect"
	"testing"
)

// TestExtractToolRefsFiltersNonToolForms 验证提取器只挑「形如工具名」的标识符。
//
// 这条测试守的是**零误报**：门禁一旦产生假阳性，使用者就会加白名单绕过，
// 护栏随即失效（比漏检更坏的结局）。因此下面这些真实存在于提示词里的
// 混杂形态必须被正确排除。
func TestExtractToolRefsFiltersNonToolForms(t *testing.T) {
	text := strings_Join(
		"- 调用 `create_plan` 与 `update_plan` 生成方案；",
		"- 记忆落 `.harness/memory/`（含点号，不是工具名）；",
		"- 禁 `Stop-Process -Name <name>`、`taskkill /IM`（含空格大写，不是工具名）；",
		"- JSON 标签 `json:\"name\"`（含冒号引号，不是工具名）；",
		"- `recognize_image(path, prompt)`（带括号参数，不是工具名）；",
		"- 被禁命令 `killall`、`pkill`（已建模为操作系统命令，应排除）；",
	)
	got := ExtractToolRefs(text)
	want := []string{"create_plan", "update_plan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("提取结果不符:\n got=%v\nwant=%v", got, want)
	}
}

// TestExtractToolRefsDedupAndSort 验证去重与稳定排序（失败信息可比对）。
func TestExtractToolRefsDedupAndSort(t *testing.T) {
	got := ExtractToolRefs("`todo` `write_file` `todo` `edit_file`")
	want := []string{"edit_file", "todo", "write_file"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("去重排序不符: got=%v want=%v", got, want)
	}
}

// TestExternalCommandSetStaysTiny 卡住操作系统命令集合的上限。
//
// 该集合是门禁的"语义分类"，不是豁免名单。一旦有人图省事往里塞本产品
// 工具名来"修红"，门禁就会被悄悄削弱 —— 这条测试让那种做法无法通过。
func TestExternalCommandSetStaysTiny(t *testing.T) {
	const maxExternalCommands = 4
	if n := ExternalCommandCount(); n > maxExternalCommands {
		t.Fatalf("externalCommands 已达 %d 条（上限 %d）：请确认新条目确实是操作系统命令，"+
			"而不是为了让门禁变绿而塞进来的工具名", n, maxExternalCommands)
	}
}

// strings_Join 避免为一条测试引入 strings 包（保持本文件依赖极小）。
func strings_Join(parts ...string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}
