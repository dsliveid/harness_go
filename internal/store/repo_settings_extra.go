package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// All 读取全部配置键值（设置页一次性读取用）。
func (r *SettingsRepo) All(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := r.s.conn().QueryContext(ctx, `SELECT key, value_json FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询配置失败: %w", err)
	}
	defer rows.Close()
	out := make(map[string]json.RawMessage)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: 解析配置失败: %w", err)
		}
		out[k] = json.RawMessage(v)
	}
	return out, rows.Err()
}
