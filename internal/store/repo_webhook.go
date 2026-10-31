package store

// Webhook 订阅仓储（主库，P1 / 需求 §4.4）。
//
// secret 以 AES-256-GCM 密文落库（与 providers.api_key 同一 Cipher）；
// 明文只在 Create/Update 时出现一次，API 永不回读。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"harness-go/internal/secret"
)

// ErrWebhookNotFound Webhook 订阅不存在。
var ErrWebhookNotFound = errors.New("store: Webhook 订阅不存在")

// Webhook 订阅行。
type Webhook struct {
	ID        string   `json:"id"`
	URL       string   `json:"url"`
	SecretEnc []byte   `json:"-"` // 密文（明文永不落日志/回显）
	HasSecret bool     `json:"has_secret"`
	Events    []string `json:"events"` // 白名单（空 = 全部一期事件）
	Enabled   bool     `json:"enabled"`
	CreatedAt int64    `json:"created_at"`
}

// WebhooksRepo Webhook 订阅仓储。
type WebhooksRepo struct{ s *MainStore }

// Webhooks 返回 Webhook 仓储。
func (s *MainStore) Webhooks() *WebhooksRepo { return &WebhooksRepo{s: s} }

// Create 新增订阅（SecretEnc 必须已经 EncryptSecret 加密；明文不进本层）。
func (r *WebhooksRepo) Create(ctx context.Context, w *Webhook) error {
	if w.ID == "" || w.URL == "" {
		return errors.New("store: Webhook ID 与 URL 不能为空")
	}
	if w.CreatedAt == 0 {
		w.CreatedAt = NowMillis()
	}
	eventsJSON, err := json.Marshal(w.Events)
	if err != nil {
		return fmt.Errorf("store: 序列化事件白名单失败: %w", err)
	}
	_, err = r.s.conn().ExecContext(ctx,
		`INSERT INTO webhooks(id, url, secret_enc, events_json, enabled, created_at)
		 VALUES(?,?,?,?,?,?)`,
		w.ID, w.URL, w.SecretEnc, string(eventsJSON), boolToInt(w.Enabled), w.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: 写入 Webhook 失败: %w", err)
	}
	return nil
}

// EncryptSecret 把明文 secret 加密为存储形态（调用方在 Create/Update 前调用）。
func EncryptSecret(c *secret.Cipher, plain string) ([]byte, bool, error) {
	if plain == "" {
		return nil, false, nil
	}
	if c == nil {
		return nil, false, errors.New("store: 缺少加密器")
	}
	enc, err := c.Encrypt([]byte(plain))
	if err != nil {
		return nil, false, fmt.Errorf("store: 加密 Webhook secret 失败: %w", err)
	}
	return enc, true, nil
}

// DecryptSecret 解出明文 secret（分发器签名用；未设置返回空串）。
func (w *Webhook) DecryptSecret(c *secret.Cipher) (string, error) {
	if len(w.SecretEnc) == 0 {
		return "", nil
	}
	if c == nil {
		return "", errors.New("store: 缺少加密器，无法解密 Webhook secret")
	}
	plain, err := c.Decrypt(w.SecretEnc)
	if err != nil {
		return "", fmt.Errorf("store: 解密 Webhook secret 失败: %w", err)
	}
	return string(plain), nil
}

// List 全部订阅（创建时间正序）。
func (r *WebhooksRepo) List(ctx context.Context) ([]Webhook, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, url, secret_enc, events_json, enabled, created_at FROM webhooks ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 列出 Webhook 失败: %w", err)
	}
	defer rows.Close()
	out := []Webhook{}
	for rows.Next() {
		w, wErr := scanWebhook(rows)
		if wErr != nil {
			return nil, wErr
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Get 单条订阅。
func (r *WebhooksRepo) Get(ctx context.Context, id string) (*Webhook, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT id, url, secret_enc, events_json, enabled, created_at FROM webhooks WHERE id = ?`, id)
	w, err := scanWebhook(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrWebhookNotFound, id)
		}
		return nil, err
	}
	return &w, nil
}

// SetEnabled 启用/停用。
func (r *WebhooksRepo) SetEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE webhooks SET enabled = ? WHERE id = ?`, boolToInt(enabled), id)
	if err != nil {
		return fmt.Errorf("store: 更新 Webhook 状态失败: %w", err)
	}
	return ensureAffected(res, "Webhook "+id)
}

// Delete 删除订阅。
func (r *WebhooksRepo) Delete(ctx context.Context, id string) error {
	res, err := r.s.conn().ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除 Webhook 失败: %w", err)
	}
	return ensureAffected(res, "Webhook "+id)
}

func scanWebhook(sc interface{ Scan(...any) error }) (Webhook, error) {
	var (
		w          Webhook
		secretEnc  []byte
		eventsJSON string
		enabled    int
	)
	if err := sc.Scan(&w.ID, &w.URL, &secretEnc, &eventsJSON, &enabled, &w.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Webhook{}, ErrWebhookNotFound
		}
		return Webhook{}, fmt.Errorf("store: 解析 Webhook 行失败: %w", err)
	}
	w.SecretEnc = secretEnc
	w.HasSecret = len(secretEnc) > 0
	if err := json.Unmarshal([]byte(eventsJSON), &w.Events); err != nil {
		return Webhook{}, fmt.Errorf("store: 解析事件白名单失败: %w", err)
	}
	w.Enabled = enabled != 0
	return w, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
