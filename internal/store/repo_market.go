package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"harness-go/internal/secret"
)

// ============ MCP Server 配置仓储（主库，阶段 E / MCP接入设计 §3） ============
//
// env 与 headers 以 AES-256-GCM 密文落库（与 providers.api_key 同一 Cipher）；
// 仓储层只管「密文进出」，加解密封装为 EncryptedMap 值对象，
// 保证「明文 map ⇄ 密文 BLOB」的转换只有一条路径，杜绝漏加密。

// EncryptedMap 键明文 + 值密文的 JSON map（env / headers 的存储形态）。
//
// 为什么要「键明文」：API 脱敏口径是「只回键名不回值」（MCP接入设计 §3.2），
// 整体加密会让键名也无法列出；逐值加密让键名可枚举、值永不落明文。
type EncryptedMap struct {
	enc map[string][]byte // nil/空 = 未设置
}

// NewEncryptedMap 把明文 map 逐值加密为存储形态；nil/空 map 返回未设置态。
func NewEncryptedMap(c *secret.Cipher, m map[string]string) (EncryptedMap, error) {
	if len(m) == 0 {
		return EncryptedMap{}, nil
	}
	if c == nil {
		return EncryptedMap{}, errors.New("store: 缺少加密器，拒绝明文落库")
	}
	enc := make(map[string][]byte, len(m))
	for k, v := range m {
		b, err := c.Encrypt([]byte(v))
		if err != nil {
			return EncryptedMap{}, fmt.Errorf("store: 加密 %s 失败: %w", k, err)
		}
		enc[k] = b
	}
	return EncryptedMap{enc: enc}, nil
}

// Plain 解密为明文 map（未设置时返回 nil）。
func (e EncryptedMap) Plain(c *secret.Cipher) (map[string]string, error) {
	if len(e.enc) == 0 {
		return nil, nil
	}
	if c == nil {
		return nil, errors.New("store: 缺少加密器，无法解密")
	}
	out := make(map[string]string, len(e.enc))
	for k, blob := range e.enc {
		raw, err := c.Decrypt(blob)
		if err != nil {
			return nil, fmt.Errorf("store: 解密 %s 失败: %w", k, err)
		}
		out[k] = string(raw)
	}
	return out, nil
}

// Keys 返回键名列表（API 脱敏展示用，永不返回值）。
func (e EncryptedMap) Keys() []string {
	out := make([]string, 0, len(e.enc))
	for k := range e.enc {
		out = append(out, k)
	}
	return out
}

// MarshalJSON 存储序列化（BLOB 列内容：{"KEY": "<base64 密文>"}）。
func (e EncryptedMap) MarshalJSON() ([]byte, error) { return json.Marshal(e.enc) }

// UnmarshalJSON 存储反序列化。
func (e *EncryptedMap) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	return json.Unmarshal(b, &e.enc)
}

// MCPServerRow mcp_servers 行（密文字段以 EncryptedMap 承载）。
type MCPServerRow struct {
	ID              string
	Name            string
	Transport       string // stdio | sse | http
	Command         string
	Args            []string
	Env             EncryptedMap
	Cwd             string
	URL             string
	Headers         EncryptedMap
	Enabled         bool
	TimeoutMS       int
	MaxOutput       int
	DefaultRisk     string
	AllowTools      []string
	DenyTools       []string
	HealthIntervalS int
	CreatedAt       int64
	UpdatedAt       int64
}

// MCPServersRepo mcp_servers 仓储。
type MCPServersRepo struct{ s *MainStore }

// MCPServers 返回 MCP Server 配置仓储。
func (s *MainStore) MCPServers() *MCPServersRepo { return &MCPServersRepo{s: s} }

const mcpServerCols = `id, name, transport, command, args_json, env_enc, cwd, url, headers_enc,
  enabled, timeout_ms, max_output, default_risk, allow_tools_json, deny_tools_json,
  health_interval_s, created_at, updated_at`

func scanMCPServer(sc interface{ Scan(...any) error }) (MCPServerRow, error) {
	var (
		r         MCPServerRow
		argsJSON  string
		env       []byte
		headers   []byte
		allowJSON string
		denyJSON  string
		enabled   int
	)
	err := sc.Scan(&r.ID, &r.Name, &r.Transport, &r.Command, &argsJSON, &env, &r.Cwd, &r.URL, &headers,
		&enabled, &r.TimeoutMS, &r.MaxOutput, &r.DefaultRisk, &allowJSON, &denyJSON,
		&r.HealthIntervalS, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, err
	}
	r.Enabled = enabled == 1
	_ = json.Unmarshal(env, &r.Env)
	_ = json.Unmarshal(headers, &r.Headers)
	_ = json.Unmarshal([]byte(argsJSON), &r.Args)
	_ = json.Unmarshal([]byte(allowJSON), &r.AllowTools)
	_ = json.Unmarshal([]byte(denyJSON), &r.DenyTools)
	return r, nil
}

// UpsertMCPServer 幂等写入（存在即全量更新，保留 created_at）。
func (r *MCPServersRepo) Upsert(ctx context.Context, row *MCPServerRow) error {
	now := NowMillis()
	if row.CreatedAt == 0 {
		row.CreatedAt = now
	}
	row.UpdatedAt = now
	argsJSON, _ := json.Marshal(row.Args)
	allowJSON, _ := json.Marshal(row.AllowTools)
	denyJSON, _ := json.Marshal(row.DenyTools)
	envJSON, _ := json.Marshal(row.Env)
	headersJSON, _ := json.Marshal(row.Headers)
	_, err := r.s.conn().ExecContext(ctx, `INSERT INTO mcp_servers
	    (id, name, transport, command, args_json, env_enc, cwd, url, headers_enc,
	     enabled, timeout_ms, max_output, default_risk, allow_tools_json, deny_tools_json,
	     health_interval_s, created_at, updated_at)
	  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	  ON CONFLICT(id) DO UPDATE SET
	    name=excluded.name, transport=excluded.transport, command=excluded.command,
	    args_json=excluded.args_json, env_enc=excluded.env_enc, cwd=excluded.cwd,
	    url=excluded.url, headers_enc=excluded.headers_enc, enabled=excluded.enabled,
	    timeout_ms=excluded.timeout_ms, max_output=excluded.max_output,
	    default_risk=excluded.default_risk, allow_tools_json=excluded.allow_tools_json,
	    deny_tools_json=excluded.deny_tools_json, health_interval_s=excluded.health_interval_s,
	    updated_at=excluded.updated_at`,
		row.ID, row.Name, row.Transport, row.Command, string(argsJSON), envJSON, row.Cwd, row.URL, headersJSON,
		boolInt(row.Enabled), row.TimeoutMS, row.MaxOutput, row.DefaultRisk, string(allowJSON), string(denyJSON),
		row.HealthIntervalS, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: 写入 MCP Server 失败: %w", err)
	}
	return nil
}

// GetMCPServer 按 ID 读取；不存在返回 (nil, nil)。
func (r *MCPServersRepo) Get(ctx context.Context, id string) (*MCPServerRow, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT `+mcpServerCols+` FROM mcp_servers WHERE id = ?`, id)
	srv, err := scanMCPServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取 MCP Server 失败: %w", err)
	}
	return &srv, nil
}

// ListMCPServers 全量列表（数量小，无分页）。
func (r *MCPServersRepo) List(ctx context.Context) ([]MCPServerRow, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+mcpServerCols+` FROM mcp_servers ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 列出 MCP Server 失败: %w", err)
	}
	defer rows.Close()
	var out []MCPServerRow
	for rows.Next() {
		srv, serr := scanMCPServer(rows)
		if serr != nil {
			return nil, fmt.Errorf("store: 扫描 MCP Server 失败: %w", serr)
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// DeleteMCPServer 删除配置。
func (r *MCPServersRepo) Delete(ctx context.Context, id string) error {
	if _, err := r.s.conn().ExecContext(ctx, `DELETE FROM mcp_servers WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: 删除 MCP Server 失败: %w", err)
	}
	return nil
}

// ============ 本地工具包仓储（tool_packages） ============

// PackageRow tool_packages 行。
type PackageRow struct {
	ID              string // manifest.id
	Version         string // 当前启用版本
	PreviousVersion string // 回滚指针（空 = 无旧版本）
	Name            string
	Trust           string // signed | unsigned
	Namespace       string // pkg:<id>
	InstallPath     string // data/packages/<id>/<ver>/（相对 data 目录）
	BinaryRelPath   string
	ManifestJSON    string
	Signature       string
	Enabled         bool
	InstalledAt     int64
	UpdatedAt       int64
}

// PackagesRepo tool_packages 仓储。
type PackagesRepo struct{ s *MainStore }

// Packages 返回工具包仓储。
func (s *MainStore) Packages() *PackagesRepo { return &PackagesRepo{s: s} }

const packageCols = `id, version, previous_version, name, trust, namespace, install_path,
  binary_rel_path, manifest_json, signature, enabled, installed_at, updated_at`

func scanPackage(sc interface{ Scan(...any) error }) (PackageRow, error) {
	var (
		p       PackageRow
		enabled int
	)
	err := sc.Scan(&p.ID, &p.Version, &p.PreviousVersion, &p.Name, &p.Trust, &p.Namespace,
		&p.InstallPath, &p.BinaryRelPath, &p.ManifestJSON, &p.Signature, &enabled, &p.InstalledAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.Enabled = enabled == 1
	return p, nil
}

// UpsertPackage 写入/更新包（保留 installed_at）。
func (r *PackagesRepo) Upsert(ctx context.Context, p *PackageRow) error {
	now := NowMillis()
	if p.InstalledAt == 0 {
		p.InstalledAt = now
	}
	p.UpdatedAt = now
	_, err := r.s.conn().ExecContext(ctx, `INSERT INTO tool_packages
	    (id, version, previous_version, name, trust, namespace, install_path,
	     binary_rel_path, manifest_json, signature, enabled, installed_at, updated_at)
	  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	  ON CONFLICT(id) DO UPDATE SET
	    version=excluded.version, previous_version=excluded.previous_version,
	    name=excluded.name, trust=excluded.trust, namespace=excluded.namespace,
	    install_path=excluded.install_path, binary_rel_path=excluded.binary_rel_path,
	    manifest_json=excluded.manifest_json, signature=excluded.signature,
	    enabled=excluded.enabled, updated_at=excluded.updated_at`,
		p.ID, p.Version, p.PreviousVersion, p.Name, p.Trust, p.Namespace,
		p.InstallPath, p.BinaryRelPath, p.ManifestJSON, p.Signature, boolInt(p.Enabled),
		p.InstalledAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: 写入工具包失败: %w", err)
	}
	return nil
}

// GetPackage 按 ID 读取；不存在返回 (nil, nil)。
func (r *PackagesRepo) Get(ctx context.Context, id string) (*PackageRow, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT `+packageCols+` FROM tool_packages WHERE id = ?`, id)
	p, err := scanPackage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取工具包失败: %w", err)
	}
	return &p, nil
}

// ListPackages 全量列表。
func (r *PackagesRepo) List(ctx context.Context) ([]PackageRow, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+packageCols+` FROM tool_packages ORDER BY installed_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 列出工具包失败: %w", err)
	}
	defer rows.Close()
	var out []PackageRow
	for rows.Next() {
		p, serr := scanPackage(rows)
		if serr != nil {
			return nil, fmt.Errorf("store: 扫描工具包失败: %w", serr)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePackage 删除行。
func (r *PackagesRepo) Delete(ctx context.Context, id string) error {
	if _, err := r.s.conn().ExecContext(ctx, `DELETE FROM tool_packages WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: 删除工具包失败: %w", err)
	}
	return nil
}

// SetPackageEnabled 只翻转启用位（启停不重写其他字段）。
func (r *PackagesRepo) SetEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE tool_packages SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolInt(enabled), NowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新工具包启用位失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: 工具包 %s 不存在", id)
	}
	return nil
}
