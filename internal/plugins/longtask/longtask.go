// Package longtask 是长任务插件（阶段 G / M4）。
//
// 只做**工具入口**（docs/插件体系设计.md §3.4）：状态机、检查点与回滚的
// 执行体在内核（service.TaskManager + snapshot.Store），经 Host.Tasks() /
// Host.Snapshots() 受控访问。任务状态落会话库（跨轮次可查），
// 检查点 = Run + 快照引用的原子登记。
package longtask

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

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
				"start_long_task":      invokeStart,
				"checkpoint_long_task": invokeCheckpoint,
				"long_task_control":    invokeControl,
			},
		}, nil
	}}
}

// invokeStart 登记长任务。
func invokeStart(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Title string `json:"title"`
		Steps []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"steps"`
		Paths []string `json:"paths"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("start_long_task: %v", err), nil
	}
	if strings.TrimSpace(args.Title) == "" {
		return pluginkit.ErrorResult("start_long_task: title 不能为空"), nil
	}
	steps := make([]toolapi.TaskStep, 0, len(args.Steps))
	for i, s := range args.Steps {
		id := s.ID
		if id == "" {
			id = fmt.Sprintf("step-%d", i+1)
		}
		steps = append(steps, toolapi.TaskStep{ID: id, Title: s.Title, Status: toolapi.TaskPending})
	}
	meta := map[string]any{}
	if len(args.Paths) > 0 {
		meta["paths"] = args.Paths
	}
	task, err := host.Tasks().Start(ctx, toolapi.TaskSpec{
		SessionID: call.SessionID,
		RunID:     call.RunID,
		Title:     args.Title,
		Steps:     steps,
		Meta:      meta,
	})
	if err != nil {
		return pluginkit.ErrorResult("start_long_task: %v", err), nil
	}
	return pluginkit.DataResult(
		fmt.Sprintf("长任务已登记：%s（%s，running）\n步骤 %d 个；检查点文件 %d 个。",
			task.ID(), args.Title, len(task.Steps()), len(args.Paths)),
		map[string]any{"task_id": task.ID(), "state": string(task.State()), "steps": task.Steps()},
	), nil
}

// invokeCheckpoint 生成检查点（Run + 快照引用）。
func invokeCheckpoint(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		TaskID string `json:"task_id"`
		Note   string `json:"note"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("checkpoint_long_task: %v", err), nil
	}
	if strings.TrimSpace(args.TaskID) == "" {
		return pluginkit.ErrorResult("checkpoint_long_task: task_id 不能为空"), nil
	}
	var cp toolapi.TaskCheckpoint
	var err error
	if m, ok := host.Tasks().(toolapi.TaskNoteCheckpointer); ok {
		cp, err = m.CheckpointNote(ctx, args.TaskID, args.Note)
	} else {
		cp, err = host.Tasks().Checkpoint(ctx, args.TaskID)
	}
	if err != nil {
		return pluginkit.ErrorResult("checkpoint_long_task: %v", err), nil
	}
	return pluginkit.DataResult(
		fmt.Sprintf("检查点已登记：任务 %s @ 快照 %s（Run %s）", cp.TaskID, cp.SnapshotID, cp.RunID),
		map[string]any{"checkpoint": cp},
	), nil
}

// invokeControl 控制：pause/resume/cancel/complete/failed/get/list/checkpoints/rollback。
func invokeControl(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Action       string `json:"action"`
		TaskID       string `json:"task_id"`
		CheckpointID string `json:"checkpoint_id"`
		Note         string `json:"note"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("long_task_control: %v", err), nil
	}
	mgr := host.Tasks()
	switch strings.TrimSpace(args.Action) {
	case "pause", "resume", "cancel", "complete", "failed":
		if strings.TrimSpace(args.TaskID) == "" {
			return pluginkit.ErrorResult("long_task_control: task_id 不能为空"), nil
		}
		task, ok := mgr.Get(args.TaskID)
		if !ok {
			return pluginkit.ErrorResult("long_task_control: 任务 %s 不在途（重启后任务只可查询）", args.TaskID), nil
		}
		var err error
		switch args.Action {
		case "pause":
			err = task.Pause()
		case "resume":
			err = task.Resume()
		case "cancel":
			err = task.Cancel()
		case "complete", "failed":
			state := toolapi.TaskCompleted
			if args.Action == "failed" {
				state = toolapi.TaskFailed
			}
			if f, ok := mgr.(toolapi.TaskFinisher); ok {
				err = f.Finish(ctx, args.TaskID, state, args.Note)
			} else {
				err = fmt.Errorf("内核不支持任务终态登记")
			}
		}
		if err != nil {
			return pluginkit.ErrorResult("long_task_control: %v", err), nil
		}
		return pluginkit.DataResult(
			fmt.Sprintf("长任务 %s → %s", args.TaskID, task.State()),
			map[string]any{"task_id": args.TaskID, "state": string(task.State())},
		), nil

	case "get":
		if strings.TrimSpace(args.TaskID) == "" {
			return pluginkit.ErrorResult("long_task_control: task_id 不能为空"), nil
		}
		task, ok := mgr.Get(args.TaskID)
		if ok {
			return pluginkit.DataResult(
				fmt.Sprintf("长任务 %s：%s，步骤 %d 个", task.ID(), task.State(), len(task.Steps())),
				map[string]any{"task_id": task.ID(), "state": string(task.State()), "steps": task.Steps()},
			), nil
		}
		if l, ok := mgr.(toolapi.TaskLister); ok {
			sums, lErr := l.ListSession(ctx, call.SessionID)
			if lErr != nil {
				return pluginkit.ErrorResult("long_task_control: %v", lErr), nil
			}
			for _, s := range sums {
				if s.ID == args.TaskID {
					return pluginkit.DataResult(
						fmt.Sprintf("长任务 %s：%s（持久态，不在途）", s.ID, s.State),
						map[string]any{"task_id": s.ID, "state": string(s.State)},
					), nil
				}
			}
		}
		return pluginkit.ErrorResult("long_task_control: 任务 %s 不存在", args.TaskID), nil

	case "list":
		out := []map[string]any{}
		if l, ok := mgr.(toolapi.TaskLister); ok {
			sums, lErr := l.ListSession(ctx, call.SessionID)
			if lErr != nil {
				return pluginkit.ErrorResult("long_task_control: %v", lErr), nil
			}
			for _, s := range sums {
				out = append(out, map[string]any{"task_id": s.ID, "title": s.Title, "state": string(s.State)})
			}
		} else {
			for _, t := range mgr.List() {
				out = append(out, map[string]any{"task_id": t.ID(), "state": string(t.State())})
			}
		}
		b, _ := json.Marshal(out)
		return pluginkit.TextResult("会话内任务 %d 个：%s", len(out), string(b)), nil

	case "checkpoints":
		if strings.TrimSpace(args.TaskID) == "" {
			return pluginkit.ErrorResult("long_task_control: task_id 不能为空"), nil
		}
		r, ok := mgr.(toolapi.TaskCheckpointReader)
		if !ok {
			return pluginkit.ErrorResult("long_task_control: 内核不支持检查点查询"), nil
		}
		cps, err := r.Checkpoints(ctx, args.TaskID)
		if err != nil {
			return pluginkit.ErrorResult("long_task_control: %v", err), nil
		}
		lines := make([]string, 0, len(cps))
		for _, cp := range cps {
			lines = append(lines, fmt.Sprintf("- %s（快照 %s，Run %s）%s", cp.TaskID, cp.SnapshotID, cp.RunID, cp.Note))
		}
		return pluginkit.DataResult(
			fmt.Sprintf("任务 %s 检查点 %d 个：\n%s", args.TaskID, len(cps), strings.Join(lines, "\n")),
			map[string]any{"checkpoints": cps},
		), nil

	case "rollback":
		if strings.TrimSpace(args.TaskID) == "" || strings.TrimSpace(args.CheckpointID) == "" {
			return pluginkit.ErrorResult("long_task_control: rollback 需要 task_id 与 checkpoint_id（= 检查点快照 ID，先 checkpoints 查询）"), nil
		}
		r, ok := mgr.(toolapi.TaskCheckpointReader)
		if !ok {
			return pluginkit.ErrorResult("long_task_control: 内核不支持检查点查询"), nil
		}
		cps, err := r.Checkpoints(ctx, args.TaskID)
		if err != nil {
			return pluginkit.ErrorResult("long_task_control: %v", err), nil
		}
		var snapID string
		for _, cp := range cps {
			if cp.SnapshotID == args.CheckpointID {
				snapID = cp.SnapshotID
				break
			}
		}
		if snapID == "" {
			return pluginkit.ErrorResult("long_task_control: 任务 %s 不存在检查点快照 %s", args.TaskID, args.CheckpointID), nil
		}
		if err := host.Snapshots().Restore(ctx, toolapi.SnapshotRef{ID: snapID}); err != nil {
			return pluginkit.ErrorResult("long_task_control: 回滚到检查点失败: %v", err), nil
		}
		return pluginkit.DataResult(
			fmt.Sprintf("已回滚到检查点（快照 %s）：任务声明文件已物理写回", snapID),
			map[string]any{"checkpoint_id": args.CheckpointID, "snapshot_id": snapID},
		), nil

	default:
		return pluginkit.ErrorResult("long_task_control: 未知 action %q", args.Action), nil
	}
}
