/**
 * `lib/sessionEvents.ts` 传输层契约单测。
 *
 * # 为什么要测一个「EventSource 的薄封装」
 *
 * 服务端测试（`internal/api/session_events_test.go`）只保证**帧格式正确**，
 * 却管不到前端这边的三个真坑：
 *   ① 帧解析健壮性——一条畸形 JSON 不得让整个订阅崩掉（否则列表从此不再更新，
 *      用户只能刷新页面）；
 *   ② 建连幂等——React 18 的 effect 双调用 / 依赖抖动会重复 connect()，
 *      若不去重就会开两条 SSE 连接，服务端订阅者翻倍；
 *   ③ 退避重连与 dispose——断线后必须能自愈，卸载后必须彻底停止（否则泄漏）。
 * 这三条都是「不抛错、不 500，只是行为悄悄不对」的类型，必须用测试钉住。
 *
 * # 手法：EventSource 替身
 *
 * jsdom 不实现 EventSource（它是网络 API），故注入一个可控替身：测试自己触发
 * onopen / 具名事件 / onerror，从而在毫秒级内精确验证状态机，无需真实网络。
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { SessionEventsClient, type SessionEventsState } from './sessionEvents'

/** 可控的 EventSource 替身：记录所有实例，便于断言「建了几条连接」。 */
class FakeEventSource {
  static instances: FakeEventSource[] = []
  readonly url: string
  readonly withCredentials: boolean
  closed = false
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  private readonly listeners = new Map<string, Array<(ev: Event) => void>>()

  constructor(url: string, init?: { withCredentials?: boolean }) {
    this.url = url
    this.withCredentials = init?.withCredentials ?? false
    FakeEventSource.instances.push(this)
  }

  addEventListener(type: string, cb: (ev: Event) => void): void {
    const list = this.listeners.get(type) ?? []
    list.push(cb)
    this.listeners.set(type, list)
  }

  close(): void {
    this.closed = true
  }

  /** 触发一条具名事件帧（模拟服务端 `event: <type>` + `data: <json>`）。 */
  emit(type: string, payload: unknown): void {
    const data = typeof payload === 'string' ? payload : JSON.stringify(payload)
    const ev = { data } as MessageEvent<string>
    for (const cb of this.listeners.get(type) ?? []) cb(ev as unknown as Event)
  }

  /** 触发连接错误（模拟断网 / 服务端断开）。 */
  fail(): void {
    this.onerror?.()
  }

  /** 触发连接建立。 */
  open(): void {
    this.onopen?.()
  }

  static latest(): FakeEventSource {
    // 不用 Array.prototype.at：项目 lib 为 ES2020（.at 是 ES2022），
    // 用它会让 `tsc -b` 报 TS2550。索引访问同样清晰且不抬高 target。
    const last = FakeEventSource.instances[FakeEventSource.instances.length - 1]
    if (!last) throw new Error('尚未创建任何 EventSource')
    return last
  }

  static reset(): void {
    FakeEventSource.instances = []
  }
}

/** 已创建的 client（afterEach 统一 dispose，防止 visibilitychange 监听串场）。 */
const created: SessionEventsClient[] = []

/** 收集所有回调结果的测试夹具。 */
function makeClient(over: Partial<ConstructorParameters<typeof SessionEventsClient>[0]> = {}) {
  const snapshots: unknown[] = []
  const updates: unknown[] = []
  const removals: string[] = []
  const states: SessionEventsState[] = []
  const client = new SessionEventsClient({
    onSnapshot: (all) => snapshots.push(all),
    onSessionUpdated: (s) => updates.push(s),
    onSessionRemoved: (id) => removals.push(id),
    onState: (st) => states.push(st),
    ...over,
  })
  created.push(client)
  return { client, snapshots, updates, removals, states }
}

beforeEach(() => {
  FakeEventSource.reset()
  vi.stubGlobal('EventSource', FakeEventSource)
})

afterEach(() => {
  // 🚨 必须销毁：每个 client 都在 constructor 里给 document 挂了 visibilitychange
  // 监听。不销毁的话，上一个用例的 client 会被下一个用例 dispatch 的
  // visibilitychange 唤醒并另建一条连接 —— 表现为「期望 +1 实际 +2」的假失败。
  for (const c of created) c.dispose()
  created.length = 0
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('SessionEventsClient 帧解析', () => {
  it('snapshot 帧转交全量会话列表', () => {
    const { client, snapshots } = makeClient()
    client.connect()
    FakeEventSource.latest().emit('snapshot', {
      type: 'snapshot',
      items: [{ id: 's-1' }, { id: 's-2' }],
      at: 1,
    })
    expect(snapshots).toEqual([[{ id: 's-1' }, { id: 's-2' }]])
  })

  it('session.updated 帧转交单个会话', () => {
    const { client, updates } = makeClient()
    client.connect()
    FakeEventSource.latest().emit('session.updated', {
      type: 'session.updated',
      session: { id: 's-9', title: 'x' },
      at: 1,
    })
    expect(updates).toEqual([{ id: 's-9', title: 'x' }])
  })

  it('session.removed 帧转交会话 id', () => {
    const { client, removals } = makeClient()
    client.connect()
    FakeEventSource.latest().emit('session.removed', {
      type: 'session.removed',
      session_id: 's-7',
      at: 1,
    })
    expect(removals).toEqual(['s-7'])
  })

  it('畸形 JSON 帧被静默跳过，且不影响后续帧（列表不会永久失联）', () => {
    const { client, snapshots } = makeClient()
    client.connect()
    const es = FakeEventSource.latest()

    expect(() => es.emit('snapshot', '{ this is not json')).not.toThrow()
    expect(snapshots).toEqual([])

    // 关键：后续正常帧仍能处理（若畸形帧让连接崩掉，这里会是空的）
    es.emit('snapshot', { type: 'snapshot', items: [{ id: 's-1' }], at: 2 })
    expect(snapshots).toHaveLength(1)
  })

  it('缺字段的帧被忽略（items 非数组 / session 缺失 / session_id 缺失）', () => {
    const { client, snapshots, updates, removals } = makeClient()
    client.connect()
    const es = FakeEventSource.latest()

    es.emit('snapshot', { type: 'snapshot', items: 'not-an-array', at: 1 })
    es.emit('session.updated', { type: 'session.updated', at: 1 })
    es.emit('session.removed', { type: 'session.removed', at: 1 })

    expect(snapshots).toEqual([])
    expect(updates).toEqual([])
    expect(removals).toEqual([])
  })
})

describe('SessionEventsClient 领域列表信号', () => {
  it('projects.changed 触发 onDomainChanged("projects")', () => {
    const domains: string[] = []
    const { client } = makeClient({ onDomainChanged: (d) => domains.push(d) })
    client.connect()
    FakeEventSource.latest().emit('projects.changed', { type: 'projects.changed', at: 1 })
    expect(domains).toEqual(['projects'])
  })

  it('workspaces.changed 触发 onDomainChanged("workspaces")', () => {
    const domains: string[] = []
    const { client } = makeClient({ onDomainChanged: (d) => domains.push(d) })
    client.connect()
    FakeEventSource.latest().emit('workspaces.changed', { type: 'workspaces.changed', at: 1 })
    expect(domains).toEqual(['workspaces'])
  })

  it('信号帧是纯信号：即使 data 为空也要触发（没有载荷可解析）', () => {
    // 服务端刻意不带数据。若这里要求解析 payload.session 之类字段，
    // 信号就会被静默丢弃 → 列表永远不刷新。故监听器必须不依赖 data 内容。
    const domains: string[] = []
    const { client } = makeClient({ onDomainChanged: (d) => domains.push(d) })
    client.connect()
    const es = FakeEventSource.latest()
    es.emit('projects.changed', '')
    es.emit('workspaces.changed', '{}')
    expect(domains).toEqual(['projects', 'workspaces'])
  })

  it('未提供 onDomainChanged 时静默忽略（可选回调，不影响连接）', () => {
    const { client, snapshots } = makeClient()
    client.connect()
    const es = FakeEventSource.latest()
    expect(() => es.emit('projects.changed', { type: 'projects.changed', at: 1 })).not.toThrow()
    // 连接仍然可用
    es.emit('snapshot', { type: 'snapshot', items: [{ id: 's-1' }], at: 2 })
    expect(snapshots).toHaveLength(1)
  })

  it('领域信号不影响会话事件分发（共用一条连接互不干扰）', () => {
    const domains: string[] = []
    const { client, updates } = makeClient({ onDomainChanged: (d) => domains.push(d) })
    client.connect()
    const es = FakeEventSource.latest()
    es.emit('projects.changed', { type: 'projects.changed', at: 1 })
    es.emit('session.updated', { type: 'session.updated', session: { id: 's-1' }, at: 2 })
    expect(domains).toEqual(['projects'])
    expect(updates).toEqual([{ id: 's-1' }])
  })
})

describe('SessionEventsClient 建连与重连', () => {
  it('connect() 幂等：重复调用只建一条连接', () => {
    const { client } = makeClient()
    client.connect()
    client.connect()
    client.connect()
    expect(FakeEventSource.instances).toHaveLength(1)
  })

  it('已 open 后再 connect() 不重复建连', () => {
    const { client } = makeClient()
    client.connect()
    FakeEventSource.latest().open()
    client.connect()
    expect(FakeEventSource.instances).toHaveLength(1)
  })

  it('同源 Cookie 鉴权：withCredentials 必须为 true', () => {
    const { client } = makeClient()
    client.connect()
    // 若为 false，跨域部署下 Cookie 不带，SSE 会被鉴权中间件直接 401。
    expect(FakeEventSource.latest().withCredentials).toBe(true)
  })

  it('连接错误后走指数退避重建连', () => {
    vi.useFakeTimers()
    const { client } = makeClient({ baseDelayMs: 100, maxDelayMs: 1000, maxAttempts: 3 })
    client.connect()
    const first = FakeEventSource.latest()
    expect(FakeEventSource.instances).toHaveLength(1)

    first.fail()
    // 退避是 100ms * 2^(attempts-1) * jitter(0.7~1.3)，推进足够时间即可
    vi.advanceTimersByTime(200)
    expect(FakeEventSource.instances.length).toBeGreaterThan(1)
  })

  it('超过 maxAttempts 后停止重连并置为 closed（不做无限重试耗电）', () => {
    vi.useFakeTimers()
    const { client, states } = makeClient({ baseDelayMs: 10, maxDelayMs: 20, maxAttempts: 2 })
    client.connect()

    // 连续失败 maxAttempts 次，每次都推进计时器触发下一次建连
    for (let i = 0; i < 5; i++) {
      FakeEventSource.latest().fail()
      vi.advanceTimersByTime(100)
    }
    expect(client.currentState).toBe('closed')
    expect(states).toContain('closed')
  })

  it('dispose() 后彻底停止：不再重连（防 React 卸载后泄漏）', () => {
    vi.useFakeTimers()
    const { client } = makeClient({ baseDelayMs: 10, maxAttempts: 5 })
    client.connect()
    const es = FakeEventSource.latest()

    client.dispose()
    expect(es.closed).toBe(true)

    const countAfterDispose = FakeEventSource.instances.length
    es.fail()
    vi.advanceTimersByTime(500)
    // 卸载后不得再新建连接
    expect(FakeEventSource.instances.length).toBe(countAfterDispose)
    expect(client.currentState).toBe('closed')
  })

  it('dispose() 移除 visibilitychange 监听（不留悬挂监听器）', () => {
    const removeSpy = vi.spyOn(document, 'removeEventListener')
    const { client } = makeClient()
    client.connect()
    client.dispose()
    expect(removeSpy).toHaveBeenCalledWith('visibilitychange', expect.any(Function))
    removeSpy.mockRestore()
  })

  it('回前台时立即重连（移动端锁屏后不必等退避）', () => {
    vi.useFakeTimers()
    const { client } = makeClient({ baseDelayMs: 10_000, maxAttempts: 5 })
    client.connect()
    FakeEventSource.latest().fail() // 进入 backoff（等待 10s）
    expect(client.currentState).toBe('backoff')

    const countBefore = FakeEventSource.instances.length
    // 模拟切回前台
    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true })
    document.dispatchEvent(new Event('visibilitychange'))

    // 立即重连，不需要等 10s 退避
    expect(FakeEventSource.instances.length).toBe(countBefore + 1)
  })

  it('connect 后状态依次为 connecting → open', () => {
    const { client, states } = makeClient()
    client.connect()
    expect(states).toEqual(['connecting'])
    FakeEventSource.latest().open()
    expect(states).toEqual(['connecting', 'open'])
    expect(client.currentState).toBe('open')
  })
})
