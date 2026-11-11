import { create } from 'zustand'
import type {
  Approval, DataScope, Message, Run, ToolCall,
} from '../types'
import { api, type Decision, type RunHandle, type StreamEvent } from '../lib/api'
import type { ActiveRunDTO } from '../lib/apiTypes'
import type { SseState as SseStateName, SseStateInfo } from '../lib/sse'
import { DATA_SCOPE_LABEL } from '../lib/dataScopes'
import { useSessions } from './sessionStore'
import { useSse } from './sseStore'
import { useUI } from './uiStore'
import { useWorkspace } from './workspaceStore'

// 模块级流式缓冲：delta 累积后按 50ms 批量提交，避免逐 token setState
//
// handle 只承载唯一的真实通道（订阅 / 停止 / 重连）；服务端是 Run 的唯一执行方。
let handle: RunHandle | null = null
// startRun 的 POST 在途时点了「停止」：拿到句柄后立即补一次 stop，避免服务端 Run 空跑
let cancelPending = false
let pendingText = ''
let pendingReasoning = ''
let flushTimer: ReturnType<typeof setTimeout> | null = null
let streamingMsgId: string | null = null
let currentRunId: string | null = null
/** 当前被跟踪 Run 所属会话（切会话后仍要继续跟踪原 Run，故与 currentId 分开记） */
let currentRunSessionId: string | null = null
let stepStartAt = 0
/** 每会话最近一次消息级回滚的事件 id（real 模式 reapply 依据；S10 语义，刷新后从服务端 rollbacks 列表不可得时降级为不可重放） */
const lastRollbackBySession: Record<string, string> = {}

/**
 * 把「当前会话的消息序列」推给 workspaceStore 派生右侧浮动栏的运行态数据
 * （todos / plans / subagents / commands）。
 *
 * 为什么挂在这里：这四类数据后端没有专用端点，唯一真源就是 agent 工具调用，
 * 而工具调用已经作为 `messages[].toolCalls` 存在——SSE 实时增量与
 * `GET /sessions/{sid}/messages` 历史回灌产出的都是同一形状，所以只要在
 * 「消息里的工具态发生变化」的所有出口调用一次，实时与刷新恢复就一并打通。
 * 过滤掉别的会话，避免切换会话时串台展示上一条会话的清单。
 */
function syncWorkspaceFromMessages() {
  const sessionId = useSessions.getState().currentId
  const msgs = useChat.getState().messages.filter((m) => !sessionId || m.sessionId === sessionId)
  useWorkspace.getState().syncFromMessages(msgs)
}

/** 单会话 token 账本条目（inputs / outputs / cached）。 */
export interface SessionUsage {
  tokensIn: number
  tokensOut: number
  cachedTokens: number
}

/**
 * 每会话 token 账本（sessionId → 累计用量）。
 *
 * 为什么需要它：tokensIn/tokensOut/cachedTokens 若只保留一份全局状态，
 * 切换/新建会话时数值不会变化（新对话仍显示上个会话的累计量），
 * 且切回旧会话后无法恢复它自己的水位。账本按会话隔离即两全。
 * 仅前端内存态（delta 不落库、以 DB 为准的铁律不变）。
 */
const usageBySession: Record<string, SessionUsage> = {}

/** 把一次 Run 的用量累加进该会话账本，并返回累加后的最新值。 */
function accumulateUsage(sessionId: string, delta: SessionUsage): SessionUsage {
  const prev = usageBySession[sessionId] ?? { tokensIn: 0, tokensOut: 0, cachedTokens: 0 }
  const next: SessionUsage = {
    tokensIn: prev.tokensIn + delta.tokensIn,
    tokensOut: prev.tokensOut + delta.tokensOut,
    cachedTokens: prev.cachedTokens + delta.cachedTokens,
  }
  usageBySession[sessionId] = next
  return next
}

/** 读取某会话的用量水位（无记录 = 全零，绝不借用别的会话数值）。 */
function usageOf(sessionId: string): SessionUsage {
  return usageBySession[sessionId] ?? { tokensIn: 0, tokensOut: 0, cachedTokens: 0 }
}

/**
 * SSE 真实状态上报转发（lib/api.ts → 顶栏 SseStatus）。
 * 单独抽成函数而非内联，是为了让开始 / 恢复两条订阅路径共用同一套转发与终态清理逻辑。
 */
function applySseState(state: SseStateName, info: SseStateInfo) {
  useSse.getState().applyReal(state, info)
}

/** Run 是否已到终态（completed / failed / cancelled / interrupted）——用于 resync 收口判定。 */
function isRunTerminal(status: Run['status']): boolean {
  return status !== 'pending' && status !== 'running' && status !== 'waiting_approval'
}

interface ChatState {
  messages: Message[]
  runStatus: Run['status']
  approval: Approval | null
  queued: string[]
  streaming: boolean
  reasoning: string
  tokensIn: number
  tokensOut: number
  /** 会话累计缓存命中 tokens（加权命中率分母为 tokensIn） */
  cachedTokens: number
  lastError: string | null
  // 说明：上下文「智能压缩」交互链路已整体下线（后端无压缩器 / 无压缩记录表 / 无压缩接口，
  // 保留入口即静默失败）。此处不再持有任何压缩状态与方法，真实 token 水位仅由 tokensIn/tokensOut/cachedTokens 承载。
  /** 推送一条数据权限审批（不阻塞 Run） */
  pushDataApproval: (input: { scope: DataScope; toolName: string; summary: string; args: Record<string, unknown> }) => void
  // 输入框草稿（供撤回 / 重新引用回填）
  draft: string
  setDraft: (text: string) => void
  send: (input: string) => void
  stop: () => void
  retry: () => void
  editResend: (msgId: string, content: string) => void
  guide: (input: string) => void
  removeQueued: (index: number) => void
  respond: (decision: Decision, reason?: string) => void
  fork: (msgId: string) => void
  revertTo: (msgId: string) => void
  reapply: (msgId: string) => void
  deleteFrom: (msgId: string) => void
  messagesOf: (sessionId: string) => Message[]
  /** 以 DB 为准重拉某会话的消息快照（real 模式；断线重连/resync 时对齐） */
  reloadMessages: (sessionId: string) => Promise<void>
  /**
   * 刷新/切回会话后，接回服务端仍在跑的 Run（重建「进行中」UI 与 SSE 订阅）。
   *
   * 根因背景：streaming / runStatus / streamingMsgId 全是纯内存态，刷新即归零，
   * 而服务端 Run 仍在跑 → 界面完全看不出对话还在进行。此方法是对该缺陷的收口。
   */
  resumeRun: (sessionId: string) => Promise<void>
  /**
   * 切换会话时把 token 水位切到该会话自己的账本上（新会话 = 全零）。
   *
   * 背景（踩坑）：tokensIn/tokensOut/cachedTokens 曾是全局单份状态，
   * 导致「新开对话后输入框下方的 token 用量与上下文占用纹丝不动」——
   * 数值是上一个会话的累计量。账本按 sessionId 隔离后，新对话从 0 起算，
   * 切回旧会话还能恢复它自己的真实水位。
   */
  activateSession: (sessionId: string) => void
  /** 本地壳会话 id → 服务端真实 id 时迁徙用量账本（否则切回后水位丢失） */
  migrateUsage: (fromId: string, toId: string) => void
}

export const useChat = create<ChatState>((set, get) => {
  const flush = () => {
    if (flushTimer) {
      clearTimeout(flushTimer)
      flushTimer = null
    }
    if (!streamingMsgId) {
      pendingText = ''
      pendingReasoning = ''
      return
    }
    const chunk = pendingText
    const think = pendingReasoning
    pendingText = ''
    pendingReasoning = ''
    if (!chunk && !think) return
    const id = streamingMsgId
    set((s) => ({
      messages: s.messages.map((m) =>
        m.id === id
          ? {
              ...m,
              content: chunk ? m.content + chunk : m.content,
              // 思考过程与正文同样累积到当前步消息上（刷新/重连后由 DB 快照对齐）
              reasoning: think ? `${m.reasoning ?? ''}${think}` : m.reasoning,
            }
          : m,
      ),
      // 全局水位同步累积：仅作为「消息尚未承载 reasoning」时的兜底展示来源
      reasoning: think ? `${s.reasoning}${think}` : s.reasoning,
    }))
  }

  const scheduleFlush = () => {
    if (flushTimer) return
    flushTimer = setTimeout(flush, 50)
  }

  const patchTool = (callId: string, patch: Partial<ToolCall>, appendResult?: string, appendLive?: string) => {
    flush()
    set((s) => ({
      messages: s.messages.map((m) => {
        if (!m.toolCalls?.some((c) => c.id === callId)) return m
        return {
          ...m,
          toolCalls: m.toolCalls.map((c) =>
            c.id === callId
              ? {
                  ...c,
                  ...patch,
                  result: appendResult !== undefined ? `${c.result ? `${c.result}\n` : ''}${appendResult}` : c.result,
                  liveOutput: appendLive !== undefined ? `${c.liveOutput ?? ''}${appendLive}` : c.liveOutput,
                }
              : c,
          ),
        }
      }),
    }))
  }

  /**
   * 流式事件状态机（**全局唯一一份**）。
   *
   * 为什么抽到闭包级：`startRun`（新建 Run）与 `resumeRun`（刷新后接回服务端仍在跑的 Run）
   * 必须共用同一套 delta / tool / step / approval / error / done 收口逻辑。
   * 此前它内联在 startRun 里，导致刷新恢复路径根本无法复用 —— 这正是
   * 「刷新后界面看不出对话还在进行」的根因之一（另两个：本地流式态是纯内存、未重新订阅 SSE）。
   *
   * @param runSessionId 该 Run 所属会话（**不是**当前选中的会话）：用户切走后
   *   Run 仍会继续产出，收口必须落回它自己的会话与用量账本。
   */
  /**
   * 单点收口（单一真相铁律）：所有 Run 终结路径的唯一出口。
   *
   * 为什么必须只有这一处：收口动作曾散落在 done / error / resumeRun-onResync 三处各写一遍，
   * 结果每次修好一处、另两处仍是旧行为 —— 历史缺陷「对话突然停止」与「刷新后永久转圈」
   * 都是这么复发的。任何终结路径都必须经由 settleRun。
   *
   * 收口职责：结算当前流式消息（停转圈 + 记耗时）→ 复位全局运行态 → 清模块级指针 →
   * 断开 SSE 订阅 → 若有排队输入则续跑下一条。
   */
  const settleRun = (
    runStatus: Run['status'],
    opts: { error?: string; toast?: string; usage?: { tokensIn: number; tokensOut: number; cachedTokens?: number } } = {},
  ) => {
    flush()
    const nowSettle = Date.now()
    const id = streamingMsgId
    const sessionId = currentRunSessionId
    let usagePatch: Partial<Pick<ChatState, 'tokensIn' | 'tokensOut' | 'cachedTokens'>> = {}
    if (opts.usage && sessionId) {
      // 用量记进本 Run 所属会话的账本（不是全局单份）：
      // 只有该会话仍是当前会话时才刷新展示值，避免 A 会话 Run 结束污染 B 会话水位条。
      const usage = accumulateUsage(sessionId, {
        tokensIn: opts.usage.tokensIn,
        tokensOut: opts.usage.tokensOut,
        cachedTokens: opts.usage.cachedTokens ?? 0,
      })
      const isCurrent = useSessions.getState().currentId === sessionId
      usagePatch = {
        tokensIn: isCurrent ? usage.tokensIn : get().tokensIn,
        tokensOut: isCurrent ? usage.tokensOut : get().tokensOut,
        cachedTokens: isCurrent ? usage.cachedTokens : get().cachedTokens,
      }
    }
    set((s) => ({
      messages: s.messages.map((m) =>
        m.id === id
          ? {
              ...m,
              streaming: false,
              durationMs: nowSettle - stepStartAt,
              ...(opts.usage
                ? {
                    tokens: opts.usage.tokensOut + opts.usage.tokensIn,
                    promptTokens: opts.usage.tokensIn,
                    cachedTokens: opts.usage.cachedTokens,
                  }
                : {}),
            }
          : m,
      ),
      streaming: false,
      runStatus,
      reasoning: '',
      approval: null,
      ...(opts.error ? { lastError: opts.error } : {}),
      ...usagePatch,
    }))
    if (opts.toast) useUI.getState().toast('warn', opts.toast)
    streamingMsgId = null
    currentRunId = null
    currentRunSessionId = null
    handle = null
    // 订阅结束：如实回到空闲态（顶栏不再残留「已连接」假象）
    useSse.getState().detachRun()
    // 排队输入继续依次执行（失败与成功同语义：不因一次结束阻塞后续）
    const queued = get().queued
    if (queued.length > 0) {
      const next = queued[0]
      set({ queued: queued.slice(1) })
      setTimeout(() => startRun(next), 260)
    }
  }

  const makeStreamEventHandler = (runSessionId: string) => (e: StreamEvent) => {
        // 事件隔离守卫：本地同一时刻只跟踪一个 Run（streamingMsgId / pendingText 是模块级单份）。
        // 用户切走该会话（或恢复了别的会话的 Run）后，本 Run 的残留事件不得再写入全局 UI 态，
        // 否则会把 A 会话的增量/收口错误地打到 B 会话的消息与状态上。
        if (currentRunSessionId !== runSessionId) return
        // Run 所属会话（收口、建新步骤消息、用量记账都落在它身上，与用户当前正在看哪个会话无关）
        const sessionId = runSessionId
        switch (e.type) {
          case 'delta':
            pendingText += e.text
            scheduleFlush()
            break
          case 'reasoning':
            // 思考过程增量：与正文同批次提交（累积到当前步消息的 reasoning 上）
            pendingReasoning += e.text
            scheduleFlush()
            break
          case 'tool_start':
            flush()
            set((s) => ({
              messages: s.messages.map((m) =>
                m.id === streamingMsgId
                  ? { ...m, toolCalls: [...(m.toolCalls || []), e.call] }
                  : m,
              ),
            }))
            // 工具调用一起来就刷新运行态：todo_write / plan_set 的 args 此刻已完整，
            // 右侧清单随之即时出现，不用等整轮跑完
            syncWorkspaceFromMessages()
            break
          case 'tool_result': {
            // 终态 status 以事件为准：denied 走「已拒绝」样式，error/timeout 走错误态
            const status: ToolCall['status'] = e.skipped ? 'denied' : e.failed ? 'error' : 'ok'
            if (e.durationMs === 0) {
              // real 模式 tool.result 不带耗时（服务端未下发），不能因此丢弃 result / diff
              patchTool(e.callId, { status, ...(e.diff ? { diff: e.diff } : {}) }, e.result)
            } else {
              patchTool(e.callId, {
                status,
                durationMs: e.durationMs,
                ...(e.diff ? { diff: e.diff } : {}),
                result: e.result,
              })
            }
            // 终态落定后再派生一次：命令从「运行中」转「已退出」、子代理转显式终态
            syncWorkspaceFromMessages()
            break
          }
          case 'tool_progress':
            // 实时增量输出（需求 §10-4）：追加到运行中的工具卡片输出区；
            // 终态结果仍以 tool.result 为准（完成后 ToolCard 展示 result，不再展示 liveOutput）
            patchTool(e.callId, {}, undefined, e.chunk)
            // 命令行面板复用它显示实时输出，故同步一次
            syncWorkspaceFromMessages()
            break
          case 'approval':
            flush()
            set({ approval: e.approval, runStatus: 'waiting_approval' })
            break
          case 'step': {
            // 结算当前步（耗时 + tokens），开启同轮新步骤
            flush()
            const now2 = Date.now()
            const prevId = streamingMsgId
            const sessionMsgs2 = get().messages.filter((m) => m.sessionId === sessionId)
            const nextSeq = sessionMsgs2.reduce((a, m) => Math.max(a, m.seq), 0) + 1
            const turnSeq = sessionMsgs2.find((m) => m.id === prevId)?.turnSeq ?? 1
            const nextMsg: Message = {
              id: `ma-${now2}`, sessionId, runId: currentRunId ?? undefined,
              seq: nextSeq, turnSeq, role: 'assistant', content: '',
              createdAt: now2, streaming: true, toolCalls: [],
            }
            // message.end 携带的 reasoning 为终值（delta 仅预览，断线补发可能缺失）；
            // 服务端未产出思考过程时为空串，此时保留已累积的内容，绝不用空串覆盖
            const finalReasoning = e.reasoning && e.reasoning.trim() ? e.reasoning : undefined
            set((s) => ({
              messages: [
                ...s.messages.map((m) =>
                  m.id === prevId
                    ? {
                        ...m,
                        streaming: false,
                        durationMs: now2 - stepStartAt,
                        tokens: e.tokens ?? m.tokens,
                        promptTokens: e.promptTokens ?? m.promptTokens,
                        cachedTokens: e.cachedTokens ?? m.cachedTokens,
                        reasoning: finalReasoning ?? m.reasoning,
                      }
                    : m,
                ),
                nextMsg,
              ],
              // 步骤已结算，全局兜底水位清空（新步骤重新累积）
              reasoning: '',
            }))
            streamingMsgId = nextMsg.id
            stepStartAt = now2
            break
          }
          case 'error': {
            // Run 非成功终态（failed / cancelled）必须完整收口，否则 UI 会停在
            // 「气泡转圈 + 顶栏已连接」的假运行态。收口动作统一走 settleRun（单点）。
            settleRun('failed', { error: `${e.code}: ${e.message}`, toast: e.message })
            break
          }
          case 'done': {
            // 成功终态：用量记进本 Run 所属会话的账本后收口（单点 settleRun）。
            settleRun('completed', {
              usage: { tokensIn: e.tokensIn, tokensOut: e.tokensOut, cachedTokens: e.cachedTokens },
            })
            break
          }
        }
  }

  /**
   * 订阅一个 Run 的事件流（**唯一订阅装配点**）。
   *
   * 存在意义：`startRun`（POST 后订阅新建 Run）与 `resumeRun`（刷新后接回服务端仍在跑的 Run）
   * 曾经各写一套 onEvent/onResync/onSseState 装配，导致两条路径的收口语义会各自漂移
   * —— 这正是「刷新后永久转圈」「对话突然停止」反复复发的结构性原因。
   *
   * resync 处理（流程清晰化的关键落点）：resync 只做「对齐 + 二选一」，绝不猜状态：
   *   ① 先按 DB 快照对齐消息（delta 只是预览，DB 才是权威）；
   *   ② 再查 Run 真实状态：已终态 → settleRun 收口；未终态 → 立即重连继续收事件。
   */
  const attachToRun = (opts: {
    sessionId: string
    runId: string
    fromSeq?: number
    onEvent: (e: StreamEvent) => void
  }) => {
    const onResync = () => {
      void (async () => {
        await get().reloadMessages(opts.sessionId)
        // 本地已不再跟踪该 Run（用户切会话/已收口）：不重连、不重复收口
        if (currentRunSessionId !== opts.sessionId || currentRunId !== opts.runId) return
        try {
          const run = await api.getRun(opts.sessionId, opts.runId)
          if (isRunTerminal(run.status)) {
            settleRun(run.status)
            return
          }
          // Run 仍活着：SSE 因慢消费者/断线被服务端断开过，必须重连才收得到后续事件
          handle?.reconnect()
        } catch {
          /* 查询失败：保持现状，等下一次 resync 再对齐（绝不猜测状态） */
        }
      })()
    }
    const h = api.attachRun({
      sessionId: opts.sessionId,
      runId: opts.runId,
      fromSeq: opts.fromSeq,
      onEvent: opts.onEvent,
      onResync,
      onSseState: (state, info) => {
        applySseState(state, info)
        // 服务端返回真实 runId 后回填订阅目标（本地壳 id 与真实 id 不同）
        if (state === 'open' && currentRunId && useSse.getState().runId !== currentRunId) {
          useSse.getState().retargetRun(currentRunId)
        }
      },
      onSseEventSeq: (seq) => useSse.getState().noteEvent(seq),
    })
    handle = h
    return h
  }

  const startRun = (input: string) => {
    const sessionId = useSessions.getState().currentId
    // 没有「默认模型」的隐式回退：会话未选择模型时直接拦截提示（避免静默落到 fake 回显模型）
    const session = useSessions.getState().sessions.find((s) => s.id === sessionId)
    if (!session?.modelRef) {
      useUI.getState().toast('warn', '请先选择模型')
      return
    }
    // 发送新消息将覆盖撤回边界之后的历史（对齐桌面端）
    if (get().messages.some((m) => m.sessionId === sessionId && m.revertedAt)) {
      set((s) => ({ messages: s.messages.filter((m) => !(m.sessionId === sessionId && m.revertedAt)) }))
    }
    const { messages } = get()
    const sessionMsgs = messages.filter((m) => m.sessionId === sessionId && !m.reverted)
    const maxSeq = sessionMsgs.reduce((a, m) => Math.max(a, m.seq), 0)
    const maxTurn = sessionMsgs.reduce((a, m) => Math.max(a, m.turnSeq), 0)
    const now = Date.now()
    const runId = `run-${now}`
    const userMsg: Message = {
      id: `mu-${now}`, sessionId, runId, seq: maxSeq + 1, turnSeq: maxTurn + 1,
      role: 'user', content: input, createdAt: now,
    }
    const assistantMsg: Message = {
      id: `ma-${now}`, sessionId, runId, seq: maxSeq + 2, turnSeq: maxTurn + 1,
      role: 'assistant', content: '', createdAt: now, streaming: true, toolCalls: [],
    }
    streamingMsgId = assistantMsg.id
    currentRunId = runId
    currentRunSessionId = sessionId
    stepStartAt = now
    pendingText = ''
    pendingReasoning = ''
    set((s) => ({
      messages: [...s.messages, userMsg, assistantMsg],
      streaming: true,
      runStatus: 'running',
      reasoning: '',
      lastError: null,
      approval: null,
    }))

    const onStreamEvent = makeStreamEventHandler(sessionId)

    // Run 在服务端执行（本地 runId 仅用于消息壳，真实 id 以服务端返回为准）
    cancelPending = false
    // 订阅开始：顶栏如实进入「连接中」，此后由 SseClient 状态机接管
    useSse.getState().attachRun(runId)
    // 模型引用：startRun 入口已保证会话已选模型
    const modelRef = session.modelRef || undefined
    void api
      .startRun({
        sessionId,
        input,
        modelRef,
        onEvent: onStreamEvent,
        onResync: () => {
          // 新建 Run 的 resync 与恢复路径共用 attachToRun 的对齐逻辑；
          // 此处仅在收到真实 runId 前兜底重拉快照（订阅装配由 attachToRun 统一负责）。
          void get().reloadMessages(sessionId)
        },
        onSseState: (state, info) => {
          applySseState(state, info)
          // 服务端返回真实 runId 后回填订阅目标（本地壳 id 与真实 id 不同）
          if (state === 'open' && currentRunId && useSse.getState().runId !== currentRunId) {
            useSse.getState().retargetRun(currentRunId)
          }
        },
        onSseEventSeq: (seq) => useSse.getState().noteEvent(seq),
      })
      .then((h) => {
        handle = h
        currentRunId = h.runId
        // 只回填 id，不重置已建立的连接阶段
        if (useSse.getState().runId && useSse.getState().runId !== h.runId) useSse.getState().retargetRun(h.runId)
        if (cancelPending) {
          cancelPending = false
          h.stop()
        }
      })
      .catch((err: unknown) => {
        const msg = err instanceof Error ? err.message : '启动对话失败'
        // 启动失败没有任何订阅：如实收口为失败态（不留下转圈气泡）
        settleRun('failed', { error: msg })
        useUI.getState().toast('error', msg)
      })
  }

  return {
    // 消息一律来自 DB（登录后 / reloadMessages 拉取），绝不预置演示数据
    messages: [],
    runStatus: 'completed',
    approval: null,
    queued: [],
    streaming: false,
    reasoning: '',
    tokensIn: 0,
    tokensOut: 0,
    cachedTokens: 0,
    lastError: null,
    draft: '',

    setDraft: (text) => set({ draft: text }),

    messagesOf: (sessionId) => get().messages.filter((m) => m.sessionId === sessionId && !m.reverted),

    // 以 DB 为准重拉消息快照：断线重连（resync）后覆盖本地流式态
    reloadMessages: async (sessionId) => {
      if (!sessionId) return
      try {
        const list = await api.listMessages(sessionId)
        set((s) => ({
          messages: [...s.messages.filter((m) => m.sessionId !== sessionId), ...list],
        }))
        // 历史消息里的工具调用是运行态数据的回灌源：切换会话 / 刷新后右侧浮动栏
        // 靠这一次派生恢复出该会话自己的 todos/plans/subagents/commands
        syncWorkspaceFromMessages()
        // 快照映射（toUiMessages）不产出 streaming 字段 → 刷新后气泡不会转圈。
        // 若本地正跟踪某 Run（刷新后由 resumeRun 建立），把流式标记补到目标消息上，
        // 让「转圈光标/思考中…」在 resync 后依然如实存在（不假装已完成）。
        if (get().streaming && streamingMsgId) {
          const id = streamingMsgId
          set((s) => ({ messages: s.messages.map((m) => (m.id === id ? { ...m, streaming: true } : m)) }))
        }
      } catch {
        /* 拉取失败保留本地态，等待下一次 resync 再对齐 */
      }
    },

    /**
     * 刷新/切回会话后接回服务端仍在跑的 Run。
     *
     * 三步：查服务端进行中 Run → 对齐本地流式目标（消息壳）→ 以 fromSeq=-1 重新订阅。
     * fromSeq=-1 = 只要新事件、不补发历史：此时 DB 快照已给出消息全文，
     * 再补发历史 delta 会把正文重复追加一遍（脏数据级缺陷）。
     */
    resumeRun: async (sessionId) => {
      if (!sessionId) return
      // 仅当本地正在跟踪的**就是该会话**的 Run 时才跳过；跟踪别的会话不影响本会话恢复
      if (get().streaming && currentRunId && useSessions.getState().currentId === sessionId
        && currentRunSessionId === sessionId) return
      // 切到另一个会话：先把上一会话的「进行中」本地态归位（服务端 Run 不受影响，仍继续跑；
      // 切回时靠本方法重新接回）。否则输入框会误显「运行中」、顶栏残留连接态。
      if (get().streaming && currentRunSessionId && currentRunSessionId !== sessionId) {
        flush()
        set({ streaming: false, runStatus: 'completed', reasoning: '', approval: null })
        streamingMsgId = null
        currentRunId = null
        currentRunSessionId = null
        handle = null
        useSse.getState().detachRun()
      }
      let active: ActiveRunDTO | null = null
      try {
        active = await api.getActiveRun(sessionId)
      } catch {
        return // 查询失败不打扰用户：保持空闲态，绝不展示假运行
      }
      if (!active?.run_id) return
      // 查询期间用户可能已自行发起新对话（同样的会话）——此时本地是更新的真相，不覆盖
      if (get().streaming && currentRunId && currentRunSessionId === sessionId) return

      const runId = active.run_id
      const mine = get().messages.filter((m) => m.sessionId === sessionId && !m.reverted)
      // 流式目标 = 该 Run 已落库的最后一条 assistant 消息；等首包期间刷新则补一个本地壳
      const target = [...mine].reverse().find((m) => m.role === 'assistant' && m.runId === runId)
      const now = Date.now()
      if (target) {
        streamingMsgId = target.id
      } else {
        const maxSeq = mine.reduce((a, m) => Math.max(a, m.seq), 0)
        const maxTurn = mine.reduce((a, m) => Math.max(a, m.turnSeq), 0)
        const shell: Message = {
          id: `ma-resume-${now}`, sessionId, runId,
          seq: maxSeq + 1, turnSeq: Math.max(maxTurn, 1),
          role: 'assistant', content: '', createdAt: now, streaming: true, toolCalls: [],
        }
        streamingMsgId = shell.id
        set((s) => ({ messages: [...s.messages, shell] }))
      }
      currentRunId = runId
      currentRunSessionId = sessionId
      stepStartAt = now
      pendingText = ''
      pendingReasoning = ''
      set((s) => ({
        streaming: true,
        runStatus: active?.status === 'waiting_approval' ? 'waiting_approval' : 'running',
        reasoning: '',
        lastError: null,
        messages: s.messages.map((m) => (m.id === streamingMsgId ? { ...m, streaming: true } : m)),
      }))
      // 顶栏如实进入「连接中」，此后由 SseClient 状态机接管
      useSse.getState().attachRun(runId)

      // 多端/刷新一致性关键补强：`waiting_approval` 状态下审批闸门正阻塞在服务端，
      // 但 approval.request 事件早已发过、不会重放（fromSeq=-1 不补发历史）。
      // 若不在此重建审批条目，用户会看到「等待审批」却无审批条可点 → 对话永久卡死。
      // 依据 tool_events 的 status=pending（= 待决审批，approval_id 即该事件 id）。
      if (active?.status === 'waiting_approval') {
        void (async () => {
          try {
            const evs = await api.listToolEvents(sessionId, runId)
            const pendingEv = evs.find((ev) => ev.status === 'pending')
            if (!pendingEv) return
            // 期间状态已变（用户已决策 / 已切会话）则不再覆盖
            if (currentRunSessionId !== sessionId || !get().streaming) return
            set({
              approval: {
                id: pendingEv.id,
                runId,
                toolCallId: pendingEv.id,
                toolName: pendingEv.tool_name,
                risk: pendingEv.risk,
                args: pendingEv.args ?? {},
                summary: '',
                createdAt: pendingEv.started_at ?? Date.now(),
              },
              runStatus: 'waiting_approval',
            })
          } catch {
            /* 重建失败不阻断：用户仍可通过再次刷新触发，且 SSE 后续事件会自然对齐 */
          }
        })()
      }

      const onStreamEvent = makeStreamEventHandler(sessionId)
      // 订阅装配统一走 attachToRun（与 startRun 同一份 onResync / 收口语义）
      attachToRun({ sessionId, runId, fromSeq: -1, onEvent: onStreamEvent })
    },

    // 切换会话：把展示用的 token 水位切到该会话自己的账本（新会话 = 归零）
    activateSession: (sessionId) => {
      const u = usageOf(sessionId)
      set({ tokensIn: u.tokensIn, tokensOut: u.tokensOut, cachedTokens: u.cachedTokens })
    },

    // 会话 id 迁徙（乐观壳 → 服务端真实 id）：账本跟着搬，展示值若正指向旧 id 也一并刷新
    migrateUsage: (fromId, toId) => {
      if (!fromId || !toId || fromId === toId) return
      const prev = usageBySession[fromId]
      if (prev) {
        usageBySession[toId] = prev
        delete usageBySession[fromId]
      }
      if (useSessions.getState().currentId === toId) {
        const u = usageOf(toId)
        set({ tokensIn: u.tokensIn, tokensOut: u.tokensOut, cachedTokens: u.cachedTokens })
      }
    },

    send: (input) => {
      const text = input.trim()
      if (!text) return
      if (get().streaming) {
        get().guide(text)
        return
      }
      startRun(text)
    },

    stop: () => {
      if (!handle) {
        // POST 仍在途：先记账，拿到句柄后立刻 stop
        if (get().streaming) cancelPending = true
        return
      }
      // 服务端 Run 真实停止（打 stop 端点）
      handle.stop()
      handle = null
      flush()
      const id = streamingMsgId
      streamingMsgId = null
      currentRunId = null
      currentRunSessionId = null
      // 主动停止：SSE 订阅随之关闭，顶栏如实显示「已断开」而非残留连接态
      useSse.getState().detachRun()
      set((s) => ({
        streaming: false,
        runStatus: 'cancelled',
        approval: null,
        reasoning: '',
        messages: s.messages.map((m) =>
          m.id === id ? { ...m, streaming: false, durationMs: Date.now() - stepStartAt } : m,
        ),
      }))
      useUI.getState().toast('info', '已停止本次运行（Run 标记为 cancelled）')
    },

    retry: () => {
      const sessionId = useSessions.getState().currentId
      const msgs = get().messages.filter((m) => m.sessionId === sessionId && !m.reverted)
      const lastUser = [...msgs].reverse().find((m) => m.role === 'user')
      if (!lastUser) {
        useUI.getState().toast('warn', '没有可重试的用户消息')
        return
      }
      // 重试 = 作废该 user 之后的所有消息，重新发起
      set((s) => ({
        messages: s.messages.map((m) =>
          m.sessionId === sessionId && m.seq > lastUser.seq ? { ...m, reverted: true } : m,
        ),
      }))
      startRun(lastUser.content)
    },

    editResend: (msgId, content) => {
      const target = get().messages.find((m) => m.id === msgId)
      if (!target) return
      set((s) => ({
        messages: s.messages.map((m) => {
          if (m.id === msgId) return { ...m, content }
          if (m.sessionId === target.sessionId && m.seq > target.seq) return { ...m, reverted: true }
          return m
        }),
      }))
      // 服务端上下文同步截断（S10 消息级回滚）：目标消息起置灰 + 受影响文件回滚到 before 快照。
      // 必须先回滚再起 Run（引擎在 Run 启动时装载历史，晚到会把待置灰消息喂进本轮上下文）；
      // 回滚失败仍继续本地重发（服务端历史以 DB 为准，可稍后手工回滚），但会给出警示。
      void (async () => {
        try {
          const rid = await api.rollbackToMessage(target.sessionId, target.seq)
          lastRollbackBySession[target.sessionId] = rid
        } catch {
          useUI.getState().toast('warn', '服务端回滚失败，本轮上下文可能仍含旧消息')
        } finally {
          startRun(content)
        }
      })()
    },

    guide: (input) => {
      const text = input.trim()
      if (!text) return
      if (!get().streaming) {
        startRun(text)
        return
      }
      if (currentRunId) {
        // 引导（需求 §4.2）：运行中立即介入——消息经服务端落库（响应就地追加），
        // 引擎在轮次边界回灌上下文；Run 已结束（409）降级为待执行队列
        const sessionId = useSessions.getState().currentId
        void api
          .sendGuidance(sessionId, currentRunId, text)
          .then((msg) => {
            set((s) => ({
              messages: [
                ...s.messages,
                {
                  id: msg.id, sessionId, runId: currentRunId ?? undefined,
                  seq: msg.seq || s.messages.reduce((a, m) => Math.max(a, m.seq), 0) + 1,
                  turnSeq: s.messages.filter((m) => m.sessionId === sessionId && m.role === 'user').length + 1,
                  role: 'user' as const, content: msg.content, createdAt: Date.now(),
                },
              ],
            }))
            useUI.getState().toast('success', '已引导：模型将在当前轮结束后看到这条消息')
          })
          .catch(() => {
            set((s) => ({ queued: [...s.queued, text] }))
            useUI.getState().toast('info', 'Run 已结束，已加入待执行队列')
          })
        return
      }
      set((s) => ({ queued: [...s.queued, text] }))
      useUI.getState().toast('info', '已加入待执行队列，将在本轮结束后执行')
    },

    removeQueued: (index) => set((s) => ({ queued: s.queued.filter((_, i) => i !== index) })),

    // 分支：以此消息为截止点分叉新会话（历史仅本地复制，服务端不复制历史消息）
    fork: (msgId) => {
      const target = get().messages.find((m) => m.id === msgId)
      if (!target) return
      const sessions = useSessions.getState()
      const origin = sessions.sessions.find((x) => x.id === target.sessionId)
      const newId = sessions.create(origin?.projectId)
      sessions.rename(newId, `分支 · ${origin?.title ?? '新对话'}`)
      const copied = get()
        .messages.filter((m) => m.sessionId === target.sessionId && m.seq <= target.seq && !m.reverted)
        .map((m) => ({ ...m, id: `${newId}-${m.id}`, sessionId: newId }))
      set((s) => ({ messages: [...s.messages, ...copied] }))
      // 后端无「分叉时复制历史消息」端点：新会话在服务端是真建库的空会话，
      // 前序消息只是本端缓存副本。文案必须如实说明，不得暗示服务端已有历史。
      useUI
        .getState()
        .toast(
          'info',
          `已新建分支会话：服务端为空会话，此处 ${copied.length} 条前序消息仅本端可见；下一条消息起按新会话独立落库`,
        )
    },

    // 撤回对话到此处：该消息起后续全部置灰（保留可见），内容回填输入框
    revertTo: (msgId) => {
      const target = get().messages.find((m) => m.id === msgId)
      if (!target) return
      const now = Date.now()
      // 先留一份本地快照：real 模式服务端回滚失败时必须原样还原，严禁静默留下列置灰
      const prevMessages = get().messages
      const prevDraft = get().draft
      set((s) => ({
        messages: s.messages.map((m) =>
          m.sessionId === target.sessionId && m.seq >= target.seq && !m.reverted
            ? { ...m, revertedAt: now }
            : m,
        ),
        draft: target.content,
      }))
      // 等价性：api.rollbackToMessage(sid, seq) = POST /sessions/{sid}/rollback，
      // 服务端「seq 起消息置灰（hidden，组装上下文时排除）+ 受影响文件回滚到 before 快照」，
      // 与「撤回对话到此处」语义一致，故直接复用该已有方法。
      void api
        .rollbackToMessage(target.sessionId, target.seq)
        .then((rid) => {
          lastRollbackBySession[target.sessionId] = rid
          useUI
            .getState()
            .toast('info', '已撤回至此处（服务端历史同步置灰），内容已回填输入框；发送新消息将覆盖此后历史')
          return get().reloadMessages(target.sessionId)
        })
        .catch(() => {
          // 失败：还原本地置灰与草稿，保证界面与服务端一致（否则会出现「看着撤回了、实际没撤」）
          set({ messages: prevMessages, draft: prevDraft })
          useUI.getState().toast('error', '服务端回滚失败，已还原本地状态（历史以服务端为准）')
        })
    },

    // 重新应用：清除撤回边界，所有消息与文件还原为最新
    reapply: (msgId) => {
      const target = get().messages.find((m) => m.id === msgId)
      if (!target) return
      set((s) => ({
        messages: s.messages.map((m) =>
          m.sessionId === target.sessionId && m.seq >= target.seq
            ? { ...m, revertedAt: undefined }
            : m,
        ),
        draft: get().draft === target.content ? '' : get().draft,
      }))
      const rid = lastRollbackBySession[target.sessionId]
      if (!rid) {
        useUI.getState().toast('warn', '本端没有可重放的回滚记录（重新应用仅在回滚后可用）')
        return
      }
      void api
        .reapplyRollback(target.sessionId, rid)
        .then(() => get().reloadMessages(target.sessionId))
        .then(() => useUI.getState().toast('success', '已重新应用，消息与文件还原为最新'))
        .catch(() => useUI.getState().toast('error', '重新应用失败（回滚可能已被重放过）'))
    },

    // 删除对话：删除该提问及其后全部消息
    deleteFrom: (msgId) => {
      const target = get().messages.find((m) => m.id === msgId)
      if (!target) return
      const prevMessages = get().messages
      set((s) => ({
        messages: s.messages.filter(
          (m) => !(m.sessionId === target.sessionId && m.seq >= target.seq),
        ),
      }))
      // 后端无「删除一段消息」端点。能做到的最接近语义是 S10 消息级回滚：
      // 服务端把 seq 起消息置灰（hidden，组装上下文时排除）并回滚受影响文件，可 reapply 恢复；
      // 界面按「已删除」呈现。⚠️ 必须显式告知用户：服务端是置灰而非物理删除，
      // 否则用户会以为聊天记录已在服务端被抹掉。
      void api
        .rollbackToMessage(target.sessionId, target.seq)
        .then((rid) => {
          lastRollbackBySession[target.sessionId] = rid
          // 不 reloadMessages：服务端置灰消息重拉回来会「删了又出现」，与本端「已删除」呈现冲突
          useUI
            .getState()
            .toast(
              'success',
              '已删除该对话及后续内容：服务端仅置灰不再参与上下文（可重新应用恢复），未物理删除',
            )
        })
        .catch(() => {
          set({ messages: prevMessages })
          useUI.getState().toast('error', '服务端回滚失败，已还原本地视图（未删除任何内容）')
        })
    },

    // ---------------- 数据权限审批（独立于工具审批，不阻塞 Run） ----------------
    pushDataApproval: ({ scope, toolName, summary, args }) => {
      const approval: Approval = {
        id: `ap-data-${Date.now()}`,
        runId: 'manual',
        toolCallId: `data-${scope}`,
        toolName,
        risk: 'write',
        args,
        summary,
        kind: 'data',
        dataScope: scope,
        createdAt: Date.now(),
      }
      set({ approval })
    },

    respond: (decision, reason) => {
      const ap = get().approval
      if (!ap) return

      // 数据权限审批：只改本地策略与台账，不触碰工具调用句柄
      if (ap.kind === 'data') {
        const sessions = useSessions.getState()
        const session = sessions.sessions.find((x) => x.id === sessions.currentId)
        if (decision === 'allow_session' && session?.projectId && ap.dataScope) {
          sessions.setDataScope(session.projectId, ap.dataScope, 'allow')
          // 后端无「数据授权 / 项目策略」端点（router.go 仅有 Run 工具审批 decide），
          // 本地 sessionStore.dataGrants 为内存态且刷新即丢：文案必须如实说明未持久化。
          useUI
            .getState()
            .toast(
              'info',
              `本项目「${DATA_SCOPE_LABEL[ap.dataScope]}」已在本地设为允许：服务端未持久化，刷新页面后失效`,
            )
        } else if (decision === 'allow_once') {
          useUI.getState().toast('info', `已允许本次「${ap.dataScope ? DATA_SCOPE_LABEL[ap.dataScope] : '数据'}」访问（一次性，仅本次调用）`)
        } else if (decision === 'deny') {
          useUI.getState().toast('info', '已拒绝该数据访问请求，原因将回传给调用方')
        }
        set({ approval: null })
        return
      }

      // 工具审批：凭据来源只认 ap.id。
      //
      // 曾经走 handle.resume()，而它依赖 attachRun 闭包里的 approvalId；
      // 该变量只在收到 approval.request 事件时赋值，刷新恢复路径（fromSeq=-1 不补发历史）
      // 永远拿不到 → 审批条「看得见、点不动」。改为直接调 decideApproval（凭 ap.id）。
      const sessionId = ap.runId ? currentRunSessionId ?? useSessions.getState().currentId : useSessions.getState().currentId
      const runId = ap.runId || currentRunId
      if (!sessionId || !runId) {
        // 缺少定位信息时不假装成功：如实告知，保留审批条让用户重试
        useUI.getState().toast('warn', '审批上下文已失效，请刷新后重试')
        return
      }
      // 先乐观收起审批条（避免重复点击），失败再如实回填提示
      set({ approval: null, runStatus: 'running' })
      void api.decideApproval(sessionId, runId, ap.id, decision, reason).catch((err: unknown) => {
        const msg = err instanceof Error ? err.message : '审批提交失败'
        useUI.getState().toast('error', msg)
      })
      if (decision === 'allow_session') {
        useUI.getState().toast('success', `已记住规则：${ap.toolName} 在本会话内允许`)
      } else if (decision === 'deny') {
        useUI.getState().toast('info', '已拒绝，拒绝原因将回传给模型')
      }
    },
  }
})
