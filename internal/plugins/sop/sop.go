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

	"harness-go/internal/pluginkit"
	"harness-go/internal/sop"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

// SOP 目录与硬上限（真源统一在 internal/sop，此处保留别名以兼容既有引用）。
const (
	// SOPDir SOP 文件目录（工作区相对）。
	SOPDir = sop.EntriesDir
	// MaxEntries 条数硬上限。
	MaxEntries = sop.MaxEntries
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
//
// 保留本别名是为了不破坏既有调用方；真源类型见 internal/sop.Entry。
type SOPEntry = sop.Entry

// LoadAll 读取全部 SOP（经 host.Workspace() 沙箱读取，供 save_sop 校验用）。
//
// 架构说明：**注入路径已不再走本函数** —— 内核装配点改为直接调用
// sop.LoadEntries(wsPath)，从而摆脱 host root 启动期固化导致的跨项目串味。
// 本函数仅保留给插件工具自身在沙箱内做「条数上限」校验使用。
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
		id := strings.TrimSuffix(filepath.Base(e.Path), ".md")
		out = append(out, SOPEntry{ID: id, Title: id, Body: body, Tokens: sop.EstimateTokens(body)})
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
	tokens := sop.EstimateTokens(body)
	existing := LoadAll(host)
	known := false
	ids := make([]string, 0, len(existing))
	for _, e := range existing {
		if e.ID == id {
			known = true
		}
		ids = append(ids, e.ID)
	}
	// 条数上限与「拒绝不截断」口径统一由 internal/sop 提供（前端 REST 同源）。
	if err := sop.ValidateEntry(id, body, ids, known); err != nil {
		return pluginkit.ErrorResult("save_sop: %v", err), nil
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

// EstimateTokens 启发式 token 估算（真源在 internal/sop，此处保留别名以兼容既有引用）。
func EstimateTokens(s string) int { return sop.EstimateTokens(s) }

// slugify 标题 → 条目 ID（真源在 internal/sop，此处保留别名以兼容既有引用）。
func slugify(title string) string { return sop.Slugify(title) }
