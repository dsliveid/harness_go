import { create } from 'zustand'

// ============ SSE 客户端状态机（断线续传可见态，原型 mock） ============
// 对齐 docs/架构设计文档.md 的客户端状态机：idle → connecting → open → backoff → resync → open
// 真实实现：EventSource/fetch-stream + lastEventId 游标；断开指数退避重连，
// 重连成功先走 resync（对齐拉取错过的事件）再回 open；visibilitychange 回前台立即重连。

export type SsePhase = 'connecting' | 'open' | 'backoff' | 'resync' | 'offline'

export interface SseDrop {
  at: number
  reason: string
  /** 重连恢复后补拉的事件数 */
  resynced: number
  backoffMs: number
}

export interface SseState {
  phase: SsePhase
  /** 已接收事件游标（lastEventId） */
  lastSeq: number
  /** 服务端已知总事件数（模拟 resync 差额用） */
  totalSeq: number
  /** 订阅通道缓冲占用（容量 256） */
  bufferUsed: number
  bufferCap: number
  /** 累计重连次数 */
  reconnects: number
  /** 最近一次断开记录 */
  lastDrop: SseDrop | null
  /** 是否正在演示断线重连 */
  demoRunning: boolean
  simulateDrop: () => void
  resetDemo: () => void
}

const initial: SseState = {
  phase: 'open',
  lastSeq: 214,
  totalSeq: 218,
  bufferUsed: 47,
  bufferCap: 256,
  reconnects: 2,
  lastDrop: { at: Date.now() - 43 * 60_000, reason: '订阅通道缓冲已满，主动断开慢消费者', resynced: 3, backoffMs: 1200 },
  demoRunning: false,
  simulateDrop: () => {},
  resetDemo: () => {},
}

export const SSE_PHASE_META: Record<SsePhase, { label: string; tone: 'ok' | 'warn' | 'accent2' | 'err'; hint: string }> = {
  open: { label: '已连接', tone: 'ok', hint: 'SSE 订阅正常，实时接收事件流' },
  connecting: { label: '连接中', tone: 'warn', hint: '正在与 /api/events 建立 SSE 订阅' },
  backoff: { label: '重连中', tone: 'warn', hint: '连接断开，指数退避重连（1.2s 起）' },
  resync: { label: '补同步中', tone: 'accent2', hint: '重连成功，按 lastEventId 游标补拉错过的事件' },
  offline: { label: '已断开', tone: 'err', hint: '订阅不可用；发送与工具执行将不可用' },
}

export const useSse = create<SseState>((set, get) => ({
  ...initial,

  // 演示：open → backoff（退避）→ resync（补拉差额）→ open，让断线续传全链路可见
  simulateDrop: () => {
    const s = get()
    if (s.demoRunning || s.phase !== 'open') return
    set({ demoRunning: true, phase: 'backoff' })

    const reconnects = s.reconnects + 1
    const drop: SseDrop = {
      at: Date.now(),
      reason: '网络抖动 / EventSource 连接被重置（模拟）',
      resynced: 0,
      backoffMs: 1200,
    }

    window.setTimeout(() => {
      // 重连成功 → 进入 resync：按游标补拉错过的里程碑事件
      const cur = get()
      set({
        phase: 'resync',
        reconnects,
        lastDrop: { ...drop, resynced: Math.max(0, cur.totalSeq - cur.lastSeq) },
      })

      window.setTimeout(() => {
        const done = get()
        set({
          phase: 'open',
          lastSeq: done.totalSeq,
          demoRunning: false,
        })
        // 游标对齐后再制造一点新事件与缓冲占用，保持仪表真实感
        window.setTimeout(() => {
          set((st) => ({ lastSeq: st.lastSeq + 2, totalSeq: st.lastSeq + 2, bufferUsed: Math.max(0, st.bufferUsed - 5) }))
        }, 2600)
      }, 1400)
    }, 1200)
  },

  resetDemo: () => set({ ...initial }),
}))
