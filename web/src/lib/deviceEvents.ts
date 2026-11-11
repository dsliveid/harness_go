/**
 * 设备状态实时订阅（SSE）：`GET /api/v1/devices/events`。
 *
 * ## 与 `sse.ts`（Run 事件流）的关键差异（勿混用）
 *
 * | 维度     | `sse.ts`（Run 事件）        | 本文件（设备状态）        |
 * | -------- | --------------------------- | ------------------------- |
 * | 数据性质 | 事件流（含 seq）            | **当前态**快照 + 跃迁     |
 * | 续传     | `Last-Event-ID` 按 seq 补发 | **不续传**，重连重拿快照  |
 * | 帧类型   | message.delta / tool.* 等   | snapshot / device.online  |
 *
 * 因为设备状态是「当前态」而非历史，重连时**不需要**携带 `Last-Event-ID`
 * —— 服务端每次连接都会先发一帧全量 `snapshot`，直接覆盖本地状态即可。
 *
 * ## 为什么不直接用 `SseClient`
 * `SseClient` 的 `resync` 处理与「按 seq 续传」强绑定，套用到设备状态上会
 * 引入一层无意义的对齐逻辑（设备状态没有 seq，也无法「对齐历史」）。
 * 这里用轻量实现：只需 onopen / 具名事件 / 指数退避重连 / 回前台重连。
 *
 * ## 红线
 * 服务端响应中**不含任何令牌字段**（该端点在用户令牌中间件之后）。
 * 本文件也绝不把 Cookie / token 拼进 URL。
 */

import { API_PATHS } from './apiTypes'

/** 单台设备的对外状态（与服务端 `device.DeviceState` 对齐）。 */
export interface DeviceLiveState {
  device_id: string
  name: string
  kind: string
  /**
   * 设备归属账号（devices.user_id）。
   *
   * 与 token 无关的非机密标识。为空串表示「未绑定账号」——这类设备是 0011 迁移
   * 之前配对的历史设备，仍在正常工作，只是无法凭设备令牌自续会话。
   * 前端应如实展示而不是隐藏该设备。
   */
  user_id?: string
  online: boolean
  /** Unix 毫秒 */
  last_seen_at: number
  protocol_version?: number
}

/** 服务端事件类型。
 *
 * `device.revoked`（= 解除绑定）与 `device.offline` 的语义**必须分开**：
 * - offline：暂时连不上，设备仍在绑定关系中 → 卡片保留、标灰；
 * - revoked：绑定关系终止 → 卡片必须**从列表移除**。
 * 若把两者混用，管理端点「解除绑定」后设备只会变灰而不会消失，
 * 用户会认为解绑没生效（这正是本事件引入前的 bug）。
 */
export type DeviceLiveEventType = 'snapshot' | 'device.online' | 'device.offline' | 'device.revoked'

interface DeviceLivePayload {
  type: DeviceLiveEventType
  device?: DeviceLiveState
  all?: DeviceLiveState[]
  at: number
}

export interface DeviceEventsOptions {
  /** 完整 URL（含协议与主机）。省略时用同源 `${API_PATHS.deviceEvents}`。 */
  url?: string
  /** 收到快照（全量覆盖本地状态）。 */
  onSnapshot: (all: DeviceLiveState[]) => void
  /** 收到单台设备上线/离线/被解绑（按 device_id 落位；revoked 应从列表移除）。 */
  onDeviceChange: (ev: { type: DeviceLiveEventType; device: DeviceLiveState }) => void
  /** 连接状态变化（UI 可据此展示「实时同步中 / 重连中」）。 */
  onState?: (state: DeviceEventsState, info: { attempts?: number; reason?: string }) => void
  /** 首次退避（默认 500ms）。 */
  baseDelayMs?: number
  /** 退避上限（默认 15s）。 */
  maxDelayMs?: number
  /** 最大连续重试次数，超过后停止（默认 12）。 */
  maxAttempts?: number
}

export type DeviceEventsState = 'idle' | 'connecting' | 'open' | 'backoff' | 'closed'

const jitter = (ms: number) => Math.floor(ms * (0.7 + Math.random() * 0.6))

/**
 * 设备状态订阅器。
 *
 * 用法：
 * ```ts
 * const sub = new DeviceEventsClient({
 *   onSnapshot: (all) => setDevices(byId(all)),
 *   onDeviceChange: ({ type, device }) => upsert(device),
 * })
 * sub.connect()
 * // 组件卸载
 * sub.dispose()
 * ```
 */
export class DeviceEventsClient {
  private readonly opts: Required<
    Pick<DeviceEventsOptions, 'baseDelayMs' | 'maxDelayMs' | 'maxAttempts'>
  > &
    DeviceEventsOptions
  private es: EventSource | null = null
  private timer: ReturnType<typeof setTimeout> | null = null
  private attempts = 0
  private state: DeviceEventsState = 'idle'
  private closedByUser = false
  private readonly onVisible: () => void

  constructor(opts: DeviceEventsOptions) {
    this.opts = {
      baseDelayMs: 500,
      maxDelayMs: 15_000,
      maxAttempts: 12,
      ...opts,
    }
    // 回前台立即重连：移动端/锁屏后 SSE 连接必然已断，等浏览器自己的
    // 退避可能要十几秒，用户看到的是「设备列表一直不更新」。
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

  get currentState(): DeviceEventsState {
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

  /** 释放资源（组件卸载必调，否则 visibilitychange 监听泄漏）。 */
  dispose(): void {
    this.close()
    if (typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', this.onVisible)
    }
  }

  // ---------- 内部 ----------

  private open(): void {
    this.setState('connecting')
    const url = this.opts.url ?? API_PATHS.deviceEvents
    // 同源部署靠 Cookie 鉴权；跨源时需要凭据。
    const es = new EventSource(url, { withCredentials: true })
    this.es = es

    es.onopen = () => {
      this.attempts = 0
      this.setState('open')
    }

    // 具名事件（服务端用 `event: <type>` 帧）。
    es.addEventListener('snapshot', (ev) => {
      const payload = this.parse(ev)
      if (!payload || !Array.isArray(payload.all)) return
      this.opts.onSnapshot(payload.all)
    })

    // device.revoked 与上下线走同一通道：区别由 type 表达，由调用方决定
    // 「落位」还是「移除」（设备列表要知道「绑定关系终止了」）。
    for (const type of ['device.online', 'device.offline', 'device.revoked'] as const) {
      es.addEventListener(type, (ev) => {
        const payload = this.parse(ev)
        if (!payload || !payload.device) return
        this.opts.onDeviceChange({ type, device: payload.device })
      })
    }

    es.onerror = () => {
      // 服务端主动断开（慢消费者）与网络错误都走这里。
      this.cleanup()
      if (this.closedByUser) return
      this.scheduleReconnect()
    }
  }

  private parse(ev: Event): DeviceLivePayload | null {
    const data = (ev as MessageEvent<string>).data
    if (!data) return null
    try {
      return JSON.parse(data) as DeviceLivePayload
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

  private setState(state: DeviceEventsState, info: { attempts?: number; reason?: string } = {}): void {
    this.state = state
    this.opts.onState?.(state, info)
  }
}
