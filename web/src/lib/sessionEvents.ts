/**
 * 会话列表实时订阅（SSE）：`GET /api/v1/sessions/events`。
 *
 * ## 存在意义
 *
 * 在此之前，只有「已打开会话内的消息」会实时刷新（`sse.ts` 的 Run 事件流）。
 * 左侧**对话列表本身**与每个会话的**状态**（运行中脉冲点 / 自动标题 / 归档 /
 * 删除 / token 用量）只在页面挂载时拉取一次 `GET /api/v1/sessions`，
 * 此后永不刷新 —— 跨端协作时另一端完全静默，必须手动刷新页面才能看到。
 * 本文件补上这条缺失的通道。
 *
 * ## 与 `deviceEvents.ts` / `sse.ts` 的区别（三条流勿混用）
 *
 * | 维度     | `sse.ts`（Run 事件）      | `deviceEvents.ts`（设备）  | 本文件（会话列表）        |
 * | -------- | ------------------------- | -------------------------- | ------------------------- |
 * | 观测对象 | 单个 Run 的事件流         | 设备当前态                 | **会话列表**的变更        |
 * | 续传     | `Last-Event-ID` 按 seq    | 不续传，重连拿快照         | 不续传，重连拿快照        |
 * | 帧类型   | message.delta / tool.* 等 | snapshot / device.online   | snapshot / session.*      |
 *
 * 为什么照搬 `deviceEvents.ts` 而不复用 `sse.ts`：后者与「按 seq 续传」强绑定，
 * 会话列表没有 seq、也无法「对齐历史」，套上去只会引入一层无意义的 resync 逻辑。
 *
 * ## 红线
 * 服务端响应中**不含任何令牌字段**；本文件绝不把 Cookie / token 拼进 URL，
 * 同源部署靠 Cookie 鉴权（`withCredentials: true`）。
 */

import { API_PATHS } from './apiTypes'

/** 服务端事件类型（与 internal/sessionbus 常量逐字对齐）。 */
export type SessionLiveEventType = 'snapshot' | 'session.updated' | 'session.removed'

/** 服务端返回的会话视图（`model.Session`，snake_case）。 */
export interface SessionLiveView {
  id: string
  title: string
  project_dir?: string
  project_id?: string
  workspace_id?: string
  access_mode?: string
  model_ref?: string
  message_count?: number
  last_message_at?: number
  total_tokens?: number
  last_run_status?: string
  created_at?: number
  updated_at?: number
  archived_at?: number
}

interface SessionLivePayload {
  type: SessionLiveEventType
  /** 快照帧：全量会话。 */
  items?: SessionLiveView[]
  /** 增量帧：单个会话（已由服务端回查数据库组装）。 */
  session?: SessionLiveView
  /** 删除帧：被删会话 id。 */
  session_id?: string
  at: number
}

export interface SessionEventsOptions {
  /** 完整 URL（含协议与主机）。省略时用同源 `${API_PATHS.sessionEvents}`。 */
  url?: string
  /** 收到快照（**全量覆盖**本地列表）。 */
  onSnapshot: (all: SessionLiveView[]) => void
  /** 收到单个会话变更（按 id upsert）。 */
  onSessionUpdated: (session: SessionLiveView) => void
  /** 收到会话删除（从列表移除）。 */
  onSessionRemoved: (sessionId: string) => void
  /**
   * 收到「领域列表变了」的**信号**（不含数据）。
   *
   * 服务端刻意只发信号不带载荷：project 有前端独有字段（workspaceId 分组），
   * 强行推全量会覆盖掉这些字段。收到后调用方自行整表重拉（loadProjects /
   * loadWorkspaces），天然复用既有的合并语义。
   */
  onDomainChanged?: (domain: 'projects' | 'workspaces') => void
  /** 连接状态变化（UI 可据此展示「实时同步中 / 重连中」）。 */
  onState?: (state: SessionEventsState, info: { attempts?: number; reason?: string }) => void
  /** 首次退避（默认 500ms）。 */
  baseDelayMs?: number
  /** 退避上限（默认 15s）。 */
  maxDelayMs?: number
  /** 最大连续重试次数，超过后停止（默认 12）。 */
  maxAttempts?: number
}

export type SessionEventsState = 'idle' | 'connecting' | 'open' | 'backoff' | 'closed'

const jitter = (ms: number) => Math.floor(ms * (0.7 + Math.random() * 0.6))

/**
 * 会话列表订阅器。
 *
 * 用法：
 * ```ts
 * const sub = new SessionEventsClient({
 *   onSnapshot: (all) => applyServerSnapshot(all),
 *   onSessionUpdated: (s) => applyServerPatch(s),
 *   onSessionRemoved: (id) => applyServerRemoval(id),
 * })
 * sub.connect()
 * // 组件卸载
 * sub.dispose()
 * ```
 */
export class SessionEventsClient {
  private readonly opts: Required<
    Pick<SessionEventsOptions, 'baseDelayMs' | 'maxDelayMs' | 'maxAttempts'>
  > &
    SessionEventsOptions
  private es: EventSource | null = null
  private timer: ReturnType<typeof setTimeout> | null = null
  private attempts = 0
  private state: SessionEventsState = 'idle'
  private closedByUser = false
  private readonly onVisible: () => void

  constructor(opts: SessionEventsOptions) {
    this.opts = {
      baseDelayMs: 500,
      maxDelayMs: 15_000,
      maxAttempts: 12,
      ...opts,
    }
    // 回前台立即重连：移动端/锁屏后 SSE 连接必然已断，等浏览器自己的退避
    // 可能要十几秒，用户看到的就是「列表一直不更新」。
    this.onVisible = () => {
      if (typeof document === 'undefined') return
      if (document.visibilityState !== 'visible') return
      if (this.state === 'backoff' || this.state === 'closed') {
        this.attempts = 0
        this.reconnectNow()
      }
    }
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', this.onVisible)
    }
  }

  get currentState(): SessionEventsState {
    return this.state
  }

  /** 建立连接（幂等）。 */
  connect(): void {
    if (this.es || this.state === 'connecting' || this.state === 'open') return
    this.closedByUser = false
    this.open()
  }

  /** 立即重连（跳过退避）。 */
  reconnectNow(): void {
    if (this.closedByUser) return
    this.clearTimer()
    this.cleanup()
    this.open()
  }

  /** 主动关闭（不再重连）。 */
  close(): void {
    this.closedByUser = true
    this.clearTimer()
    this.cleanup()
    this.setState('closed', { reason: 'client_closed' })
  }

  /** 释放资源（卸载必调，否则 visibilitychange 监听泄漏）。 */
  dispose(): void {
    this.close()
    if (typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', this.onVisible)
    }
  }

  // ---------- 内部 ----------

  private open(): void {
    this.setState('connecting')
    const url = this.opts.url ?? API_PATHS.sessionEvents
    const es = new EventSource(url, { withCredentials: true })
    this.es = es

    es.onopen = () => {
      this.attempts = 0
      this.setState('open')
    }

    // 具名事件（服务端用 `event: <type>` 帧）。
    es.addEventListener('snapshot', (ev) => {
      const payload = this.parse(ev)
      if (!payload || !Array.isArray(payload.items)) return
      this.opts.onSnapshot(payload.items)
    })

    es.addEventListener('session.updated', (ev) => {
      const payload = this.parse(ev)
      if (!payload || !payload.session) return
      this.opts.onSessionUpdated(payload.session)
    })

    es.addEventListener('session.removed', (ev) => {
      const payload = this.parse(ev)
      if (!payload || !payload.session_id) return
      this.opts.onSessionRemoved(payload.session_id)
    })

    // 领域列表变更（纯信号）：不带数据，只告知「该重拉哪份列表」。
    es.addEventListener('projects.changed', () => {
      this.opts.onDomainChanged?.('projects')
    })
    es.addEventListener('workspaces.changed', () => {
      this.opts.onDomainChanged?.('workspaces')
    })

    es.onerror = () => {
      // 服务端主动断开（慢消费者）与网络错误都走这里。
      this.cleanup()
      if (this.closedByUser) return
      this.scheduleReconnect()
    }
  }

  private parse(ev: Event): SessionLivePayload | null {
    const data = (ev as MessageEvent<string>).data
    if (!data) return null
    try {
      return JSON.parse(data) as SessionLivePayload
    } catch {
      // 畸形帧不得让整个订阅崩掉：静默跳过（下一条事件仍会正常处理）。
      return null
    }
  }

  private scheduleReconnect(): void {
    if (this.attempts >= this.opts.maxAttempts) {
      this.setState('closed', { reason: 'max_attempts', attempts: this.attempts })
      return
    }
    this.attempts += 1
    const raw = this.opts.baseDelayMs * 2 ** (this.attempts - 1)
    const delay = Math.min(this.opts.maxDelayMs, jitter(raw))
    this.setState('backoff', { attempts: this.attempts })
    this.clearTimer()
    this.timer = setTimeout(() => {
      this.timer = null
      if (this.closedByUser) return
      this.open()
    }, delay)
  }

  private cleanup(): void {
    if (!this.es) return
    this.es.onopen = null
    this.es.onerror = null
    this.es.close()
    this.es = null
  }

  private clearTimer(): void {
    if (!this.timer) return
    clearTimeout(this.timer)
    this.timer = null
  }

  private setState(state: SessionEventsState, info: { attempts?: number; reason?: string } = {}): void {
    this.state = state
    this.opts.onState?.(state, info)
  }
}
