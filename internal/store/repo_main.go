package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"harness-go/internal/model"
)

// SessionRepo 主库会话仓储。
//
// 铁律：会话列表与统计**只读本仓储**，绝不扫描会话库（docs/数据存储方案.md §2.4）。
type SessionRepo struct{ s *MainStore }

// Sessions 返回主库会话仓储。
func (s *MainStore) Sessions() *SessionRepo { return &SessionRepo{s: s} }

// Create 创建会话（只写主库，**不建会话库文件**，首次写消息时才建）。
func (r *SessionRepo) Create(ctx context.Context, sess *model.Session) error {
	if sess.ID == "" {
		return errors.New("store: 会话 ID 不能为空")
	}
	now := NowMillis()
	if sess.CreatedAt == 0 {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	if sess.AccessMode == "" {
		sess.AccessMode = "confirm"
	}
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO sessions(id, title, workspace_path, access_mode, model_ref, user_id,
		                     created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		sess.ID, sess.Title, sess.WorkspacePath, sess.AccessMode, sess.ModelRef, sess.UserID,
		sess.CreatedAt, sess.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: 创建会话失败: %w", err)
	}
	return nil
}

const sessionColumns = `id, title, workspace_path, access_mode, model_ref, user_id,
	message_count, last_message_at, total_tokens, last_run_status,
	created_at, updated_at, deleted_at`

func scanSession(sc interface{ Scan(...any) error }) (model.Session, error) {
	var (
		s       model.Session
		lastMsg sql.NullInt64
		status  sql.NullString
		deleted sql.NullInt64
	)
	err := sc.Scan(&s.ID, &s.Title, &s.WorkspacePath, &s.AccessMode, &s.ModelRef, &s.UserID,
		&s.MessageCount, &lastMsg, &s.TotalTokens, &status,
		&s.CreatedAt, &s.UpdatedAt, &deleted)
	if err != nil {
		return model.Session{}, err
	}
	if lastMsg.Valid {
		v := lastMsg.Int64
		s.LastMessageAt = &v
	}
	if status.Valid {
		s.LastRunStatus = model.RunStatus(status.String)
	}
	if deleted.Valid {
		v := deleted.Int64
		s.DeletedAt = &v
	}
	return s, nil
}

// Get 按 ID 取会话（已软删的返回 ErrNotFound）。
func (r *SessionRepo) Get(ctx context.Context, id string) (*model.Session, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id=? AND deleted_at IS NULL`, id)
	s, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 会话 %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: 查询会话失败: %w", err)
	}
	return &s, nil
}

// List 分页列出会话（按更新时间倒序）。
func (r *SessionRepo) List(ctx context.Context, limit, offset int) ([]model.Session, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE deleted_at IS NULL
		 ORDER BY updated_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: 列出会话失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.Session, 0, limit)
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 解析会话行失败: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Count 会话总数（不含软删）。
func (r *SessionRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE deleted_at IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: 统计会话数失败: %w", err)
	}
	return n, nil
}

// Rename 重命名会话。
func (r *SessionRepo) Rename(ctx context.Context, id, title string) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE sessions SET title=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		title, NowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 重命名会话失败: %w", err)
	}
	return ensureAffected(res, "会话 "+id)
}

// SoftDelete 软删会话（物理目录由上层在事务成功后删除）。
func (r *SessionRepo) SoftDelete(ctx context.Context, id string) error {
	now := NowMillis()
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE sessions SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		now, now, id)
	if err != nil {
		return fmt.Errorf("store: 删除会话失败: %w", err)
	}
	return ensureAffected(res, "会话 "+id)
}

// TouchMessage 摘要回写：消息数 +1、更新最后消息时间。
// 与会话库写入**不同事务**（最终一致，失败只影响列表新鲜度）。
func (r *SessionRepo) TouchMessage(ctx context.Context, id string, at int64) error {
	if at == 0 {
		at = NowMillis()
	}
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE sessions SET message_count = message_count + 1,
		        last_message_at=?, updated_at=?
		 WHERE id=? AND deleted_at IS NULL`, at, at, id)
	if err != nil {
		return fmt.Errorf("store: 回写消息摘要失败: %w", err)
	}
	return ensureAffected(res, "会话 "+id)
}

// AddTokensAndStatus 摘要回写：累加 token 用量并记录 Run 终态。
func (r *SessionRepo) AddTokensAndStatus(ctx context.Context, id string, tokens int64, status model.RunStatus) error {
	now := NowMillis()
	var statusArg any
	if status != "" {
		statusArg = string(status)
	}
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE sessions SET total_tokens = total_tokens + ?,
		        last_run_status = COALESCE(?, last_run_status),
		        updated_at=?
		 WHERE id=? AND deleted_at IS NULL`, tokens, statusArg, now, id)
	if err != nil {
		return fmt.Errorf("store: 回写 Run 摘要失败: %w", err)
	}
	return ensureAffected(res, "会话 "+id)
}

// ============ 账号 ============

// UserRepo 主库账号仓储。
type UserRepo struct{ s *MainStore }

// Users 返回账号仓储。
func (s *MainStore) Users() *UserRepo { return &UserRepo{s: s} }

// Create 创建账号。
func (r *UserRepo) Create(ctx context.Context, u *model.User) error {
	now := NowMillis()
	if u.CreatedAt == 0 {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	if u.Role == "" {
		u.Role = "owner"
	}
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, display_name, role, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		u.ID, u.Username, u.PasswordHash, u.DisplayName, u.Role, u.CreatedAt, u.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: 创建账号失败: %w", err)
	}
	return nil
}

// GetByUsername 按用户名取账号。
func (r *UserRepo) GetByUsername(ctx context.Context, username string) (*model.User, error) {
	var u model.User
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT id, username, password_hash, display_name, role, created_at, updated_at
		 FROM users WHERE username=?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 账号 %s", ErrNotFound, username)
		}
		return nil, fmt.Errorf("store: 查询账号失败: %w", err)
	}
	return &u, nil
}

// GetByID 按 ID 取账号。
func (r *UserRepo) GetByID(ctx context.Context, id string) (*model.User, error) {
	var u model.User
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT id, username, password_hash, display_name, role, created_at, updated_at
		 FROM users WHERE id=?`, id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 账号 %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: 查询账号失败: %w", err)
	}
	return &u, nil
}

// First 返回最早的账号（一期单用户：用于把 API 令牌绑定到该账号）。
func (r *UserRepo) First(ctx context.Context) (*model.User, error) {
	var u model.User
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT id, username, password_hash, display_name, role, created_at, updated_at
		 FROM users ORDER BY created_at ASC LIMIT 1`).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 账号", ErrNotFound)
		}
		return nil, fmt.Errorf("store: 查询账号失败: %w", err)
	}
	return &u, nil
}

// Count 账号数量（首启引导用：为 0 时创建初始账号）。
func (r *UserRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := r.s.conn().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计账号数失败: %w", err)
	}
	return n, nil
}

// UpdatePassword 更新口令哈希。
func (r *UserRepo) UpdatePassword(ctx context.Context, id, hash string) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE users SET password_hash=?, updated_at=? WHERE id=?`, hash, NowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新口令失败: %w", err)
	}
	return ensureAffected(res, "账号 "+id)
}

// ============ 令牌 ============

// TokenRepo 主库令牌仓储（api / web_session / sso 统一）。
type TokenRepo struct{ s *MainStore }

// Tokens 返回令牌仓储。
func (s *MainStore) Tokens() *TokenRepo { return &TokenRepo{s: s} }

// Create 写入令牌（只存 sha256）。
func (r *TokenRepo) Create(ctx context.Context, t *model.Token) error {
	if t.CreatedAt == 0 {
		t.CreatedAt = NowMillis()
	}
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO api_tokens(id, user_id, name, kind, token_hash, scope, expires_at, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		t.ID, t.UserID, t.Name, t.Kind, t.TokenHash, t.Scope, t.ExpiresAt, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: 创建令牌失败: %w", err)
	}
	return nil
}

// GetByHash 按哈希取令牌（过期/吊销判定由上层做，便于统一错误码）。
func (r *TokenRepo) GetByHash(ctx context.Context, hash string) (*model.Token, error) {
	var (
		t        model.Token
		expires  sql.NullInt64
		lastUsed sql.NullInt64
		revoked  sql.NullInt64
	)
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT id, user_id, name, kind, token_hash, scope, expires_at, last_used_at, revoked_at, created_at
		 FROM api_tokens WHERE token_hash=?`, hash).
		Scan(&t.ID, &t.UserID, &t.Name, &t.Kind, &t.TokenHash, &t.Scope,
			&expires, &lastUsed, &revoked, &t.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 令牌", ErrNotFound)
		}
		return nil, fmt.Errorf("store: 查询令牌失败: %w", err)
	}
	if expires.Valid {
		v := expires.Int64
		t.ExpiresAt = &v
	}
	if lastUsed.Valid {
		v := lastUsed.Int64
		t.LastUsedAt = &v
	}
	if revoked.Valid {
		v := revoked.Int64
		t.RevokedAt = &v
	}
	return &t, nil
}

// Touch 更新最后使用时间（节流由上层控制，避免每请求写库）。
func (r *TokenRepo) Touch(ctx context.Context, id string, at int64) error {
	_, err := r.s.conn().ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at=? WHERE id=?`, at, id)
	if err != nil {
		return fmt.Errorf("store: 更新令牌使用时间失败: %w", err)
	}
	return nil
}

// CountByKind 统计某类型未吊销令牌数量（首启引导用）。
func (r *TokenRepo) CountByKind(ctx context.Context, kind string) (int64, error) {
	var n int64
	if err := r.s.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM api_tokens WHERE kind=? AND revoked_at IS NULL`, kind).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计令牌数失败: %w", err)
	}
	return n, nil
}

// Revoke 吊销令牌。
func (r *TokenRepo) Revoke(ctx context.Context, id string, at int64) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, at, id)
	if err != nil {
		return fmt.Errorf("store: 吊销令牌失败: %w", err)
	}
	return ensureAffected(res, "令牌 "+id)
}

// ============ 配置 ============

// SettingsRepo 主库键值配置仓储（含 tools.tiers 的全局层）。
type SettingsRepo struct{ s *MainStore }

// Settings 返回配置仓储。
func (s *MainStore) Settings() *SettingsRepo { return &SettingsRepo{s: s} }

// Get 读取配置值（JSON）。
func (r *SettingsRepo) Get(ctx context.Context, key string) (json.RawMessage, error) {
	var raw string
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key=?`, key).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 配置 %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("store: 读取配置失败: %w", err)
	}
	return json.RawMessage(raw), nil
}

// Set 写入配置值（JSON）。
func (r *SettingsRepo) Set(ctx context.Context, key string, value json.RawMessage) error {
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO settings(key, value_json, updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json, updated_at=excluded.updated_at`,
		key, string(value), NowMillis())
	if err != nil {
		return fmt.Errorf("store: 写入配置失败: %w", err)
	}
	return nil
}

// ensureAffected 校验 UPDATE/DELETE 命中行数。
func ensureAffected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return nil
}
