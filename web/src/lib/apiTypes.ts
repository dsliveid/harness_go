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
  /** 归属项目 ID（空串 = 纯对话，不隶属任何项目） */
  project_id?: string;
  /**
   * 归属工作空间（执行面）ID：空 = 纯对话（无执行宿主）。
   * 与 workspace_path（磁盘目录）互相独立：workspace_id 决定能力天花板，path 只是落盘目录。
   */
  workspace_id?: string;
  message_count: number;
  last_message_at?: number;
  total_tokens: number;
  last_run_status?: RunStatus;
  created_at: number;
  updated_at: number;
  archived_at?: number;
}

export interface CreateSessionRequest {
  title?: string;
  model_ref?: string;
  workspace_path?: string;
  /** 归属项目；非空且未传 access_mode 时，服务端按项目默认控制权限快照继承 */
  project_id?: string;
  /** 可选显式覆盖（confirm|full）；留空 = 走项目默认权限继承 */
  access_mode?: 'confirm' | 'full';
}

/* ============ 项目域（阶段七：项目为服务端一等实体） ============ */

export interface Project {
  id: string;
  name: string;
  path: string;
  /** 项目级默认控制权限：只对之后新建的会话生效（快照一次） */
  default_access_mode: 'confirm' | 'full';
  pinned: boolean;
  created_at: number;
  updated_at: number;
  deleted_at?: number;
}

export interface ProjectApprovalRule {
  id: string;
  project_id: string;
  tool_name: string;
  pattern: string;
  created_at: number;
}

/* ============ 工作空间域（执行面一等实体 / 能力天花板依据） ============ */

/**
 * 工作空间类型：与后端 `model.WorkspaceKind`（internal/model/workspace.go:10-19）
 * 以及 UI 的 `types.ts#WorkspaceKind` 逐字一致，**不额外引入 cli / tablet**。
 */
export type WorkspaceKindDTO = 'server' | 'desktop' | 'mobile' | 'browser';

/**
 * 工作空间 DTO：字段严格对齐后端实际 JSON。
 * 来源：`model.Workspace`（internal/model/workspace.go:39-47）
 *     + `service.WorkspaceView` 的派生字段（internal/service/workspace_service.go）。
 * 派生字段只在 service 层组装、**从不落库**：
 *   - online：内置服务端空间恒 true；否则取设备 Hub 实时在线判定；
 *   - host：设备空间为 `device:<deviceID>`，服务端内置空间为 `server`；
 *   - tool_policy：工具能力**权威裁决**（full|readonly|none），前端不得自行推导。
 * created_at / updated_at 为毫秒时间戳（store.NowMillis）。
 */
export interface WorkspaceDTO {
  id: string;
  name: string;
  kind: WorkspaceKindDTO;
  /** 空串 = 服务端内置空间；非空 = 归属一台已配对设备 */
  device_id: string;
  /** 宿主侧根目录；服务端内置空间为空串 */
  root_path: string;
  online: boolean;
  host: string;
  /** 是否绑定了设备（内置服务端空间恒 true，它是自洽的执行宿主） */
  bound: boolean;
  /** 归属设备名/形态/状态（未绑定或设备已删除时为空串） */
  device_name?: string;
  device_kind?: string;
  device_status?: string;
  /** 绑定的设备记录已不存在（需重新绑定；与「离线」严格区分） */
  device_missing: boolean;
  /** 设备已解绑（不可恢复，需重新配对） */
  device_revoked: boolean;
  /** 设备最近活跃时间（毫秒） */
  last_seen_at?: number;
  /**
   * 工具能力策略（服务端权威值）：full 全工具 / readonly 仅只读 / none 零工具。
   * 🚨 前端**不得**自行按 online/kind 推导 —— 判定只在 service 层实现一处。
   */
  tool_policy: ToolPolicyDTO;
  /** 策略降级原因（人类可读，可直接展示） */
  policy_reason?: string;
  /** 是否可发起对话：恒 true（无设备也可对话，只是没有工具能力） */
  can_chat: boolean;
  created_at: number;
  updated_at: number;
}

/** 工具能力策略（与 internal/agent.ToolPolicy 逐字一致） */
export type ToolPolicyDTO = 'full' | 'readonly' | 'none';

/** GET /api/v1/workspaces 响应体（handler 用 map[string]any{"items": items} 包裹） */
export interface WorkspaceListResponseDTO {
  items: WorkspaceDTO[];
}

/** POST /api/v1/workspaces 请求体（internal/api/workspace_handler.go:26-33 createWorkspaceReq） */
export interface CreateWorkspaceDTO {
  name: string;
  kind: WorkspaceKindDTO;
  device_id?: string;
  root_path?: string;
}

/**
 * PATCH /api/v1/workspaces/{wid} 请求体：全部字段可选，缺省 = 不改（真 PATCH 语义）。
 * 🚨 故意**不提供 kind**：kind 是能力天花板依据，改 kind = 提权，
 * 服务端仅接受与原值相同的 kind，不同即 400（docs/工作空间模型与三端能力收口协议.md §3）。
 */
export interface UpdateWorkspaceDTO {
  name?: string;
  device_id?: string;
  root_path?: string;
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
  hidden?: boolean;
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

/**
 * 会话当前进行中的 Run（GET /sessions/{sid}/active-run）。
 *
 * 语义铁律：**无进行中 Run 不是 404** —— `run_id` 为空串即表示空闲，
 * 前端无需分支处理错误码（与 SessionLeaseDTO 的 `active=false` 同口径）。
 */
export interface ActiveRunDTO {
  run_id: string;
  status?: RunStatus;
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

/**
 * 注入载荷中的工具规格（逐字段对齐 `pkg/toolapi/spec.go` 的 `Spec` 结构体 JSON 标签）。
 *
 * 为什么不再用 `Record<string, unknown>`：`GET /tools/injection` 的 core / on_demand
 * 直接序列化 `toolapi.Spec`，字段是确定的；窄化后才能在前端如实渲染而无需 `as` 断言。
 */
export interface ToolSpecBrief {
  name: string;
  /** 例：builtin / pkg:<id> / mcp:<server>（Go 侧 omitempty，缺省由 Normalized() 补为 builtin） */
  namespace?: string;
  description: string;
  schema_json?: unknown;
  tier: ToolTier;
  executor: Executor;
  capabilities?: string[];
  cost_tokens: number;
  risk: Risk;
  /** 运行期依赖（如 chromium、git） */
  requires?: string[];
  version?: string;
}

export interface ToolInjection {
  core: ToolSpecBrief[];
  deferred_index: ToolIndexEntry[];
  on_demand: ToolSpecBrief[];
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

/* ============ WebView 免登票据（internal/api/webview_ticket_handler.go） ============ */

/**
 * 签发 ticket 的入参（POST /api/v1/auth/webview-ticket，需用户会话）。
 * 两个字段均可选；`ttl_s` 只能请求更短（请求 > 60s 服务端仍给 60s）。
 */
export interface WebviewTicketRequest {
  /** 仅用于日志与审计溯源，不参与兑换校验 */
  device_id?: string;
  /** 可选有效期（秒）；缺省/≤0 = 默认 60s */
  ttl_s?: number;
}

/** 签发响应：ticket 明文**仅此一次返回**，服务端不落库、不写日志。 */
export interface WebviewTicketResponse {
  /** 32 字节随机值的 base64url（无填充）编码，可直接放进 URL fragment */
  ticket: string;
  /** 过期时间，Unix 毫秒 */
  expires_at: number;
}

/** 兑换入参（POST /api/v1/auth/webview-exchange，免鉴权）。 */
export interface WebviewExchangeRequest {
  /** 由 webview-ticket 签发的票据明文；空串按无效处理（直接 401） */
  ticket: string;
}

/**
 * 兑换响应：**不含任何令牌字段**——会话凭据只通过 `Set-Cookie: hg_sess=...` 下发，
 * 且该会话 token kind=`webview`（可读会话、会话内发消息，禁止审批决策）。
 */
export interface WebviewExchangeResponse {
  ok: boolean;
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
  /** 签发一次性 WebView 免登 ticket（需用户会话） */
  authWebviewTicket: '/api/v1/auth/webview-ticket',
  /** 凭 ticket 兑换降级 hg_sess 会话（免鉴权，publicPaths） */
  authWebviewExchange: '/api/v1/auth/webview-exchange',
  sessions: '/api/v1/sessions',
  session: (sid: string) => `/api/v1/sessions/${sid}`,
  // 项目域（阶段七）：项目 + 项目审批规则
  projects: '/api/v1/projects',
  project: (pid: string) => `/api/v1/projects/${pid}`,
  // 工作空间域（执行面一等实体；router.go:67-71，必须登录鉴权）
  workspaces: '/api/v1/workspaces',
  workspace: (wid: string) => `/api/v1/workspaces/${wid}`,
  projectRules: (pid: string) => `/api/v1/projects/${pid}/approval-rules`,
  projectRule: (pid: string, rid: string) => `/api/v1/projects/${pid}/approval-rules/${rid}`,
  sessionArchive: (sid: string) => `/api/v1/sessions/${sid}/archive`,
  sessionUnarchive: (sid: string) => `/api/v1/sessions/${sid}/unarchive`,
  messages: (sid: string) => `/api/v1/sessions/${sid}/messages`,
  runs: (sid: string) => `/api/v1/sessions/${sid}/runs`,
  /**
   * 会话当前进行中的 Run（刷新后重建流式态用）。
   * 刻意不嵌套在 runs 子路径下：那样会被 runs 详情端点（rid 通配）抢先匹配。
   * 语义：无进行中 Run 返回 200 且 run_id 为空串（不是 404）。
   */
  activeRun: (sid: string) => `/api/v1/sessions/${sid}/active-run`,
  run: (sid: string, rid: string) => `/api/v1/sessions/${sid}/runs/${rid}`,
  runStop: (sid: string, rid: string) => `/api/v1/sessions/${sid}/runs/${rid}/stop`,
  runGuidance: (sid: string, rid: string) => `/api/v1/sessions/${sid}/runs/${rid}/guidance`,
  runEvents: (sid: string, rid: string) => `/api/v1/sessions/${sid}/runs/${rid}/events`,
  runToolEvents: (sid: string, rid: string) =>
    `/api/v1/sessions/${sid}/runs/${rid}/tool-events`,
  /** B-3：审批决策（aid = 等待中的审批 ID = tool_events.id） */
  approvalDecide: (sid: string, rid: string, aid: string) =>
    `/api/v1/sessions/${sid}/runs/${rid}/approvals/${aid}/decide`,
  tools: '/api/v1/tools',
  toolsInjection: '/api/v1/tools/injection',
  toolsTiers: '/api/v1/tools/tiers',
  /** 核心 SOP（agent-core）：GET 快照 / PUT 保存自定义 / DELETE 重置为出厂内置 */
  sopCore: '/api/v1/sop/core',
  /** 生效预览（核心 SOP + 项目级条目 + token 合计） */
  sopEffective: '/api/v1/sop/effective',
  maintenance: '/api/v1/settings/maintenance',
  /* B-4：厂商 / 配置 / 统计 / 日志 */
  providers: '/api/v1/providers',
  provider: (pid: string) => `/api/v1/providers/${pid}`,
  providerTest: (pid: string) => `/api/v1/providers/${pid}/test`,
  providerModels: (pid: string) => `/api/v1/providers/${pid}/models`,
  providerSecret: (pid: string) => `/api/v1/providers/${pid}/secret`,
  settings: '/api/v1/settings',
  stats: '/api/v1/stats',
  logs: '/api/v1/logs',
  /* 日志与审计查询（保留策略 / 审计副本 / 报文回溯 / 用量聚合） */
  logConfig: '/api/v1/logs/config',
  auditToolEvents: '/api/v1/audit/tool-events',
  /** LLM 调用流水（跨会话；session_id 为可选过滤）。旧单会话路径 llmTrace 是它的别名。 */
  auditLlmCalls: '/api/v1/audit/llm-calls',
  tokenStats: '/api/v1/stats/tokens',
  llmTrace: (sid: string) => `/api/v1/sessions/${sid}/llm-trace`,
  /* 工作区宿主目录列举（L0 服务端宿主；docs/工作区宿主与目录选择对接约定.md） */
  fsRoots: '/api/v1/fs/roots',
  fsDirs: '/api/v1/fs/dirs',
  /* 阶段 C：设备（配对 / 列表 / 长连接 / 吊销） */
  devices: '/api/v1/devices',
  device: (did: string) => `/api/v1/devices/${did}`,
  devicePairCodes: '/api/v1/devices/pair-codes',
  devicePair: '/api/v1/devices/pair',
  /** C-3：端侧出站 WS（鉴权走 query `?token=`，浏览器端不持有设备令牌） */
  deviceWS: '/api/v1/devices/ws',
  /**
   * 凭设备令牌续签用户会话（免全局鉴权，handler 内只认 Bearer 设备令牌）。
   *
   * Web 前端**不调用**它：浏览器侧没有 device_token，续期由原生端（移动/桌面）完成。
   * 列在这里仅为与 openapi.yaml / router.go 保持三方一致（契约自检要求）。
   */
  deviceSession: '/api/v1/devices/session',
  /**
   * 端侧自我解约（免全局鉴权，handler 内只认 Bearer 设备令牌）。
   *
   * **双向解绑的客户端方向**：端侧「解除本机绑定」时调用，服务端据此吊销该设备，
   * 使其从 `GET /api/v1/devices` 默认列表消失。
   *
   * 与 `device(did)` 的 DELETE 分工：后者是管理端按 ID 吊销（需用户令牌），
   * 本端点由端侧主动调用（只需 device_token）。端侧解绑时 user_token 可能已过期，
   * 所以不能复用 DELETE —— 那是「客户端取消绑定后服务端没取消」的根因。
   *
   * Web 前端**不调用**它：浏览器侧没有 device_token。列在这里仅为与
   * openapi.yaml / router.go 保持三方一致（契约自检要求）。
   */
  deviceUnpair: '/api/v1/devices/unpair',
  /** 设备状态实时推送（SSE）：首帧 snapshot + 增量上下线 + device.revoked；**不续传** */
  deviceEvents: '/api/v1/devices/events',
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
  /* B-2：制品下载 / 会话导出 / 全量备份恢复（均为二进制流，不做 JSON 解析） */
  artifact: (id: string) => `/api/v1/artifacts/${id}`,
  /** 会话导出（format=zip → application/zip 流） */
  sessionExport: (sid: string) => `/api/v1/sessions/${sid}/export`,
  /** 全量数据备份（owner 专用，application/zip 流） */
  dataBackup: '/api/v1/data/backup',
  /** 从备份 zip 恢复（POST 上传） */
  dataRestore: '/api/v1/data/restore',
} as const;

/** 免鉴权路径（与 internal/api/router.go 的 publicPaths 保持一致）。 */
export const PUBLIC_PATHS = [
  API_PATHS.authLogin,
  API_PATHS.authSSO,
  // 端侧配对：凭码即凭据（一次性 + TTL 120s + 失败限流）
  API_PATHS.devicePair,
  // WebView ticket 兑换：WebView 首屏尚无 Cookie，凭 ticket 本身即凭据（一次性 + 默认 TTL 60s）
  API_PATHS.authWebviewExchange,
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

/**
 * 连通测试入参（均可选）：
 * model 留空用厂商首个模型；base_url / api_key / models 携带草稿里「尚未保存」的
 * 当前值，实现所见即所测（厂商未落库时 base_url 必填；api_key 仅本次测试使用不落库）。
 */
export interface ProviderTestInput {
  model?: string;
  base_url?: string;
  api_key?: string;
  models?: string[];
}

/** 上游真实模型清单（获取模型列表）；失败时后端返回错误，不回退本地兜底。 */
export interface ProviderModelsResult {
  items: string[];
  count: number;
}

/**
 * 获取模型列表入参（均可选）：携带草稿里「尚未保存」的当前值，实现所见即所测。
 * 厂商未落库时 base_url 必填；api_key 仅本次请求使用，不落库、不进日志。
 */
export interface ProviderModelsInput {
  base_url?: string;
  api_key?: string;
}

/** 厂商密钥明文（眼睛图标显示 / 复制）；未配置时 api_key 为空串、has_key=false。 */
export interface ProviderSecretResult {
  api_key: string;
  has_key: boolean;
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

/**
 * 报文抓取档位（settings 键 `log.snapshot_mode`）。
 * - off：不落报文快照，日增≈50MB（仅日志文本）
 * - preview：落截断预览，日增≈500MB（30 天≈15GB）
 * - full：落完整报文，日增≈5GB（30 天≈150GB，高风险）
 */
export type SnapshotMode = 'off' | 'preview' | 'full';

/** 日志查询参数（GET /api/v1/logs，全部为可选 query）。 */
export interface LogsQuery {
  limit?: number;
  /** 精确级别过滤（error / warn / info / debug），服务端过滤而非前端过滤 */
  level?: string;
  /** 起始时间（RFC3339） */
  from?: string;
  /** 结束时间（RFC3339） */
  to?: string;
  traceId?: string;
  keyword?: string;
  offset?: number;
}

/**
 * 日志查询响应（GET /api/v1/logs）。
 * ⚠️ 诚实边界：retention_days / earliest_available_day / snapshot_mode 由后端返回真实的
 * 天分片日志库族现状；后端未返回时前端显示「保留策略未知」，绝不硬编码天数假装已知。
 */
export interface LogsResponse {
  items: LogLine[];
  /** 命中总数（服务端统计，用于分页/提示；后端未返回时为 undefined） */
  total?: number;
  has_more?: boolean;
  retention_days?: number;
  /** 最早可查日，格式 YYYYMMDD */
  earliest_available_day?: string;
  snapshot_mode?: SnapshotMode | string;
  limit?: number;
}

/* ============ 日志与审计查询（B-4 扩展：LLM 调用流水 / 用量聚合） ============ */

/** LLM 调用流水查询参数（GET /api/v1/audit/llm-calls，全部为可选 query）。 */
export interface AuditLlmCallsQuery {
  /** Unix 毫秒；缺省 = 最近 24 小时（服务端默认，避免全量扫天库） */
  from?: number;
  to?: number;
  /** 可选：只查某个会话。为空 = 跨会话全局查询 */
  session_id?: string;
  run_id?: string;
  message_id?: string;
  trace_id?: string;
  model?: string;
  /** ok | error */
  status?: string;
  keyword?: string;
  /** 单条精确回查时传 id，服务端会顺带返回正文（full 档才有） */
  id?: string;
  limit?: number;
  offset?: number;
}

/** LLM 调用流水行（对应后端 logvault.LLMRow）。 */
export interface AuditLlmCallItem {
  id: string;
  created_at: number;
  session_id: string;
  run_id: string;
  message_id: string;
  trace_id: string;
  model: string;
  provider: string;
  endpoint: string;
  /** 脱敏预览（preview/full 档才有；off 档为空） */
  payload_preview: string;
  payload_inline?: string;
  payload_sha256: string;
  payload_bytes: number;
  /** 正文是否可取（inline 或 blob 存在） */
  payload_available: boolean;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  is_estimated: boolean;
  ttft_ms: number;
  duration_ms: number;
  tokens_per_sec: number;
  status: string;
  error_code?: string;
  error_message?: string;
}

/** LLM 调用流水响应（GET /api/v1/audit/llm-calls）。 */
export interface AuditLlmCallsResponse {
  items: AuditLlmCallItem[];
  total: number;
  /** 单条回查时的正文（仅 full 档有值） */
  payload?: string;
  /** 正文不可得时的人话说明（preview/off 档） */
  payload_hint?: string;
  snapshot_mode: SnapshotMode | string;
  retention_days: number;
  earliest_available_day: string;
}

/** 工具调用审计行（对应后端 logvault.ToolRow）。 */
export interface AuditToolEventItem {
  id: string;
  created_at: number;
  session_id: string;
  run_id: string;
  tool_event_id: string;
  tool_name: string;
  namespace: string;
  params_preview: string;
  result_preview: string;
  bytes: number;
  status: string;
  risk: string;
  error?: string;
  duration_ms: number;
}

/** 工具调用审计响应（GET /api/v1/audit/tool-events）。 */
export interface AuditToolEventsResponse {
  items: AuditToolEventItem[];
  total: number;
  retention_days: number;
  earliest_available_day: string;
}

/** 用量聚合单行（对应后端 logvault.TokenStat）。
 *  注意：与 web/src/types.ts 的 TokenStatRow（UI 形状，含 model/provider/calibRatio）
 *  不是同一个东西——后者是前端展示模型，这里是后端原始形状，故加 Api 后缀区分。 */
export interface TokenStatApiRow {
  key: string;
  calls: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  estimated_calls: number;
  cache_hit_rate: number;
}

/** 用量聚合汇总（GET /api/v1/stats/tokens）。 */
export interface TokenStatsResult {
  from: number;
  to: number;
  group_by: string;
  items: TokenStatApiRow[];
  total_calls: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cache_hit_rate: number;
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

/**
 * MCP Server 列表包装：`GET /api/v1/mcp/servers` → `{"items":[...]}`。
 * 来源：internal/api/market_handler.go:30（writeJSON map[string]any{"items": items}）。
 */
export interface MCPListResponseDTO {
  items: MCPServerViewDTO[];
}

/**
 * 工具包列表包装：`GET /api/v1/packages` → `{"items":[...]}`。
 * 来源：internal/api/market_handler.go:206。
 */
export interface PackageListResponseDTO {
  items: PackageViewDTO[];
}

/**
 * 工具包安装入参：`POST /api/v1/packages/install {"path":"<zip 路径>"}`。
 * 来源：internal/api/market_handler.go:215-217（installPackageReq）。
 * 说明：一期只接受「服务端本地路径」，无 HTTP 上传通道（见 :213-214 设计说明）。
 */
export interface PackageInstallRequestDTO {
  path: string;
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

/**
 * Webhook 列表包装：`GET /api/v1/webhooks` → `{"items":[...]}`。
 * 来源：internal/api/webhook_handler.go:36（writeJSON map[string]any{"items": items}）。
 */
export interface WebhookListResponseDTO {
  items: Webhook[];
}

/**
 * Webhook 联通测试结果：`POST /api/v1/webhooks/{id}/test`。
 * 来源：internal/api/webhook_handler.go:106（{"status": int, "ok": bool, "error": string}）。
 * 说明：投递失败不抛错——`ok=false` 且 `error` 携带对端失败原因（非 2xx / 超时 / 连接拒绝）。
 */
export interface WebhookTestResult {
  status: number;
  ok: boolean;
  error: string;
}

/* ===== 核心 SOP（agent-core）：出厂内置 + 运行时覆盖 ===== */

/** 核心 SOP 生效快照。source=builtin 为出厂 embed（永不落盘），custom 为运行时覆盖。 */
export interface SOPCore {
  content: string;
  source: 'builtin' | 'custom';
  tokens: number;
  /** 总 token 预算（0 = 不限） */
  budget: number;
  /** 出厂原文：前端「系统内置」页签渲染它，切换页签不需要任何读写请求 */
  builtin_content: string;
  /** 是否存在运行时覆盖（等价于「处于自定义模式」） */
  overridden: boolean;
}

/** 项目级 SOP 条目（<ws>/.harness/sop/<id>.md）。 */
export interface SOPEntry {
  id: string;
  title: string;
  body: string;
  tokens: number;
}

/** 生效预览：核心 SOP + 项目级条目 + token 合计。 */
export interface SOPEffective {
  core: SOPCore;
  entries: SOPEntry[];
  workspace: string;
  total_tokens: number;
  system_preview: string;
  /** 是否有条目因条数上限被截断（D10：必须可见） */
  truncated: boolean;
  entry_limit: number;
}
