// Package sop 是项目规范插件（阶段 G / M4，D10 口径）。
//
// SOP = 项目级 Markdown 流程规范，落盘 `<ws>/.harness/sop/<slug>.md`
// （文件是唯一真源）；内核装配点每轮全量读取注入系统提示
// （prompt.Input.SOP → 「## 项目规范（SOP）」段）。
//
// D10 决策：**不做渐进披露**——SOP 低频变更、数量少，全量注入 + 硬上限
// （≤6 条 × 每条 ≤150 token）胜过一套索引机制。超限**显式拒绝写入**，
// 绝不静默截断（prompt.Builder 侧另有二次校验）。
//
// 非必要流程沉淀到 skill 插件（按需加载，天然承接「可选流程」）。
package sop

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

// SOP 目录与硬上限（D10）。
const (
	// SOPDir SOP 文件目录（工作区相对）。
	SOPDir = ".harness/sop"
	// MaxEntries 条数硬上限。
	MaxEntries = 6
	// MaxEntryTokens 单条 token 硬上限（启发式估算）。
	MaxEntryTokens = 150
)

// Manifest 返回插件清单。
func Manifest() toolapi.Manifest {
	return pluginkit.ManifestFromJSON(manifestJSON)
}

// Factory 返回插件工厂。
func Factory() toolapi.Factory {
	return toolapi.FactoryFunc{M: Manifest(), NewF: func(host toolapi.Host, _ toolapi.Config) (toolapi.Plugin, error) {
		return &pluginkit.PluginFromSpecs{
			M:    Manifest(),
			Host: host,
			Invokes: map[string]pluginkit.InvokeFunc{
				"list_sops": invokeList,
				"save_sop":  invokeSave,
			},
		}, nil
	}}
}

// SOPEntry 单条 SOP（注入与列表共用形状）。
type SOPEntry struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Tokens int    `json:"tokens"`
}

// LoadAll 读取全部 SOP（内核装配点注入用；按文件名排序保证注入稳定）。
// 目录不存在 = 项目未配置 SOP（返回空，不是错误）。
func LoadAll(host toolapi.Host) []SOPEntry {
	entries, err := host.Workspace().List(context.Background(), SOPDir)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	out := make([]SOPEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir || !strings.HasSuffix(e.Path, ".md") {
			continue
		}
		data, rErr := host.Workspace().ReadFile(context.Background(), e.Path)
		if rErr != nil {
			continue
		}
		body := strings.TrimSpace(string(data))
		if body == "" {
			continue
		}
		out = append(out, SOPEntry{
			ID:     strings.TrimSuffix(filepath.Base(e.Path), ".md"),
			Title:  strings.TrimSuffix(filepath.Base(e.Path), ".md"),
			Body:   body,
			Tokens: EstimateTokens(body),
		})
	}
	return out
}

// invokeList 列出 SOP 条目。
func invokeList(_ context.Context, host toolapi.Host, _ *toolapi.Call) (*toolapi.Result, error) {
	entries := LoadAll(host)
	if len(entries) == 0 {
		return pluginkit.TextResult("项目暂无 SOP（可用 save_sop 写入，落 .harness/sop/，全量注入系统提示）"), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "项目 SOP %d 条（全量注入中）：\n", len(entries))
	for _, e := range entries {
		fmt.Fprintf(&b, "- %s（%d 行，≈%d token）\n", e.ID, strings.Count(e.Body, "\n")+1, e.Tokens)
	}
	return pluginkit.TextResult("%s", strings.TrimRight(b.String(), "\n")), nil
}

// invokeSave 保存 / 删除 SOP（硬上限在写入侧强制）。
func invokeSave(_ context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Title   string `json:"title"`
		Content string `json:"content"`
		Delete  bool   `json:"delete"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("save_sop: %v", err), nil
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		return pluginkit.ErrorResult("save_sop: title 不能为空"), nil
	}
	id := slugify(title)
	if id == "" {
		return pluginkit.ErrorResult("save_sop: title 无法生成有效条目 ID"), nil
	}
	ws := host.Workspace()

	if args.Delete {
		exists, _ := ws.Exists(context.Background(), SOPDir+"/"+id+".md")
		if !exists {
			return pluginkit.ErrorResult("save_sop: 条目 %s 不存在", id), nil
		}
		if err := ws.Remove(context.Background(), SOPDir+"/"+id+".md"); err != nil {
			return pluginkit.ErrorResult("save_sop: 删除 %s 失败: %v", id, err), nil
		}
		return pluginkit.DataResult(
			fmt.Sprintf("已删除 SOP：%s", id),
			map[string]any{"deleted": id},
		), nil
	}

	body := strings.TrimSpace(args.Content)
	if body == "" {
		return pluginkit.ErrorResult("save_sop: content 不能为空（删除条目请用 delete=true）"), nil
	}
	tokens := EstimateTokens(body)
	if tokens > MaxEntryTokens {
		return pluginkit.ErrorResult(
			"save_sop: 该条 ≈%d token，超过单条上限 %d（D10 硬上限，拒绝写入不截断）——请精简正文或沉淀到 skill",
			tokens, MaxEntryTokens), nil
	}
	existing := LoadAll(host)
	known := false
	for _, e := range existing {
		if e.ID == id {
			known = true
			break
		}
	}
	if !known && len(existing) >= MaxEntries {
		ids := make([]string, 0, len(existing))
		for _, e := range existing {
			ids = append(ids, e.ID)
		}
		return pluginkit.ErrorResult(
			"save_sop: 已有 %d 条 SOP（%s），达到条数上限 %d（D10 硬上限）——请先删除或合并低价值条目",
			len(existing), strings.Join(ids, ", "), MaxEntries), nil
	}
	if err := ws.WriteFile(context.Background(), SOPDir+"/"+id+".md", []byte(body+"\n"), 0o644); err != nil {
		return pluginkit.ErrorResult("save_sop: 写入 %s 失败: %v", id, err), nil
	}
	action := "已更新"
	if !known {
		action = "已新增"
	}
	return pluginkit.DataResult(
		fmt.Sprintf("%s SOP：%s（≈%d token，下一轮对话起全量注入系统提示）", action, id, tokens),
		map[string]any{"id": id, "tokens": tokens, "total": len(existing)},
	), nil
}

// EstimateTokens 启发式 token 估算：CJK/全角 ≈ 1 token/字符，其余 ≈ 1 token/4 字符。
// 只用于硬上限校验（与 tokenizer.Heuristic 同数量级，不追求精确）。
func EstimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) ||
			(r > 0x2E80 && r <= 0x9FFF) || (r >= 0xFF00 && r <= 0xFFEF) {
			cjk++
			continue
		}
		if !unicode.IsSpace(r) {
			other++
		}
	}
	return cjk + (other+3)/4
}

// slugify 标题 → 条目 ID（文件名安全；保留 CJK 与字母数字，其余折叠为 -）。
func slugify(title string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.TrimSpace(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			lastDash = false
		case r == ' ' || r == '-' || r == '_' || r == '/' || r == '\\':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() > 80 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}
