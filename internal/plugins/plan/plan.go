package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// Step 计划步骤。
type Step struct {
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

// Plan 计划文档。
type Plan struct {
	Title string `json:"title"`
	Steps []Step `json:"steps"`
}

// createPlan 创建（或覆盖）计划。
func createPlan(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args Plan
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("create_plan: %v", err), nil
	}
	if strings.TrimSpace(args.Title) == "" {
		return pluginkit.ErrorResult("create_plan: title 不能为空"), nil
	}
	if len(args.Steps) == 0 {
		return pluginkit.ErrorResult("create_plan: steps 不能为空（计划至少要有一个步骤）"), nil
	}
	for i, s := range args.Steps {
		if strings.TrimSpace(s.Title) == "" {
			return pluginkit.ErrorResult("create_plan: 第 %d 步 title 为空", i+1), nil
		}
		args.Steps[i].Title = strings.TrimSpace(s.Title)
	}
	return save(ctx, host, args, "create_plan")
}

// updatePlan 更新计划：改标题 / 替换步骤 / 标记某步完成或未完成。
//
// # 为什么单独提供"按序号标记"（而不是要求模型回传整份清单）
//
// 模型回传整份 steps 时很容易漏项或改动措辞，导致"推进进度"意外丢步骤。
// 按序号标记只需一个整数，语义最小、出错面最窄，这才是"更新"最常用的形态。
func updatePlan(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Title          string `json:"title"`
		Steps          []Step `json:"steps"`
		CompleteStep   int    `json:"complete_step"`
		UncompleteStep int    `json:"uncomplete_step"`
		// StepIndex 兼容别名（2026-11）：模型稳定倾向于写 `step_index` + `status`，
		// 而实现只认 complete_step/uncomplete_step → 直接报「未知参数 "status"」，
		// 白白浪费一步再猜一次。这里做输入归一，规范参数名保持唯一真源。
		StepIndex int    `json:"step_index"`
		Status    string `json:"status"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("update_plan: %v", err), nil
	}
	// 别名归一：step_index + status 是模型最自然的表达，落到规范字段上。
	if args.StepIndex > 0 && args.CompleteStep == 0 && args.UncompleteStep == 0 {
		switch strings.ToLower(strings.TrimSpace(args.Status)) {
		case "", "completed", "complete", "done", "finished", "ok":
			args.CompleteStep = args.StepIndex
		case "pending", "in_progress", "doing", "todo", "not_started", "uncompleted":
			args.UncompleteStep = args.StepIndex
		}
	}
	p, err := load(ctx, host)
	if err != nil {
		return pluginkit.ErrorResult("update_plan: %v", err), nil
	}
	// 无计划时更新 = 语义错误：必须先 create_plan（避免"更新"静默造出空壳计划）。
	if p == nil {
		return pluginkit.ErrorResult("update_plan: 当前没有执行计划，请先调用 create_plan"), nil
	}
	if t := strings.TrimSpace(args.Title); t != "" {
		p.Title = t
	}
	if args.Steps != nil {
		if len(args.Steps) == 0 {
			return pluginkit.ErrorResult("update_plan: steps 不能为空（如需清空请重建计划）"), nil
		}
		for i, s := range args.Steps {
			if strings.TrimSpace(s.Title) == "" {
				return pluginkit.ErrorResult("update_plan: 第 %d 步 title 为空", i+1), nil
			}
			args.Steps[i].Title = strings.TrimSpace(s.Title)
		}
		p.Steps = args.Steps
	}
	if args.CompleteStep > 0 {
		if err := markStep(p, args.CompleteStep, true); err != nil {
			return pluginkit.ErrorResult("update_plan: %v", err), nil
		}
	}
	if args.UncompleteStep > 0 {
		if err := markStep(p, args.UncompleteStep, false); err != nil {
			return pluginkit.ErrorResult("update_plan: %v", err), nil
		}
	}
	return save(ctx, host, *p, "update_plan")
}

// readPlan 读取当前计划。
func readPlan(ctx context.Context, host toolapi.Host, _ *toolapi.Call) (*toolapi.Result, error) {
	p, err := load(ctx, host)
	if err != nil {
		return pluginkit.ErrorResult("read_plan: %v", err), nil
	}
	if p == nil {
		return pluginkit.TextResult("当前无执行计划"), nil
	}
	return pluginkit.TextResult("%s", render(*p)), nil
}

// listPlans 列出已保存的计划。
//
// 当前存储为**单计划**（.harness/plan.json）。这里如实反映该事实，
// 不虚构多计划能力：声称支持多计划却只返回一条，会让模型据此做出
// 「换一个计划看看」这类永远失败的尝试。
func listPlans(ctx context.Context, host toolapi.Host, _ *toolapi.Call) (*toolapi.Result, error) {
	p, err := load(ctx, host)
	if err != nil {
		return pluginkit.ErrorResult("list_plans: %v", err), nil
	}
	if p == nil {
		return pluginkit.TextResult("当前无执行计划（单计划存储，尚无任何计划）"), nil
	}
	return pluginkit.TextResult("当前执行计划（单计划存储）：\n- %s（%d/%d 完成）",
		p.Title, countDone(p.Steps), len(p.Steps)), nil
}

// markStep 按 1 起序号标记步骤完成状态。
func markStep(p *Plan, no int, done bool) error {
	if no > len(p.Steps) {
		return fmt.Errorf("步骤序号 %d 越界（当前共 %d 步）", no, len(p.Steps))
	}
	p.Steps[no-1].Done = done
	return nil
}

// save 落盘并回显渲染结果。
func save(ctx context.Context, host toolapi.Host, p Plan, tool string) (*toolapi.Result, error) {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return pluginkit.ErrorResult("%s: 序列化失败: %v", tool, err), nil
	}
	if err := host.Workspace().WriteFile(ctx, StorePath, raw, 0o644); err != nil {
		return pluginkit.ErrorResult("%s: 写入失败: %v", tool, err), nil
	}
	return pluginkit.TextResult("%s", render(p)), nil
}

// load 读取计划；文件不存在返回 (nil, nil) 表示"尚无计划"。
func load(ctx context.Context, host toolapi.Host) (*Plan, error) {
	data, err := host.Workspace().ReadFile(ctx, StorePath)
	if err != nil {
		return nil, nil
	}
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("计划文件损坏: %w", err)
	}
	return &p, nil
}

func render(p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "计划：%s（%d/%d 完成）\n", p.Title, countDone(p.Steps), len(p.Steps))
	for i, s := range p.Steps {
		mark := "○"
		if s.Done {
			mark = "●"
		}
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, mark, s.Title)
	}
	return strings.TrimRight(b.String(), "\n")
}

func countDone(steps []Step) int {
	n := 0
	for _, s := range steps {
		if s.Done {
			n++
		}
	}
	return n
}
