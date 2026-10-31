package service

// RollbackService 实现 S10「按消息/轮次/工具级 revert & reapply」（阶段 G）。
//
// 语义（docs/基础服务设计.md S10）：
//   - revert 是**物理写回**（tmp→rename 原子），不是改标记；每个路径恢复为
//       受影响工具事件里**最早**的 before 快照（absent → 物理删除文件）；
//   - reapply 恢复**最晚**的 after 快照，并把消息级回滚置灰的消息恢复可见；
//   - 回滚点之后的消息**置灰但保留**（messages.hidden；引擎组装上下文排除），
//       命令等非 fs 插件造成的文件变更不在快照覆盖范围（诚实边界）；
//   - 每次回滚落 rollbacks 审计行（affected/restored 快照对 + 置灰起点），
//       reapply 以它为依据且不可重复执行。

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/snapshot"
	"harness-go/internal/store"
)

// RollbackServiceOptions 回滚服务依赖。
type RollbackServiceOptions struct {
	Main      *store.MainStore
	Sessions  *store.SessionStore
	Snapshots *snapshot.Store
	// Now 时钟（测试注入）。
	Now func() time.Time
}

// RollbackService 回滚与重放。
type RollbackService struct {
	main     *store.MainStore
	sessions *store.SessionStore
	snaps    *snapshot.Store
	now      func() time.Time
}

// NewRollbackService 创建回滚服务。
func NewRollbackService(opts RollbackServiceOptions) *RollbackService {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &RollbackService{
		main:     opts.Main,
		sessions: opts.Sessions,
		snaps:    opts.Snapshots,
		now:      now,
	}
}

// RevertRequest 一次回滚请求（mode 与对应目标字段必须配套）。
type RevertRequest struct {
	Mode        model.RollbackMode `json:"mode"`
	ToolEventID string             `json:"tool_event_id,omitempty"`
	RunID       string             `json:"run_id,omitempty"`
	MessageSeq  int64              `json:"message_seq,omitempty"`
}

// Revert 物理回滚并登记回滚事件。
func (s *RollbackService) Revert(ctx context.Context, sessionID string, req RevertRequest) (*model.Rollback, error) {
	if _, err := s.main.Sessions().Get(ctx, sessionID); err != nil {
		return nil, wrapStoreErr(err, apperr.CodeSessionNotFound, "会话不存在")
	}
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// ① 收集受影响工具事件的 before/after 快照对
	pairs, err := s.affectedPairs(ctx, sdb, sessionID, req)
	if err != nil {
		return nil, err
	}

	// ② 逐路径恢复最早的 before（先快照后……这里本身就是恢复：顺序按快照时间升序，
	//    同路径首个 before 生效；partial 失败时已写回部分保留——物理语义，错误显式上抛）
	restored := make([]model.RollbackRestored, 0, len(pairs))
	claimed := map[string]struct{}{}
	for _, p := range pairs {
		if p.SnapshotBefore == "" {
			continue
		}
		snap, gErr := s.snaps.Get(ctx, p.SnapshotBefore)
		if gErr != nil {
			return nil, fmt.Errorf("读取快照 %s 失败: %w", p.SnapshotBefore, gErr)
		}
		for _, b := range snap.Paths {
			if _, done := claimed[b.Path]; done {
				continue
			}
			claimed[b.Path] = struct{}{}
			res, rErr := s.snaps.RestorePath(ctx, p.SnapshotBefore, b.Path)
			if rErr != nil {
				return nil, rErr
			}
			restored = append(restored, res)
		}
	}

	// ③ 消息级：置灰回滚点之后的消息
	hiddenFrom := int64(0)
	if req.Mode == model.RollbackMessage {
		if req.MessageSeq <= 0 {
			return nil, invalidArg("message 模式必须给 message_seq")
		}
		n, hErr := sdb.Messages().SetHiddenFrom(ctx, req.MessageSeq)
		if hErr != nil {
			return nil, wrapStoreErr(hErr, apperr.CodeInternal, "置灰消息失败")
		}
		hiddenFrom = req.MessageSeq
		_ = n
	}

	// ④ 登记回滚事件（审计 + reapply 依据）
	rb := &model.Rollback{
		ID:            "rb-" + newID(),
		Mode:          req.Mode,
		Target:        revertTarget(req),
		Affected:      pairs,
		HiddenFromSeq: hiddenFrom,
		Restored:      restored,
		CreatedAt:     s.now().UnixMilli(),
	}
	if err := sdb.Rollbacks().Insert(ctx, rb); err != nil {
		return nil, wrapStoreErr(err, apperr.CodeInternal, "登记回滚事件失败")
	}
	return rb, nil
}

// Reapply 撤销一次回滚：恢复其 after 快照（同路径取最晚）+ 恢复置灰消息。
func (s *RollbackService) Reapply(ctx context.Context, sessionID, rollbackID string) (*model.Rollback, error) {
	if _, err := s.main.Sessions().Get(ctx, sessionID); err != nil {
		return nil, wrapStoreErr(err, apperr.CodeSessionNotFound, "会话不存在")
	}
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rb, err := sdb.Rollbacks().Get(ctx, rollbackID)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "回滚事件不存在")
	}
	if rb.ReappliedAt != nil {
		return nil, apperr.New(apperr.CodeConflict, "该回滚已被 reapply，不可重复执行")
	}

	restored := make([]model.RollbackRestored, 0, len(rb.Affected))
	claimed := map[string]struct{}{}
	// 最晚 after 优先：按快照创建时间降序考察
	type afterCand struct {
		snapID string
		at     int64
		paths  []model.SnapshotBinding
	}
	cands := make([]afterCand, 0, len(rb.Affected))
	for _, a := range rb.Affected {
		if a.SnapshotAfter == "" {
			continue
		}
		snap, gErr := s.snaps.Get(ctx, a.SnapshotAfter)
		if gErr != nil {
			return nil, fmt.Errorf("读取快照 %s 失败: %w", a.SnapshotAfter, gErr)
		}
		cands = append(cands, afterCand{snapID: snap.ID, at: snap.CreatedAt, paths: snap.Paths})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].at > cands[j].at })
	for _, c := range cands {
		for _, b := range c.paths {
			if _, done := claimed[b.Path]; done {
				continue
			}
			claimed[b.Path] = struct{}{}
			res, rErr := s.snaps.RestorePath(ctx, c.snapID, b.Path)
			if rErr != nil {
				return nil, rErr
			}
			restored = append(restored, res)
		}
	}

	if rb.HiddenFromSeq > 0 {
		if _, cErr := sdb.Messages().ClearHiddenFrom(ctx, rb.HiddenFromSeq); cErr != nil {
			return nil, wrapStoreErr(cErr, apperr.CodeInternal, "恢复置灰消息失败")
		}
	}

	if err := sdb.Rollbacks().MarkReapplied(ctx, rollbackID); err != nil {
		return nil, wrapStoreErr(err, apperr.CodeConflict, "标记 reapply 失败")
	}
	updated, err := sdb.Rollbacks().Get(ctx, rollbackID)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeInternal, "回读回滚事件失败")
	}
	updated.Restored = restored
	return &updated, nil
}

// List 会话内回滚事件（时间倒序）。
func (s *RollbackService) List(ctx context.Context, sessionID string) ([]model.Rollback, error) {
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return sdb.Rollbacks().List(ctx)
}

// SnapshotList 会话内快照锚点（时间倒序；UI 展示与手动恢复）。
func (s *RollbackService) SnapshotList(ctx context.Context, sessionID string, limit int) ([]model.Snapshot, error) {
	return s.snaps.ListBySession(ctx, sessionID, limit)
}

// —— 内部 ——

// affectedPairs 收集受影响工具事件的 before/after 快照对（事件时间升序）。
func (s *RollbackService) affectedPairs(
	ctx context.Context, sdb *store.SessionDB, sessionID string, req RevertRequest,
) ([]model.RollbackAffected, error) {
	snaps, err := s.snaps.ListBySession(ctx, sessionID, 500)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeInternal, "读取会话快照失败")
	}
	runScope := map[string]struct{}{}
	switch req.Mode {
	case model.RollbackTool:
		if strings.TrimSpace(req.ToolEventID) == "" {
			return nil, invalidArg("tool 模式必须给 tool_event_id")
		}
	case model.RollbackTurn:
		if strings.TrimSpace(req.RunID) == "" {
			return nil, invalidArg("turn 模式必须给 run_id")
		}
		runScope[req.RunID] = struct{}{}
	case model.RollbackMessage:
		if req.MessageSeq <= 0 {
			return nil, invalidArg("message 模式必须给 message_seq")
		}
		msgs, mErr := sdb.Messages().List(ctx, req.MessageSeq-1, 500)
		if mErr != nil {
			return nil, wrapStoreErr(mErr, apperr.CodeInternal, "读取消息失败")
		}
		for _, msg := range msgs {
			if msg.RunID != "" {
				runScope[msg.RunID] = struct{}{}
			}
		}
	default:
		return nil, invalidArg("mode 必须是 tool|turn|message")
	}

	// 按工具事件分组：before 取最早，after 取最晚
	type pairAcc struct {
		runID         string
		before, after *model.Snapshot
	}
	acc := map[string]*pairAcc{}
	for i := range snaps {
		snap := &snaps[i]
		if req.Mode == model.RollbackTool && snap.ToolEventID != req.ToolEventID {
			continue
		}
		if req.Mode != model.RollbackTool {
			if _, ok := runScope[snap.RunID]; !ok {
				continue
			}
		}
		if snap.ToolEventID == "" {
			continue // 非工具事件锚点（如检查点快照）不参与消息/轮次回滚
		}
		p, ok := acc[snap.ToolEventID]
		if !ok {
			p = &pairAcc{runID: snap.RunID}
			acc[snap.ToolEventID] = p
		}
		switch snap.Kind {
		case model.SnapshotKindBefore:
			if p.before == nil || snap.CreatedAt < p.before.CreatedAt ||
				(snap.CreatedAt == p.before.CreatedAt && snap.ID < p.before.ID) {
				p.before = snap
			}
		case model.SnapshotKindAfter:
			if p.after == nil || snap.CreatedAt > p.after.CreatedAt ||
				(snap.CreatedAt == p.after.CreatedAt && snap.ID > p.after.ID) {
				p.after = snap
			}
		}
	}

	// 时间升序输出（事件序 = 快照 created_at 序）
	events := make([]string, 0, len(acc))
	eventAt := map[string]int64{}
	for evID, p := range acc {
		at := int64(0)
		if p.before != nil {
			at = p.before.CreatedAt
		} else if p.after != nil {
			at = p.after.CreatedAt
		}
		events = append(events, evID)
		eventAt[evID] = at
	}
	sort.Slice(events, func(i, j int) bool {
		if eventAt[events[i]] != eventAt[events[j]] {
			return eventAt[events[i]] < eventAt[events[j]]
		}
		return events[i] < events[j]
	})

	out := make([]model.RollbackAffected, 0, len(events))
	for _, evID := range events {
		p := acc[evID]
		paths := []string{}
		src := p.before
		if src == nil {
			src = p.after
		}
		if src != nil {
			for _, b := range src.Paths {
				paths = append(paths, b.Path)
			}
		}
		aff := model.RollbackAffected{ToolEventID: evID, RunID: p.runID, Paths: paths}
		if p.before != nil {
			aff.SnapshotBefore = p.before.ID
		}
		if p.after != nil {
			aff.SnapshotAfter = p.after.ID
		}
		out = append(out, aff)
	}
	return out, nil
}

func revertTarget(req RevertRequest) string {
	switch req.Mode {
	case model.RollbackTool:
		return req.ToolEventID
	case model.RollbackTurn:
		return req.RunID
	case model.RollbackMessage:
		return strconv.FormatInt(req.MessageSeq, 10)
	}
	return ""
}
