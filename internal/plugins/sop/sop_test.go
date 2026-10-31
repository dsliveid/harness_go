package sop

// SOP 插件单测：硬上限（D10）与文件真源读写。

import (
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func invoke(t *testing.T, h *plugintest.Host, name, args string) *toolapi.Result {
	t.Helper()
	p, err := Factory().New(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range p.Tools() {
		if tool.Spec().Name != name {
			continue
		}
		res, iErr := tool.Invoke(t.Context(), &toolapi.Call{CallID: name + ":1", Name: name, Args: []byte(args)})
		if iErr != nil {
			t.Fatalf("%s invoke: %v", name, iErr)
		}
		return res
	}
	t.Fatalf("插件未声明工具 %s", name)
	return nil
}

// TestSaveListAndLoad 保存 → 文件真源落盘 → LoadAll 可读（注入源）。
func TestSaveListAndLoad(t *testing.T) {
	h := plugintest.New(t)
	res := invoke(t, h, "save_sop", `{"title":"发布流程","content":"1. 跑测试\n2. 打标签"}`)
	if res.IsError {
		t.Fatalf("save 失败: %s", res.Content)
	}
	entries := LoadAll(h)
	if len(entries) != 1 || entries[0].ID != "发布流程" {
		t.Fatalf("LoadAll 应读到 1 条: %+v", entries)
	}
	// 文件在 .harness/sop/ 下
	exists, _ := h.Exists(t.Context(), ".harness/sop/发布流程.md")
	if !exists {
		t.Fatal("SOP 文件未落盘")
	}
	list := invoke(t, h, "list_sops", `{}`)
	if list.IsError || !strings.Contains(list.Content, "发布流程") {
		t.Fatalf("list 异常: %s", list.Content)
	}
}

// TestSaveRejectsOverToken 单条超 150 token（启发式）显式拒绝——不截断。
func TestSaveRejectsOverToken(t *testing.T) {
	h := plugintest.New(t)
	big := strings.Repeat("长内容段落。", 120) // ≈ 600 CJK token
	res := invoke(t, h, "save_sop", `{"title":"超限","content":"`+big+`"}`)
	if !res.IsError {
		t.Fatal("超 token 上限应拒绝")
	}
	if !strings.Contains(res.Content, "150") {
		t.Fatalf("错误应说明上限: %s", res.Content)
	}
}

// TestSaveRejectsOverCount 超过 6 条显式拒绝。
func TestSaveRejectsOverCount(t *testing.T) {
	h := plugintest.New(t)
	for i := 1; i <= MaxEntries; i++ {
		res := invoke(t, h, "save_sop", `{"title":"条目`+string(rune('0'+i))+`","content":"内容 `+string(rune('0'+i))+`"}`)
		if res.IsError {
			t.Fatalf("第 %d 条不应失败: %s", i, res.Content)
		}
	}
	res := invoke(t, h, "save_sop", `{"title":"第七条","content":"超出"}`)
	if !res.IsError {
		t.Fatal("第 7 条应拒绝")
	}
	// 更新已有条目不受条数上限影响
	res = invoke(t, h, "save_sop", `{"title":"条目1","content":"更新后的内容"}`)
	if res.IsError {
		t.Fatalf("更新已有条目应放行: %s", res.Content)
	}
}

// TestDeleteAndSlug 删除 + 标题 slug 化。
func TestDeleteAndSlug(t *testing.T) {
	h := plugintest.New(t)
	if res := invoke(t, h, "save_sop", `{"title":"Code Review Rules","content":"先看再改"}`); res.IsError {
		t.Fatal(res.Content)
	}
	entries := LoadAll(h)
	if len(entries) != 1 || entries[0].ID != "code-review-rules" {
		t.Fatalf("slug 应小写折叠: %+v", entries)
	}
	if res := invoke(t, h, "save_sop", `{"title":"Code Review Rules","delete":true}`); res.IsError {
		t.Fatalf("删除失败: %s", res.Content)
	}
	if len(LoadAll(h)) != 0 {
		t.Fatal("删除后应为空")
	}
	if res := invoke(t, h, "save_sop", `{"title":"不存在","delete":true}`); !res.IsError {
		t.Fatal("删除不存在条目应报错")
	}
}

// TestEstimateTokens 启发式计数方向正确（CJK 密、ASCII 疏）。
func TestEstimateTokens(t *testing.T) {
	cjk := EstimateTokens(strings.Repeat("中", 100))
	if cjk < 90 || cjk > 110 {
		t.Fatalf("CJK ≈1/字符: %d", cjk)
	}
	ascii := EstimateTokens(strings.Repeat("abcd", 25))
	if ascii < 20 || ascii > 35 {
		t.Fatalf("ASCII ≈1/4字符: %d", ascii)
	}
}
