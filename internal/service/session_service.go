package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

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

// UpdateModel 更新会话级模型引用（顶栏模型选择器持久化；空串 = 未选择）。
func (s *SessionService) UpdateModel(ctx context.Context, id, modelRef string) error {
	return wrapStoreErr(s.main.Sessions().UpdateModel(ctx, id, strings.TrimSpace(modelRef)), apperr.CodeSessionNotFound, "会话不存在")
}

// Archive 归档会话（归档中心可见，侧栏默认列表不再显示；状态落库，刷新/重启不丢）。
func (s *SessionService) Archive(ctx context.Context, id string) error {
	return wrapStoreErr(s.main.Sessions().Archive(ctx, id), apperr.CodeSessionNotFound, "会话不存在")
}

// Unarchive 取消归档（回到原分组；对未归档会话幂等）。
func (s *SessionService) Unarchive(ctx context.Context, id string) error {
	return wrapStoreErr(s.main.Sessions().Unarchive(ctx, id), apperr.CodeSessionNotFound, "会话不存在")
}

// SetAccessMode 更新会话访问模式（顶栏「变更前确认/完全访问」切换落库；
// 非法值 400，引擎每次审批实时重读该值，运行中切换立即生效）。
func (s *SessionService) SetAccessMode(ctx context.Context, id, mode string) error {
	m := normalizeAccessModeStrict(mode)
	if m == "" {
		return invalidArg("access_mode 必须是 confirm 或 full")
	}
	return wrapStoreErr(s.main.Sessions().UpdateAccessMode(ctx, id, m), apperr.CodeSessionNotFound, "会话不存在")
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
