package store

import (
	"context"
	"testing"

	"harness-go/internal/model"
)

// 已读游标（未读红点）的存储层契约测试。
//
// 被测的核心不变量只有两条：
//  ① MarkRead 把游标推到「当前最后消息时间」（不是 now）—— 否则会出现假已读；
//  ② 游标单调不倒退 —— 重复上报、乱序上报都不得把已读状态退回未读。
func TestSessionMarkReadAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo := s.Sessions()

	if err := repo.Create(ctx, &model.Session{ID: "s-read", Title: "t"}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := repo.TouchMessage(ctx, "s-read", 1_700_000_000_000); err != nil {
		t.Fatalf("回写消息摘要失败: %v", err)
	}

	got, err := repo.Get(ctx, "s-read")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 新建会话游标为 0（DEFAULT），有消息后应为「未读」的原始形态。
	if got.LastReadAt != 0 {
		t.Fatalf("新建会话游标应初始为 0，实际 %d", got.LastReadAt)
	}
	if got.LastMessageAt == nil || *got.LastMessageAt != 1_700_000_000_000 {
		t.Fatalf("最后消息时间不符: %+v", got.LastMessageAt)
	}

	// 上报已读：游标应精确等于 last_message_at（而非服务端 now）。
	if err := repo.MarkRead(ctx, "s-read"); err != nil {
		t.Fatalf("上报已读失败: %v", err)
	}
	got, err = repo.Get(ctx, "s-read")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.LastReadAt != 1_700_000_000_000 {
		t.Fatalf("游标应推进到最后消息时间，实际 %d", got.LastReadAt)
	}
}

// 单调性：游标不得倒退（防乱序/重复上报把已读变回未读）。
func TestSessionMarkReadIsMonotonic(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo := s.Sessions()

	if err := repo.Create(ctx, &model.Session{ID: "s-mono", Title: "t"}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := repo.TouchMessage(ctx, "s-mono", 1_700_000_000_000); err != nil {
		t.Fatalf("回写失败: %v", err)
	}
	if err := repo.MarkRead(ctx, "s-mono"); err != nil {
		t.Fatalf("上报已读失败: %v", err)
	}

	// 重复上报：游标保持不变。
	if err := repo.MarkRead(ctx, "s-mono"); err != nil {
		t.Fatalf("重复上报已读失败: %v", err)
	}
	got, err := repo.Get(ctx, "s-mono")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.LastReadAt != 1_700_000_000_000 {
		t.Fatalf("重复上报不应改变游标，实际 %d", got.LastReadAt)
	}
}

// 空会话（从未有消息）：上报已读不得让游标变成「未来时间」或报错。
func TestSessionMarkReadOnEmptySession(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo := s.Sessions()

	if err := repo.Create(ctx, &model.Session{ID: "s-empty", Title: "t"}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := repo.MarkRead(ctx, "s-empty"); err != nil {
		t.Fatalf("空会话上报已读不应失败: %v", err)
	}
	got, err := repo.Get(ctx, "s-empty")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// last_message_at 为 NULL → 游标保持 0（空会话没有「未读」概念）。
	if got.LastReadAt != 0 {
		t.Fatalf("空会话游标应保持 0，实际 %d", got.LastReadAt)
	}
	if got.LastMessageAt != nil {
		t.Fatalf("空会话不应有最后消息时间: %+v", got.LastMessageAt)
	}
}

// 已删除会话上报已读应返回 not found（而不是静默成功）。
func TestSessionMarkReadDeletedSession(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo := s.Sessions()

	if err := repo.Create(ctx, &model.Session{ID: "s-del", Title: "t"}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := repo.SoftDelete(ctx, "s-del"); err != nil {
		t.Fatalf("删除会话失败: %v", err)
	}
	if err := repo.MarkRead(ctx, "s-del"); err == nil {
		t.Fatal("对已删除会话上报已读应返回错误")
	}
}
