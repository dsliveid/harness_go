/**
 * SSE 客户端（阶段 B-1 新增，demo 无此文件）。
 *
 * 契约依据：docs/protocols.md §4（SSE 传输协议）
 *   - 帧：`id: <seq>\nevent: <type>\ndata: <json>\n\n`，心跳 `: ping`；
 *   - 续传：重连时带 `Last-Event-ID`；
 *   - 终态对齐：服务端在断开前发 `resync`（含 last_seq / run_status），客户端据此重拉消息快照。
 *
 * 状态机：idle → connecting → open →（断开）backoff → connecting …
 *         收到 resync → resync → closed（由上层重拉 /messages 后再决定是否重连）
 *
 * 设计要点：
 *   1. **不依赖浏览器 EventSource 的自动重连**（其退避策略不可控、且无法定制 Last-Event-ID 语义），
 *      自行 close + 指数退避重连；
 *   2. `visibilitychange` 回前台时，若处于 backoff 立即重连（移动端切后台断流是常态）；
 *   3. 慢消费者被服务端主动断开属正常现象（服务端绝不阻塞 Run），必须走 backoff 重连 + resync 对齐。
 */

export type SseState = 'idle' | 'connecting' | 'open' | 'backoff' | 'resync' | 'closed'

export interface SseEvent {
  /** 事件 id（即 per-Run 的 seq），用于 Last-Event-ID 续传 */
  id?: string
  type: string
  data: string
}

export interface SseStateInfo {
  /** 退避重连的等待毫秒数（仅 backoff 状态有值） */
  retryIn?: number
  /** 状态变化原因（便于日志与 UI 提示） */
  reason?: string
  /** 累计重连次数 */
  attempts?: number
}

export interface SseOptions {
  /** 完整 URL（含协议与主机） */
  url: string
  /** 首次连接的 Last-Event-ID（续传用） */
  lastEventId?: string
  /** 是否携带 Cookie（同源部署默认 true 即可） */
  withCredentials?: boolean
  /** 最大连续重连次数，超过后进入 closed（默认 12） */
  maxAttempts?: number
  /** 首次退避毫秒（默认 400） */
  baseDelayMs?: number
  /** 退避上限毫秒（默认 15000） */
  maxDelayMs?: number
  onEvent: (ev: SseEvent) => void
  onState?: (state: SseState, info: SseStateInfo) => void
  /** 收到 resync 事件：上层应重拉消息快照做终态对齐 */
  onResync?: (payload: unknown) => void
}

const jitter = (ms: number) => Math.floor(ms * (0.7 + Math.random() * 0.6))

export class SseClient {
  private readonly opts: Required<Pick<SseOptions, 'maxAttempts' | 'baseDelayMs' | 'maxDelayMs'>> &
    SseOptions
  private es: EventSource | null = null
  private timer: ReturnType<typeof setTimeout> | null = null
  private attempts = 0
  private lastEventId = ''
  private current: SseState = 'idle'
  private closedByUser = false
  private readonly onVisible: () => void

  constructor(opts: SseOptions) {
    this.opts = {
      maxAttempts: 12,
      baseDelayMs: 400,
      maxDelayMs: 15_000,
      withCredentials: true,
      ...opts,
    }
    this.lastEventId = opts.lastEventId ?? ''
    // 回前台立刻重连（移动端/锁屏后连接必然已死）
    this.onVisible = () => {
      if (typeof document === 'undefined') return
      if (document.visibilityState !== 'visible') return
      if (this.current === 'backoff' || this.current === 'idle') this.reconnectNow()
    }
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', this.onVisible)
    }
  }

  /** 当前状态 */
  get state(): SseState {
    return this.current
  }

  /** 累计重连次数 */
  get retries(): number {
    return this.attempts
  }

  /** 建立连接（幂等：已连接时直接返回） */
  connect(): void {
    if (this.es || this.current === 'connecting' || this.current === 'open') return
    this.closedByUser = false
    this.open()
  }

  /** 主动关闭（不再重连） */
  close(): void {
    this.closedByUser = true
    this.cleanup()
    this.setState('closed', { reason: 'client_closed' })
  }

  /** 立即重连（跳过退避，例如用户点「重试」或回前台） */
  reconnectNow(): void {
    if (this.closedByUser) return
    this.clearTimer()
    this.cleanup()
    this.attempts = 0
    this.open()
  }

  // ---------- 内部 ----------

  private open(): void {
    this.setState('connecting')
    let url = this.opts.url
    // EventSource 无法自定义请求头：续传参数走 query（服务端同时支持 header 与 query）
    if (this.lastEventId) {
      const sep = url.includes('?') ? '&' : '?'
      url = `${url}${sep}last_event_id=${encodeURIComponent(this.lastEventId)}`
    }
    const es = new EventSource(url, { withCredentials: this.opts.withCredentials })
    this.es = es

    es.onopen = () => {
      this.attempts = 0
      this.setState('open')
    }

    es.onmessage = (ev: MessageEvent<string>) => {
      const sseEvent: SseEvent = { id: ev.lastEventId || undefined, type: 'message', data: ev.data }
      this.dispatch(sseEvent)
    }

    // 命名事件（服务端用 event: <type> 帧）
    const named = [
      'run.status',
      'message.start',
      'message.delta',
      'message.end',
      'tool.started',
      'tool.progress',
      'tool.result',
      'approval.request',
      'approval.decision',
      'resync',
      'error',
    ]
    for (const type of named) {
      es.addEventListener(type, (ev) => {
        const me = ev as MessageEvent<string>
        this.dispatch({ id: me.lastEventId || undefined, type, data: me.data })
      })
    }

    es.onerror = () => {
      // 服务端主动断开（含慢消费者）与网络错误都会走到这里
      this.cleanup()
      if (this.closedByUser) return
      this.scheduleReconnect()
    }
  }

  private dispatch(ev: SseEvent): void {
    if (ev.id) this.lastEventId = ev.id
    this.opts.onEvent(ev)
    if (ev.type !== 'resync') return

    // 终态对齐：停止自动重连，交由上层重拉消息后决定
    this.setState('resync', { reason: 'server_resync' })
    this.cleanup()
    let payload: unknown = ev.data
    try {
      payload = JSON.parse(ev.data)
    } catch {
      /* 保留原始字符串 */
    }
    this.opts.onResync?.(payload)
  }

  private scheduleReconnect(): void {
    if (this.attempts >= this.opts.maxAttempts) {
      this.setState('closed', { reason: 'max_attempts', attempts: this.attempts })
      return
    }
    this.attempts += 1
    const raw = this.opts.baseDelayMs * 2 ** (this.attempts - 1)
    const delay = Math.min(this.opts.maxDelayMs, jitter(raw))
    this.setState('backoff', { retryIn: delay, attempts: this.attempts })
    this.clearTimer()
    this.timer = setTimeout(() => {
      this.timer = null
      if (this.closedByUser) return
      this.open()
    }, delay)
  }

  private cleanup(): void {
    if (this.es) {
      this.es.onopen = null
      this.es.onmessage = null
      this.es.onerror = null
      this.es.close()
      this.es = null
    }
  }

  private clearTimer(): void {
    if (this.timer) {
      clearTimeout(this.timer)
      this.timer = null
    }
  }

  private setState(state: SseState, info: SseStateInfo = {}): void {
    this.current = state
    this.opts.onState?.(state, { attempts: this.attempts, ...info })
  }

  /** 释放监听器（组件卸载时调用） */
  dispose(): void {
    this.close()
    if (typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', this.onVisible)
    }
  }
}
