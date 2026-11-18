// Package sop —— 规则渲染层（目录定义 → 提示词文本）。
//
// 职责分离：
//
//	catalog.go  只声明「有哪些规则、每条规则的形态」（数据）
//	render.go   只负责「作用域过滤 → 开关过滤 → 变体命中 → 编号渲染」（算法）
//
// 渲染口径与老版本 harness_mini `sop_catalog::render_variant` 逐字对齐：
//
//	N. 【标题】：正文
//
// 编号由调用方传入起点（rule_num），因为主进程提示词在 SOP 段之前还有
// 「工作区 / 扩展工具目录」等固定条目，必须连续编号而不是从 1 重新起算。
package sop

import (
	"fmt"
	"strings"
)

// RenderRules 渲染本次上下文实际生效的规则行（每行一条，含序号）。
//
// 三阶段过滤，顺序不可调换：
//
//	① 作用域硬过滤 —— main_only 绝不出现在子会话，sub_only 绝不出现在主会话；
//	   这是**安全边界**，与用户开关无关，永远先行。
//	② 开关过滤 —— 非硬规则且出现在 Disabled 中则整条跳过（硬规则恒生效）。
//	③ 变体命中 —— 取第一个 Active(ctx) 为真的变体；无命中则整条跳过
//	   （这是「同一条规则在不同模式下措辞不同」的唯一实现方式）。
//
// 返回 (规则行, 下一条可用编号)。startNum <= 0 时按 1 起算。
func RenderRules(ctx Context, startNum int) ([]string, int) {
	if startNum <= 0 {
		startNum = 1
	}
	ruleNum := startNum
	out := make([]string, 0, len(Definitions()))
	for _, def := range Definitions() {
		// ① 作用域硬过滤
		if ctx.IsSub && def.Item.Scope == ScopeMainOnly {
			continue
		}
		if !ctx.IsSub && def.Item.Scope == ScopeSubOnly {
			continue
		}
		// ② 用户开关（硬规则恒生效）
		if ctx.isDisabled(def) {
			continue
		}
		// ③ 取首个命中变体
		hit := false
		for _, v := range def.Variants {
			if v.Active == nil || !v.Active(ctx) {
				continue
			}
			out = append(out, renderVariant(ruleNum, v, ctx))
			ruleNum++
			hit = true
			break
		}
		if !hit {
			continue
		}
	}
	return out, ruleNum
}

// renderVariant 渲染单个变体：「N. 【标题】：正文」，并替换正文占位符。
func renderVariant(ruleNum int, v Variant, ctx Context) string {
	body := replacePlaceholders(v.Body, ctx)
	return fmt.Sprintf("%d. 【%s】：%s", ruleNum, v.Title, body)
}

// replacePlaceholders 替换正文占位符。
//
//	{report_file} → <sessionID>_report.md
//
// SessionID 为空时用 "subtask" 兜底，避免直接把 `{report_file}` 原文喂给模型
// （老版本踩过：占位符残留会让子会话把字面量当成真实文件名去写）。
func replacePlaceholders(body string, ctx Context) string {
	if !strings.Contains(body, "{report_file}") {
		return body
	}
	id := strings.TrimSpace(ctx.SessionID)
	if id == "" {
		id = "subtask"
	}
	return strings.ReplaceAll(body, "{report_file}", id+"_report.md")
}

// ActiveIDs 返回本次上下文中真正生效的规则 ID（含变体命中判定，不含作用域外规则）。
//
// 用途：/sop/effective 预览与排查「为什么某条规则没出现」——
// 界面可直接对比「目录全集」与「生效子集」，避免用户误以为开关坏了。
func ActiveIDs(ctx Context) []string {
	out := make([]string, 0, len(Definitions()))
	for _, def := range Definitions() {
		if ctx.IsSub && def.Item.Scope == ScopeMainOnly {
			continue
		}
		if !ctx.IsSub && def.Item.Scope == ScopeSubOnly {
			continue
		}
		if ctx.isDisabled(def) {
			continue
		}
		for _, v := range def.Variants {
			if v.Active != nil && v.Active(ctx) {
				out = append(out, def.Item.ID)
				break
			}
		}
	}
	return out
}
