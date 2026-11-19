// Package toolcatalog 是工具发现插件：让模型能按名取到任意已注册工具的完整参数规范。
//
// # 为什么需要这个插件（结构性缺口）
//
// 提示词的工具分三档注入：
//
//	core      → 全量定义（name + description + schema）
//	deferred  → 只有「名称 + 一行描述」进 system 索引段
//	on_demand → 完全不注入（模型不可自主发现）
//
// 而 builder 渲染索引段时写的是「可按需调用的工具（需要时请显式加载）」——
// 但改造前**全仓没有任何"显式加载"的手段**（无 lookup_tool / call_extended_tool）。
// 结果：模型看得见 deferred 的名字，却拿不到 schema，只能盲猜参数调用；
// on_demand 更彻底——连名字都看不到，等于死档。
//
// lookup_tool 补上这一环后：
//   - deferred 真正"可自举"（看见名字 → 取 schema → 正确调用）；
//   - on_demand 变成"默认零成本、需要时按名拉取"的合理第三档，
//     不必为了让它可用而把每个工具都提级到 deferred（那要每轮付常驻 token）。
//
// 本插件自身必须声明为 **core** 档：它是自举的入口，若连它都不可见，整条链就断了。
package toolcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// lookupTool 返回指定工具的完整参数规范；不传 name 时列出全部工具名录。
func lookupTool(_ context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("lookup_tool: %v", err), nil
	}
	catalogHost, ok := host.(toolapi.ToolCatalogHost)
	if !ok {
		return pluginkit.ErrorResult("lookup_tool: 工具目录能力未接线"), nil
	}
	specs := catalogHost.ToolSpecs()
	if len(specs) == 0 {
		return pluginkit.ErrorResult("lookup_tool: 注册表为空，无法查询"), nil
	}

	name := strings.TrimSpace(args.Name)
	if name == "" {
		return listCatalog(specs), nil
	}
	for _, s := range specs {
		if s.Name != name {
			continue
		}
		return renderSpec(s), nil
	}
	return pluginkit.ErrorResult("lookup_tool: 未找到工具 %q。"+
		"省略 name 可查看全部可用工具名录（注意工具名必须完全一致）", name), nil
}

// listCatalog 渲染全部工具名录（按 tier 分组，便于模型判断哪些需要先 lookup）。
func listCatalog(specs []toolapi.Spec) *toolapi.Result {
	byTier := map[toolapi.Tier][]string{}
	for _, s := range specs {
		n := s.Normalized()
		byTier[n.Tier] = append(byTier[n.Tier], n.Name)
	}
	var b strings.Builder
	b.WriteString("可用工具名录（用 lookup_tool 传 name 可取到完整参数规范）：\n")
	for _, group := range []struct {
		tier toolapi.Tier
		desc string
	}{
		{toolapi.TierCore, "core（定义已全量注入，通常无需 lookup）"},
		{toolapi.TierDeferred, "deferred（仅有名称索引，调用前建议先 lookup）"},
		{toolapi.TierOnDemand, "on_demand（零注入，必须 lookup 后才能正确调用）"},
	} {
		names := byTier[group.tier]
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "\n## %s\n%s\n", group.desc, strings.Join(names, ", "))
	}
	return pluginkit.TextResult("%s", strings.TrimRight(b.String(), "\n"))
}

// renderSpec 渲染单个工具的完整规范（schema 原样输出 JSON）。
func renderSpec(s toolapi.Spec) *toolapi.Result {
	n := s.Normalized()
	schema := "{}"
	if len(n.SchemaJSON) > 0 {
		// 压缩成单行：pretty 打印会让输出膨胀，且模型的解析能力不依赖缩进。
		var buf bytes.Buffer
		if err := json.Compact(&buf, n.SchemaJSON); err == nil {
			schema = buf.String()
		} else {
			schema = string(n.SchemaJSON)
		}
	}
	return pluginkit.TextResult(
		"【工具 %s 完整参数规范】\n"+
			"功能描述：%s\n"+
			"注入档位：%s\n"+
			"风险级别：%s（readonly 无需审批；write/command 需按访问模式审批；danger 任何模式都逐次审批）\n"+
			"参数 Schema：%s",
		n.Name, strings.TrimSpace(n.Description), n.Tier, n.Risk, schema)
}
