package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"harness-go/internal/model"
)

// ProviderRepo 模型厂商仓储（主库）。
//
// api_key 只以密文（api_key_enc）落库，加解密由 service 层持有密钥完成，
// store 不感知明文（见 docs/数据存储方案.md §2）。
type ProviderRepo struct{ s *MainStore }

// Providers 返回厂商仓储。
func (s *MainStore) Providers() *ProviderRepo { return &ProviderRepo{s: s} }

const providerCols = `id, name, base_url, api_key_enc, models_json, enabled, created_at, updated_at`

// Create 新增厂商。
func (r *ProviderRepo) Create(ctx context.Context, p *model.Provider) error {
	if p.ID == "" || p.Name == "" {
		return errors.New("store: 厂商缺少必要字段")
	}
	models, err := json.Marshal(p.Models)
	if err != nil {
		return fmt.Errorf("store: 序列化模型列表失败: %w", err)
	}
	now := NowMillis()
	p.CreatedAt, p.UpdatedAt = now, now
	_, err = r.s.conn().ExecContext(ctx,
		`INSERT INTO providers(`+providerCols+`) VALUES(?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.BaseURL, nullBytes(p.APIKeyEnc), string(models), boolInt(p.Enabled), now, now)
	if err != nil {
		return fmt.Errorf("store: 新增厂商失败: %w", err)
	}
	return nil
}

// Update 更新厂商（api_key_enc 为 nil 表示保留原密钥）。
func (r *ProviderRepo) Update(ctx context.Context, p *model.Provider) error {
	models, err := json.Marshal(p.Models)
	if err != nil {
		return fmt.Errorf("store: 序列化模型列表失败: %w", err)
	}
	now := NowMillis()
	var res sql.Result
	if p.APIKeyEnc == nil {
		res, err = r.s.conn().ExecContext(ctx,
			`UPDATE providers SET name=?, base_url=?, models_json=?, enabled=?, updated_at=? WHERE id=?`,
			p.Name, p.BaseURL, string(models), boolInt(p.Enabled), now, p.ID)
	} else {
		res, err = r.s.conn().ExecContext(ctx,
			`UPDATE providers SET name=?, base_url=?, api_key_enc=?, models_json=?, enabled=?, updated_at=? WHERE id=?`,
			p.Name, p.BaseURL, nullBytes(p.APIKeyEnc), string(models), boolInt(p.Enabled), now, p.ID)
	}
	if err != nil {
		return fmt.Errorf("store: 更新厂商失败: %w", err)
	}
	p.UpdatedAt = now
	return ensureAffected(res, "厂商 "+p.ID)
}

// Delete 删除厂商。
func (r *ProviderRepo) Delete(ctx context.Context, id string) error {
	res, err := r.s.conn().ExecContext(ctx, `DELETE FROM providers WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除厂商失败: %w", err)
	}
	return ensureAffected(res, "厂商 "+id)
}

// Get 取单个厂商（不存在返回 ErrNotFound）。
func (r *ProviderRepo) Get(ctx context.Context, id string) (*model.Provider, error) {
	list, err := r.query(ctx, `WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// List 列出全部厂商（按创建时间倒序）。
func (r *ProviderRepo) List(ctx context.Context) ([]model.Provider, error) {
	return r.query(ctx, `ORDER BY created_at DESC`)
}

func (r *ProviderRepo) query(ctx context.Context, clause string, args ...any) ([]model.Provider, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+providerCols+` FROM providers `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询厂商失败: %w", err)
	}
	defer rows.Close()
	var out []model.Provider
	for rows.Next() {
		var (
			p      model.Provider
			enc    []byte
			models string
			on     int
		)
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &enc, &models, &on, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: 解析厂商失败: %w", err)
		}
		p.APIKeyEnc = enc
		p.HasAPIKey = len(enc) > 0
		p.Enabled = on != 0
		if models != "" {
			if err := json.Unmarshal([]byte(models), &p.Models); err != nil {
				return nil, fmt.Errorf("store: 解析模型列表失败: %w", err)
			}
		}
		if p.Models == nil {
			p.Models = []string{}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
