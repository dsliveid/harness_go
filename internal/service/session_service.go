package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// SessionService 会话用例（列表/统计只读主库，绝不扫会话库）。
type SessionService struct {
	main     *store.MainStore
	sessions *store.SessionStore
	// onDeleted 可选钩子：删除会话后清理跨会话副产物（阶段 G：全局快照行）。
	onDeleted func(ctx context.Context, sessionID string)
}

// NewSessionService 创建会话用例。
func NewSessionService(main *store.MainStore, sessions *store.SessionStore) *SessionService {
	return &SessionService{main: main, sessions: sessions}
}

// SetDeletedHook 注入会话删除后的清理钩子（快照服务：ForgetSession + GC）。
func (s *SessionService) SetDeletedHook(fn func(ctx context.Context, sessionID string)) {
	s.onDeleted = fn
}

// Create 新建会话（**只写主库，不建会话库文件**；首次发消息时才建）。
func (s *SessionService) Create(ctx context.Context, title, modelRef, workspacePath string) (*model.Session, error) {
	sess := &model.Session{
		ID:            newID(),
		Title:         title,
		ModelRef:      modelRef,
		WorkspacePath: workspacePath,
	}
	if sess.Title == "" {
		sess.Title = "新对话"
	}
	if err := s.main.Sessions().Create(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// List 分页列出会话。
func (s *SessionService) List(ctx context.Context, limit, offset int) ([]model.Session, error) {
	return s.main.Sessions().List(ctx, limit, offset)
}

// Get 取会话。
func (s *SessionService) Get(ctx context.Context, id string) (*model.Session, error) {
	sess, err := s.main.Sessions().Get(ctx, id)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeSessionNotFound, "会话不存在")
	}
	return sess, nil
}

// Rename 重命名。
func (s *SessionService) Rename(ctx context.Context, id, title string) error {
	if title == "" {
		return invalidArg("标题不能为空")
	}
	return wrapStoreErr(s.main.Sessions().Rename(ctx, id, title), apperr.CodeSessionNotFound, "会话不存在")
}

// Delete 删除会话：主库软删 → 删会话目录（失败留孤儿，由启动清理）。
func (s *SessionService) Delete(ctx context.Context, id string) error {
	if err := s.main.Sessions().SoftDelete(ctx, id); err != nil {
		return wrapStoreErr(err, apperr.CodeSessionNotFound, "会话不存在")
	}
	if err := s.sessions.DeleteSessionDir(id); err != nil {
		return fmt.Errorf("service: 会话行已删除，但目录清理失败（孤儿待启动清理）: %w", err)
	}
	if s.onDeleted != nil {
		s.onDeleted(ctx, id)
	}
	return nil
}

// Messages 读取会话消息（增量分页）。先校验会话存在，避免为不存在的会话建库。
func (s *SessionService) Messages(ctx context.Context, id string, afterSeq int64, limit int) ([]model.Message, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	sdb, err := s.sessions.Acquire(ctx, id)
	if err != nil {
		return nil, err
	}
	return sdb.Messages().List(ctx, afterSeq, limit)
}

// newID 生成 16 字节随机 ID。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("id-%d", store.NowMillis())
	}
	return hex.EncodeToString(b[:])
}
