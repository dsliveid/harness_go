/**
 * harness-go API 类型与路径常量 —— **由 `api/openapi.yaml` 人工对齐生成**。
 *
 * 契约铁律（docs/protocols.md §3）：
 *   1. `api/openapi.yaml` 是唯一物理边界，本文件必须与之逐字段一致；
 *   2. 字段变更顺序固定：改 yaml → 改本文件 → 双端同步，禁止口头约定字段；
 *   3. 阶段 A 冻结后只允许**向后兼容的新增字段**（可选字段 + 默认值）。
 *
 * 时间戳一律 Unix 毫秒（number）。
 */

/* ============ 基础 ============ */

export type RunStatus =
  | 'pending'
  | 'running'
  | 'waiting_approval'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'interrupted';

export type ToolEventStatus =
  | 'pending'
  | 'approved'
  | 'denied'
  | 'running'
  | 'ok'
  | 'error'
  | 'timeout';

export type Risk = 'readonly' | 'write' | 'command' | 'danger';
export type Executor = 'server' | 'desktop' | 'mobile' | 'browser' | 'any';
export type ToolTier = 'core' | 'deferred' | 'on_demand';
export type MessageRole = 'system' | 'user' | 'assistant' | 'tool';
export type PluginKind = 'builtin' | 'mcp' | 'http' | 'package' | 'wasm';
export type HealthStatus = 'available' | 'degraded' | 'unavailable';

export interface ApiErrorBody {
  error: {
    code: string;
    message: string;
    trace_id?: string;
    details?: Record<string, unknown>;
  };
}

export interface Page<T> {
  items: T[];
  limit?: number;
  offset?: number;
}

/* ============ 认证 ============ */

export interface User {
  id: string;
  username: string;
  display_name: string;
  role: string;
  created_at: number;
  updated_at: number;
}

export interface LoginRequest {
  username: string;
  password: string;
}

export interface LoginResponse {
  user: User;
  token: string;
  expires_at?: number;
}

export interface SSORequest {
  user_name: string;
  pwd?: string;
  ts?: number;
  sign?: string;
}

export interface ChangePasswordRequest {
  old_password: string;
  new_password: string;
}

/* ============ 会话 / 消息 / Run ============ */

export interface Session {
  id: string;
  title: string;
  workspace_path: string;
  access_mode: 'readonly' | 'confirm' | 'full';
  model_ref: string;
  user_id: string;
  message_count: number;
  last_message_at?: number;
  total_tokens: number;
  last_run_status?: RunStatus;
  created_at: number;
  updated_at: number;
}

export interface CreateSessionRequest {
  title?: string;
  model_ref?: string;
  workspace_path?: string;
}

export interface Message {
  id: string;
  seq: number;
  run_id?: string;
  role: MessageRole;
  content: string;
  reasoning?: string;
  tool_calls?: Array<Record<string, unknown>>;
  tool_call_id?: string;
  attachments?: Array<Record<string, unknown>>;
  token_count: number;
  usage?: Record<string, unknown>;
  created_at: number;
}

export interface Run {
  id: string;
  trigger_type: string;
  status: RunStatus;
  stop_reason?: string;
  model_ref: string;
  executor: Executor;
  device_id?: string;
  parent_run_id?: string;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  is_estimated: boolean;
  error_code?: string;
  error_message?: string;
  started_at?: number;
  ended_at?: number;
  created_at: number;
}

export interface CreateRunRequest {
  input: string;
  model_ref?: string;
  /** C-6：执行端设备（可选）。非空 = 该设备成为会话租约持有者（抢占旧端 epoch+1） */
  device_id?: string;
}

export interface CreateRunResponse {
  run_id: string;
  status: RunStatus;
}

export interface ToolEvent {
  id: string;
  run_id: string;
  message_id?: string;
  tool_name: string;
  namespace: string;
  args?: Record<string, unknown>;
  /** 标准化结果：{ content, meta?, cost_tokens? } */
  result?: Record<string, unknown>;
  result_blob?: string;
  status: ToolEventStatus;
  risk: Risk;
  executor: Executor;
  device_id?: string;
  source_trust: 'trusted' | 'untrusted';
  approved_by?: string;
  error?: string;
  started_at?: number;
  ended_at?: number;
}

/* ============ 工具契约（A-10） ============ */

export interface ToolHealth {
  status: HealthStatus;
  message?: string;
  checked_at: string;
}

export interface ToolSummary {
  name: string;
  namespace: string;
  tier: ToolTier;
  default_tier: ToolTier;
  cost_tokens: number;
  risk: Risk;
  executor: Executor;
  plugin_id: string;
  health: ToolHealth;
}

export interface ToolPluginInfo {
  id: string;
  name: string;
  version: string;
  kind: PluginKind;
  default_tier: ToolTier;
  tools: string[];
}

export interface ToolSnapshot {
  total: number;
  core: ToolSummary[];
  deferred: ToolSummary[];
  on_demand: ToolSummary[];
  cost_total: number;
  cost_core: number;
  cost_deferred: number;
  limits: { core_soft: number; deferred_soft: number };
  warnings: string[];
  plugins: ToolPluginInfo[];
  overrides: Record<string, ToolTier>;
  generated_at: string;
}

export interface ToolIndexEntry {
  name: string;
  description: string;
  cost_tokens: number;
}

export interface ToolInjection {
  core: Array<Record<string, unknown>>;
  deferred_index: ToolIndexEntry[];
  on_demand: Array<Record<string, unknown>>;
  deferred_hidden?: string[];
  cost_core: number;
  cost_index: number;
  cost_total: number;
  limits: { core_soft: number; deferred_soft: number };
  warnings?: string[];
}

export interface ToolTierPatch {
  tiers: Record<string, ToolTier>;
}

/* ============ SSE 事件（docs/protocols.md §2/§4） ============ */

export type RunEventType =
  | 'run.status'
  | 'message.start'
  | 'message.delta'
  | 'message.end'
  | 'tool.started'
  | 'tool.progress'
  | 'tool.result'
  | 'approval.request'
  | 'approval.decision'
  | 'resync'
  | 'error';

export interface RunEvent<T = Record<string, unknown>> {
  v: number;
  seq: number;
  type: RunEventType;
  run_id: string;
  at: number;
  payload: T;
}

/* ============ 路径常量（与 openapi.yaml 的 paths 一一对应） ============ */

export const API_PATHS = {
  healthz: '/healthz',
  root: '/',
  authLogin: '/api/v1/auth/login',
  authSSO: '/api/v1/auth/sso',
  authMe: '/api/v1/auth/me',
  authLogout: '/api/v1/auth/logout',
  authPassword: '/api/v1/auth/password',
  sessions: '/api/v1/sessions',
  session: (sid: string) => `/api/v1/sessions/${sid}`,
  messages: (sid: string) => `/api/v1/sessions/${sid}/messages`,
  runs: (sid: string) => `/api/v1/sessions/${sid}/runs`,
  run: (sid: string, rid: string) => `/api/v1/sessions/${sid}/runs/${rid}`,
  runStop: (sid: string, rid: string) => `/api/v1/sessions/${sid}/runs/${rid}/stop`,
  runEvents: (sid: string, rid: string) => `/api/v1/sessions/${sid}/runs/${rid}/events`,
  runToolEvents: (sid: string, rid: string) =>
    `/api/v1/sessions/${sid}/runs/${rid}/tool-events`,
  /** B-3：审批决策（aid = 等待中的审批 ID = tool_events.id） */
  approvalDecide: (sid: string, rid: string, aid: string) =>
    `/api/v1/sessions/${sid}/runs/${rid}/approvals/${aid}/decide`,
  tools: '/api/v1/tools',
  toolsInjection: '/api/v1/tools/injection',
  toolsTiers: '/api/v1/tools/tiers',
  maintenance: '/api/v1/settings/maintenance',
  /* B-4：厂商 / 配置 / 统计 / 日志 */
  providers: '/api/v1/providers',
  provider: (pid: string) => `/api/v1/providers/${pid}`,
  providerTest: (pid: string) => `/api/v1/providers/${pid}/test`,
  settings: '/api/v1/settings',
  stats: '/api/v1/stats',
  logs: '/api/v1/logs',
  /* 阶段 C：设备（配对 / 列表 / 长连接 / 吊销） */
  devices: '/api/v1/devices',
  device: (did: string) => `/api/v1/devices/${did}`,
  devicePairCodes: '/api/v1/devices/pair-codes',
  devicePair: '/api/v1/devices/pair',
  /** C-3：端侧出站 WS（鉴权走 query `?token=`，浏览器端不持有设备令牌） */
  deviceWS: '/api/v1/devices/ws',
  /** C-6：会话租约（跨端写者归属） */
  sessionLease: (sid: string) => `/api/v1/sessions/${sid}/lease`,
  /* P1：Webhook 事件订阅 */
  webhooks: '/api/v1/webhooks',
  webhook: (id: string) => `/api/v1/webhooks/${id}`,
  webhookEnable: (id: string) => `/api/v1/webhooks/${id}/enable`,
  webhookDisable: (id: string) => `/api/v1/webhooks/${id}/disable`,
  webhookTest: (id: string) => `/api/v1/webhooks/${id}/test`,
  /* P1：OpenAI 兼容端点 */
  openAIModels: '/v1/models',
  openAIChat: '/v1/chat/completions',
  /* 阶段 G：快照回滚（S10 消息/轮次/工具级 revert & reapply） */
  rollback: (sid: string) => `/api/v1/sessions/${sid}/rollback`,
  rollbacks: (sid: string) => `/api/v1/sessions/${sid}/rollbacks`,
  rollbackReapply: (sid: string, rid: string) =>
    `/api/v1/sessions/${sid}/rollbacks/${rid}/reapply`,
  sessionSnapshots: (sid: string) => `/api/v1/sessions/${sid}/snapshots`,
  /* 阶段 E：外部插件与工具市场 */
  mcpServers: '/api/v1/mcp/servers',
  mcpServer: (id: string) => `/api/v1/mcp/servers/${id}`,
  mcpTest: '/api/v1/mcp/test',
  mcpReload: (id: string) => `/api/v1/mcp/servers/${id}/reload`,
  mcpEnable: (id: string) => `/api/v1/mcp/servers/${id}/enable`,
  mcpDisable: (id: string) => `/api/v1/mcp/servers/${id}/disable`,
  mcpHealth: (id: string) => `/api/v1/mcp/servers/${id}/health`,
  packages: '/api/v1/packages',
  packagesInstall: '/api/v1/packages/install',
  package: (id: string) => `/api/v1/packages/${id}`,
  packageRollback: (id: string) => `/api/v1/packages/${id}/rollback`,
  packageEnable: (id: string) => `/api/v1/packages/${id}/enable`,
  packageDisable: (id: string) => `/api/v1/packages/${id}/disable`,
} as const;

/** 免鉴权路径（与 internal/api/router.go 的 publicPaths 保持一致）。 */
export const PUBLIC_PATHS = [
  API_PATHS.authLogin,
  API_PATHS.authSSO,
  // 端侧配对：凭码即凭据（一次性 + TTL 120s + 失败限流）
  API_PATHS.devicePair,
] as const;

/* ============ B-3 / B-4 新增 DTO（与 openapi.yaml schemas 对应） ============ */

/** 审批决策（aid = 等待中的审批 ID = tool_events.id）。 */
export type ApprovalDecision = 'allow_once' | 'allow_session' | 'deny';

export interface ApprovalDecisionRequest {
  decision: ApprovalDecision;
  /** 拒绝原因（会作为工具结果回传模型） */
  reason?: string;
  /** C-7：决策端设备 ID。端侧 Runner 决策时填自身 device_id；浏览器端留空 = 服务端本地 */
  device_id?: string;
}

export interface ApprovalDecisionResponse {
  approval_id: string;
  decision: ApprovalDecision;
  status: 'accepted';
}

/** 模型厂商（api_key 出参恒为空，has_api_key 表示是否已配置密钥）。 */
export interface ProviderDTO {
  id: string;
  name: string;
  base_url: string;
  api_key?: string;
  has_api_key: boolean;
  models: string[];
  enabled: boolean;
  created_at: number;
  updated_at: number;
}

export interface ProviderInput {
  name?: string;
  base_url?: string;
  /** 留空表示保留原密钥（更新时） */
  api_key?: string;
  models?: string[];
  enabled?: boolean;
}

export interface ProviderTestResult {
  ok: boolean;
  latency_ms: number;
  model: string;
  code?: string;
  error?: string;
}

/** 通用配置：键到任意 JSON 片段（白名单键见后端 service.settingsWhitelist）。 */
export interface SettingsResponse {
  items: Record<string, unknown>;
}

/** 日志尾行。 */
export interface LogLine {
  level: string;
  text: string;
}

/** 日志尾行响应（GET /api/v1/logs?limit=&level=）。 */
export interface LogsResponse {
  items: LogLine[];
  limit: number;
}

/* ============ 阶段 C：设备（HALO M5，与 openapi.yaml 设备段对应） ============ */

export type DeviceKind = 'desktop' | 'tablet' | 'mobile' | 'cli' | 'browser';
export type DeviceStatus = 'online' | 'offline' | 'revoked';

/** 设备能力项（(device_id, name) 唯一）。 */
export interface CapabilityDTO {
  device_id?: string;
  name: string;
  version?: string;
  updated_at?: number;
}

/** 6 位一次性配对码（TTL 默认 120s）。 */
export interface PairCodeDTO {
  code: string;
  /** Unix 毫秒 */
  expires_at: number;
  ttl_s: number;
  device_name?: string;
  device_kind?: DeviceKind;
}

/** 配对成功结果：device_token 明文仅此一次返回（服务端只存 sha256）。 */
export interface PairResultDTO {
  device_id: string;
  device_token: string;
  /** 端侧出站长连接路径 */
  ws_path: string;
  protocol_version: number;
}

export interface DeviceDTO {
  id: string;
  name: string;
  kind: DeviceKind;
  platform?: string;
  status: DeviceStatus;
  capabilities_hash?: string;
  protocol_version?: number;
  app_version?: string;
  paired_at?: number;
  last_seen_at?: number;
  revoked_at?: number;
  capabilities?: CapabilityDTO[];
}

export interface DeviceListResponse {
  items: DeviceDTO[];
  pair_ttl_s: number;
  heartbeat_s: number;
  offline_after_s: number;
}

/** C-2：签发配对码请求（name/kind 仅作端侧默认值提示，可被端侧覆盖）。 */
export interface CreateDevicePairCodeRequest {
  name?: string;
  kind?: DeviceKind;
}

/** C-2：端侧凭码配对请求（`code` 必填；其余为设备自述信息）。 */
export interface DevicePairRequest {
  code: string;
  name?: string;
  kind?: DeviceKind;
  /** 如 windows / darwin / linux */
  platform?: string;
  app_version?: string;
  capabilities?: Array<{ name: string; version?: string }>;
}

/**
 * C-6：会话租约视图（GET /sessions/{sid}/lease）。
 *
 * 语义铁律：**无租约不是 404** —— `active=true / epoch=0 / holder=''` 表示「无跨端写者，
 * 服务端本地可写」，前端可直接展示而无需要分支。
 */
export interface SessionLeaseDTO {
  session_id: string;
  /** 写者归属设备 ID；空串 = 服务端本地执行面 */
  holder: string;
  holder_kind: 'server' | 'device';
  /** 写者纪元：换人或过期接管 +1，同持有者续期不变 */
  epoch: number;
  active: boolean;
  /** 毫秒；0 = 无租约 */
  expires_at?: number;
  heartbeat_at?: number;
}


/* ============ 阶段 E：外部插件与工具市场 DTO ============ */

/** MCP Server 视图（env/headers 只回键名不回值——安全边界）。 */
export interface MCPServerViewDTO {
  id: string;
  name: string;
  transport: 'stdio' | 'sse' | 'http';
  command?: string;
  args?: string[];
  env_keys?: string[];
  cwd?: string;
  url?: string;
  header_keys?: string[];
  enabled: boolean;
  timeout_ms: number;
  max_output: number;
  default_risk: string;
  allow_tools?: string[];
  deny_tools?: string[];
  health_interval_s: number;
  state?: 'ready' | 'degraded' | 'failed' | 'disabled';
  calls?: number;
  failures?: number;
  tool_count?: number;
  last_error?: string;
}

/** MCP Server 创建/更新入参（env/headers 明文进、密文出库）。 */
export interface MCPServerInputDTO {
  name: string;
  transport: 'stdio' | 'sse' | 'http';
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  cwd?: string;
  url?: string;
  headers?: Record<string, string>;
  enabled?: boolean;
  timeout_ms?: number;
  max_output?: number;
  default_risk?: 'readonly' | 'write' | 'command' | 'danger';
  allow_tools?: string[];
  deny_tools?: string[];
  health_interval_s?: number;
}

/** 联通测试结果（工具清单 + 耗时 + Server 自述）。 */
export interface MCPTestResultDTO {
  tool_names: string[];
  elapsed_ms: number;
  server_info?: string;
}

/** 健康详情。 */
export interface MCPHealthDTO {
  id: string;
  name: string;
  transport: string;
  state: string;
  last_error?: string;
  calls: number;
  failures: number;
  tool_count: number;
}

/** 本地工具包视图（trust = signed | unsigned）。 */
export interface PackageViewDTO {
  id: string;
  version: string;
  previous_version?: string;
  name: string;
  trust: 'signed' | 'unsigned';
  namespace: string;
  enabled: boolean;
  description?: string;
  tool_names?: string[];
  state?: string;
  last_error?: string;
}

/** 安装结果。 */
export interface PackageInstallResultDTO {
  view: PackageViewDTO;
  signed: boolean;
  tools: number;
}

/* ============ 阶段 G：快照回滚（与 openapi.yaml schemas 对应） ============ */

/** 单路径快照绑定（hash 为空且 absent=true 表示快照时文件不存在）。 */
export interface SnapshotBinding {
  path: string;
  hash?: string;
  size?: number;
  absent?: boolean;
}

/** 全局快照锚点行（内容本体在 data/snapshots/blobs CAS）。 */
export interface Snapshot {
  id: string;
  session_id?: string;
  run_id?: string;
  tool_event_id?: string;
  task_id?: string;
  kind: 'before' | 'after' | 'checkpoint';
  paths: SnapshotBinding[];
  size: number;
  created_at: number;
}

/** 参与回滚的工具事件快照对。 */
export interface RollbackAffected {
  tool_event_id: string;
  run_id?: string;
  snapshot_before?: string;
  snapshot_after?: string;
  paths?: string[];
}

/** 单路径物理写回结果。 */
export interface RollbackRestored {
  path: string;
  action: 'restored' | 'deleted';
  bytes?: number;
}

/** 一次回滚事件（审计 + reapply 依据）。 */
export interface Rollback {
  id: string;
  mode: 'tool' | 'turn' | 'message';
  target: string;
  affected?: RollbackAffected[];
  hidden_from_seq?: number;
  restored?: RollbackRestored[];
  created_at: number;
  reapplied_at?: number | null;
}

/** 物理回滚请求（mode 与对应目标字段配套）。 */
export interface RevertRequest {
  mode: 'tool' | 'turn' | 'message';
  tool_event_id?: string;
  run_id?: string;
  message_seq?: number;
}

/* ============ P1：Webhook 订阅（与 openapi.yaml schemas 对应） ============ */

/** Webhook 订阅（secret 只写不读）。 */
export interface Webhook {
  id: string;
  url: string;
  has_secret: boolean;
  events: string[];
  enabled: boolean;
  created_at: number;
}

/** Webhook 创建入参。 */
export interface WebhookCreateInput {
  url: string;
  secret?: string;
  events?: string[];
}
