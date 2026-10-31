package model

import (
	"encoding/json"
	"time"
)

// ============ 阶段 G：长任务 / 快照回滚 ============

// TaskState 长任务状态（与 toolapi.TaskState 取值一致，避免插件层 import 内核）。
type TaskState string

// 长任务状态机取值。
const (
	TaskPending   TaskState = "pending"
	TaskRunning   TaskState = "running"
	TaskPaused    TaskState = "paused"
	TaskCompleted TaskState = "completed"
	TaskFailed    TaskState = "failed"
	TaskCancelled TaskState = "cancelled"
)

// Terminal 判断是否终态。
func (s TaskState) Terminal() bool {
	return s == TaskCompleted || s == TaskFailed || s == TaskCancelled
}

// TaskStep 长任务步骤。
type TaskStep struct {
	ID     string    `json:"id"`
	Title  string    `json:"title"`
	Status TaskState `json:"status"`
	Note   string    `json:"note,omitempty"`
}

// Task 长任务持久态（会话库 tasks 表）。
type Task struct {
	ID        string          `json:"id"`
	RunID     string          `json:"run_id,omitempty"`
	Title     string          `json:"title"`
	State     TaskState       `json:"state"`
	Steps     []TaskStep      `json:"steps,omitempty"`
	MetaJSON  json.RawMessage `json:"meta,omitempty"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
}

// TaskCheckpoint 长任务检查点（Run + 快照引用）。
type TaskCheckpoint struct {
	ID         string `json:"id"`
	TaskID     string `json:"task_id"`
	RunID      string `json:"run_id,omitempty"`
	SnapshotID string `json:"snapshot_id"`
	Note       string `json:"note,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

// SnapshotBinding 快照内单路径绑定（hash 为空且 absent=true 表示当时文件不存在）。
type SnapshotBinding struct {
	Path   string `json:"path"`
	Hash   string `json:"hash,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Absent bool   `json:"absent,omitempty"`
}

// Snapshot 全局快照行（主库 snapshots 表；内容本体在 data/snapshots/blobs CAS）。
type Snapshot struct {
	ID          string            `json:"id"`
	SessionID   string            `json:"session_id,omitempty"`
	RunID       string            `json:"run_id,omitempty"`
	ToolEventID string            `json:"tool_event_id,omitempty"`
	TaskID      string            `json:"task_id,omitempty"`
	Kind        string            `json:"kind"` // before | after | checkpoint
	Paths       []SnapshotBinding `json:"paths"`
	Size        int64             `json:"size"`
	CreatedAt   int64             `json:"created_at"`
}

// RollbackMode 回滚粒度。
type RollbackMode string

// 快照语义（snapshots.kind）。
const (
	SnapshotKindBefore     = "before"     // 写入前（revert 方向）
	SnapshotKindAfter      = "after"      // 写入后（reapply 方向）
	SnapshotKindCheckpoint = "checkpoint" // 长任务检查点
)

// 回滚粒度取值。
const (
	RollbackTool    RollbackMode = "tool"    // 单个工具事件
	RollbackTurn    RollbackMode = "turn"    // 一轮 Run 的全部文件变更
	RollbackMessage RollbackMode = "message" // 回滚到消息（其后消息置灰）
)

// RollbackAffected 一次回滚涉及的单工具事件快照对。
type RollbackAffected struct {
	ToolEventID    string   `json:"tool_event_id"`
	RunID          string   `json:"run_id,omitempty"`
	SnapshotBefore string   `json:"snapshot_before,omitempty"`
	SnapshotAfter  string   `json:"snapshot_after,omitempty"`
	Paths          []string `json:"paths,omitempty"`
}

// RollbackRestored 单路径物理写回结果。
type RollbackRestored struct {
	Path   string `json:"path"`
	Action string `json:"action"` // restored | deleted
	Bytes  int64  `json:"bytes,omitempty"`
}

// Rollback 一次回滚事件（会话库 rollbacks 表）。
type Rollback struct {
	ID            string             `json:"id"`
	Mode          RollbackMode       `json:"mode"`
	Target        string             `json:"target"`
	Affected      []RollbackAffected `json:"affected,omitempty"`
	HiddenFromSeq int64              `json:"hidden_from_seq,omitempty"`
	Restored      []RollbackRestored `json:"restored,omitempty"`
	CreatedAt     int64              `json:"created_at"`
	ReappliedAt   *int64             `json:"reapplied_at,omitempty"`
}

// NowMillis 当前毫秒时间戳（迁移与仓储通用）。
func NowMillis() int64 { return time.Now().UnixMilli() }
