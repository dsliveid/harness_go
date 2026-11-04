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

import type { AccessMode, Message, Run, RunStatus, Session, ToolCall, Risk, ToolStatus } from '../types'
import * as mockData from '../mock/data'
import { toolList } from '../mock/settingsData'
import { startMockRun, type Decision, type StreamEvent } from '../mock/stream'
import { SseClient, type SseState as SseStateName, type SseStateInfo } from './sse'
import {
  API_PATHS,
  type ChangePasswordRequest,
  type CreateDevicePairCodeRequest,
  type CreateRunResponse,
  type DeviceListResponse,
  type DeviceDTO,
  type LogLine,
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
  type ProviderModelsResult,
  type ProviderSecretResult,
  type ProviderTestInput,
  type ProviderTestResult,
  type Run as ApiRun,
  type RunStatus as ApiRunStatus,
  type Session as ApiSession,
  type SessionLeaseDTO,
  type SSORequest,
  type ToolEvent as ApiToolEvent,
  type ToolInjection,
  type ToolSnapshot,
  type ToolTier,
  type User,
} from './apiTypes'

/* ============ 模式选择 ============ */

export type ApiMode = 'mock' | 'real'

/** 模式持久化键（开发期可在控制台 `localStorage.setItem('harness.apiMode','real')` 切换） */
export const API_MODE_STORAGE_KEY = 'harness.apiMode'

/**
 * 解析当前数据模式：
 *   1. `VITE_API_MODE`（构建期注入，便于 CI/预览强制指定）
 *   2. `localStorage['harness.apiMode']`（运行期手动切换，便于对比 mock 基线）
 *   3. 默认：**开发态 mock**（可离线调 UI）/ **构建产物 real**（B-2 起接真实接口）
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
  // 生产构建默认 real：单二进制内嵌产物直连本机后端；dev 默认 mock 保留 demo 基线
  return env?.DEV === true || env?.DEV === 'true' ? 'mock' : 'real'
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
  /** 审批决定（阶段 B-3 接入 approval 端点后生效） */
  resume(decision: Decision, reason?: string): void
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

export interface CreateSessionInput {
  title?: string
  modelRef?: string
  workspacePath?: string
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
  // 消息
  listMessages(sessionId: string, afterSeq?: number): Promise<Message[]>
  // Run
  startRun(opts: StartRunOptions): Promise<RunHandle>
  getRun(sessionId: string, runId: string): Promise<Run>
  stopRun(sessionId: string, runId: string): Promise<void>
  /** 运行中引导（§4.2）：消息立即落库并就地返回，引擎在轮次边界回灌上下文；Run 已结束抛 409 */
  sendGuidance(sessionId: string, runId: string, input: string): Promise<{ id: string; seq: number; content: string }>
  listToolEvents(sessionId: string, runId: string): Promise<ApiToolEvent[]>
  // 工具（A-10 三端点）
  getTools(): Promise<ToolSnapshot>
  getToolsInjection(): Promise<ToolInjection>
  patchToolTiers(tiers: Record<string, ToolTier>): Promise<ToolSnapshot>
  // 运维
  getMaintenance(): Promise<Record<string, unknown>>
  // 厂商（B-4）
  listProviders(): Promise<ProviderDTO[]>
  createProvider(input: ProviderInput): Promise<ProviderDTO>
  updateProviderById(id: string, input: ProviderInput): Promise<ProviderDTO>
  deleteProvider(id: string): Promise<void>
  /** 连通测试：用该厂商发一次极小请求，返回延迟与错误码；入参可携带草稿未保存值（所见即所测） */
  testProvider(id: string, input?: ProviderTestInput): Promise<ProviderTestResult>
  /** 获取上游真实模型清单（失败直接抛错，不回退本地兜底） */
  listProviderModels(id: string): Promise<ProviderModelsResult>
  /** 读取厂商密钥明文（眼睛图标显示 / 复制） */
  revealProviderKey(id: string): Promise<ProviderSecretResult>
  // 配置 / 统计 / 日志（B-4）
  getSettings(): Promise<Record<string, unknown>>
  patchSettings(patch: Record<string, unknown>): Promise<Record<string, unknown>>
  getStats(): Promise<Record<string, unknown>>
  getLogs(params?: { limit?: number; level?: string }): Promise<LogLine[]>
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
}

/* ============ HTTP 基础设施 ============ */

const toAbsolute = (path: string) => (path.startsWith('http') ? path : `${location.origin}${path}`)

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(toAbsolute(path), {
    credentials: 'include', // Cookie hg_sess（HttpOnly）与 Bearer 双通道
    headers: {
      'Content-Type': 'application/json',
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
    workspacePath: s.workspace_path || undefined,
    accessMode: toAccessMode(s.access_mode),
    modelRef: s.model_ref,
    updatedAt: s.updated_at,
    running: s.last_run_status === 'running' || s.last_run_status === 'pending',
    archivedAt: s.archived_at,
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

export function toUiMessages(list: ApiMessage[], sessionId: string): Message[] {
  turnCounter = 0
  return list.map((m) => {
    if (m.role === 'user') turnCounter += 1
    return {
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
            const tc = raw as { id?: string; name?: string; arguments?: unknown; function?: { name?: string; arguments?: string } }
            return {
              id: tc.id ?? '',
              name: tc.name ?? tc.function?.name ?? '',
              args: (tc.arguments as Record<string, unknown>) ?? {},
              status: 'done' as ToolStatus,
              risk: 'write' as Risk,
            } satisfies ToolCall
          })
        : undefined,
      createdAt: m.created_at,
      tokens: m.token_count || undefined,
      // 服务端消息级回滚（S10）置灰的消息：保留可见、上下文组装已排除（hidden）
      ...(m.hidden ? { reverted: true, revertedAt: m.created_at } : {}),
    }
  })
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
    const sess: Session = {
      id: `s-mock-${Date.now()}`,
      title: input.title || '新对话',
      workspacePath: input.workspacePath,
      accessMode: 'confirm',
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
  async deleteSession(id) {
    const i = mockData.sessions.findIndex((s) => s.id === id)
    if (i >= 0) mockData.sessions.splice(i, 1)
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
      resume: (decision, reason) => handle.resume(decision, reason),
    }
  },
  async getRun(sessionId, runId) {
    return { id: runId, sessionId, status: 'running' }
  },
  async stopRun() {
    /* mock 由 RunHandle.stop 处理 */
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
    return []
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
}

/* ============ real 实现（B-2 起逐端点启用） ============ */

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
    case 'message.end':
      return { type: 'step', tokens: Number(payload.token_count ?? 0) }
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
    case 'tool.result':
      return {
        type: 'tool_result',
        callId: String(payload.tool_event_id ?? ''),
        result: typeof payload.result === 'string' ? payload.result : JSON.stringify(payload.result ?? ''),
        durationMs: 0,
        skipped: payload.status === 'denied' ? true : undefined,
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
          args: (payload.args as Record<string, unknown>) ?? {},
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
 * 决策后服务端解除闸门阻塞，Run 继续在**同一条事件流**上推送（订阅不关闭）。
 * 失败（409 已被决策 / 404 审批不存在）不打断本地 UI，由后续 run.status 事件收口。
 */
async function decideApproval(
  sessionId: string,
  runId: string,
  approvalId: string,
  decision: Decision,
  reason?: string,
): Promise<void> {
  await request<void>(API_PATHS.approvalDecide(sessionId, runId, approvalId), {
    method: 'POST',
    headers: authHeaders(),
    body: JSON.stringify({ decision, reason: reason ?? '' }),
  })
}

export const realApi: ApiClient = {
  mode: 'real',

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
    const body = { title: input.title, model_ref: input.modelRef, workspace_path: input.workspacePath }
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
    // 当前挂起的审批 id（由 approval.request 事件携带），resume 时回填给 decide 端点
    let approvalId = ''
    const sse = new SseClient({
      url: toAbsolute(API_PATHS.runEvents(opts.sessionId, created.run_id)),
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
        if (mapped.type === 'approval') approvalId = mapped.approval.id
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
      runId: created.run_id,
      stop: () => {
        sse.close()
        void realApi.stopRun(opts.sessionId, created.run_id)
      },
      resume: (decision, reason) => {
        if (!approvalId) return
        const aid = approvalId
        approvalId = ''
        void decideApproval(opts.sessionId, created.run_id, aid, decision, reason).catch(() => undefined)
      },
    }
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
  async listProviderModels(id) {
    return request<ProviderModelsResult>(API_PATHS.providerModels(id), {
      method: 'GET',
      headers: authHeaders(),
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
    const path = q.toString() ? `${API_PATHS.logs}?${q}` : API_PATHS.logs
    const res = await request<{ items: LogLine[] }>(path, { headers: authHeaders() })
    return res.items ?? []
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
}

/* ============ 出口 ============ */

/** 当前生效的客户端（B-1 默认 mock，与 demo 行为一致） */
export const api: ApiClient = apiMode === 'real' ? realApi : mockApi

/** 便于调试：控制台可查看当前模式 */
if (typeof window !== 'undefined') {
  ;(window as unknown as { __harnessApiMode?: ApiMode }).__harnessApiMode = apiMode
}
