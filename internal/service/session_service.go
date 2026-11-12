package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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

// CreateSessionInput 新建会话入参（集中默认值裁决，避免长参数列表继续膨胀）。
//
// ProjectID 非空表示会话归属某项目；AccessMode 留空即走「项目默认权限继承」。
type CreateSessionInput struct {
	Title         string
	ModelRef      string
	WorkspacePath string
	ProjectID     string
	// WorkspaceID 会话所属执行面（协议 §3.3）：空 = 纯对话（不允许工具）。
	// 能力天花板由它所指 Workspace 的 kind 决定，是权限判定的唯一依据。
	WorkspaceID string
	AccessMode  string
}

// Create 新建会话（**只写主库，不建会话库文件**；首次发消息时才建）。
//
// 归属裁决（协议 §3.3 / §5）：
//   - WorkspaceID 非空 → 校验 Workspace 存在，否则 400（不留悬空引用）；
//   - ProjectID 非空且 WorkspaceID 为空 → 从项目的 workspace_id 继承（缺省归属）；
//   - 两者都空 → 纯对话，能力天花板为「仅对话」（由 run_service.resolveToolPolicy 判定）。
//
// 术语：WorkspaceID = 工作空间（执行面）；WorkspacePath = **项目目录**（磁盘落盘路径）。
// 二者无关，权威定义见 docs/术语表-工作空间与项目目录.md。
//
// AccessMode 裁决链（顺序即优先级）：
//  1. 显式传入的 in.AccessMode 合法（confirm|full）→ 直接采用；非空但非法 → 400 拒绝；
//  2. 否则若 in.ProjectID 非空 → 继承项目 default_access_mode（**仅在新建这一刻快照一次**，
//     之后项目改默认值不影响已建会话）；项目不存在 → 400「项目不存在」；
//  3. 否则保持空串，交由 SessionRepo.Create 内部的 confirm 兜底（最后防线）。
func (s *SessionService) Create(ctx context.Context, in CreateSessionInput) (*model.Session, error) {
	mode := ""
	if raw := strings.TrimSpace(in.AccessMode); raw != "" {
		mode = normalizeAccessModeStrict(raw)
		if mode == "" {
			return nil, invalidArg("access_mode 必须是 confirm 或 full")
		}
	} else if pid := strings.TrimSpace(in.ProjectID); pid != "" {
		p, err := s.main.Projects().Get(ctx, pid)
		if err != nil {
			// 权衡：apperr 中暂无项目级错误码，且「引用了不存在的项目」本质是调用方入参错误，
			// 故就近用 invalidArg（400），避免误用 CodeSessionNotFound 混淆语义。
			if errors.Is(err, store.ErrNotFound) {
				return nil, invalidArg("项目不存在")
			}
			return nil, err
		}
		mode = normalizeAccessModeStrict(p.DefaultAccessMode)
		// 项目里若存了非法值（历史脏数据），一律回落空串走 confirm 兜底，绝不放大权限。
	}

	// 执行面归属：显式 workspace_id 优先，其次从项目继承，最后缺省绑服务端内置空间。
	//
	// 为什么必须校验存在性：能力判定靠 workspace.kind，悬空外键会让判定
	// 走 fail-closed 分支（按仅对话），用户会莫名其妙失去工具能力却查不到原因。
	//
	// 【R1 工作空间必填】会话**必须**有工作空间归属，纯对话也不例外：
	//   - 纯对话 = 绑服务端内置空间（ws-server）+ 无项目，能力天花板由 resolveToolPolicy
	//     细分判定（无项目 → readonly，见 R1b）；
	//   - 这样消除了「workspace_id 为空」这个特例分支，会话归属语义唯一。
	// 项目仍为可选：纯对话就是「有工作空间、但没有项目」。
	wsID := strings.TrimSpace(in.WorkspaceID)
	if wsID != "" {
		if _, err := s.main.Workspaces().Get(ctx, wsID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, invalidArg("工作空间不存在")
			}
			return nil, err
		}
	} else if pid := strings.TrimSpace(in.ProjectID); pid != "" {
		// 项目已在上方校验过存在性（AccessMode 分支），此处可安全读取。
		if p, err := s.main.Projects().Get(ctx, pid); err == nil {
			wsID = strings.TrimSpace(p.WorkspaceID)
		}
	}
	if wsID == "" {
		// 缺省归属：服务端内置空间。纯对话亦须绑定，不留空串。
		// 注意：项目本身没有 workspace_id（或未指定项目）时才会走到这里。
		wsID = store.BuiltinWorkspaceServerID
	}

	sess := &model.Session{
		ID:            newID(),
		Title:         in.Title,
		ModelRef:      in.ModelRef,
		WorkspacePath: in.WorkspacePath,
		ProjectID:     strings.TrimSpace(in.ProjectID),
		WorkspaceID:   wsID,
		AccessMode:    mode,
	}
	if sess.Title == "" {
		sess.Title = "新对话"
	}
	if err := s.main.Sessions().Create(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// SetProject 调整会话归属项目（projectID 为空 = 移出项目，回纯对话）。
//
// 非空时先校验项目存在，避免写入悬空引用。
func (s *SessionService) SetProject(ctx context.Context, id, projectID string) error {
	pid := strings.TrimSpace(projectID)
	if pid != "" {
		if _, err := s.main.Projects().Get(ctx, pid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return invalidArg("项目不存在")
			}
			return err
		}
	}
	return wrapStoreErr(s.main.Sessions().SetProject(ctx, id, pid), apperr.CodeSessionNotFound, "会话不存在")
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
