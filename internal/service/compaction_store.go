package service

// 上下文压缩的存储适配。
//
// # 为什么需要按 sessionID 动态取库
//
// compact.Store 的接口方法**没有 sessionID 参数**（设计上刻意如此：
// 压缩算法不应关心"数据在哪"，只关心"有哪些记录"）。但本项目的存储是
// **每会话一个独立 SQLite 库**（data/sessions/<id>/session.db），
// 压缩表也在会话库内。
//
// 因此这里做一次"绑定会话"的适配：每次压缩请求构造一个绑定了 sessionID 的
// Store 视图。这样：
//   - compact 包保持与存储布局无关（它可以被单测用内存桩替换）；
//   - 会话隔离由存储层天然保证（不可能读到别的会话的摘要）。

import (
	"context"
	"fmt"

	"harness-go/internal/compact"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// sessionCompactionStore 把「会话级压缩仓储」绑定到具体 sessionID。
type sessionCompactionStore struct {
	ss        *store.SessionStore
	sessionID string
}

// 编译期断言：必须满足 compact.Store。
var _ compact.Store = (*sessionCompactionStore)(nil)

// newSessionCompactionStore 创建绑定会话的压缩存储视图。
func newSessionCompactionStore(ss *store.SessionStore, sessionID string) compact.Store {
	return &sessionCompactionStore{ss: ss, sessionID: sessionID}
}

// repo 取得当前会话的压缩仓储。
func (s *sessionCompactionStore) repo(ctx context.Context) (*store.CompactionRepo, error) {
	db, err := s.ss.Acquire(ctx, s.sessionID)
	if err != nil {
		return nil, fmt.Errorf("service: 获取会话库失败: %w", err)
	}
	return db.Compactions(), nil
}

// Create 实现 compact.Store。
func (s *sessionCompactionStore) Create(ctx context.Context, c *model.Compaction) error {
	r, err := s.repo(ctx)
	if err != nil {
		return err
	}
	return r.Create(ctx, c)
}

// List 实现 compact.Store。
func (s *sessionCompactionStore) List(ctx context.Context) ([]model.Compaction, error) {
	r, err := s.repo(ctx)
	if err != nil {
		return nil, err
	}
	return r.List(ctx)
}

// LatestEndSeq 实现 compact.Store。
func (s *sessionCompactionStore) LatestEndSeq(ctx context.Context) (int64, error) {
	r, err := s.repo(ctx)
	if err != nil {
		return 0, err
	}
	return r.LatestEndSeq(ctx)
}
