package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"harness-go/internal/model"
)

// ============ 设备（主库，阶段 C / 多端协同 HALO M5） ============
//
// 铁律：devices / device_pair_codes / capabilities / session_leases 全部在主库，
// 会话库不动业务表 → 零跨库事务（docs/数据存储方案.md v2.0 §4）。

// DeviceRepo 设备仓储（主库）。
type DeviceRepo struct{ s *MainStore }

// Devices 返回设备仓储。
func (s *MainStore) Devices() *DeviceRepo { return &DeviceRepo{s: s} }

const deviceCols = `id, name, kind, platform, status, capabilities_hash, protocol_version,
	app_version, load_json, token_hash, paired_at, last_seen_at, revoked_at, created_at, updated_at`

// Create 新增设备（配对成功后写入；token_hash 必须是 sha256 摘要）。
func (r *DeviceRepo) Create(ctx context.Context, d *model.Device) error {
	if d.ID == "" || d.TokenHash == "" {
		return errors.New("store: 设备缺少必要字段（id / token_hash）")
	}
	if d.Kind == "" {
		d.Kind = model.DeviceKindDesktop
	}
	if d.Status == "" {
		d.Status = model.DeviceOffline
	}
	if d.ProtocolVersion == 0 {
		d.ProtocolVersion = model.DefaultDeviceProtocolVersion
	}
	now := NowMillis()
	if d.PairedAt == 0 {
		d.PairedAt = now
	}
	d.CreatedAt, d.UpdatedAt = now, now
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO devices(`+deviceCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.Name, string(d.Kind), d.Platform, string(d.Status), d.CapabilitiesHash,
		d.ProtocolVersion, d.AppVersion, rawOrNil(d.LoadJSON), d.TokenHash,
		d.PairedAt, d.LastSeenAt, d.RevokedAt, d.CreatedAt, d.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: 新增设备失败: %w", err)
	}
	return nil
}

// Get 按 ID 取设备（不存在返回 ErrNotFound）。
func (r *DeviceRepo) Get(ctx context.Context, id string) (*model.Device, error) {
	list, err := r.query(ctx, `WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// GetByTokenHash 按令牌摘要取设备（WS 升级前鉴权用；已吊销不返回）。
func (r *DeviceRepo) GetByTokenHash(ctx context.Context, tokenHash string) (*model.Device, error) {
	list, err := r.query(ctx, `WHERE token_hash=? AND revoked_at IS NULL`, tokenHash)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// List 列出全部设备（含已吊销，按配对时间倒序）。
func (r *DeviceRepo) List(ctx context.Context) ([]model.Device, error) {
	return r.query(ctx, `ORDER BY paired_at DESC`)
}

// Count 设备总数（维护/统计用）。
func (r *DeviceRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := r.s.conn().QueryRowContext(ctx, `SELECT COUNT(*) FROM devices`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计设备失败: %w", err)
	}
	return n, nil
}

// MarkOnline 标记设备在线并刷新心跳。
func (r *DeviceRepo) MarkOnline(ctx context.Context, id string, at int64) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE devices SET status=?, last_seen_at=?, updated_at=?
		 WHERE id=? AND revoked_at IS NULL`,
		string(model.DeviceOnline), at, at, id)
	if err != nil {
		return fmt.Errorf("store: 标记设备在线失败: %w", err)
	}
	return ensureAffected(res, "设备 "+id)
}

// MarkOffline 标记设备离线（断线/心跳超时）。
func (r *DeviceRepo) MarkOffline(ctx context.Context, id string, at int64) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE devices SET status=?, last_seen_at=?, updated_at=?
		 WHERE id=? AND revoked_at IS NULL`,
		string(model.DeviceOffline), at, at, id)
	if err != nil {
		return fmt.Errorf("store: 标记设备离线失败: %w", err)
	}
	return ensureAffected(res, "设备 "+id)
}

// UpdateHeartbeat 刷新心跳与负载快照（ping 帧携带 load 时写）。
func (r *DeviceRepo) UpdateHeartbeat(ctx context.Context, id string, at int64,
	load json.RawMessage, capabilitiesHash string, protocolVersion int) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE devices
		    SET status=?, last_seen_at=?, updated_at=?,
		        load_json=COALESCE(?, load_json),
		        capabilities_hash=CASE WHEN ? = '' THEN capabilities_hash ELSE ? END,
		        protocol_version=CASE WHEN ? = 0 THEN protocol_version ELSE ? END
		  WHERE id=? AND revoked_at IS NULL`,
		string(model.DeviceOnline), at, at,
		rawOrNil(load), capabilitiesHash, capabilitiesHash,
		protocolVersion, protocolVersion, id)
	if err != nil {
		return fmt.Errorf("store: 刷新设备心跳失败: %w", err)
	}
	return ensureAffected(res, "设备 "+id)
}

// Revoke 吊销设备（终态；同时清空 token_hash 使令牌立即失效）。
func (r *DeviceRepo) Revoke(ctx context.Context, id string, at int64) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE devices SET status=?, revoked_at=?, token_hash=?, updated_at=?
		 WHERE id=? AND revoked_at IS NULL`,
		string(model.DeviceRevoked), at, "revoked:"+id, at, id)
	if err != nil {
		return fmt.Errorf("store: 吊销设备失败: %w", err)
	}
	return ensureAffected(res, "设备 "+id)
}

// Delete 物理删除设备（连带删除其能力项，同库事务）。
func (r *DeviceRepo) Delete(ctx context.Context, id string) error {
	return r.s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM capabilities WHERE device_id=?`, id); err != nil {
			return fmt.Errorf("store: 删除设备能力失败: %w", err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE id=?`, id)
		if err != nil {
			return fmt.Errorf("store: 删除设备失败: %w", err)
		}
		return ensureAffected(res, "设备 "+id)
	})
}

func (r *DeviceRepo) query(ctx context.Context, clause string, args ...any) ([]model.Device, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+deviceCols+` FROM devices `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询设备失败: %w", err)
	}
	defer rows.Close()
	var out []model.Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDevice(sc interface{ Scan(...any) error }) (model.Device, error) {
	var (
		d     model.Device
		kind  string
		state string
		load  []byte
	)
	if err := sc.Scan(&d.ID, &d.Name, &kind, &d.Platform, &state, &d.CapabilitiesHash,
		&d.ProtocolVersion, &d.AppVersion, &load, &d.TokenHash, &d.PairedAt,
		&d.LastSeenAt, &d.RevokedAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return d, fmt.Errorf("store: 解析设备失败: %w", err)
	}
	d.Kind = model.DeviceKind(kind)
	d.Status = model.DeviceStatus(state)
	if len(load) > 0 {
		d.LoadJSON = json.RawMessage(load)
	}
	return d, nil
}

// ============ 一次性配对码 ============

// PairCodeRepo 配对码仓储（主库）。
//
// 一次性消费由单条 UPDATE 的 WHERE 条件保证原子性（consumed_at IS NULL AND expires_at > now），
// 不依赖"先查后改"，多连接并发下也不会出现双消费。
type PairCodeRepo struct{ s *MainStore }

// PairCodes 返回配对码仓储。
func (s *MainStore) PairCodes() *PairCodeRepo { return &PairCodeRepo{s: s} }

// Create 生成一条配对码（code 冲突由调用方重试）。
func (r *PairCodeRepo) Create(ctx context.Context, pc *model.DevicePairCode) error {
	if pc.Code == "" || pc.ExpiresAt == 0 {
		return errors.New("store: 配对码缺少必要字段（code / expires_at）")
	}
	now := NowMillis()
	if pc.CreatedAt == 0 {
		pc.CreatedAt = now
	}
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO device_pair_codes(code, device_name, device_kind, expires_at, consumed_at, created_at)
		 VALUES(?,?,?,?,?,?)`,
		pc.Code, pc.DeviceName, string(pc.DeviceKind), pc.ExpiresAt, pc.ConsumedAt, pc.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: 生成配对码失败: %w", err)
	}
	return nil
}

// Get 取配对码（不存在返回 ErrNotFound；消费判定交给 Consume 原子完成）。
func (r *PairCodeRepo) Get(ctx context.Context, code string) (*model.DevicePairCode, error) {
	var (
		pc   model.DevicePairCode
		kind string
	)
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT code, device_name, device_kind, expires_at, consumed_at, created_at
		   FROM device_pair_codes WHERE code=?`, code).
		Scan(&pc.Code, &pc.DeviceName, &kind, &pc.ExpiresAt, &pc.ConsumedAt, &pc.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询配对码失败: %w", err)
	}
	pc.DeviceKind = model.DeviceKind(kind)
	return &pc, nil
}

// Consume 原子消费配对码：成功返回 true。
//
// 单条 UPDATE 携带全部前置条件（存在 / 未消费 / 未过期），命中行数即结论：
//   - 返回 (false, nil) = 码不存在 / 已消费 / 已过期（上层统一返回同一错误，防枚举）
//   - 返回 (false, err) = 真正的基础设施错误
func (r *PairCodeRepo) Consume(ctx context.Context, code string, now int64) (bool, error) {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE device_pair_codes SET consumed_at=?
		  WHERE code=? AND consumed_at IS NULL AND expires_at > ?`,
		now, code, now)
	if err != nil {
		return false, fmt.Errorf("store: 消费配对码失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	return n == 1, nil
}

// PurgeExpired 清理已过期配对码，返回删除行数（可由维护任务周期调用）。
func (r *PairCodeRepo) PurgeExpired(ctx context.Context, now int64) (int64, error) {
	res, err := r.s.conn().ExecContext(ctx,
		`DELETE FROM device_pair_codes WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, fmt.Errorf("store: 清理过期配对码失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	return n, nil
}

// ============ 设备能力 ============

// CapabilityRepo 设备能力仓储（主库）。按设备维度全量替换。
type CapabilityRepo struct{ s *MainStore }

// Capabilities 返回能力仓储。
func (s *MainStore) Capabilities() *CapabilityRepo { return &CapabilityRepo{s: s} }

// Replace 用给定集合全量替换某设备的能力项（同库事务，先删后插）。
func (r *CapabilityRepo) Replace(ctx context.Context, deviceID string, caps []model.Capability) error {
	if deviceID == "" {
		return errors.New("store: 能力替换缺少 device_id")
	}
	now := NowMillis()
	return r.s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM capabilities WHERE device_id=?`, deviceID); err != nil {
			return fmt.Errorf("store: 清理设备能力失败: %w", err)
		}
		for _, c := range caps {
			if c.Name == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO capabilities(device_id, name, version, updated_at) VALUES(?,?,?,?)`,
				deviceID, c.Name, c.Version, now); err != nil {
				return fmt.Errorf("store: 写入设备能力失败: %w", err)
			}
		}
		return nil
	})
}

// ListByDevice 列出某设备的能力项（按名称升序）。
func (r *CapabilityRepo) ListByDevice(ctx context.Context, deviceID string) ([]model.Capability, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT device_id, name, version, updated_at FROM capabilities
		  WHERE device_id=? ORDER BY name`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询设备能力失败: %w", err)
	}
	defer rows.Close()
	var out []model.Capability
	for rows.Next() {
		var c model.Capability
		if err := rows.Scan(&c.DeviceID, &c.Name, &c.Version, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: 解析设备能力失败: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ============ 会话租约 ============

// LeaseRepo 会话租约仓储（主库）。
//
// 语义：session_id 唯一；每次 Acquire 若写者换人或已过期则 epoch+1，
// 一致持有者只续期不涨 epoch。抢占判定放在单条 UPSERT 中完成，避免"先查后改"竞态。
type LeaseRepo struct{ s *MainStore }

// Leases 返回租约仓储。
func (s *MainStore) Leases() *LeaseRepo { return &LeaseRepo{s: s} }

// Acquire 抢占 / 续期租约，返回生效后的租约与"是否发生写者变更（需通知旧端 revoked）"。
//
// epoch 语义：新持有者或过期接管 → epoch = 旧 epoch + 1；同一持有者未过期 → epoch 不变。
func (r *LeaseRepo) Acquire(ctx context.Context, sessionID, deviceID string,
	now, ttlMillis int64) (model.SessionLease, bool, error) {
	if sessionID == "" {
		return model.SessionLease{}, false, errors.New("store: 租约缺少 session_id")
	}
	if ttlMillis <= 0 {
		return model.SessionLease{}, false, errors.New("store: 租约 ttl 必须为正")
	}
	expires := now + ttlMillis

	var (
		lease   model.SessionLease
		changed bool
	)
	err := r.s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			curDevice  string
			curEpoch   int64
			curExpires int64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT device_id, epoch, expires_at FROM session_leases WHERE session_id=?`,
			sessionID).Scan(&curDevice, &curEpoch, &curExpires)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			lease = model.SessionLease{
				SessionID: sessionID, DeviceID: deviceID, Epoch: 1,
				ExpiresAt: expires, HeartbeatAt: now, CreatedAt: now,
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO session_leases(session_id, device_id, epoch, expires_at, heartbeat_at, created_at)
				 VALUES(?,?,?,?,?,?)`,
				sessionID, deviceID, lease.Epoch, lease.ExpiresAt, now, now); err != nil {
				return fmt.Errorf("store: 建立会话租约失败: %w", err)
			}
			changed = deviceID != ""
			return nil
		case err != nil:
			return fmt.Errorf("store: 查询会话租约失败: %w", err)
		}

		epoch := curEpoch
		// 同一持有者且未过期 → 纯续期，不涨 epoch
		if curDevice != deviceID || curExpires <= now {
			epoch = curEpoch + 1
			changed = curDevice != deviceID
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE session_leases SET device_id=?, epoch=?, expires_at=?, heartbeat_at=?
			  WHERE session_id=?`,
			deviceID, epoch, expires, now, sessionID); err != nil {
			return fmt.Errorf("store: 更新会话租约失败: %w", err)
		}
		lease = model.SessionLease{
			SessionID: sessionID, DeviceID: deviceID, Epoch: epoch,
			ExpiresAt: expires, HeartbeatAt: now, CreatedAt: now,
		}
		return nil
	})
	if err != nil {
		return model.SessionLease{}, false, err
	}
	return lease, changed, nil
}

// Get 查询某会话当前租约（不存在返回 ErrNotFound）。
func (r *LeaseRepo) Get(ctx context.Context, sessionID string) (*model.SessionLease, error) {
	var l model.SessionLease
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT session_id, device_id, epoch, expires_at, heartbeat_at, created_at
		   FROM session_leases WHERE session_id=?`, sessionID).
		Scan(&l.SessionID, &l.DeviceID, &l.Epoch, &l.ExpiresAt, &l.HeartbeatAt, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询会话租约失败: %w", err)
	}
	return &l, nil
}

// Release 释放租约（仅持有者本人可释放；返回是否真的释放了）。
func (r *LeaseRepo) Release(ctx context.Context, sessionID, deviceID string) (bool, error) {
	res, err := r.s.conn().ExecContext(ctx,
		`DELETE FROM session_leases WHERE session_id=? AND device_id=?`, sessionID, deviceID)
	if err != nil {
		return false, fmt.Errorf("store: 释放会话租约失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	return n == 1, nil
}

// DeleteExpired 清理已过期租约，返回删除行数。
func (r *LeaseRepo) DeleteExpired(ctx context.Context, now int64) (int64, error) {
	res, err := r.s.conn().ExecContext(ctx,
		`DELETE FROM session_leases WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, fmt.Errorf("store: 清理过期租约失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	return n, nil
}
