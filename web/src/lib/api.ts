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
import { SseClient } from './sse'
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
  type PairCodeDTO,
  type ProviderDTO,
  type ProviderInput,
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
  deleteSession(id: string): Promise<void>
  // 消息
  listMessages(sessionId: string, afterSeq?: number): Promise<Message[]>
  // Run
  startRun(opts: StartRunOptions): Promise<RunHandle>
  getRun(sessionId: string, runId: string): Promise<Run>
  stopRun(sessionId: string, runId: string): Promise<void>
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
  /** 连通测试：用该厂商发一次极小请求，返回延迟与错误码 */
  testProvider(id: string): Promise<ProviderTestResult>
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
    const handle = startMockRun({
      sessionId: opts.sessionId,
      runId,
      input: opts.input,
      onEvent: opts.onEvent,
    })
    return {
      runId,
      stop: () => handle.stop(),
      resume: (decision, reason) => handle.resume(decision, reason),
    }
  },
  async getRun(sessionId, runId) {
    return { id: runId, sessionId, status: 'running' }
  },
  async stopRun() {
    /* mock 由 RunHandle.stop 处理 */
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
      // 增量输出仅用于本地预览，最终结果以 tool.result 为准
      return null
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
        return { type: 'done', tokensIn: 0, tokensOut: 0 }
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
  async deleteSession(id) {
    await request<void>(API_PATHS.session(id), { method: 'DELETE', headers: authHeaders() })
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
  async testProvider(id) {
    return request<ProviderTestResult>(API_PATHS.providerTest(id), {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify({}),
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
}

/* ============ 出口 ============ */

/** 当前生效的客户端（B-1 默认 mock，与 demo 行为一致） */
export const api: ApiClient = apiMode === 'real' ? realApi : mockApi

/** 便于调试：控制台可查看当前模式 */
if (typeof window !== 'undefined') {
  ;(window as unknown as { __harnessApiMode?: ApiMode }).__harnessApiMode = apiMode
}
