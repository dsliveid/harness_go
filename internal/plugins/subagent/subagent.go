package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// 子代理协作闭环：spawn（可阻塞/非阻塞）→ status / wait → reject 返工 / stop。
//
// # 为什么 spawn 需要非阻塞模式
//
// 改造前的 spawn_subagent **强制阻塞**直到子 Run 终态才返回。这意味着父代理
// 永远拿不到"在途句柄"：既然工具返回时就已结束，就无从 wait、无法 stop、
// 更谈不上并行管理多个子任务。此时若凭空加 wait_subagents / stop_subagent，
// 它们永远是"查不到东西"的死接口——工具存在但不可用，比没有更糟。
//
// 因此先给 spawn 增加 wait=false（立即返回 subagent_id），补齐工具族才有意义。
// 默认仍为阻塞（wait 缺省 true）：保持既有行为零变化，只有显式要并行时才切非阻塞。
//
// # 状态与任务记录为什么落文件
//
// 插件是**无状态**的（每次调用独立），而 reject_subagent 需要知道"原任务是什么"
// 才能重新派发。若不落盘，驳回就只能靠模型在参数里重述任务，容易失真。
// 记录落 `.harness/subagents.json`，与 todo/plan 同口径（工作区文件即真源）。

// record 一条子代理派发记录。
type record struct {
	ID        string `json:"id"`
	Task      string `json:"task"`
	Role      string `json:"role,omitempty"`
	ModelRef  string `json:"model_ref,omitempty"`
	Status    string `json:"status"` // running | completed | failed | cancelled
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	Attempt   int    `json:"attempt"`
	CreatedAt string `json:"created_at"`
}

// spawnSubagent 派生子代理。wait 缺省 true（阻塞），false 时立即返回 ID。
func spawnSubagent(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Task     string `json:"task"`
		ModelRef string `json:"model_ref"`
		Role     string `json:"role"`
		Wait     *bool  `json:"wait"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("spawn_subagent: %v", err), nil
	}
	if strings.TrimSpace(args.Task) == "" {
		return pluginkit.ErrorResult("spawn_subagent: task 不能为空"), nil
	}
	// 缺省阻塞：显式传 false 才非阻塞（保持既有行为零变化）。
	blocking := args.Wait == nil || *args.Wait
	role := strings.TrimSpace(args.Role)
	if role == "" {
		role = "subagent"
	}
	return dispatch(ctx, host, call, args.Task, role, args.ModelRef, blocking)
}

// dispatch 真正派发一个子 Run（spawn 与 reject 返工共用同一条路径）。
func dispatch(ctx context.Context, host toolapi.Host, call *toolapi.Call, task, role, modelRef string, blocking bool) (*toolapi.Result, error) {
	if host.Spawn() == nil {
		return pluginkit.ErrorResult("spawn_subagent: 嵌套 Run 能力未接线"), nil
	}
	child, err := host.Spawn().Spawn(ctx, toolapi.SpawnRequest{
		ParentRunID: call.RunID,
		SessionID:   call.SessionID,
		AgentRole:   role,
		Input:       task,
		ModelRef:    modelRef,
	})
	if err != nil {
		return pluginkit.ErrorResult("spawn_subagent: %v", err), nil
	}
	rec := record{
		ID:        child.ID(),
		Task:      task,
		Role:      role,
		ModelRef:  modelRef,
		Status:    "running",
		Attempt:   1,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if prev, _ := loadRecord(ctx, host, child.ID()); prev != nil {
		rec.Attempt = prev.Attempt + 1
	}
	_ = saveRecord(ctx, host, rec)

	if !blocking {
		return pluginkit.TextResult("已派生子代理（id=%s，状态 running）。"+
			"可用 get_subagent_status 查询、wait_subagents 等待汇聚、stop_subagent 停止。", child.ID()), nil
	}

	res, werr := child.Wait(ctx)
	if werr != nil {
		rec.Status, rec.Error = "failed", werr.Error()
		_ = saveRecord(ctx, host, rec)
		return pluginkit.ErrorResult("spawn_subagent: 等待子 Run 失败: %v", werr), nil
	}
	rec.Status, rec.Output, rec.Error = string(res.Status), res.Output, res.Error
	_ = saveRecord(ctx, host, rec)
	if res.Status != toolapi.ChildCompleted {
		detail := res.Error
		if detail == "" {
			detail = string(res.Status)
		}
		return &toolapi.Result{Content: "子代理未成功完成（" + detail + "）", IsError: true,
			// 两个键并存：child_run_id 是既有消费方（前端/测试）已在用的名字，
			// subagent_id 是新工具族（status/wait/stop/reject）的统一叫法。
			// 保留旧键是向后兼容，不是双真源——它们是同一个值。
			Meta: map[string]any{
				"subagent_id":  res.RunID,
				"child_run_id": res.RunID,
				"child_status": string(res.Status),
			}}, nil
	}
	return pluginkit.TextResult("子代理完成（id=%s）：\n%s", res.RunID, res.Output), nil
}

// getSubagentStatus 查询子代理状态。
func getSubagentStatus(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		SubagentID string `json:"subagent_id"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("get_subagent_status: %v", err), nil
	}
	id := strings.TrimSpace(args.SubagentID)
	if id == "" {
		return pluginkit.ErrorResult("get_subagent_status: subagent_id 不能为空"), nil
	}
	rec, err := loadRecord(ctx, host, id)
	if err != nil {
		return pluginkit.ErrorResult("get_subagent_status: %v", err), nil
	}
	if rec == nil {
		return pluginkit.TextResult("未找到子代理 %s（可能未由本工作区派生）", id), nil
	}
	// 状态以**运行时**为准：文件里可能是派发时的 running 快照。
	if host.Spawn() != nil {
		if child, ok := host.Spawn().Get(id); ok {
			rec.Status = string(child.Status())
		}
	}
	out := fmt.Sprintf("子代理 %s：%s（第 %d 次尝试）", rec.ID, rec.Status, rec.Attempt)
	if rec.Output != "" {
		out += "\n结果：" + pluginkit.FirstLine(rec.Output, 300)
	}
	if rec.Error != "" {
		out += "\n错误：" + pluginkit.FirstLine(rec.Error, 300)
	}
	return pluginkit.TextResult("%s", out), nil
}

// waitSubagents 等待子代理结束并汇总。
func waitSubagents(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		SubagentIDs []string `json:"subagent_ids"`
		TimeoutSecs int      `json:"timeout_secs"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("wait_subagents: %v", err), nil
	}
	if host.Spawn() == nil {
		return pluginkit.ErrorResult("wait_subagents: 嵌套 Run 能力未接线"), nil
	}
	timeout := args.TimeoutSecs
	if timeout <= 0 {
		timeout = 600
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	ids := args.SubagentIDs
	if len(ids) == 0 {
		// 省略 ID 时取全部在途子代理（running 记录）。
		recs, err := loadAll(ctx, host)
		if err != nil {
			return pluginkit.ErrorResult("wait_subagents: %v", err), nil
		}
		for _, r := range recs {
			if r.Status == "running" {
				ids = append(ids, r.ID)
			}
		}
	}
	if len(ids) == 0 {
		return pluginkit.TextResult("当前没有在途子代理需要等待"), nil
	}

	var b strings.Builder
	stillRunning := 0
	for _, id := range ids {
		child, ok := host.Spawn().Get(id)
		if !ok {
			fmt.Fprintf(&b, "- %s：未找到（可能已归档）\n", id)
			continue
		}
		res, err := child.Wait(waitCtx)
		if err != nil {
			stillRunning++
			fmt.Fprintf(&b, "- %s：仍在运行或等待超时（%v）\n", id, err)
			continue
		}
		rec, _ := loadRecord(ctx, host, id)
		if rec == nil {
			rec = &record{ID: id, Attempt: 1}
		}
		rec.Status, rec.Output, rec.Error = string(res.Status), res.Output, res.Error
		_ = saveRecord(ctx, host, *rec)
		fmt.Fprintf(&b, "- %s：%s", id, rec.Status)
		if res.Output != "" {
			fmt.Fprintf(&b, " — %s", pluginkit.FirstLine(res.Output, 200))
		}
		b.WriteString("\n")
	}
	if stillRunning > 0 {
		return pluginkit.TextResult("%s\n⚠️ 仍有 %d 个子代理未结束：严禁提前输出最终业务结论，请再次调用 wait_subagents 直到全部完成。",
			strings.TrimRight(b.String(), "\n"), stillRunning), nil
	}
	return pluginkit.TextResult("子代理汇总：\n%s", strings.TrimRight(b.String(), "\n")), nil
}

// rejectSubagent 驳回并要求返工：带整改要求重新派发同一子任务。
func rejectSubagent(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		SubagentID string `json:"subagent_id"`
		Feedback   string `json:"feedback"`
		Task       string `json:"task"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("reject_subagent: %v", err), nil
	}
	id := strings.TrimSpace(args.SubagentID)
	if id == "" || strings.TrimSpace(args.Feedback) == "" {
		return pluginkit.ErrorResult("reject_subagent: subagent_id 与 feedback 都不能为空"), nil
	}
	prev, _ := loadRecord(ctx, host, id)
	task := strings.TrimSpace(args.Task)
	if task == "" && prev != nil {
		task = prev.Task
	}
	if task == "" {
		return pluginkit.ErrorResult("reject_subagent: 找不到原任务描述，请在 task 参数中给出要返工的任务"), nil
	}
	role, modelRef := "subagent", ""
	attempt := 2
	if prev != nil {
		if prev.Role != "" {
			role = prev.Role
		}
		modelRef = prev.ModelRef
		attempt = prev.Attempt + 1
	}
	// 把驳回意见写进任务，子代理才知道要改什么。
	retryTask := fmt.Sprintf("%s\n\n【验收未通过，需返工】\n%s", task, strings.TrimSpace(args.Feedback))

	// 若驳回目标仍在运行，先停止它，避免两个同任务子代理并行改同一批文件。
	if host.Spawn() != nil {
		if child, ok := host.Spawn().Get(id); ok && child.Status() == toolapi.ChildRunning {
			_ = child.Cancel()
		}
	}
	// 记录原记录为已驳回（保留可追溯性）
	if prev != nil {
		prev.Status = "rejected"
		_ = saveRecord(ctx, host, *prev)
	}

	res, err := dispatch(ctx, host, call, retryTask, role, modelRef, true)
	if err != nil {
		return res, err
	}
	// dispatch 已写入新记录（Attempt = prev.Attempt+1），此处仅补充说明。
	return pluginkit.TextResult("已驳回子代理 %s 并重新派发（第 %d 次尝试）。\n%s", id, attempt, res.Content), nil
}

// stopSubagent 停止在途子代理。
func stopSubagent(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		SubagentID string `json:"subagent_id"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("stop_subagent: %v", err), nil
	}
	id := strings.TrimSpace(args.SubagentID)
	if id == "" {
		return pluginkit.ErrorResult("stop_subagent: subagent_id 不能为空"), nil
	}
	if host.Spawn() == nil {
		return pluginkit.ErrorResult("stop_subagent: 嵌套 Run 能力未接线"), nil
	}
	child, ok := host.Spawn().Get(id)
	if !ok {
		return pluginkit.ErrorResult("stop_subagent: 未找到子代理 %s（可能已结束并归档）", id), nil
	}
	if err := child.Cancel(); err != nil {
		return pluginkit.ErrorResult("stop_subagent: 停止失败: %v", err), nil
	}
	if rec, _ := loadRecord(ctx, host, id); rec != nil {
		rec.Status = "cancelled"
		_ = saveRecord(ctx, host, *rec)
	}
	return pluginkit.TextResult("已停止子代理 %s", id), nil
}

// ============ 记录持久化（.harness/subagents.json） ============

// loadAll 读取全部派发记录。
func loadAll(ctx context.Context, host toolapi.Host) ([]record, error) {
	data, err := host.Workspace().ReadFile(ctx, StorePath)
	if err != nil {
		return nil, nil // 无记录是正常态
	}
	var recs []record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("子代理记录文件损坏: %w", err)
	}
	return recs, nil
}

// loadRecord 按 ID 读取单条记录（不存在返回 nil, nil）。
func loadRecord(ctx context.Context, host toolapi.Host, id string) (*record, error) {
	recs, err := loadAll(ctx, host)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		if recs[i].ID == id {
			return &recs[i], nil
		}
	}
	return nil, nil
}

// saveRecord 新增或更新一条记录（按 ID 覆盖）。
func saveRecord(ctx context.Context, host toolapi.Host, rec record) error {
	recs, err := loadAll(ctx, host)
	if err != nil {
		return err
	}
	replaced := false
	for i := range recs {
		if recs[i].ID == rec.ID {
			recs[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		recs = append(recs, rec)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].CreatedAt < recs[j].CreatedAt })
	raw, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	return host.Workspace().WriteFile(ctx, StorePath, raw, 0o644)
}
