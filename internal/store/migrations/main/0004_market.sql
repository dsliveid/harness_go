-- 主库 0004：外部插件与工具市场（阶段 E / M2-MCP + M7）
-- 依据 docs/MCP接入设计.md §3（配置模型）与 docs/插件体系设计.md §2/§3.5（插件形态）。
--
-- 设计要点：
--   1. mcp_servers：MCP Server 配置真源。env 与 headers 含密钥，
--      以 AES-256-GCM 密文 BLOB 存储（与 providers.api_key 同一 Cipher），
--      API 响应与日志一律只回键名不回值。
--   2. tool_packages：本地工具包（pkg.zip）安装状态真源。一包一行，
--      previous_version + 保留旧版本目录实现回滚。
--   3. 信任分级落库：builtin（编译期，不入库）/ signed（签名校验通过）/
--      unsigned（未签名，默认安装被拒，仅显式 allow_unsigned 时落库）。
--   4. 全部在主库：外部插件配置与索引跨会话共享，避免跨库事务。
-- 铁律：已发布的迁移文件永不修改，只能追加新文件。

-- ============ MCP Server 配置 ============
CREATE TABLE IF NOT EXISTS mcp_servers (
  id                TEXT PRIMARY KEY,                  -- 服务端生成 ID
  name              TEXT    NOT NULL,                  -- 展示名（工具名据此规范化：mcp__<key>__<tool>）
  transport         TEXT    NOT NULL,                  -- stdio | sse | http（Streamable HTTP）
  command           TEXT    NOT NULL DEFAULT '',       -- stdio：可执行文件
  args_json         TEXT    NOT NULL DEFAULT '[]',     -- stdio：参数
  env_enc           BLOB,                              -- stdio：环境变量（密文 JSON map，键名可明文展示）
  cwd               TEXT    NOT NULL DEFAULT '',       -- stdio：工作目录
  url               TEXT    NOT NULL DEFAULT '',       -- sse/http：端点
  headers_enc       BLOB,                              -- sse/http：自定义头（密文 JSON map）
  enabled           INTEGER NOT NULL DEFAULT 1,
  timeout_ms        INTEGER NOT NULL DEFAULT 120000,   -- 单次工具调用超时
  max_output        INTEGER NOT NULL DEFAULT 32768,    -- 注入模型的输出字节上限
  default_risk      TEXT    NOT NULL DEFAULT 'write',  -- 外部工具默认风险级
  allow_tools_json  TEXT    NOT NULL DEFAULT '[]',     -- 工具白名单（空=全部）
  deny_tools_json   TEXT    NOT NULL DEFAULT '[]',     -- 工具黑名单（优先于白名单）
  health_interval_s INTEGER NOT NULL DEFAULT 60,       -- 主动健康间隔（0=关闭）
  created_at        INTEGER NOT NULL DEFAULT 0,
  updated_at        INTEGER NOT NULL DEFAULT 0
);

-- ============ 本地工具包（pkg.zip 安装状态） ============
CREATE TABLE IF NOT EXISTS tool_packages (
  id               TEXT PRIMARY KEY,                   -- 包 ID（manifest.id，命名空间 pkg:<id>）
  version          TEXT    NOT NULL,                   -- 当前启用版本
  previous_version TEXT    NOT NULL DEFAULT '',        -- 升级前版本（回滚指针；目录保留）
  name             TEXT    NOT NULL,
  trust            TEXT    NOT NULL,                   -- signed | unsigned（builtin 不入库）
  namespace        TEXT    NOT NULL,                   -- pkg:<id>
  install_path     TEXT    NOT NULL,                   -- data/packages/<id>/<ver>/（相对 data 目录）
  binary_rel_path  TEXT    NOT NULL,                   -- 当前平台可执行文件相对路径
  manifest_json    TEXT    NOT NULL,                   -- 完整 manifest（含工具声明，唯一真源）
  signature        TEXT    NOT NULL DEFAULT '',        -- ed25519 签名（hex，对 manifest+二进制哈希）
  enabled          INTEGER NOT NULL DEFAULT 1,
  installed_at     INTEGER NOT NULL DEFAULT 0,
  updated_at       INTEGER NOT NULL DEFAULT 0
);
