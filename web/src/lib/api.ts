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
  type CreateRunResponse,
  type LoginRequest,
  type LoginResponse,
  type Message as ApiMessage,
  type Run as ApiRun,
  type RunStatus as ApiRunStatus,
  type Session as ApiSession,
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
 *   1. `VITE_API_MODE`（构建期注入，便于 CI/预览切 real）
 *   2. `localStorage['harness.apiMode']`（运行期手动切换）
 *   3. 默认 `mock`（B-1 出口要求与 demo 行为一致）
 */
export function resolveApiMode(): ApiMode {
  try {
    const fromEnv = (import.meta as unknown as { env?: Record<string, string | undefined> }).env
      ?.VITE_API_MODE
    if (fromEnv === 'real' || fromEnv === 'mock') return fromEnv
    const stored = typeof localStorage !== 'undefined' ? localStorage.getItem(API_MODE_STORAGE_KEY) : null
    if (stored === 'real' || stored === 'mock') return stored
  } catch {
    /* 工作于非浏览器环境（SSR/测试）时忽略 */
  }
  return 'mock'
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

export const mockApi: ApiClient = {
  mode: 'mock',

  async me() {
    // mock 模式沿用 demo「直接进演示模式」的行为
    return { id: 'mock-user', username: 'demo', display_name: '演示用户', role: 'admin', created_at: 0, updated_at: 0 }
  },
  async login(req) {
    return { user: { id: 'mock-user', username: req.username, display_name: '演示用户', role: 'admin', created_at: 0, updated_at: 0 }, token: 'mock-token' }
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
    case 'tool.result':
      return {
        type: 'tool_result',
        callId: String(payload.tool_event_id ?? ''),
        result: typeof payload.result === 'string' ? payload.result : JSON.stringify(payload.result ?? ''),
        durationMs: 0,
      }
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
    const sse = new SseClient({
      url: toAbsolute(API_PATHS.runEvents(opts.sessionId, created.run_id)),
      onEvent: (ev) => {
        const mapped = mapSseToStreamEvent(ev.type, ev.data)
        if (mapped) opts.onEvent(mapped)
      },
      onResync: () => {
        // 终态对齐：上层应重拉消息快照（chatStore 在 B-3 接入）
        opts.onEvent({ type: 'done', tokensIn: 0, tokensOut: 0 })
      },
    })
    sse.connect()
    return {
      runId: created.run_id,
      stop: () => {
        sse.close()
        void realApi.stopRun(opts.sessionId, created.run_id)
      },
      resume: () => {
        // 审批端点属阶段 B-3（approval decide API 尚未落地），此处保留签名
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
}

/* ============ 出口 ============ */

/** 当前生效的客户端（B-1 默认 mock，与 demo 行为一致） */
export const api: ApiClient = apiMode === 'real' ? realApi : mockApi

/** 便于调试：控制台可查看当前模式 */
if (typeof window !== 'undefined') {
  ;(window as unknown as { __harnessApiMode?: ApiMode }).__harnessApiMode = apiMode
}
