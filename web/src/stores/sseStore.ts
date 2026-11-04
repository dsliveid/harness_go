import { create } from 'zustand'
import { apiMode } from '../lib/api'
import type { SseState as ClientPhase, SseStateInfo } from '../lib/sse'

// ============ SSE 客户端状态（**真实**上报容器） ============
//
// 对齐 docs/架构设计文档.md 的客户端状态机：idle → connecting → open → backoff → resync → open
//
// ⚠️ 历史教训（务必保留这段注释）：
//   本 store 曾经是纯硬编码假值容器（lastSeq:214 / totalSeq:218 / reconnects:2 /
//   lastDrop 永为「43 分钟前缓冲已满」），并被 features/chat/SseStatus.tsx 常驻渲染在
//   layout/TopBar.tsx 上 —— 也就是**每一个登录页顶部都在展示假的 SSE 连接状态**，
//   且 RealModeNotice 机制对它无效（它是状态点不是面板）。
//
//   现在改为「真实状态上报容器」：唯一写入方是 lib/sse.ts 的 SseClient.onState，
//   经 lib/api.ts（real.startRun）→ stores/chatStore（转发）落到 applyReal()。
//   没有活跃订阅时 phase 恒为 idle / closed，UI 必须如实显示「未订阅」，**不得伪造指标**。
//
// 🚫 禁止再引入后端无对应上报概念的字段（例如曾经的 totalSeq / bufferUsed / bufferCap）：
//    后端 per-Run SSE 只推送事件帧，不暴露「服务端总事件数」或「订阅缓冲占用」，
//    凭空展示这类数字等价于造假。

export type SsePhase = ClientPhase

export interface SseDrop {
  at: number
  /** 断开原因（来自 SseClient 的 info.reason 或浏览器 error） */
  reason: string
  /** 本次退避等待毫秒数（仅 backoff 有值） */
  retryIn?: number
  /** 断开时的累计重连次数 */
  attempts: number
}

export interface SseState {
  phase: SsePhase
  /** 当前订阅的 Run（无订阅为 null） */
  runId: string | null
  /** 已接收事件的 lastEventId 游标（真实值，无订阅归零） */
  lastSeq: number
  /** 累计重连次数（真实值） */
  reconnects: number
  /** 退避等待毫秒（仅 backoff 有值） */
  retryIn: number | null
  /** 最近一次断开记录 */
  lastDrop: SseDrop | null
  /** 是否正在演示断线重连（仅 mock 模式可用） */
  demoRunning: boolean

  /** 开始/切换订阅目标（Run 启动时由 chatStore 调用） */
  attachRun: (runId: string) => void
  /** 仅回填真实 runId（服务端返回后补正本地壳 id），**不改动**当前连接阶段 */
  retargetRun: (runId: string) => void
  /** 结束订阅（Run 终态 / 停止 / 组件卸载时调用），回到诚实空闲态 */
  detachRun: () => void
  /** 收到一个真实事件：推进游标 */
  noteEvent: (seq: number) => void
  /** 真实状态机上报（唯一真实入口，由 lib/api.ts → chatStore 转发） */
  applyReal: (phase: ClientPhase, info?: SseStateInfo) => void
  /** mock 模式演示断线重连（real 模式下为空操作） */
  simulateDrop: () => void
  resetDemo: () => void
}

/** 诚实空闲态：没有任何订阅时不展示任何虚构指标 */
const idleState = {
  phase: 'idle' as SsePhase,
  runId: null,
  lastSeq: 0,
  reconnects: 0,
  retryIn: null,
  lastDrop: null,
  demoRunning: false,
}

export const SSE_PHASE_META: Record<SsePhase, { label: string; tone: 'ok' | 'warn' | 'accent2' | 'err' | 'dim'; hint: string }> = {
  idle: { label: '未订阅', tone: 'dim', hint: '当前没有进行中的 Run，未建立 SSE 订阅（空闲不占用连接）' },
  open: { label: '已连接', tone: 'ok', hint: 'SSE 订阅正常，实时接收事件流' },
  connecting: { label: '连接中', tone: 'warn', hint: '正在与 /sessions/{sid}/runs/{rid}/events 建立 SSE 订阅' },
  backoff: { label: '重连中', tone: 'warn', hint: '连接断开，指数退避重连中（携带 lastEventId 续传）' },
  resync: { label: '补同步中', tone: 'accent2', hint: '收到 resync 帧，正以数据库为准重拉消息快照' },
  closed: { label: '已断开', tone: 'err', hint: '订阅已关闭（主动停止或超过最大重连次数）' },
}

export const useSse = create<SseState>((set, get) => ({
  ...idleState,

  attachRun: (runId) =>
    set({
      ...idleState,
      runId,
      phase: 'connecting',
    }),

  retargetRun: (runId) => set({ runId }),

  detachRun: () => set({ ...idleState }),

  noteEvent: (seq) => {
    if (!Number.isFinite(seq)) return
    set((st) => ({ lastSeq: seq > st.lastSeq ? seq : st.lastSeq }))
  },

  applyReal: (phase, info = {}) => {
    const s = get()
    // demo 进行中时不被真实状态覆盖（demo 仅 mock 模式可触发，且无真实订阅并发）
    if (s.demoRunning && apiMode === 'mock') return

    const patch: Partial<SseState> = { phase }

    if (typeof info.attempts === 'number') patch.reconnects = info.attempts
    if (typeof info.retryIn === 'number') patch.retryIn = info.retryIn

    if (phase === 'backoff') {
      patch.lastDrop = {
        at: Date.now(),
        reason: info.reason ?? '连接中断，进入指数退避重连',
        retryIn: info.retryIn,
        attempts: info.attempts ?? s.reconnects,
      }
    }
    if (phase === 'closed' && s.phase !== 'closed') {
      patch.lastDrop = {
        at: Date.now(),
        reason: info.reason === 'max_attempts' ? '超过最大重连次数，订阅已关闭' : info.reason === 'client_closed' ? '主动关闭订阅' : (info.reason ?? '订阅已关闭'),
        attempts: info.attempts ?? s.reconnects,
      }
    }
    if (phase === 'open') patch.retryIn = null
    if (phase === 'closed') patch.runId = null

    set(patch)
  },

  // 演示：仅 mock 模式可用（real 模式下真实状态由 SseClient 驱动，不得伪造）
  simulateDrop: () => {
    if (apiMode !== 'mock') return
    const s = get()
    if (s.demoRunning || s.phase === 'backoff') return
    const reconnects = s.reconnects + 1
    set({ demoRunning: true, phase: 'backoff', retryIn: 1200, runId: s.runId ?? 'demo-run', lastSeq: s.lastSeq || 0 })

    window.setTimeout(() => {
      set({ phase: 'resync', reconnects, lastDrop: { at: Date.now(), reason: '网络抖动 / EventSource 连接被重置（演示）', retryIn: 1200, attempts: reconnects } })
      window.setTimeout(() => {
        set((st) => ({ phase: 'open', retryIn: null, demoRunning: false, lastSeq: st.lastSeq + 3 }))
      }, 1400)
    }, 1200)
  },

  resetDemo: () => set({ ...idleState }),
}))
