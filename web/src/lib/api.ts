/**
 * 统一数据访问层。
 *
 * 存在意义：让 UI 只依赖一层「同签名」的 ApiClient，不直接拼 HTTP 细节。
 *
 * 历史沿革（2026-11 正式版）：本层曾同时提供 `mockApi` 与 `realApi` 两套实现，
 * 由 `apiMode` 在运行期二选一（演示数据 / 真实后端）。正式版已下线整个 mock 通路，
 * 现只剩 `realApi` 一套实现，`api` 直接指向它。
 *
 * 契约铁律：
 *   - HTTP 类型一律来自 `./apiTypes`（由 `api/openapi.yaml` 唯一定义，禁止自造字段）；
 *   - UI 领域模型来自 `../types`（Session/Message/Run 等），本层负责 DTO → UI 映射；
 *   - 错误统一为 `ApiError`（携带 code / status / trace_id，trace_id 与后端日志一致）。
 */

import type { AccessMode, Approval, ApprovalRule, FileDiff, Message, Project, ProjectApprovalRule, Run, RunStatus, Session, ToolCall, Risk, ToolPolicy, ToolStatus, Workspace, WorkspaceKind } from '../types'
import { SseClient, type SseState as SseStateName, type SseStateInfo } from './sse'
import { extractFileDiff, extractResultContent } from './toolDiff'
import {
  API_PATHS,
  type ActiveRunDTO,
  type ChangePasswordRequest,
  type CreateDevicePairCodeRequest,
  type CreateRunResponse,
  type DeviceListResponse,
  type DeviceDTO,
  type LogsQuery,
  type LogsResponse,
  type AuditLlmCallsQuery,
  type AuditLlmCallsResponse,
  type AuditToolEventsResponse,
  type SOPCore,
  type SOPEffective,
  type TokenStatsResult,
  type LoginRequest,
  type LoginResponse,
  type Message as ApiMessage,
  type MCPHealthDTO,
  type MCPListResponseDTO,
  type MCPServerInputDTO,
  type MCPServerViewDTO,
  type MCPTestResultDTO,
  type PackageInstallRequestDTO,
  type PackageInstallResultDTO,
  type PackageListResponseDTO,
  type PackageViewDTO,
  type PairCodeDTO,
  type ProviderDTO,
  type ProviderInput,
  type ProviderModelsInput,
  type ProviderModelsResult,
  type ProviderSecretResult,
  type ProviderTestInput,
  type ProviderTestResult,
  type Project as ApiProject,
  type ProjectApprovalRule as ApiProjectApprovalRule,
  type Rollback,
  type Run as ApiRun,
  type RunStatus as ApiRunStatus,
  type Session as ApiSession,
  type SessionLeaseDTO,
  type Snapshot,
  type SSORequest,
  type ToolEvent as ApiToolEvent,
  type ToolInjection,
  type ToolSnapshot,
  type ToolTier,
  type User,
  type WebviewExchangeRequest,
  type WebviewExchangeResponse,
  type Webhook,
  type WebhookCreateInput,
  type WebhookListResponseDTO,
  type WebhookTestResult,
  type CreateWorkspaceDTO,
  type UpdateWorkspaceDTO,
  type WorkspaceDTO,
} from './apiTypes'

/* ============ 模式（正式版：恒为真实后端） ============ */

/**
 * 数据来源类型。
 *
 * 历史沿革（2026-11 正式版收口）：项目曾有 `mock | real` 双模式，靠
 * `localStorage['harness.apiMode']` 与 `VITE_API_MODE` 在演示数据与真实后端间切换，
 * 并配有全局 REAL/DEMO 角标一键切换。正式版已**整体下线 mock 通路**：
 * 演示数据目录、mockApi 实现、模式开关与角标全部删除，`api` 恒等于真实后端实现。
 *
 * 保留本类型（值域收敛为 `'real'`）是为了让 `ApiClient.mode` 等既有签名不必改动。
 */
export type ApiMode = 'real'

/* ============ 错误 ============ */

export class ApiError extends Error {
  readonly code: string
  readonly status: number
  readonly traceId: string

  constructor(message: string, code = 'unknown', status = 0, traceId = '') {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
    this.traceId = traceId
  }
}

/* ============ 公共类型 ============ */

/**
 * 流式事件（服务端 SSE payload 归一后的 UI 形态）。
 *
 * 历史沿革：本类型曾定义在 `../mock/stream`（演示数据生成器）里，正式版下线 mock 后
 * 迁至此处的真实信源唯一入口，避免 UI 依赖已删除的演示模块。
 */
export type StreamEvent =
  | { type: 'delta'; text: string }
  | { type: 'reasoning'; text: string }
  | { type: 'tool_start'; call: ToolCall }
  | { type: 'tool_result'; callId: string; result: string; durationMs: number; diff?: FileDiff; skipped?: boolean; failed?: boolean }
  | { type: 'tool_progress'; callId: string; chunk: string }
  | { type: 'step'; tokens?: number; promptTokens?: number; cachedTokens?: number; reasoning?: string }
  | { type: 'approval'; approval: Approval }
  | { type: 'done'; tokensIn: number; tokensOut: number; cachedTokens?: number }
  | { type: 'error'; code: string; message: string }

/** 审批决策（与后端 approval 端点取值一致） */
export type Decision = 'allow_once' | 'allow_session' | 'deny'

/** 一次 Run 的句柄 */
export interface RunHandle {
  readonly runId: string
  /** 停止 Run（服务端幂等） */
  stop(): void
  /**
   * 立即重连事件流（跳过退避）。
   *
   * 用途：收到服务端 `resync` 且查明 Run **尚未终态**时，重新订阅继续收事件。
   * 语义边界：SseClient 收到 resync 后**不自动重连**（Terminal Run 重连只会拿到
   * 「事件源已关闭」，形成重连风暴）；是否重连必须由上层依据 Run 真实状态决定。
   */
  reconnect(): void
}

export interface StartRunOptions {
  sessionId: string
  input: string
  modelRef?: string
  /** 流式事件回调（UI 侧再做 ~50ms 批量提交） */
  onEvent: (e: StreamEvent) => void
  /**
   * 断线/重连对齐回调（阶段 B-3）：SSE 重连或收到服务端 `resync` 事件时触发，
   * 上层应重拉消息快照覆盖本地流式态（铁律：消息最终内容以 DB 为准）。
   */
  onResync?: () => void
  /**
   * SSE 连接状态上报（阶段 B-5 补）：把 lib/sse.ts 的 SseClient 真实状态机
   * （connecting/open/backoff/resync/closed）外泄给上层，供顶栏 SseStatus 展示**真实**状态。
   *
   * ⚠️ 独白：此前 sseStore 是纯硬编码假值（lastSeq:214 / reconnects:2 / 永为「43 分钟前断开」），
   * 又被 TopBar 常驻渲染 → 每个页面顶部都在展示假连接状态。本回调是消除该假象的唯一真实数据源，
   * 请勿在 UI 侧再补任何编造指标。
   */
  onSseState?: (state: SseStateName, info: SseStateInfo) => void
  /**
   * SSE 事件游标上报：每收到一帧携带 `id`（即 per-Run seq）的事件时回调，
   * 供顶栏展示真实 lastEventId。**不做任何插值或估算**，无 id 的帧不上报。
   */
  onSseEventSeq?: (seq: number) => void
}

/**
 * 订阅一个**已存在** Run 的事件流（StartRunOptions 的订阅部分 + 目标 Run 标识）。
 *
 * 存在意义：`startRun` 原本把「POST 创建 Run」与「new SseClient 订阅事件」焊死在一起，
 * 导致页面刷新（F5）后无法重新接上服务端仍在跑的 Run —— 这正是「刷新后看不到进行中状态」的根因。
 * 抽出后，创建路径与恢复路径共用同一套订阅/事件映射/状态上报逻辑，杜绝两份状态机。
 */
export interface AttachRunOptions {
  sessionId: string
  runId: string
  /**
   * 起始事件序号（服务端 Bus.Subscribe 的 fromSeq 语义）：
   *   - 缺省 0：从头补发历史（新建 Run 时不会漏掉首屏事件）；
   *   - **-1：只要新事件、绝不补发历史** —— 刷新恢复场景必须用它。
   *
   * ⚠️ 刷新恢复时必须传 -1：此时前端已通过 DB 快照拿到消息全文，
   * 若再补发历史 `message.delta`，文本会被重复追加一遍（脏数据级缺陷）。
   */
  fromSeq?: number
  onEvent: (e: StreamEvent) => void
  onResync?: () => void
  onSseState?: (state: SseStateName, info: SseStateInfo) => void
  onSseEventSeq?: (seq: number) => void
}

export interface CreateSessionInput {
  title?: string
  modelRef?: string
  workspacePath?: string
  /**
   * 归属工作空间（执行面）ID。决定会话的能力天花板（能否执行命令/写文件）。
   * 留空 = 纯对话；服务端会从 projectId 继承，未继承到则为空。
   */
  workspaceId?: string
  /**
   * 归属项目（服务端 sessions.project_id）。**仅**用于告诉服务端「这个会话属于哪个项目」；
   * 该会话的初始控制权限由服务端按项目的 default_access_mode 裁决并快照。
   */
  projectId?: string
  /**
   * 显式控制权限。铁律：**前端常规创建会话时绝不可传**（默认权限继承由服务端裁决），
   * 仅在服务端接口层面保留该字段以支持显式指定场景。
   */
  accessMode?: AccessMode
}

/* ---- 项目级治理（阶段七）：入参类型（字段全部可选/最小必要，real 分支只发送存在的字段） ---- */

/** 新建项目入参（对应 POST /projects body: {name,path,default_access_mode}） */
export interface CreateProjectInput {
  name: string
  path: string
  defaultAccessMode?: AccessMode
}

/** 更新项目入参（对应 PATCH /projects/{id}）—— 未提供的字段不发送，避免覆盖成空值 */
export interface UpdateProjectInput {
  name?: string
  defaultAccessMode?: AccessMode
  pinned?: boolean
}

/** 新增项目审批规则入参（对应 POST /projects/{pid}/approval-rules body） */
export interface CreateProjectRuleInput {
  toolName: string
  pattern: string
}

/* ---- 工作空间域：入参类型（字段与 internal/api/workspace_handler.go 逐一对齐） ---- */

/**
 * 新建工作空间入参（POST /workspaces body: {name,kind,device_id,root_path}）。
 * kind 必填且**创建后不可改**：它是能力天花板依据（server|desktop|mobile|browser）。
 * deviceId 留空 = 服务端内置空间语义（恒在线）。
 */
export interface CreateWorkspaceInput {
  name: string
  kind: WorkspaceKind
  deviceId?: string
  rootPath?: string
}

/**
 * 更新工作空间入参（PATCH /workspaces/{wid}）：未提供字段不发送，服务端保留原值。
 *
 * 🚨 类型层面**故意不提供 kind**：kind 是权限天花板依据，改 kind = 提权。
 * 服务端 PATCH 仅接受与原值相同的 kind，不同即 400
 * （docs/工作空间模型与三端能力收口协议.md §3）。前端从类型上就不给这条提权路径。
 */
export interface UpdateWorkspaceInput {
  name?: string
  deviceId?: string
  rootPath?: string
}

export interface ApiClient {
  readonly mode: ApiMode
  // 认证
  me(): Promise<User | null>
  login(req: LoginRequest): Promise<LoginResponse>
  /** SSO 免登（前端兜底自愈：vite dev / 纯静态部署时服务端未拦截 URL 参数）；凭据以 Cookie 落地，不回传 token */
  ssoLogin(req: SSORequest): Promise<{ user: User }>
  /** 凭一次性 ticket 兑换降级 hg_sess 会话（免鉴权，凭据只走 Set-Cookie，不回传令牌） */
  exchangeWebviewTicket(ticket: string): Promise<void>
  /** 修改密码（需已登录；成功后其他会话被吊销） */
  changePassword(req: ChangePasswordRequest): Promise<void>
  logout(): Promise<void>
  // 会话
  listSessions(params?: { limit?: number; offset?: number }): Promise<Session[]>
  getSession(id: string): Promise<Session>
  createSession(input: CreateSessionInput): Promise<Session>
  renameSession(id: string, title: string): Promise<void>
  /** 更新会话级模型选择（空串 = 未选择；服务端持久化，重载后仍生效） */
  setSessionModel(id: string, modelRef: string): Promise<void>
  /** 更新会话访问模式（confirm/full；引擎每次审批实时重读，运行中切换立即生效） */
  setSessionAccessMode(id: string, mode: 'confirm' | 'full'): Promise<void>
  /** 归档会话（落库；侧栏默认列表不再显示，归档中心可见） */
  archiveSession(id: string): Promise<void>
  /** 取消归档（回到原分组） */
  unarchiveSession(id: string): Promise<void>
  deleteSession(id: string): Promise<void>
  /** 消息级回滚（S10）：seq 之后的消息置灰、受影响文件回滚到 before 快照；返回回滚事件 id（供 reapply） */
  rollbackToMessage(sessionId: string, messageSeq: number): Promise<string>
  /** 重新应用一次回滚：恢复 after 快照 + 恢复置灰消息 */
  reapplyRollback(sessionId: string, rollbackId: string): Promise<void>
  /** 会话回滚历史（后端时间倒序；GET /sessions/{sid}/rollbacks → {items}） */
  listRollbacks(sessionId: string): Promise<Rollback[]>
  /** 会话快照锚点（后端时间倒序；GET /sessions/{sid}/snapshots → {items}） */
  listSessionSnapshots(sessionId: string): Promise<Snapshot[]>

  // ---- 项目级治理（阶段七：项目为服务端一等实体 + 项目审批规则） ----
  /** 全部项目（GET /projects → {items}） */
  listProjects(): Promise<Project[]>
  /** 新建项目（POST /projects → 201 Project）；defaultAccessMode 缺省由服务端定 */
  createProject(input: CreateProjectInput): Promise<Project>
  /** 更新项目（PATCH /projects/{id} → 204）：只发送传入的字段 */
  updateProject(id: string, patch: UpdateProjectInput): Promise<void>
  /** 删除项目（DELETE /projects/{id} → 204） */
  deleteProject(id: string): Promise<void>
  /** 项目审批规则（GET /projects/{pid}/approval-rules → {items}） */
  listProjectRules(projectId: string): Promise<ProjectApprovalRule[]>
  /** 新增项目审批规则（POST → 201 ProjectApprovalRule） */
  addProjectRule(projectId: string, input: CreateProjectRuleInput): Promise<ProjectApprovalRule>
  /** 删除项目审批规则（DELETE → 204） */
  removeProjectRule(projectId: string, ruleId: string): Promise<void>

  // ---- 工作空间域（执行面一等实体；ABILITY 天花板依据，router.go:67-71） ----
  /**
   * 全部工作空间（GET /workspaces → {items}）。
   * 服务端内置空间（ws-server）恒排最前，其余按创建时间升序；
   * online / host 为服务层实时派生字段（不落库）。
   */
  listWorkspaces(): Promise<Workspace[]>
  /** 新建工作空间（POST /workspaces → 201 WorkspaceView）；kind 创建后不可修改 */
  createWorkspace(input: CreateWorkspaceInput): Promise<Workspace>
  /**
   * 更新工作空间（PATCH /workspaces/{wid} → 200 WorkspaceView）：只发送传入的字段。
   * 不支持改 kind（改 kind = 提权，服务端直接 400）。
   */
  updateWorkspace(id: string, patch: UpdateWorkspaceInput): Promise<Workspace>
  /**
   * 删除工作空间（DELETE /workspaces/{wid} → 204）。
   *
   * migrateRefs：
   *   - false/缺省 = 严格模式：仍被会话/项目引用时 409，需先迁移引用；
   *   - true        = 确认移除语义（Web 管理端使用）：服务端在同一主库事务内把该空间的
   *     会话/项目一并软删、清项目审批规则、物理删除空间行，刷新后不会再出现。
   *     🚨 不是「迁到内置 ws-server」—— 那会把 readonly/离线会话提权到 full（违反 D8）。
   */
  deleteWorkspace(id: string, opts?: { migrateRefs?: boolean }): Promise<void>
  // 消息
  listMessages(sessionId: string, afterSeq?: number): Promise<Message[]>
  // Run
  startRun(opts: StartRunOptions): Promise<RunHandle>
  /**
   * 订阅一个**已存在** Run 的事件流（刷新恢复入口）。
   * `fromSeq: -1` = 只要新事件、不补发历史（恢复场景必须用它，否则正文重复）。
   */
  attachRun(opts: AttachRunOptions): RunHandle
  /** 会话当前进行中的 Run；无则 null（服务端语义：无进行中 Run 不是 404） */
  getActiveRun(sessionId: string): Promise<ActiveRunDTO | null>
  getRun(sessionId: string, runId: string): Promise<Run>
  stopRun(sessionId: string, runId: string): Promise<void>
  /**
   * 提交审批决策（B-3）：POST /sessions/{sid}/runs/{rid}/approvals/{aid}/decide
   *
   * 唯一凭据是 `approvalId`（= tool_event id = 服务端 approval_id），
   * **不依赖事件闭包**——刷新恢复后前端未必收到过 approval.request 事件。
   */
  decideApproval(
    sessionId: string,
    runId: string,
    approvalId: string,
    decision: Decision,
    reason?: string,
  ): Promise<void>
  /** 运行中引导（§4.2）：消息立即落库并就地返回，引擎在轮次边界回灌上下文；Run 已结束抛 409 */
  sendGuidance(sessionId: string, runId: string, input: string): Promise<{ id: string; seq: number; content: string }>
  listToolEvents(sessionId: string, runId: string): Promise<ApiToolEvent[]>
  // 工具（A-10 三端点）
  getTools(): Promise<ToolSnapshot>
  getToolsInjection(): Promise<ToolInjection>
  patchToolTiers(tiers: Record<string, ToolTier>): Promise<ToolSnapshot>
  /** 核心 SOP：读取生效快照（含 builtin_content，切页签无需再请求） */
  getSOPCore(): Promise<SOPCore>
  /** 核心 SOP：保存自定义（首次保存即 fork，下一轮对话生效） */
  putSOPCore(content: string): Promise<SOPCore>
  /** 核心 SOP：重置为出厂内置（完整 undo） */
  resetSOPCore(): Promise<SOPCore>
  /** 核心 SOP：生效预览（核心 + 项目级条目 + token 合计） */
  getSOPEffective(sessionId?: string): Promise<SOPEffective>
  // 运维
  getMaintenance(): Promise<Record<string, unknown>>
  // 厂商（B-4）
  listProviders(): Promise<ProviderDTO[]>
  createProvider(input: ProviderInput): Promise<ProviderDTO>
  updateProviderById(id: string, input: ProviderInput): Promise<ProviderDTO>
  deleteProvider(id: string): Promise<void>
  /** 连通测试：用该厂商发一次极小请求，返回延迟与错误码；入参可携带草稿未保存值（所见即所测） */
  testProvider(id: string, input?: ProviderTestInput): Promise<ProviderTestResult>
  /** 获取上游真实模型清单（失败直接抛错，不回退本地兜底）；入参可携带草稿未保存值 */
  listProviderModels(id: string, input?: ProviderModelsInput): Promise<ProviderModelsResult>
  /** 读取厂商密钥明文（眼睛图标显示 / 复制） */
  revealProviderKey(id: string): Promise<ProviderSecretResult>
  // 配置 / 统计 / 日志（B-4）
  getSettings(): Promise<Record<string, unknown>>
  patchSettings(patch: Record<string, unknown>): Promise<Record<string, unknown>>
  getStats(): Promise<Record<string, unknown>>
  getLogs(params?: LogsQuery): Promise<LogsResponse>
  /** LLM 调用流水（跨会话；天库结构化审计，非 app.log 文本日志） */
  getAuditLlmCalls(params?: AuditLlmCallsQuery): Promise<AuditLlmCallsResponse>
  /** 工具调用审计（跨会话；天库副本） */
  getAuditToolEvents(params?: {
    from?: number
    to?: number
    session_id?: string
    run_id?: string
    tool?: string
    status?: string
    keyword?: string
    limit?: number
    offset?: number
  }): Promise<AuditToolEventsResponse>
  /** 用量聚合（rollup 预聚合；group_by: model|provider|day） */
  getTokenStats(params?: { from?: number; to?: number; group_by?: string }): Promise<TokenStatsResult>
  /** 日志配置（抓取档位 / 保留天数 / 当前生效日志级别） */
  getLogConfig(): Promise<{
    snapshot_mode: string
    retention_days: number
    level: string
    estimated_daily_mb?: Record<string, number>
    earliest_available_day?: string
    latest_available_day?: string
    total_bytes?: number
    dropped?: number
    written?: number
  }>
  // 设备与多端协同（阶段 C）
  /** 生成 6 位一次性配对码（TTL 由 devices.pair_ttl_s 决定，默认 120s） */
  createDevicePairCode(req?: CreateDevicePairCodeRequest): Promise<PairCodeDTO>
  /** 设备列表（含离线/已吊销与能力项；不回传令牌摘要） */
  listDevices(): Promise<DeviceListResponse>
  /** 吊销设备（令牌立即失效并断开长连接） */
  revokeDevice(id: string): Promise<void>
  /** 会话租约（无租约 = active=true / epoch=0 / holder=''，不是错误） */
  getSessionLease(sessionId: string): Promise<SessionLeaseDTO>

  // ---- 阶段 E：MCP Server 管理（10 个端点，protocol 依据 internal/api/router.go:110-119） ----
  /** 全部 Server（配置 + 运行态合并；env/headers 只回键名不回值） */
  listMcpServers(): Promise<MCPListResponseDTO>
  /** 单个 Server 视图 */
  getMcpServer(id: string): Promise<MCPServerViewDTO>
  /** 新增（start=true 时立即建连；连接失败则配置已落库但状态 failed） */
  createMcpServer(input: MCPServerInputDTO, start?: boolean): Promise<MCPServerViewDTO>
  /** 全量更新（变更后需显式 reloadMcpServer 才生效，服务端不自动重连） */
  updateMcpServer(id: string, input: MCPServerInputDTO): Promise<MCPServerViewDTO>
  /** 删除（先注销工具再关连接） */
  deleteMcpServer(id: string): Promise<void>
  /** 联通测试（配置可未落库）：返回工具清单 + 耗时，用完即清理临时连接 */
  testMcpServer(input: MCPServerInputDTO): Promise<MCPTestResultDTO>
  /** 热重载（原子替换，失败保留旧连接） */
  reloadMcpServer(id: string): Promise<void>
  /** 启用（配置 enabled=true 并建连） */
  enableMcpServer(id: string): Promise<void>
  /** 停用（断开连接但保留配置） */
  disableMcpServer(id: string): Promise<void>
  /** 健康详情（calls/failures/tool_count/state） */
  getMcpServerHealth(id: string): Promise<MCPHealthDTO>

  // ---- 阶段 E：本地工具包（6 个端点，protocol 依据 internal/api/router.go:121-126） ----
  /** 全部已安装工具包 */
  listPackages(): Promise<PackageListResponseDTO>
  /** 安装（入参为**服务端本地 zip 路径**，无 HTTP 上传通道） */
  installPackage(req: PackageInstallRequestDTO): Promise<PackageInstallResultDTO>
  /** 卸载（注销工具 + 清目录 + 删行） */
  uninstallPackage(id: string): Promise<void>
  /** 回滚到上一版本 */
  rollbackPackage(id: string): Promise<PackageViewDTO>
  /** 启用（保留安装，重新注册工具） */
  enablePackage(id: string): Promise<void>
  /** 停用（保留安装，注销工具） */
  disablePackage(id: string): Promise<void>

  // ---- P1：Webhook 事件订阅（6 个端点，protocol 依据 internal/api/router.go:57-62） ----
  /** 全部订阅（secret 只写不读：仅回 has_secret，永不回明文/密文） */
  listWebhooks(): Promise<WebhookListResponseDTO>
  /** 新增订阅（events 为空数组 = 订阅全部一期事件，由后端白名单校验） */
  createWebhook(input: WebhookCreateInput): Promise<Webhook>
  /** 删除订阅 */
  deleteWebhook(id: string): Promise<void>
  /** 启用（恢复投递） */
  enableWebhook(id: string): Promise<void>
  /** 停用（保留配置但不投递） */
  disableWebhook(id: string): Promise<void>
  /** 联通测试：同步投递一条 webhook.test 事件（不重试），失败也返回 200 + ok=false */
  testWebhook(id: string): Promise<WebhookTestResult>

  // ---- B-2：制品下载 / 会话导出 / 全量备份恢复（均为二进制流） ----
  /**
   * 下载制品（GET /artifacts/{id} → 二进制流）。成功后**立即触发浏览器下载**并返回 Blob + 文件名。
   * 失败原样抛 ApiError（含后端 message / trace_id），不静默兜底。
   */
  downloadArtifact(id: string): Promise<DownloadResult>
  /** 导出会话为 zip（GET /sessions/{sid}/export?format=zip），成功后触发下载 */
  exportSession(sessionId: string): Promise<DownloadResult>
  /** 全量数据备份 zip（GET /data/backup，owner 专用），成功后触发下载 */
  backupData(): Promise<DownloadResult>
  /** 从备份 zip 恢复（POST /data/restore，multipart 上传） */
  restoreData(file: File): Promise<RestoreResult>
}

/* ============ HTTP 基础设施 ============ */

const toAbsolute = (path: string) => (path.startsWith('http') ? path : `${location.origin}${path}`)

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  // ① FormData（如 multipart 上传）必须由浏览器自动补 multipart boundary，手写会解析失败；
  // ② Blob/File（如 /data/restore 的备份 zip 原始字节流）必须让浏览器按 body 类型自动推断，
  //    绝不能强行标 application/json —— 后端 backup_handler.go:104-110 会因 Content-Type 不符直接 400。
  const isForm = typeof FormData !== 'undefined' && init.body instanceof FormData
  const isBlob = typeof Blob !== 'undefined' && init.body instanceof Blob
  const res = await fetch(toAbsolute(path), {
    credentials: 'include', // Cookie hg_sess（HttpOnly）与 Bearer 双通道
    headers: {
      ...(isForm || isBlob ? {} : { 'Content-Type': 'application/json' }),
      ...(init.headers ?? {}),
    },
    ...init,
  })
  if (res.status === 204) return undefined as T
  const text = await res.text()
  let body: unknown = null
  if (text) {
    try {
      body = JSON.parse(text)
    } catch {
      body = text
    }
  }
  if (!res.ok) {
    const err = (body as { error?: { code?: string; message?: string; trace_id?: string } })?.error
    throw new ApiError(
      err?.message || `请求失败（HTTP ${res.status}）`,
      err?.code || 'http_error',
      res.status,
      err?.trace_id || '',
    )
  }
  return body as T
}

/* ---------- 二进制流（B-2：制品下载 / 会话导出 / 备份恢复） ----------
 * 铁律：下载类端点返回的是 application/zip 或任意二进制，**绝不能走 request<T>()**，
 * 否则 JSON.parse 失败会把内容当成字符串 body 返回并污染 Blob。
 * 失败时后端仍回 JSON 错误体（{error:{code,message,trace_id}}），必须原样解析成 ApiError 抛出。
 */

/** 一次二进制下载的结果（Blob + 后端在 Content-Disposition 声明的文件名）。 */
export interface DownloadResult {
  blob: Blob
  filename: string
}

/**
 * 从备份 zip 恢复的结果（POST /data/restore）。
 * 字段**逐字对齐**后端 `internal/service/backup_service.go:112-118` 的 RestoreResult：
 * 服务端只做「校验 + 暂存」，**不覆盖运行中的 data/**，实际替换需停机运维，
 * 故没有 ok/restored 之类字段，`requires_restart` 恒为 true。
 */
export interface RestoreResult {
  /** 解压后的暂存目录绝对路径（供运维停机替换） */
  staging_path: string
  /** 成功解压的条目数 */
  entries: number
  /** 解压总字节数 */
  bytes: number
  /** 被跳过的条目名（体积/类型过滤） */
  skipped?: string[]
  /** 恒为 true：必须停机替换暂存目录，服务不会自动生效 */
  requires_restart: boolean
  /** 后端附带的启用步骤说明 */
  note?: string
}

/** 从 Content-Disposition 取文件名：优先 RFC 5987 的 filename*=UTF-8''xxx，其次 filename="xxx"。 */
function filenameFrom(res: Response, fallback: string): string {
  const cd = res.headers.get('Content-Disposition') ?? ''
  const star = /filename\*\s*=\s*(?:UTF-8|utf-8)''([^;]+)/i.exec(cd)
  if (star?.[1]) {
    try {
      return decodeURIComponent(star[1].trim().replace(/^"|"$/g, ''))
    } catch {
      /* 解码失败则回退下一个分支 */
    }
  }
  const plain = /filename\s*=\s*"?([^";]+)"?/i.exec(cd)
  if (plain?.[1]) return plain[1].trim()
  return fallback
}

/** 触发浏览器下载。返回创建的 objectURL，调用方用 revokeObjectURL 释放。 */
function triggerDownload(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.rel = 'noopener'
  a.style.display = 'none'
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  // 立即 revoke 在部分浏览器会中断下载，延后释放
  setTimeout(() => URL.revokeObjectURL(url), 10_000)
}

/** 二进制 GET/POST：成功返回 Blob + 文件名；失败按 JSON 错误体抛 ApiError（不吞错误）。 */
async function requestBlob(
  path: string,
  init: RequestInit = {},
  fallbackName = 'download.bin',
): Promise<DownloadResult> {
  const res = await fetch(toAbsolute(path), {
    credentials: 'include',
    ...init,
    headers: { ...authHeaders(), ...(init.headers ?? {}) },
  })
  if (!res.ok) {
    const text = await res.text()
    let body: unknown = null
    if (text) {
      try {
        body = JSON.parse(text)
      } catch {
        body = text
      }
    }
    const err = (body as { error?: { code?: string; message?: string; trace_id?: string } })?.error
    const plain = typeof body === 'string' && body.trim() ? body.trim() : ''
    throw new ApiError(
      err?.message || plain || `请求失败（HTTP ${res.status}）`,
      err?.code || 'http_error',
      res.status,
      err?.trace_id || '',
    )
  }
  return { blob: await res.blob(), filename: filenameFrom(res, fallbackName) }
}

const bearer = (token: string): Record<string, string> => ({ Authorization: `Bearer ${token}` })

// 尽量使用 Cookie 会话；内存令牌用于登录后立即调用（Bearer 优先）
let memoryToken = ''
export function setApiToken(token: string): void {
  memoryToken = token
}
function authHeaders(): Record<string, string> {
  return memoryToken ? bearer(memoryToken) : {}
}

/* ============ DTO → UI 映射 ============ */

// UI 侧只区分两档（confirm / full）；服务端 readonly 在此按更保守的 confirm 展示，
// 真实审批与工具执行仍由服务端按 access_mode 原值裁决（UI 不做安全判定）。
const toAccessMode = (v: string): AccessMode => (v === 'full' ? 'full' : 'confirm')

const toUiRunStatus = (s: ApiRunStatus): RunStatus => s as RunStatus

export function toUiSession(s: ApiSession): Session {
  return {
    id: s.id,
    title: s.title,
    projectId: s.project_id || undefined,
    // 执行面归属：服务端权威字段，前端不再靠 workspace_path 反查猜测。
    workspaceId: s.workspace_id || undefined,
    workspacePath: s.workspace_path || undefined,
    accessMode: toAccessMode(s.access_mode),
    modelRef: s.model_ref,
    updatedAt: s.updated_at,
    running: s.last_run_status === 'running' || s.last_run_status === 'pending',
    archivedAt: s.archived_at,
  }
}

// 项目 DTO → UI：服务端是唯一权威源，defaultAccessMode 只做展示与项目设置回填，
// **绝不用它推导新会话的 accessMode**（裁决权在服务端，避免前后端两套默认值逻辑）。
export function toUiProject(p: ApiProject): Project {
  return {
    id: p.id,
    name: p.name,
    path: p.path,
    pinned: p.pinned,
    defaultAccessMode: toAccessMode(p.default_access_mode),
  }
}

export function toUiProjectRule(r: ApiProjectApprovalRule): ProjectApprovalRule {
  return {
    id: r.id,
    projectId: r.project_id,
    toolName: r.tool_name,
    pattern: r.pattern,
    // createdAt 统一为毫秒时间戳：与前端既有 ApprovalRule 形态一致（两接口可直接互赋）
    createdAt: r.created_at,
  }
}

export function toUiRun(r: ApiRun, sessionId: string): Run {
  return {
    id: r.id,
    sessionId,
    status: toUiRunStatus(r.status),
    startedAt: r.started_at,
    endedAt: r.ended_at,
    tokensIn: r.input_tokens,
    tokensOut: r.output_tokens,
    errorCode: r.error_code,
    errorMessage: r.error_message,
  }
}

/** 会话内按 role 累计轮次（B-1 近似实现：user 消息开启新一轮；B-2 改为读服务端轮次字段） */
let turnCounter = 0

/**
 * 历史回放（B3）：DB 里 assistant 的 `tool_calls` 与 tool 消息（`role='tool'` +
 * `tool_call_id` + `content=ResultJSON`）是分离的两条记录，必须归并后工具卡片才有
 * 结果与逐行 diff，否则刷新页面后 diff 全部丢失。
 *
 * 提取复用 lib/toolDiff.ts 的 extractFileDiff（与 SSE 实时路径同一个函数）。
 */
function collectToolResults(list: ApiMessage[]) {
  const map = new Map<string, { content: string; diff?: ReturnType<typeof extractFileDiff>; failed: boolean }>()
  for (const m of list) {
    if (m.role !== 'tool' || !m.tool_call_id) continue
    // content 同样是 ResultJSON 整包：展示文本取 content 字段，diff 取 meta.data.diff
    const shown = extractResultContent(m.content)
    map.set(m.tool_call_id, {
      content: shown,
      diff: extractFileDiff(m.content),
      failed: shown.startsWith('工具执行失败'),
    })
  }
  return map
}

/**
 * tool_calls 条目归一化。
 *
 * 后端 `internal/llm/provider.go` 的 `ToolCall{ID, Name, Arguments}` **没有 json tag**，
 * 经 `json.Marshal` 落库后是 `{"ID":..,"Name":..,"Arguments":{..}}`（大写）；
 * 而 OpenAI 兼容上游给的是 `{id, function:{name, arguments:"<json str>"}}`。
 * 两种形状都必须能吃下，否则历史工具卡片会全部空壳。
 */
function normalizeToolCall(raw: unknown): { id: string; name: string; args: Record<string, unknown> } {
  const tc = (raw ?? {}) as {
    ID?: string; Name?: string; Arguments?: unknown
    id?: string; name?: string; arguments?: unknown
    function?: { name?: string; arguments?: unknown }
  }
  const id = tc.ID ?? tc.id ?? ''
  const name = tc.Name ?? tc.name ?? tc.function?.name ?? ''
  const rawArgs = tc.Arguments ?? tc.arguments ?? tc.function?.arguments
  const args = typeof rawArgs === 'string'
    ? parseArgsOrEmpty(rawArgs)
    : typeof rawArgs === 'object' && rawArgs !== null
      ? (rawArgs as Record<string, unknown>)
      : {}
  return { id, name, args }
}

export function toUiMessages(list: ApiMessage[], sessionId: string): Message[] {
  turnCounter = 0
  const toolResults = collectToolResults(list)
  const out: Message[] = []
  for (const m of list) {
    if (m.role === 'user') turnCounter += 1
    // 工具结果消息一律不进对话轨：其结果只通过 ToolCard 呈现（在 ProcessDrawer 内）。
    //
    // 对齐老版本桌面端 MessageItem.tsx 的 `if (msg.role === "tool") return null`：
    // 这里是**无条件**跳过，而非「tool_call_id 存在且已收集到结果才跳过」。
    // 旧写法是三条件与，任一不满足（历史数据缺 tool_call_id、结果未收集到、
    // 异常中断导致配不上对）就会漏过过滤 → 该消息的 content 是整包工具输出 JSON
    // （实测 4274 字符）→ 被 MessageItem 当正文渲染 → 视觉上表现为
    // 「工具卡片在执行过程外面又显示了一份」。结构性保证不能依赖数据完整性。
    if (m.role === 'tool') continue
    out.push({
      id: m.id,
      sessionId,
      runId: m.run_id,
      seq: m.seq,
      turnSeq: turnCounter,
      role: m.role,
      content: m.content,
      reasoning: m.reasoning,
      toolCallId: m.tool_call_id,
      toolCalls: Array.isArray(m.tool_calls)
        ? (m.tool_calls as unknown[]).map((raw) => {
            const { id, name, args } = normalizeToolCall(raw)
            const done = toolResults.get(id)
            return {
              id,
              name,
              args,
              // 有对应 tool 消息即已终态；无则保持 pending（不假装成功）
              status: (done ? (done.failed ? 'error' : 'ok') : 'pending') as ToolStatus,
              risk: 'write' as Risk,
              ...(done ? { result: done.content } : {}),
              // 只认 meta.data.diff；无该字段（端侧执行 / 只读工具）保持 undefined
              ...(done?.diff ? { diff: done.diff } : {}),
            } satisfies ToolCall
          })
        : undefined,
      createdAt: m.created_at,
      tokens: m.token_count || undefined,
      // 服务端消息级回滚（S10）置灰的消息：保留可见、上下文组装已排除（hidden）
      ...(m.hidden ? { reverted: true, revertedAt: m.created_at } : {}),
    })
  }
  return out
}

/** tool_calls 里 arguments 可能是 JSON 原文串（OpenAI 兼容格式），解析失败按空对象处理。 */
function parseArgsOrEmpty(raw: string): Record<string, unknown> {
  try {
    const v = JSON.parse(raw) as unknown
    return typeof v === 'object' && v !== null ? (v as Record<string, unknown>) : {}
  } catch {
    return {}
  }
}

/**
 * 工作空间 DTO → UI：**唯一的字段映射入口**（store / 组件里严禁手写映射）。
 *
 * 映射要点（后端 snake_case 派生字段 vs UI 既有形态）：
 *   - online / kind / name / id 直传（online 是服务层实时派生，不是库里字段）；
 *   - builtin：**按 id === 'ws-server' 判定**，不再用 `device_id === ''`。
 *     旧口径把「手工建的无设备空间」也当成内置空间 → 恒在线可执行（D1 已修正）。
 *   - toolPolicy：**直传服务端权威值**，前端绝不按 online/kind 自己推导；
 *   - limbCount：后端没有「肢体数量」概念，UI 侧固定 0（不虚构能力数）；
 *   - lastSeenAt：优先用设备真实 last_seen_at，缺失时退化到 updated_at 近似。
 */
export function toUiWorkspace(w: WorkspaceDTO): Workspace {
  return {
    id: w.id,
    name: w.name,
    kind: w.kind as WorkspaceKind,
    online: w.online,
    limbCount: 0,
    lastSeenAt: w.last_seen_at ?? (w.updated_at || undefined),
    builtin: w.id === 'ws-server',
    deviceId: w.device_id || undefined,
    deviceName: w.device_name || undefined,
    deviceKind: w.device_kind || undefined,
    bound: w.bound,
    deviceMissing: w.device_missing,
    deviceRevoked: w.device_revoked,
    // 服务端未下发（旧后端）时退化为 none：宁可少给能力，也不能凭空放行工具。
    toolPolicy: (w.tool_policy as ToolPolicy) ?? 'none',
    policyReason: w.policy_reason || undefined,
  }
}

/* ============ real 实现（B-2 起逐端点启用） ============ */

/**
 * 把「可能是 JSON 字符串、也可能是对象」的参数归一成对象。
 *
 * 服务端不同事件的 args 形状不一致：`tool.started` 下发对象，而 `approval.request`
 * 为兼容既有 payload 形状下发 **JSON 字符串**。这里集中归一，
 * 避免每个消费点各写一套 `typeof === 'string'` 判断（也避免像旧版那样恒取空）。
 */
function parseArgsField(v: unknown): Record<string, unknown> {
  if (v && typeof v === 'object') return v as Record<string, unknown>
  if (typeof v !== 'string' || v.trim() === '') return {}
  try {
    const parsed: unknown = JSON.parse(v)
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, unknown>) : {}
  } catch {
    return {}
  }
}

/** SSE data（即 events.Event.payload）→ UI 流式事件 */
function mapSseToStreamEvent(type: string, data: string): StreamEvent | null {
  let payload: Record<string, unknown> = {}
  try {
    payload = data ? (JSON.parse(data) as Record<string, unknown>) : {}
  } catch {
    return null
  }
  switch (type) {
    case 'message.start':
      // 消息壳由前端在本地先建，无需额外动作
      return null
    case 'message.delta':
      return { type: 'delta', text: String(payload.delta ?? '') }
    case 'message.reasoning':
      // 思考过程增量（reasoning_content）：与 delta 同语义，前端累积到当前步消息
      return { type: 'reasoning', text: String(payload.delta ?? '') }
    case 'message.end':
      return {
        type: 'step',
        tokens: Number(payload.token_count ?? 0),
        // 终值以 message.end 为准（delta 只是预览，可能因重连缺失）
        reasoning: typeof payload.reasoning === 'string' ? payload.reasoning : undefined,
      }
    case 'tool.started':
      return {
        type: 'tool_start',
        call: {
          id: String(payload.tool_event_id ?? ''),
          name: String(payload.tool_name ?? ''),
          args: (payload.args as Record<string, unknown>) ?? {},
          status: 'running' as ToolStatus,
          risk: (payload.risk as Risk) ?? 'write',
        },
      }
    case 'tool.progress':
      // 实时增量输出（需求 §10-4）：接到运行中的工具卡片输出区，最终结果仍以 tool.result 为准
      return {
        type: 'tool_progress',
        callId: String(payload.tool_event_id ?? ''),
        chunk: String(payload.chunk ?? ''),
      }
    case 'tool.result': {
      // 逐行 diff 不在事件顶层，而是藏在 result 字符串的 meta.data.diff（见 lib/toolDiff.ts）；
      // 提取失败/形状不符一律 undefined，绝不伪造。
      // 展示文本走 extractResultContent：real 模式 result 是整包 ResultJSON（含 meta），
      // 直接铺到卡片上会带出 meta 噪声。
      const raw = typeof payload.result === 'string' ? payload.result : JSON.stringify(payload.result ?? '')
      const status = String(payload.status ?? '')
      const shown = extractResultContent(raw)
      return {
        type: 'tool_result',
        callId: String(payload.tool_event_id ?? ''),
        // 失败/超时事件不带 result，只带 error：如实把错误文案作为结果呈现
        result: shown || String(payload.error ?? ''),
        diff: extractFileDiff(raw),
        durationMs: 0,
        skipped: status === 'denied' ? true : undefined,
        failed: status === 'error' || status === 'timeout' ? true : undefined,
      }
    }
    case 'approval.request':
      // 审批闸门已阻塞在服务端（Run=waiting_approval），前端弹出 ApprovalBar
      return {
        type: 'approval',
        approval: {
          id: String(payload.approval_id ?? ''),
          runId: String(payload.run_id ?? ''),
          toolCallId: String(payload.tool_event_id ?? ''),
          toolName: String(payload.tool_name ?? ''),
          risk: (payload.risk as Risk) ?? 'write',
          // 服务端 args 是 **JSON 字符串**（与 tool.started 的 args 形状不同）：
          // 直接当对象取会恒为空，导致审批条看不到「要放行什么参数」。
          args: parseArgsField(payload.args),
          summary: String(payload.summary ?? ''),
          createdAt: Date.now(),
        },
      }
    case 'approval.decision':
      // 决策结果（含超时视为拒绝）由随后的 run.status 事件收口，此处不重复驱动 UI
      return null
    case 'resync':
      // 服务端补发完成：由 onEvent 外层触发 onResync 拉消息快照
      return null
    case 'run.status': {
      const status = String(payload.status ?? '')
      if (status === 'completed') {
        // usage 与 runs 表 AddRunUsage 同源同值（P1）：真实输入/输出 token，
        // 供用量条与上下文水位（上游未回 usage 时为 0，UI 按估算口径展示）
        return {
          type: 'done',
          tokensIn: Number(payload.input_tokens ?? 0),
          tokensOut: Number(payload.output_tokens ?? 0),
        }
      }
      if (status === 'failed' || status === 'cancelled') {
        return {
          type: 'error',
          code: String(payload.error_code ?? status),
          message: String(payload.error_message ?? 'Run 已结束'),
        }
      }
      return null
    }
    case 'error':
      return { type: 'error', code: String(payload.code ?? 'error'), message: String(payload.message ?? '') }
    default:
      return null
  }
}

/**
 * 提交审批决策（B-3）：POST /sessions/{sid}/runs/{rid}/approvals/{aid}/decide
 *
 * 决策后服务端解除闸门阻塞，Run 继续在**同一条事件流**上推送。
 * 实现收敛在下方 `realApi.decideApproval`（唯一入口）。
 */

export const realApi: ApiClient = {
  mode: 'real',

  /**
   * 提交审批决策（B-3）：POST /sessions/{sid}/runs/{rid}/approvals/{aid}/decide
   *
   * 决策后服务端解除闸门阻塞，Run 继续在**同一条事件流**上推送（订阅不关闭）。
   * 失败（409 已被决策 / 404 审批不存在）由调用方决定是否提示；不在此静默吞掉。
   */
  async decideApproval(sessionId, runId, approvalId, decision, reason) {
    await request<void>(API_PATHS.approvalDecide(sessionId, runId, approvalId), {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify({ decision, reason: reason ?? '' }),
    })
  },

  async me() {
    try {
      return await request<User>(API_PATHS.authMe, { headers: authHeaders() })
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) return null
      throw e
    }
  },
  async login(req) {
    const res = await request<LoginResponse>(API_PATHS.authLogin, {
      method: 'POST',
      body: JSON.stringify(req),
    })
    setApiToken(res.token)
    return res
  },
  async ssoLogin(req) {
    const res = await request<{ user: User }>(API_PATHS.authSSO, {
      method: 'POST',
      body: JSON.stringify(req),
    })
    // 凭据走 Set-Cookie（hg_sess），不落前端内存令牌
    return res
  },
  async exchangeWebviewTicket(ticket) {
    // 方案 C 的 Web 侧落点：fragment 里的 ticket 由浏览器读出后在这里兑换，
    // 服务端凭 ticket 下发 `Set-Cookie: hg_sess=...`（token kind=webview，只读会话）。
    // request() 已默认 credentials:'include'，此处不得重复声明。
    const body: WebviewExchangeRequest = { ticket }
    await request<WebviewExchangeResponse>(API_PATHS.authWebviewExchange, {
      method: 'POST',
      body: JSON.stringify(body),
    })
  },
  async changePassword(req) {
    await request<void>(API_PATHS.authPassword, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(req),
    })
  },
  async logout() {
    await request<void>(API_PATHS.authLogout, { method: 'POST', headers: authHeaders() })
    setApiToken('')
  },

  async listSessions(params) {
    const q = new URLSearchParams()
    if (params?.limit) q.set('limit', String(params.limit))
    if (params?.offset) q.set('offset', String(params.offset))
    const path = q.toString() ? `${API_PATHS.sessions}?${q}` : API_PATHS.sessions
    const res = await request<{ items: ApiSession[] }>(path, { headers: authHeaders() })
    return (res.items ?? []).map(toUiSession)
  },
  async getSession(id) {
    return toUiSession(await request<ApiSession>(API_PATHS.session(id), { headers: authHeaders() }))
  },
  async createSession(input) {
    // project_id 与 access_mode 只在存在时发送：不传 access_mode 即由服务端按项目默认权限裁决，
    // 前端不复刻裁决逻辑（单一权威源）。
    const body: Record<string, unknown> = {
      title: input.title,
      model_ref: input.modelRef,
      workspace_path: input.workspacePath,
    }
    if (input.projectId) body.project_id = input.projectId
    // workspace_id 决定能力天花板：必须显式发送，否则会话落入「纯对话」失去执行面归属。
    if (input.workspaceId) body.workspace_id = input.workspaceId
    if (input.accessMode) body.access_mode = input.accessMode
    return toUiSession(
      await request<ApiSession>(API_PATHS.sessions, { method: 'POST', headers: authHeaders(), body: JSON.stringify(body) }),
    )
  },
  async renameSession(id, title) {
    await request<void>(API_PATHS.session(id), {
      method: 'PATCH',
      headers: authHeaders(),
      body: JSON.stringify({ title }),
    })
  },
  async setSessionModel(id, modelRef) {
    await request<void>(API_PATHS.session(id), {
      method: 'PATCH',
      headers: authHeaders(),
      body: JSON.stringify({ model_ref: modelRef }),
    })
  },
  async setSessionAccessMode(id, mode) {
    await request<void>(API_PATHS.session(id), {
      method: 'PATCH',
      headers: authHeaders(),
      body: JSON.stringify({ access_mode: mode }),
    })
  },
  async archiveSession(id) {
    await request<void>(API_PATHS.sessionArchive(id), { method: 'POST', headers: authHeaders() })
  },
  async unarchiveSession(id) {
    await request<void>(API_PATHS.sessionUnarchive(id), { method: 'POST', headers: authHeaders() })
  },
  async deleteSession(id) {
    await request<void>(API_PATHS.session(id), { method: 'DELETE', headers: authHeaders() })
  },
  async rollbackToMessage(sessionId, messageSeq) {
    const rb = await request<{ id: string }>(API_PATHS.rollback(sessionId), {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify({ mode: 'message', message_seq: messageSeq }),
    })
    return rb.id
  },
  async reapplyRollback(sessionId, rollbackId) {
    await request<void>(API_PATHS.rollbackReapply(sessionId, rollbackId), {
      method: 'POST',
      headers: authHeaders(),
    })
  },
  async listRollbacks(sessionId) {
    const res = await request<{ items: Rollback[] }>(API_PATHS.rollbacks(sessionId), {
      headers: authHeaders(),
    })
    return res.items ?? []
  },
  async listSessionSnapshots(sessionId) {
    const res = await request<{ items: Snapshot[] }>(API_PATHS.sessionSnapshots(sessionId), {
      headers: authHeaders(),
    })
    return res.items ?? []
  },

  // ---- 项目域（real：服务端为准） ----
  async listProjects() {
    const res = await request<{ items: ApiProject[] }>(API_PATHS.projects, { headers: authHeaders() })
    return (res.items ?? []).map(toUiProject)
  },
  async createProject(input) {
    const body: Record<string, unknown> = { name: input.name, path: input.path }
    if (input.defaultAccessMode) body.default_access_mode = input.defaultAccessMode
    const p = await request<ApiProject>(API_PATHS.projects, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(body),
    })
    return toUiProject(p)
  },
  async updateProject(id, patch) {
    // 只发送传入字段：服务端对缺省字段按「保留原值」处理（尤其 default_access_mode 不能被误重置）
    const body: Record<string, unknown> = {}
    if (patch.name !== undefined) body.name = patch.name
    if (patch.defaultAccessMode !== undefined) body.default_access_mode = patch.defaultAccessMode
    if (patch.pinned !== undefined) body.pinned = patch.pinned
    await request<void>(API_PATHS.project(id), {
      method: 'PATCH',
      headers: authHeaders(),
      body: JSON.stringify(body),
    })
  },
  async deleteProject(id) {
    await request<void>(API_PATHS.project(id), { method: 'DELETE', headers: authHeaders() })
  },
  // ---- 工作空间域（real：服务端为准，router.go:67-71） ----
  async listWorkspaces() {
    // 后端 handler 用 map[string]any{"items": items} 包裹（workspace_handler.go:48）
    const res = await request<{ items: WorkspaceDTO[] }>(API_PATHS.workspaces, { headers: authHeaders() })
    return (res.items ?? []).map(toUiWorkspace)
  },
  async createWorkspace(input) {
    const body: Record<string, unknown> = { name: input.name, kind: input.kind }
    if (input.deviceId) body.device_id = input.deviceId
    if (input.rootPath) body.root_path = input.rootPath
    const w = await request<WorkspaceDTO>(API_PATHS.workspaces, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(body),
    })
    return toUiWorkspace(w)
  },
  async updateWorkspace(id, patch) {
    // 只发送传入字段（真 PATCH 语义）；**绝不发送 kind**（服务端仅接受与原值相同，不同即 400）
    const body: Record<string, unknown> = {}
    if (patch.name !== undefined) body.name = patch.name
    if (patch.deviceId !== undefined) body.device_id = patch.deviceId
    if (patch.rootPath !== undefined) body.root_path = patch.rootPath
    const w = await request<WorkspaceDTO>(API_PATHS.workspace(id), {
      method: 'PATCH',
      headers: authHeaders(),
      body: JSON.stringify(body),
    })
    return toUiWorkspace(w)
  },
  async deleteWorkspace(id, opts) {
    // migrateRefs=true：服务端事务内把该空间的会话/项目迁回内置 ws-server 后物理删除。
    // 不带该参数则维持严格模式（有引用即 409），错误交由调用方提示。
    const q = opts?.migrateRefs ? '?migrate_refs=true' : ''
    await request<void>(`${API_PATHS.workspace(id)}${q}`, { method: 'DELETE', headers: authHeaders() })
  },
  async listProjectRules(projectId) {
    const res = await request<{ items: ApiProjectApprovalRule[] }>(API_PATHS.projectRules(projectId), {
      headers: authHeaders(),
    })
    return (res.items ?? []).map(toUiProjectRule)
  },
  async addProjectRule(projectId, input) {
    const r = await request<ApiProjectApprovalRule>(API_PATHS.projectRules(projectId), {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify({ tool_name: input.toolName, pattern: input.pattern }),
    })
    return toUiProjectRule(r)
  },
  async removeProjectRule(projectId, ruleId) {
    await request<void>(API_PATHS.projectRule(projectId, ruleId), {
      method: 'DELETE',
      headers: authHeaders(),
    })
  },

  async listMessages(sessionId, afterSeq) {
    const q = afterSeq ? `?after_seq=${afterSeq}` : ''
    const res = await request<{ items: ApiMessage[] }>(`${API_PATHS.messages(sessionId)}${q}`, {
      headers: authHeaders(),
    })
    return toUiMessages(res.items ?? [], sessionId)
  },

  async startRun(opts) {
    const created = await request<CreateRunResponse>(API_PATHS.runs(opts.sessionId), {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify({ input: opts.input, model_ref: opts.modelRef }),
    })
    // 创建后立刻订阅（fromSeq 缺省 = 从头补发，新建 Run 不能漏首屏事件）
    return realApi.attachRun({ ...opts, runId: created.run_id })
  },
  /**
   * 订阅一个已存在 Run 的事件流（刷新恢复的入口）。
   *
   * 与 startRun 共用同一份 SseClient 装配逻辑；差异仅在「谁给的 runId」与 fromSeq。
   *
   * ⚠️ 审批决策**不放这里**：曾经的 `resume()` 依赖闭包变量 approvalId，
   * 而该变量只在收到 approval.request 事件时赋值；刷新恢复用 fromSeq=-1 不补发历史，
   * 于是恢复出的审批条「能看见、点不动」（resume 静默 return）。审批决策改由
   * chatStore 以 store 里的 approval.id 直接调 `decideApproval`，事件与恢复两条路径合一。
   */
  attachRun(opts) {
    const sse = new SseClient({
      url: toAbsolute(API_PATHS.runEvents(opts.sessionId, opts.runId)),
      // fromSeq < 0 = 「只要新事件、绝不补发历史」（服务端 bus.go 的 Subscribe 语义）。
      // 用一个足够大的 last_event_id 表达：刷新恢复时前端已有 DB 快照全文，
      // 若补发历史 message.delta，正文会被重复追加一遍。
      lastEventId: opts.fromSeq !== undefined && opts.fromSeq < 0 ? String(Number.MAX_SAFE_INTEGER) : undefined,
      onEvent: (ev) => {
        // 真实 lastEventId 游标上报（无 id 帧不报，不做估算）
        if (ev.id) {
          const seq = Number(ev.id)
          if (Number.isFinite(seq)) opts.onSseEventSeq?.(seq)
        }
        if (ev.type === 'resync') {
          // 服务端补发完成：以 DB 为准重拉消息快照（delta 只是预览）
          opts.onResync?.()
          return
        }
        const mapped = mapSseToStreamEvent(ev.type, ev.data)
        if (!mapped) return
        opts.onEvent(mapped)
      },
      onResync: () => {
        // 断线重连：SSE 已按 Last-Event-ID 续传，额外拉一次快照兜底
        opts.onResync?.()
      },
      // 真实状态机外泄（顶栏 SseStatus 唯一真实数据源）
      onState: (state, info) => opts.onSseState?.(state, info),
    })
    sse.connect()
    return {
      runId: opts.runId,
      stop: () => {
        sse.close()
        void realApi.stopRun(opts.sessionId, opts.runId)
      },
      reconnect: () => sse.reconnectNow(),
    }
  },
  async getActiveRun(sessionId) {
    const res = await request<ActiveRunDTO>(API_PATHS.activeRun(sessionId), { headers: authHeaders() })
    return res.run_id ? res : null
  },
  async getRun(sessionId, runId) {
    return toUiRun(await request<ApiRun>(API_PATHS.run(sessionId, runId), { headers: authHeaders() }), sessionId)
  },
  async stopRun(sessionId, runId) {
    await request<void>(API_PATHS.runStop(sessionId, runId), { method: 'POST', headers: authHeaders() })
  },
  async sendGuidance(sessionId, runId, input) {
    const msg = await request<{ id: string; seq: number; content: string }>(
      API_PATHS.runGuidance(sessionId, runId),
      { method: 'POST', headers: authHeaders(), body: JSON.stringify({ input }) },
    )
    return msg
  },
  async listToolEvents(sessionId, runId) {
    const res = await request<{ items: ApiToolEvent[] }>(API_PATHS.runToolEvents(sessionId, runId), {
      headers: authHeaders(),
    })
    return res.items ?? []
  },

  async getTools() {
    return request<ToolSnapshot>(API_PATHS.tools, { headers: authHeaders() })
  },
  async getToolsInjection() {
    return request<ToolInjection>(API_PATHS.toolsInjection, { headers: authHeaders() })
  },
  async patchToolTiers(tiers) {
    return request<ToolSnapshot>(API_PATHS.toolsTiers, {
      method: 'PATCH',
      headers: authHeaders(),
      body: JSON.stringify({ tiers }),
    })
  },

  // ---- 核心 SOP（agent-core）：出厂内置 + 运行时覆盖 ----
  /** 读取核心 SOP 生效快照（含 builtin_content，切页签无需再请求）。 */
  async getSOPCore() {
    return request<SOPCore>(API_PATHS.sopCore, { headers: authHeaders() })
  },
  /** 保存自定义核心 SOP（首次保存即 fork，下一轮对话生效）。 */
  async putSOPCore(content: string) {
    return request<SOPCore>(API_PATHS.sopCore, {
      method: 'PUT',
      headers: authHeaders(),
      body: JSON.stringify({ content }),
    })
  },
  /** 重置为出厂内置（删除覆盖层，完整 undo）。 */
  async resetSOPCore() {
    return request<SOPCore>(API_PATHS.sopCore, { method: 'DELETE', headers: authHeaders() })
  },
  /** 生效预览：核心 SOP + 项目级条目 + token 合计。 */
  async getSOPEffective(sessionId?: string) {
    const q = sessionId ? `?session_id=${encodeURIComponent(sessionId)}` : ''
    return request<SOPEffective>(`${API_PATHS.sopEffective}${q}`, { headers: authHeaders() })
  },

  async getMaintenance() {
    return request<Record<string, unknown>>(API_PATHS.maintenance, { headers: authHeaders() })
  },

  // ---- B-4：厂商 / 配置 / 统计 / 日志 ----
  async listProviders() {
    const res = await request<{ items: ProviderDTO[] }>(API_PATHS.providers, { headers: authHeaders() })
    return res.items ?? []
  },
  async createProvider(input) {
    return request<ProviderDTO>(API_PATHS.providers, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(input),
    })
  },
  async updateProviderById(id, input) {
    return request<ProviderDTO>(API_PATHS.provider(id), {
      method: 'PATCH',
      headers: authHeaders(),
      body: JSON.stringify(input),
    })
  },
  async deleteProvider(id) {
    await request<void>(API_PATHS.provider(id), { method: 'DELETE', headers: authHeaders() })
  },
  async testProvider(id: string, input: ProviderTestInput = {}) {
    return request<ProviderTestResult>(API_PATHS.providerTest(id), {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(input),
    })
  },
  async listProviderModels(id: string, input: ProviderModelsInput = {}) {
    return request<ProviderModelsResult>(API_PATHS.providerModels(id), {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(input),
    })
  },
  async revealProviderKey(id) {
    return request<ProviderSecretResult>(API_PATHS.providerSecret(id), {
      method: 'GET',
      headers: authHeaders(),
    })
  },
  async getSettings() {
    const res = await request<{ items: Record<string, unknown> }>(API_PATHS.settings, {
      headers: authHeaders(),
    })
    return res.items ?? {}
  },
  async patchSettings(patch) {
    const res = await request<{ items: Record<string, unknown> }>(API_PATHS.settings, {
      method: 'PATCH',
      headers: authHeaders(),
      body: JSON.stringify(patch),
    })
    return res.items ?? {}
  },
  async getStats() {
    return request<Record<string, unknown>>(API_PATHS.stats, { headers: authHeaders() })
  },
  async getLogs(params) {
    const q = new URLSearchParams()
    if (params?.limit) q.set('limit', String(params.limit))
    if (params?.level) q.set('level', params.level)
    if (params?.from) q.set('from', params.from)
    if (params?.to) q.set('to', params.to)
    if (params?.traceId) q.set('traceId', params.traceId)
    if (params?.keyword) q.set('keyword', params.keyword)
    if (params?.offset) q.set('offset', String(params.offset))
    const path = q.toString() ? `${API_PATHS.logs}?${q}` : API_PATHS.logs
    // ⚠️ 诚实边界：服务端可能尚未返回 total/retention_days 等新字段（渐进升级中），
    // 这里原样透传 undefined，由 UI 决定显示「保留策略未知」还是具体数值，绝不在此编造默认值。
    const res = await request<LogsResponse>(path, { headers: authHeaders() })
    return { ...res, items: res.items ?? [] }
  },

  /**
   * LLM 调用流水（跨会话；session_id 可选过滤）。
   *
   * 与 getLogs 的本质区别：getLogs 读 data/app.log 的 Agent 运维文本日志，
   * 本接口读 logvault 天库的**结构化对话审计**（模型/耗时/token/报文快照）。
   * 两者数据源完全不同，不可互相替代。
   */
  async getAuditLlmCalls(params?: AuditLlmCallsQuery) {
    const q = new URLSearchParams()
    if (params?.from) q.set('from', String(params.from))
    if (params?.to) q.set('to', String(params.to))
    if (params?.session_id) q.set('session_id', params.session_id)
    if (params?.run_id) q.set('run_id', params.run_id)
    if (params?.message_id) q.set('message_id', params.message_id)
    if (params?.trace_id) q.set('trace_id', params.trace_id)
    if (params?.model) q.set('model', params.model)
    if (params?.status) q.set('status', params.status)
    if (params?.keyword) q.set('keyword', params.keyword)
    if (params?.id) q.set('id', params.id)
    if (params?.limit) q.set('limit', String(params.limit))
    if (params?.offset) q.set('offset', String(params.offset))
    const path = q.toString() ? `${API_PATHS.auditLlmCalls}?${q}` : API_PATHS.auditLlmCalls
    const res = await request<AuditLlmCallsResponse>(path, { headers: authHeaders() })
    return { ...res, items: res.items ?? [] }
  },

  /** 工具调用审计（跨会话；天库副本，机器查）。 */
  async getAuditToolEvents(params?: {
    from?: number
    to?: number
    session_id?: string
    run_id?: string
    tool?: string
    status?: string
    keyword?: string
    limit?: number
    offset?: number
  }) {
    const q = new URLSearchParams()
    if (params?.from) q.set('from', String(params.from))
    if (params?.to) q.set('to', String(params.to))
    if (params?.session_id) q.set('session_id', params.session_id)
    if (params?.run_id) q.set('run_id', params.run_id)
    if (params?.tool) q.set('tool', params.tool)
    if (params?.status) q.set('status', params.status)
    if (params?.keyword) q.set('keyword', params.keyword)
    if (params?.limit) q.set('limit', String(params.limit))
    if (params?.offset) q.set('offset', String(params.offset))
    const path = q.toString() ? `${API_PATHS.auditToolEvents}?${q}` : API_PATHS.auditToolEvents
    const res = await request<AuditToolEventsResponse>(path, { headers: authHeaders() })
    return { ...res, items: res.items ?? [] }
  },

  /** 用量聚合（走 rollup_hourly 预聚合，不扫明细；group_by: model|provider|day）。 */
  async getTokenStats(params?: { from?: number; to?: number; group_by?: string }) {
    const q = new URLSearchParams()
    if (params?.from) q.set('from', String(params.from))
    if (params?.to) q.set('to', String(params.to))
    if (params?.group_by) q.set('group_by', params.group_by)
    const path = q.toString() ? `${API_PATHS.tokenStats}?${q}` : API_PATHS.tokenStats
    return request<TokenStatsResult>(path, { headers: authHeaders() })
  },

  /** 日志配置读取（含当前**真实生效**的日志级别）。 */
  async getLogConfig() {
    return request<{
      snapshot_mode: string
      retention_days: number
      level: string
      estimated_daily_mb?: Record<string, number>
      earliest_available_day?: string
      latest_available_day?: string
      total_bytes?: number
      dropped?: number
      written?: number
    }>(API_PATHS.logConfig, { headers: authHeaders() })
  },

  // ---- 阶段 C：设备与多端协同 ----
  async createDevicePairCode(req) {
    return request<PairCodeDTO>(API_PATHS.devicePairCodes, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(req ?? {}),
    })
  },
  async listDevices() {
    return request<DeviceListResponse>(API_PATHS.devices, { headers: authHeaders() })
  },
  async revokeDevice(id) {
    await request<void>(API_PATHS.device(id), { method: 'DELETE', headers: authHeaders() })
  },
  async getSessionLease(sessionId) {
    return request<SessionLeaseDTO>(API_PATHS.sessionLease(sessionId), { headers: authHeaders() })
  },

  // ---- 阶段 E：MCP Server 管理（路径与 internal/api/router.go:110-119 一一对应） ----
  async listMcpServers() {
    return request<MCPListResponseDTO>(API_PATHS.mcpServers, { headers: authHeaders() })
  },
  async getMcpServer(id) {
    return request<MCPServerViewDTO>(API_PATHS.mcpServer(id), { headers: authHeaders() })
  },
  async createMcpServer(input, start) {
    // start=true 为后端 query 参数语义：落库后立刻建连（market_handler.go:54 注释）
    const path = start ? `${API_PATHS.mcpServers}?start=true` : API_PATHS.mcpServers
    return request<MCPServerViewDTO>(path, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(input),
    })
  },
  async updateMcpServer(id, input) {
    return request<MCPServerViewDTO>(API_PATHS.mcpServer(id), {
      method: 'PUT',
      headers: authHeaders(),
      body: JSON.stringify(input),
    })
  },
  async deleteMcpServer(id) {
    await request<void>(API_PATHS.mcpServer(id), { method: 'DELETE', headers: authHeaders() })
  },
  async testMcpServer(input) {
    return request<MCPTestResultDTO>(API_PATHS.mcpTest, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(input),
    })
  },
  async reloadMcpServer(id) {
    await request<void>(API_PATHS.mcpReload(id), { method: 'POST', headers: authHeaders() })
  },
  async enableMcpServer(id) {
    await request<void>(API_PATHS.mcpEnable(id), { method: 'POST', headers: authHeaders() })
  },
  async disableMcpServer(id) {
    await request<void>(API_PATHS.mcpDisable(id), { method: 'POST', headers: authHeaders() })
  },
  async getMcpServerHealth(id) {
    return request<MCPHealthDTO>(API_PATHS.mcpHealth(id), { headers: authHeaders() })
  },

  // ---- 阶段 E：本地工具包（路径与 internal/api/router.go:121-126 一一对应） ----
  async listPackages() {
    return request<PackageListResponseDTO>(API_PATHS.packages, { headers: authHeaders() })
  },
  async installPackage(req) {
    return request<PackageInstallResultDTO>(API_PATHS.packagesInstall, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(req),
    })
  },
  async uninstallPackage(id) {
    await request<void>(API_PATHS.package(id), { method: 'DELETE', headers: authHeaders() })
  },
  async rollbackPackage(id) {
    return request<PackageViewDTO>(API_PATHS.packageRollback(id), {
      method: 'POST',
      headers: authHeaders(),
    })
  },
  async enablePackage(id) {
    await request<void>(API_PATHS.packageEnable(id), { method: 'POST', headers: authHeaders() })
  },
  async disablePackage(id) {
    await request<void>(API_PATHS.packageDisable(id), { method: 'POST', headers: authHeaders() })
  },

  // ---- P1：Webhook 事件订阅（路径与 internal/api/router.go:57-62 一一对应） ----
  // ⚠️ 后端 h.d.Webhooks == nil 时返回 code=unavailable，此处**不做任何静默兜底**，
  // 原样抛出 ApiError 让面板展示真实错误文案（严禁回退 mock 数据）。
  async listWebhooks() {
    return request<WebhookListResponseDTO>(API_PATHS.webhooks, { headers: authHeaders() })
  },
  async createWebhook(input) {
    return request<Webhook>(API_PATHS.webhooks, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(input),
    })
  },
  async deleteWebhook(id) {
    await request<void>(API_PATHS.webhook(id), { method: 'DELETE', headers: authHeaders() })
  },
  async enableWebhook(id) {
    await request<void>(API_PATHS.webhookEnable(id), { method: 'POST', headers: authHeaders() })
  },
  async disableWebhook(id) {
    await request<void>(API_PATHS.webhookDisable(id), { method: 'POST', headers: authHeaders() })
  },
  async testWebhook(id) {
    return request<WebhookTestResult>(API_PATHS.webhookTest(id), { method: 'POST', headers: authHeaders() })
  },

  // ---- B-2：制品下载 / 会话导出 / 全量备份恢复 ----
  // 路径严格对齐后端契约（internal/api 新增路由），**不自创端点、不猜字段**。
  // 下载成功即触发浏览器下载；失败原样抛 ApiError 让调用方展示后端错误文案。
  async downloadArtifact(id) {
    const res = await requestBlob(API_PATHS.artifact(id), { method: 'GET' }, `artifact-${id}`)
    triggerDownload(res.blob, res.filename)
    return res
  },
  async exportSession(sessionId) {
    const res = await requestBlob(
      `${API_PATHS.sessionExport(sessionId)}?format=zip`,
      { method: 'GET' },
      `session-${sessionId}.zip`,
    )
    triggerDownload(res.blob, res.filename)
    return res
  },
  async backupData() {
    const res = await requestBlob(API_PATHS.dataBackup, { method: 'GET' }, 'harness-backup.zip')
    triggerDownload(res.blob, res.filename)
    return res
  },
  async restoreData(file) {
    // ⚠️ 后端 backup_handler.go:104-110 只接受**备份包原始 zip 字节流**
    // （Content-Type: application/zip / application/octet-stream），
    // 明确拒绝 multipart/form-data。故此处必须直接传 File（Blob）本体，
    // 由 request() 的 isBlob 分支让浏览器自动推断 Content-Type。
    return request<RestoreResult>(API_PATHS.dataRestore, {
      method: 'POST',
      body: file,
      headers: { 'Content-Type': 'application/zip' },
    })
  },
}

/* ============ 出口 ============ */

/** 当前生效的客户端：正式版恒为真实后端（mock 通路已整体下线） */
export const api: ApiClient = realApi
