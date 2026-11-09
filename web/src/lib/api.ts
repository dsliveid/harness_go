/**
 * 统一数据访问层（阶段 B-1 新增，demo 无此文件）。
 *
 * 存在意义：让 UI 只依赖一层「同签名」的 ApiClient，
 * 在 `apiMode: 'mock' | 'real'` 之间切换而不改任何组件（阶段 B-2 起逐端点去 mock）。
 *
 * 契约铁律：
 *   - HTTP 类型一律来自 `./apiTypes`（由 `api/openapi.yaml` 唯一定义，禁止自造字段）；
 *   - UI 领域模型来自 `../types`（Session/Message/Run 等），real 实现负责 DTO → UI 映射；
 *   - 错误统一为 `ApiError`（携带 code / status / trace_id，trace_id 与后端日志一致）。
 */

import type { AccessMode, ApprovalRule, Message, Project, ProjectApprovalRule, Run, RunStatus, Session, ToolCall, Risk, ToolStatus, Workspace, WorkspaceKind } from '../types'
import * as mockData from '../mock/data'
import { toolList } from '../mock/settingsData'
import { startMockRun, type Decision, type StreamEvent } from '../mock/stream'
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
  type Webhook,
  type WebhookCreateInput,
  type WebhookListResponseDTO,
  type WebhookTestResult,
  type CreateWorkspaceDTO,
  type UpdateWorkspaceDTO,
  type WorkspaceDTO,
} from './apiTypes'

/* ============ 模式选择 ============ */

export type ApiMode = 'mock' | 'real'

/** 模式持久化键（开发期可在控制台 `localStorage.setItem('harness.apiMode','real')` 切换） */
export const API_MODE_STORAGE_KEY = 'harness.apiMode'

/**
 * 解析当前数据模式：
 *   1. `VITE_API_MODE`（构建期注入，便于 CI/预览强制指定）
 *   2. `localStorage['harness.apiMode']`（运行期手动切换，便于对比 mock 基线）
 *   3. 默认：**一律 real**（dev 与构建产物同口径）。
 *
 * ⚠️ 默认值历史（2026-11 翻转）：dev 曾默认 mock，导致「用前端脚本启动 → 永远是 demo 页面」，
 * 用户无法察觉自己看的是演示数据。现统一默认 real。
 * 要演示数据没有脚本开关（web/run.sh 只服务正式前端），请改用原型入口 demo/run.sh（端口 5610），
 * 或在控制台 `localStorage.setItem('harness.apiMode','mock')` 后刷新。
 */
export function resolveApiMode(): ApiMode {
  const env = (import.meta as unknown as { env?: Record<string, string | boolean | undefined> }).env
  try {
    const fromEnv = env?.VITE_API_MODE
    if (fromEnv === 'real' || fromEnv === 'mock') return fromEnv
    const stored = typeof localStorage !== 'undefined' ? localStorage.getItem(API_MODE_STORAGE_KEY) : null
    if (stored === 'real' || stored === 'mock') return stored
  } catch {
    /* 工作于非浏览器环境（SSR/测试）时忽略 */
  }
  // 默认 real：dev 与单二进制内嵌产物同口径，绝不默认给用户看演示数据
  return 'real'
}

export const apiMode: ApiMode = resolveApiMode()

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

/** 一次 Run 的句柄（mock 与 real 同签名） */
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
  /** 删除工作空间（DELETE /workspaces/{wid} → 204）；仍被会话/项目引用时 409，不级联 */
  deleteWorkspace(id: string): Promise<void>
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

/* ============ mock 实现（行为与 demo 一致） ============ */

const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v)) as T

const mockToolsSnapshot = (): ToolSnapshot => {
  const summaries = toolList.map((t) => ({
    name: t.name,
    namespace: 'builtin',
    tier: 'deferred' as ToolTier,
    default_tier: 'deferred' as ToolTier,
    cost_tokens: 18,
    risk: t.risk as Risk,
    executor: 'server' as const,
    plugin_id: 'mock',
    health: { status: 'available' as const, checked_at: new Date().toISOString() },
  }))
  return {
    total: summaries.length,
    core: [],
    deferred: summaries,
    on_demand: [],
    cost_total: summaries.length * 18,
    cost_core: 0,
    cost_deferred: summaries.length * 18,
    limits: { core_soft: 8, deferred_soft: 60 },
    warnings: [],
    plugins: [{ id: 'mock', name: 'Mock 工具集', version: '0.0.0', kind: 'builtin', default_tier: 'deferred', tools: summaries.map((s) => s.name) }],
    overrides: {},
    generated_at: new Date().toISOString(),
  }
}

/* ============ 阶段 C：设备（mock 样例数据） ============ */

/**
 * mock 设备样例：形态与真实 `GET /api/v1/devices` 一致（不回传令牌摘要）。
 * 仅用于 mock 模式下 UI 自检，**不参与任何真机逻辑**。
 */
function mockDeviceList(): DeviceDTO[] {
  const now = Date.now()
  return [
    {
      id: 'dev-mock-desktop',
      name: '桌面端 · Windows',
      kind: 'desktop',
      platform: 'windows',
      status: 'online',
      capabilities_hash: 'sha256:mock-1',
      protocol_version: 1,
      app_version: '0.1.0',
      paired_at: now - 3_600_000,
      last_seen_at: now - 8_000,
      revoked_at: undefined,
      capabilities: [
        { device_id: 'dev-mock-desktop', name: 'run_command', version: '1', updated_at: now - 8_000 },
        { device_id: 'dev-mock-desktop', name: 'read_file', version: '1', updated_at: now - 8_000 },
      ],
    },
    {
      id: 'dev-mock-mobile',
      name: '移动端 · Android',
      kind: 'mobile',
      platform: 'android',
      status: 'offline',
      capabilities_hash: 'sha256:mock-2',
      protocol_version: 1,
      app_version: '0.1.0',
      paired_at: now - 86_400_000,
      last_seen_at: now - 5_400_000,
      capabilities: [{ device_id: 'dev-mock-mobile', name: 'fetch_web_page', version: '1', updated_at: now - 5_400_000 }],
    },
  ]
}

/* ============ 阶段 E：MCP / 工具包（mock 样例数据） ============ */

/**
 * mock MCP 视图：从 `mock/data.ts` 的 mcpServers 派生，字段与后端
 * `GET /api/v1/mcp/servers`（internal/service/mcp_service.go:51-74 ServerView）逐一对齐。
 * 注意 UI 的 McpServer.tools 是 McpToolInfo[]，后端只回 tool_count/allow_tools，
 * 因此 mock 侧把工具名映射成 McpToolInfo（risk 取自后端 default_risk 口径）。
 */
function mockMcpViews(): MCPServerViewDTO[] {
  return mockData.mcpServers.map((m) => ({
    id: m.id,
    name: m.name,
    transport: m.transport,
    command: m.command,
    args: m.command ? m.command.split(' ').slice(1) : undefined,
    cwd: undefined,
    url: m.url,
    env_keys: [],
    header_keys: [],
    enabled: m.enabled,
    timeout_ms: 30_000,
    max_output: 100_000,
    default_risk: m.tools[0]?.risk ?? 'readonly',
    allow_tools: m.tools.map((t) => t.name),
    deny_tools: [],
    health_interval_s: m.healthIntervalS ?? 60,
    state: m.state === 'starting' ? 'degraded' : m.state,
    calls: m.calls,
    failures: m.failures,
    tool_count: m.tools.length,
    last_error: m.lastError,
  }))
}

/** 入参 → 视图（create/update 的 mock 回包，字段口径同 ServerView）。 */
function mockMcpViewOf(input: MCPServerInputDTO): MCPServerViewDTO {
  return {
    id: '',
    name: input.name,
    transport: input.transport,
    command: input.command,
    args: input.args,
    cwd: input.cwd,
    url: input.url,
    env_keys: Object.keys(input.env ?? {}),
    header_keys: Object.keys(input.headers ?? {}),
    enabled: input.enabled ?? true,
    timeout_ms: input.timeout_ms ?? 30_000,
    max_output: input.max_output ?? 100_000,
    default_risk: input.default_risk ?? 'readonly',
    allow_tools: input.allow_tools ?? [],
    deny_tools: input.deny_tools ?? [],
    health_interval_s: input.health_interval_s ?? 60,
    state: 'disabled',
    calls: 0,
    failures: 0,
    tool_count: 0,
  }
}

/**
 * mock 工具包视图：从 `mock/data.ts` 的 registryItems 中 kind='plugin' 派生，
 * 字段对齐 internal/service/package_service.go:71-83 PackageView。
 */
function mockPackageViews(): PackageViewDTO[] {
  return mockData.registryItems
    .filter((r) => r.kind === 'plugin')
    .map((r) => ({
      id: r.id,
      version: r.version,
      previous_version: undefined,
      name: r.name,
      trust: r.signature,
      namespace: 'demo',
      enabled: !r.installed,
      description: r.description,
      tool_names: [],
      state: r.installed ? 'ready' : 'installed',
      last_error: undefined,
    }))
}

/**
 * mock Webhook 订阅（演示态）：模块级可变数组，让新增/删除/启停在本次会话内自洽。
 * 字段对齐 internal/api/webhook_handler.go:14-21 webhookView（secret 只写不读 → 只有 has_secret）。
 */
const mockWebhookStore: Webhook[] = [
  {
    id: 'wh-demo-1',
    url: 'https://example.com/hooks/harness-go',
    has_secret: true,
    events: ['run.completed', 'run.failed'],
    enabled: true,
    created_at: Date.now() - 3 * 24 * 3_600_000,
  },
  {
    id: 'wh-demo-2',
    url: 'https://internal.example.com/approval-bot',
    has_secret: false,
    events: ['approval.requested'],
    enabled: false,
    created_at: Date.now() - 26 * 3_600_000,
  },
]

/**
 * 工作空间 DTO → UI：**唯一的字段映射入口**（store / 组件里严禁手写映射）。
 *
 * 映射要点（后端 snake_case 派生字段 vs UI 既有形态）：
 *   - online / kind / name / id 直传（online 是服务层实时派生，不是库里字段）；
 *   - builtin：`device_id === ''` 即服务端内置空间（恒在线、不可删除，store 的 removeWorkspace 依赖它）；
 *   - limbCount：后端没有「肢体数量」概念，UI 侧固定 0（不虚构能力数）；
 *   - lastSeenAt：后端无 last_seen_at，用 updated_at（毫秒）近似「最近活跃」。
 */
export function toUiWorkspace(w: WorkspaceDTO): Workspace {
  return {
    id: w.id,
    name: w.name,
    kind: w.kind as WorkspaceKind,
    online: w.online,
    limbCount: 0,
    lastSeenAt: w.updated_at || undefined,
    builtin: w.device_id === '',
  }
}

export const mockApi: ApiClient = {
  mode: 'mock',

  async me() {
    // mock 模式沿用 demo「直接进演示模式」的行为
    return { id: 'mock-user', username: 'demo', display_name: '演示用户', role: 'admin', created_at: 0, updated_at: 0 }
  },
  async login(req) {
    return { user: { id: 'mock-user', username: req.username, display_name: '演示用户', role: 'admin', created_at: 0, updated_at: 0 }, token: 'mock-token' }
  },
  async ssoLogin(req) {
    // mock 模式无服务端会话，退化为按 user_name 直接进入演示态
    const res = await mockApi.login({ username: req.user_name, password: req.pwd ?? '' })
    return { user: res.user }
  },
  async changePassword() {
    // mock 模式的密码校验由 authStore 的演示逻辑承担（localStorage 模拟服务端凭据），
    // 此方法仅在 real 模式被调用，保留空实现以满足接口完整性。
  },
  async logout() {
    /* no-op */
  },

  async listSessions() {
    return clone(mockData.sessions)
  },
  async getSession(id) {
    const found = mockData.sessions.find((s) => s.id === id)
    if (!found) throw new ApiError('会话不存在', 'session.not_found', 404)
    return clone(found)
  },
  async createSession(input) {
    // mock 侧同样遵守「项目默认权限快照一次」语义，保持与 real 行为一致（便于对比调试）。
    const proj = input.projectId ? mockData.projects.find((p) => p.id === input.projectId) : undefined
    const sess: Session = {
      id: `s-mock-${Date.now()}`,
      title: input.title || '新对话',
      projectId: input.projectId || undefined,
      workspacePath: input.workspacePath,
      // 显式值 > 项目默认 > confirm（与服务端 SessionService.Create 裁决链同口径）
      accessMode: input.accessMode ?? proj?.defaultAccessMode ?? 'confirm',
      modelRef: input.modelRef || 'openai/gpt-4o-mini',
      updatedAt: Date.now(),
    }
    mockData.sessions.unshift(sess)
    return clone(sess)
  },
  async renameSession(id, title) {
    const found = mockData.sessions.find((s) => s.id === id)
    if (found) {
      found.title = title
      found.updatedAt = Date.now()
    }
  },
  async setSessionModel(id, modelRef) {
    const found = mockData.sessions.find((s) => s.id === id)
    if (found) {
      found.modelRef = modelRef
      found.updatedAt = Date.now()
    }
  },
  async setSessionAccessMode(id, mode) {
    const found = mockData.sessions.find((s) => s.id === id)
    if (found) {
      found.accessMode = mode
      found.updatedAt = Date.now()
    }
  },
  async archiveSession(id) {
    const found = mockData.sessions.find((s) => s.id === id)
    if (found) found.archivedAt = Date.now()
  },
  async unarchiveSession(id) {
    const found = mockData.sessions.find((s) => s.id === id)
    if (found) found.archivedAt = undefined
  },
  async rollbackToMessage(_sessionId, _messageSeq) {
    // mock 无服务端历史：返回合成回滚 id（reapply 语义由 chatStore 本地状态承担）
    return `rb-mock-${Date.now()}`
  },
  async reapplyRollback() {
    /* mock：本地状态已在 chatStore 内回退 */
  },
  async listRollbacks(_sessionId) {
    // mock 无服务端回滚历史：返回一条样例事件（字段与后端 Rollback 一致）
    return [
      {
        id: 'rb-mock-1',
        mode: 'message',
        target: '12',
        affected: [
          {
            tool_event_id: 'te-mock-1',
            run_id: 'run-mock-1',
            snapshot_before: 'snap-mock-before',
            snapshot_after: 'snap-mock-after',
            paths: ['src/demo.ts'],
          },
        ],
        hidden_from_seq: 13,
        restored: [{ path: 'src/demo.ts', action: 'restored', bytes: 1024 }],
        created_at: Date.now() - 60_000,
        reapplied_at: null,
      },
    ] as Rollback[]
  },
  async listSessionSnapshots(_sessionId) {
    // mock 无服务端快照：返回一条样例锚点（字段与后端 Snapshot 一致）
    return [
      {
        id: 'snap-mock-1',
        session_id: 's-mock-1',
        run_id: 'run-mock-1',
        tool_event_id: 'te-mock-1',
        kind: 'before',
        paths: [{ path: 'src/demo.ts', hash: 'deadbeef', size: 1024 }],
        size: 1024,
        created_at: Date.now() - 120_000,
      },
    ] as Snapshot[]
  },
  async deleteSession(id) {
    const i = mockData.sessions.findIndex((s) => s.id === id)
    if (i >= 0) mockData.sessions.splice(i, 1)
  },

  // ---- 项目域（mock：内存态，语义与 real 对齐） ----
  async listProjects() {
    return clone(mockData.projects)
  },
  async createProject(input) {
    const p: Project = {
      id: `p-mock-${Date.now()}`,
      name: input.name,
      path: input.path,
      // 缺省由「服务端」定 confirm（mock 侧同口径）
      defaultAccessMode: input.defaultAccessMode ?? 'confirm',
    }
    mockData.projects.push(p)
    return clone(p)
  },
  async updateProject(id, patch) {
    const found = mockData.projects.find((p) => p.id === id)
    if (!found) throw new ApiError('项目不存在', 'project.not_found', 404)
    if (patch.name !== undefined) found.name = patch.name
    if (patch.defaultAccessMode !== undefined) found.defaultAccessMode = patch.defaultAccessMode
    if (patch.pinned !== undefined) found.pinned = patch.pinned
  },
  async deleteProject(id) {
    const i = mockData.projects.findIndex((p) => p.id === id)
    if (i >= 0) mockData.projects.splice(i, 1)
    // 级联：清规则 + 会话归属归零（对齐服务端 DeleteProjectCascade）
    for (let j = mockData.approvalRules.length - 1; j >= 0; j -= 1) {
      if (mockData.approvalRules[j].projectId === id) mockData.approvalRules.splice(j, 1)
    }
    mockData.sessions.forEach((s) => {
      if (s.projectId === id) s.projectId = undefined
    })
  },
  async listProjectRules(projectId) {
    return clone(mockData.approvalRules.filter((r) => r.projectId === projectId))
  },
  async addProjectRule(projectId, input) {
    const exists = mockData.approvalRules.find(
      (r) => r.projectId === projectId && r.toolName === input.toolName && r.pattern === input.pattern,
    )
    if (exists) return clone(exists) // 幂等：与服务端唯一索引语义一致
    const rule: ApprovalRule = {
      id: `ar-mock-${Date.now()}`,
      projectId,
      toolName: input.toolName,
      pattern: input.pattern,
      createdAt: Date.now(),
    }
    mockData.approvalRules.push(rule)
    return clone(rule)
  },
  async removeProjectRule(projectId, ruleId) {
    const i = mockData.approvalRules.findIndex((r) => r.id === ruleId && r.projectId === projectId)
    if (i >= 0) mockData.approvalRules.splice(i, 1)
  },

  // ---- 工作空间域（mock：内存态，语义与 real 对齐） ----
  async listWorkspaces() {
    return clone(mockData.workspaces)
  },
  async createWorkspace(input) {
    const seq = mockData.workspaces.filter((w) => !w.builtin).length + 1
    const w: Workspace = {
      id: `ws-mock-${Date.now()}`,
      name: input.name.trim() || `新接入设备 ${seq}`,
      kind: input.kind,
      // mock 无真实设备 Hub：新建即演示为在线
      online: true,
      limbCount: 0,
      lastSeenAt: Date.now(),
      protocol: 'v1',
    }
    mockData.workspaces.push(w)
    return clone(w)
  },
  async updateWorkspace(id, patch) {
    const found = mockData.workspaces.find((w) => w.id === id)
    if (!found) throw new ApiError('工作空间不存在', 'workspace.not_found', 404)
    // kind 不参与更新（改 kind = 提权，real 侧服务端同样 400）
    if (patch.name !== undefined) found.name = patch.name
    return clone(found)
  },
  async deleteWorkspace(id) {
    const idx = mockData.workspaces.findIndex((w) => w.id === id)
    const found = mockData.workspaces[idx]
    if (!found) throw new ApiError('工作空间不存在', 'workspace.not_found', 404)
    // 内置服务端空间不可删除（服务端 repo_workspace.go:115 同样拒绝）
    if (found.builtin) throw new ApiError('服务端内置空间不可删除', 'workspace.in_use', 409)
    mockData.workspaces.splice(idx, 1)
  },

  async listMessages(sessionId) {
    const list = mockData.messages.filter((m) => m.sessionId === sessionId)
    // demo 的 messages 是跨会话样例：无匹配时回退到 s1 的全量样例
    return clone(list.length > 0 ? list : mockData.messages)
  },

  async startRun(opts) {
    const runId = `r-mock-${Date.now()}`
    // mock 无真实 EventSource：如实上报「演示态」，顶栏同时有 🟡 DEMO 角标提示这是演示数据
    opts.onSseState?.('open', { reason: 'mock-stream' })
    const handle = startMockRun({
      sessionId: opts.sessionId,
      runId,
      input: opts.input,
      onEvent: opts.onEvent,
    })
    return {
      runId,
      stop: () => {
        handle.stop()
        opts.onSseState?.('closed', { reason: 'client_closed' })
      },
      // mock 无服务端事件流可重连（演示流不跨刷新存活）：空实现，不假装在跑
      reconnect: () => undefined,
    }
  },
  async getActiveRun() {
    // mock 无真实服务端 Run（演示流不跨刷新存活），恒为空闲态
    return null
  },
  attachRun(opts) {
    // mock 无真实 EventSource：无法重放演示流，返回空句柄（不假装在跑）
    return {
      runId: opts.runId,
      stop: () => undefined,
      reconnect: () => undefined,
    }
  },
  async getRun(sessionId, runId) {
    return { id: runId, sessionId, status: 'running' }
  },
  async stopRun() {
    /* mock 由 RunHandle.stop 处理 */
  },
  async decideApproval() {
    // mock 的审批由 startMockRun 内部的 resumeResolve 驱动（chatStore 走 handle.resume），
    // 无服务端端点可调；空实现保持契约完整（不伪造决策结果）。
  },
  async sendGuidance(_sessionId, _runId, input) {
    // mock：无服务端轮次边界，引导以普通用户消息呈现
    return { id: `m-guide-${Date.now()}`, seq: 0, content: String(input ?? '') }
  },
  async listToolEvents() {
    return []
  },

  async getTools() {
    return mockToolsSnapshot()
  },
  async getToolsInjection() {
    const snap = mockToolsSnapshot()
    return {
      core: [],
      deferred_index: snap.deferred.map((d) => ({ name: d.name, description: '', cost_tokens: d.cost_tokens })),
      on_demand: [],
      cost_core: 0,
      cost_index: snap.cost_deferred,
      cost_total: snap.cost_total,
      limits: snap.limits,
      warnings: [],
    }
  },
  async patchToolTiers() {
    return mockToolsSnapshot()
  },

  // ---- 核心 SOP（agent-core）：mock 走内存样例，仅用于无后端演示 ----
  async getSOPCore() {
    const c = '你是 harness-go 的编码助手。\n\n规则：\n1. 修改文件前先读取确认内容，不要凭猜测改动。\n2. 危险操作（删除、覆盖、执行命令）需用户确认。\n3. 回答简洁，先给结论再给细节。'
    return {
      content: c,
      source: 'builtin' as const,
      tokens: 90,
      budget: 800,
      builtin_content: c,
      overridden: false,
    }
  },
  async putSOPCore(content: string) {
    const base = await this.getSOPCore()
    return { ...base, content, source: 'custom' as const, overridden: true }
  },
  async resetSOPCore() {
    return this.getSOPCore()
  },
  async getSOPEffective() {
    const core = await this.getSOPCore()
    return {
      core,
      entries: [],
      workspace: '',
      total_tokens: core.tokens,
      system_preview: core.content,
      truncated: false,
      entry_limit: 6,
    }
  },

  async getMaintenance() {
    return { mode: 'mock', open_sessions: 0, pending_approvals: 0, unfinished_runs: 0 }
  },

  // ---- B-4：厂商 / 配置 / 统计 / 日志（mock 走 demo 样例） ----
  async listProviders() {
    return mockData.providers.map((p) => ({
      id: p.id,
      name: p.name,
      base_url: p.baseUrl,
      has_api_key: Boolean(p.apiKeyMasked),
      models: p.models.map((m) => m.id),
      enabled: p.enabled,
      created_at: 0,
      updated_at: 0,
    }))
  },
  async createProvider(input) {
    return {
      id: `pv-mock-${Date.now()}`,
      name: input.name ?? '新厂商',
      base_url: input.base_url ?? '',
      has_api_key: Boolean(input.api_key),
      models: input.models ?? [],
      enabled: input.enabled ?? true,
      created_at: Date.now(),
      updated_at: Date.now(),
    }
  },
  async updateProviderById(id, input) {
    return {
      id,
      name: input.name ?? '',
      base_url: input.base_url ?? '',
      has_api_key: Boolean(input.api_key),
      models: input.models ?? [],
      enabled: input.enabled ?? true,
      created_at: 0,
      updated_at: Date.now(),
    }
  },
  async deleteProvider() {
    /* no-op */
  },
  async testProvider() {
    return { ok: true, latency_ms: 120, model: 'mock' }
  },
  async listProviderModels() {
    return { items: ['mock-model-a', 'mock-model-b'], count: 2 }
  },
  async revealProviderKey() {
    return { api_key: 'sk-mock-demo-key', has_key: true }
  },
  async getSettings() {
    return {}
  },
  async patchSettings() {
    return {}
  },
  async getStats() {
    return { mode: 'mock' }
  },
  async getLogs() {
    // mock 模式：前端演示数据由 mock/settingsData 提供，服务端接口只回空列表。
    return { items: [] } as LogsResponse
  },
  async getAuditLlmCalls() {
    // mock 模式无天库：回空列表并显式标注档位，让面板显示「无调用记录」而非报错。
    return {
      items: [],
      total: 0,
      snapshot_mode: 'preview',
      retention_days: 30,
      earliest_available_day: '',
    } as AuditLlmCallsResponse
  },
  async getAuditToolEvents() {
    return { items: [], total: 0, retention_days: 30, earliest_available_day: '' } as AuditToolEventsResponse
  },
  async getTokenStats() {
    return {
      from: 0,
      to: 0,
      group_by: 'model',
      items: [],
      total_calls: 0,
      prompt_tokens: 0,
      completion_tokens: 0,
      cached_tokens: 0,
      cache_hit_rate: 0,
    } as TokenStatsResult
  },
  async getLogConfig() {
    return { snapshot_mode: 'preview', retention_days: 30, level: 'warn' }
  },

  // ---- 阶段 C：设备（mock 走内存样例，与真实端点同签名） ----
  async createDevicePairCode(req) {
    return {
      code: String(Math.floor(100000 + Math.random() * 900000)),
      expires_at: Date.now() + 120_000,
      ttl_s: 120,
      device_name: req?.name,
      device_kind: req?.kind,
    }
  },
  async listDevices() {
    return { items: mockDeviceList(), pair_ttl_s: 120, heartbeat_s: 15, offline_after_s: 45 }
  },
  async revokeDevice(id) {
    const d = mockDeviceList().find((x) => x.id === id)
    if (!d) throw new ApiError('设备不存在', 'device.not_found', 404)
  },
  async getSessionLease(sessionId) {
    return { session_id: sessionId, holder: '', holder_kind: 'server', epoch: 0, active: true }
  },

  // ---- 阶段 E：MCP（mock 由内置表派生，与 real 同签名，绝不抛错） ----
  async listMcpServers() {
    return { items: mockMcpViews() }
  },
  async getMcpServer(id) {
    const found = mockMcpViews().find((m) => m.id === id)
    // 与 real 一致的 404 语义（避免组件因 undefined 崩溃）
    if (!found) throw new ApiError('MCP Server 不存在', 'mcp.not_found', 404)
    return found
  },
  async createMcpServer(input) {
    return { ...mockMcpViewOf(input), id: `mcp-${Date.now()}`, state: 'disabled' }
  },
  async updateMcpServer(id, input) {
    return { ...mockMcpViewOf(input), id }
  },
  async deleteMcpServer(id) {
    if (!mockMcpViews().some((m) => m.id === id)) throw new ApiError('MCP Server 不存在', 'mcp.not_found', 404)
  },
  async testMcpServer(input) {
    // 演示口径：随便给出一个「握手成功」样例，让 UI 的测试按钮可用但不造假连接
    return { tool_names: ['demo_tool'], elapsed_ms: 218, server_info: `${input.name} (mock)` }
  },
  async reloadMcpServer(id) {
    if (!mockMcpViews().some((m) => m.id === id)) throw new ApiError('MCP Server 不存在', 'mcp.not_found', 404)
  },
  async enableMcpServer(id) {
    if (!mockMcpViews().some((m) => m.id === id)) throw new ApiError('MCP Server 不存在', 'mcp.not_found', 404)
  },
  async disableMcpServer(id) {
    if (!mockMcpViews().some((m) => m.id === id)) throw new ApiError('MCP Server 不存在', 'mcp.not_found', 404)
  },
  async getMcpServerHealth(id) {
    const m = mockMcpViews().find((x) => x.id === id)
    if (!m) throw new ApiError('MCP Server 不存在', 'mcp.not_found', 404)
    return {
      id: m.id, name: m.name, transport: m.transport, state: m.state ?? 'disabled',
      last_error: m.last_error, calls: m.calls ?? 0, failures: m.failures ?? 0,
      tool_count: m.tool_count ?? 0,
    }
  },

  // ---- 阶段 E：本地工具包（mock 由 registryItems 派生） ----
  async listPackages() {
    return { items: mockPackageViews() }
  },
  async installPackage(req) {
    // 演示：按路径 basename 造一个已安装包视图（不落盘、不注册工具）
    const base = req.path.replace(/\\/g, '/').split('/').pop() || 'demo.pkg.zip'
    const name = base.replace(/\.[^.]+$/, '')
    return {
      view: {
        id: `pkg-${Date.now()}`, version: '0.0.1', name, trust: 'signed', namespace: 'demo',
        enabled: true, description: `演示安装：${base}`, tool_names: [], state: 'ready',
      },
      signed: true,
      tools: 0,
    }
  },
  async uninstallPackage(id) {
    if (!mockPackageViews().some((p) => p.id === id)) throw new ApiError('工具包不存在', 'package.not_found', 404)
  },
  async rollbackPackage(id) {
    const p = mockPackageViews().find((x) => x.id === id)
    if (!p) throw new ApiError('工具包不存在', 'package.not_found', 404)
    return { ...p, version: p.previous_version ?? p.version, previous_version: p.version }
  },
  async enablePackage(id) {
    if (!mockPackageViews().some((p) => p.id === id)) throw new ApiError('工具包不存在', 'package.not_found', 404)
  },
  async disablePackage(id) {
    if (!mockPackageViews().some((p) => p.id === id)) throw new ApiError('工具包不存在', 'package.not_found', 404)
  },

  // ---- P1：Webhook（mock 走模块级 demo 表，与 real 同签名，绝不抛「unavailable」） ----
  async listWebhooks() {
    return { items: mockWebhookStore.map((w) => ({ ...w })) }
  },
  async createWebhook(input) {
    const created: Webhook = {
      id: `wh-demo-${Date.now().toString(36)}`,
      url: input.url,
      // secret 只写不读：演示态同样只落布尔，不保存明文
      has_secret: Boolean(input.secret),
      events: input.events ?? [],
      enabled: true,
      created_at: Date.now(),
    }
    mockWebhookStore.unshift(created)
    return { ...created }
  },
  async deleteWebhook(id) {
    const i = mockWebhookStore.findIndex((w) => w.id === id)
    if (i < 0) throw new ApiError('Webhook 订阅不存在', 'webhook.not_found', 404)
    mockWebhookStore.splice(i, 1)
  },
  async enableWebhook(id) {
    const w = mockWebhookStore.find((x) => x.id === id)
    if (!w) throw new ApiError('Webhook 订阅不存在', 'webhook.not_found', 404)
    w.enabled = true
  },
  async disableWebhook(id) {
    const w = mockWebhookStore.find((x) => x.id === id)
    if (!w) throw new ApiError('Webhook 订阅不存在', 'webhook.not_found', 404)
    w.enabled = false
  },
  async testWebhook(id) {
    const w = mockWebhookStore.find((x) => x.id === id)
    if (!w) throw new ApiError('Webhook 订阅不存在', 'webhook.not_found', 404)
    // 演示：不真正发网络请求；地址以 .invalid 结尾时模拟对端拒绝，便于查看失败样式
    if (w.url.includes('.invalid')) return { status: 502, ok: false, error: 'dial tcp: connect: connection refused' }
    return { status: 200, ok: true, error: '' }
  },

  /* ---- B-2：二进制能力在演示态**一律诚实拒绝** ---- */
  // 铁律：绝不伪造 zip 内容 / 绝不返回空 Blob 冒充成功（用户会得到一个 0 字节文件而误以为导出成功）。
  async downloadArtifact() {
    throw new ApiError(
      'mock 演示模式不支持制品下载：请切换到 real 模式（apiMode=real）后重试',
      'mock.unsupported',
      501,
    )
  },
  async exportSession() {
    throw new ApiError(
      'mock 演示模式不支持会话导出：请切换到 real 模式（apiMode=real）后重试',
      'mock.unsupported',
      501,
    )
  },
  async backupData() {
    throw new ApiError(
      'mock 演示模式不支持全量备份：请切换到 real 模式（apiMode=real）后重试',
      'mock.unsupported',
      501,
    )
  },
  async restoreData() {
    throw new ApiError(
      'mock 演示模式不支持从备份恢复：请切换到 real 模式（apiMode=real）后重试',
      'mock.unsupported',
      501,
    )
  },
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
 * 实现已收敛到 `realApi.decideApproval`（唯一入口），此处不再保留同名私有函数。
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
  async deleteWorkspace(id) {
    // 仍被会话/项目引用时服务端返回 409（不级联），错误交由调用方提示
    await request<void>(API_PATHS.workspace(id), { method: 'DELETE', headers: authHeaders() })
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

/** 当前生效的客户端（B-1 默认 mock，与 demo 行为一致） */
export const api: ApiClient = apiMode === 'real' ? realApi : mockApi

/** 便于调试：控制台可查看当前模式 */
if (typeof window !== 'undefined') {
  ;(window as unknown as { __harnessApiMode?: ApiMode }).__harnessApiMode = apiMode
}
