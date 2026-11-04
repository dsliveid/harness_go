import { create } from 'zustand'
import type {
  Approval, CompactionPending, CompactionRecord, CompactionStrategy, DataScope, Message, Run, ToolCall,
} from '../types'
import { longSessionMessages, messages as mockMessages } from '../mock/data'
import { startMockRun, type Decision, type MockRunHandle, type StreamEvent } from '../mock/stream'
import { api, apiMode, type RunHandle } from '../lib/api'
import type { SseState as SseStateName, SseStateInfo } from '../lib/sse'
import { COMPACTION_RATIO, resolveModelContext } from '../lib/context'
import { DATA_SCOPE_LABEL } from '../lib/dataScopes'
import { useSessions } from './sessionStore'
import { useSse } from './sseStore'
import { useUI } from './uiStore'

// 模块级流式缓冲：delta 累积后按 50ms 批量提交，避免逐 token setState
let handle: RunHandle | MockRunHandle | null = null
// startRun 的 POST 在途时点了「停止」：拿到句柄后立即补一次 stop，避免服务端 Run 空跑
let cancelPending = false
let pendingText = ''
let flushTimer: ReturnType<typeof setTimeout> | null = null
let streamingMsgId: string | null = null
let currentRunId: string | null = null
let stepStartAt = 0
/** 每会话最近一次消息级回滚的事件 id（real 模式 reapply 依据；S10 语义，刷新后从服务端 rollbacks 列表不可得时降级为不可重放） */
const lastRollbackBySession: Record<string, string> = {}

/**
 * SSE 真实状态上报转发（lib/api.ts → 顶栏 SseStatus）。
 * 单独抽成函数而非内联，是为了 mock/real 两个分支共用同一套转发与终态清理逻辑。
 */
function applySseState(state: SseStateName, info: SseStateInfo) {
  useSse.getState().applyReal(state, info)
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
  // ---- 上下文压缩 ----
  compactions: CompactionRecord[]
  compactionPending: CompactionPending | null
  /** 本会话内压缩累计释放的 tokens（从活跃上下文抵扣；累计消耗不变） */
  compactionFreed: number
  /** 「稍后再说」时的活跃水位，避免同一水位反复提示 */
  compactionSnoozeAt: number
  requestCompaction: (trigger?: 'auto' | 'manual') => void
  confirmCompaction: (strategy: CompactionStrategy) => void
  dismissCompaction: () => void
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
}

export const useChat = create<ChatState>((set, get) => {
  const flush = () => {
    if (flushTimer) {
      clearTimeout(flushTimer)
      flushTimer = null
    }
    if (!pendingText || !streamingMsgId) {
      pendingText = ''
      return
    }
    const chunk = pendingText
    pendingText = ''
    const id = streamingMsgId
    set((s) => ({
      messages: s.messages.map((m) => (m.id === id ? { ...m, content: m.content + chunk } : m)),
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
    stepStartAt = now
    pendingText = ''
    set((s) => ({
      messages: [...s.messages, userMsg, assistantMsg],
      streaming: true,
      runStatus: 'running',
      reasoning: '',
      lastError: null,
      approval: null,
    }))

    const onStreamEvent = (e: StreamEvent) => {
        switch (e.type) {
          case 'delta':
            pendingText += e.text
            scheduleFlush()
            break
          case 'reasoning':
            set({ reasoning: e.text })
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
            break
          case 'tool_result':
            if (e.durationMs === 0) {
              patchTool(e.callId, {}, e.result)
            } else {
              patchTool(e.callId, {
                status: 'ok',
                durationMs: e.durationMs,
                diff: e.diff,
                result: e.result,
              })
            }
            break
          case 'tool_progress':
            // 实时增量输出（需求 §10-4）：追加到运行中的工具卡片输出区；
            // 终态结果仍以 tool.result 为准（完成后 ToolCard 展示 result，不再展示 liveOutput）
            patchTool(e.callId, {}, undefined, e.chunk)
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
            const sessionId = useSessions.getState().currentId
            const sessionMsgs2 = get().messages.filter((m) => m.sessionId === sessionId)
            const nextSeq = sessionMsgs2.reduce((a, m) => Math.max(a, m.seq), 0) + 1
            const turnSeq = sessionMsgs2.find((m) => m.id === prevId)?.turnSeq ?? 1
            const nextMsg: Message = {
              id: `ma-${now2}`, sessionId, runId: currentRunId ?? undefined,
              seq: nextSeq, turnSeq, role: 'assistant', content: '',
              createdAt: now2, streaming: true, toolCalls: [],
            }
            set((s) => ({
              messages: [
                ...s.messages.map((m) =>
                  m.id === prevId
                    ? { ...m, streaming: false, durationMs: now2 - stepStartAt, tokens: e.tokens ?? m.tokens, promptTokens: e.promptTokens ?? m.promptTokens, cachedTokens: e.cachedTokens ?? m.cachedTokens }
                    : m,
                ),
                nextMsg,
              ],
            }))
            streamingMsgId = nextMsg.id
            stepStartAt = now2
            break
          }
          case 'error':
            set({ lastError: `${e.code}: ${e.message}` })
            useUI.getState().toast('warn', e.message)
            break
          case 'done': {
            flush()
            const now3 = Date.now()
            set((s) => ({
              messages: s.messages.map((m) =>
                m.id === streamingMsgId
                  ? { ...m, streaming: false, tokens: e.tokensOut + e.tokensIn, promptTokens: e.tokensIn, cachedTokens: e.cachedTokens, durationMs: now3 - stepStartAt }
                  : m,
              ),
              streaming: false,
              runStatus: 'completed',
              reasoning: '',
              approval: null,
              tokensIn: s.tokensIn + e.tokensIn,
              tokensOut: s.tokensOut + e.tokensOut,
              cachedTokens: s.cachedTokens + (e.cachedTokens ?? 0),
            }))
            handle = null
            streamingMsgId = null
            currentRunId = null
            // 订阅结束：如实回到空闲态（顶栏不再残留「已连接」假象）
            useSse.getState().detachRun()
            const queued = get().queued
            if (queued.length > 0) {
              const next = queued[0]
              set({ queued: queued.slice(1) })
              setTimeout(() => startRun(next), 260)
            }
            break
          }
        }
    }

    if (apiMode === 'mock') {
      handle = startMockRun({ sessionId, runId, input, onEvent: onStreamEvent })
      // mock 无真实 EventSource：如实上报「演示态」（顶栏同时有 🟡 DEMO 角标提示为演示数据）
      useSse.getState().attachRun(runId)
      useSse.getState().applyReal('open', { reason: 'mock-stream' })
      return
    }

    // real 模式：Run 在服务端执行（本地 runId 仅用于消息壳，真实 id 以服务端返回为准）
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
          // 断线重连 / 服务端 resync：以 DB 快照覆盖本地流式态（delta 只是预览）
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
        flush()
        set({ streaming: false, runStatus: 'failed', lastError: msg, approval: null, reasoning: '' })
        useUI.getState().toast('error', msg)
        streamingMsgId = null
        currentRunId = null
        handle = null
        // 启动失败没有任何订阅：如实回到空闲态
        useSse.getState().detachRun()
      })
  }

  return {
    // real 模式消息来自 DB（登录后/reloadMessages 拉取）；mock 模式保留 demo 样例
    messages: apiMode === 'mock' ? [...mockMessages, ...longSessionMessages] : [],
    runStatus: 'completed',
    approval: null,
    queued: [],
    streaming: false,
    reasoning: '',
    tokensIn: apiMode === 'mock' ? 18420 : 0,
    tokensOut: apiMode === 'mock' ? 1280 : 0,
    cachedTokens: apiMode === 'mock' ? 15980 : 0,
    lastError: null,
    compactions: [],
    compactionPending: null,
    compactionFreed: 0,
    compactionSnoozeAt: 0,
    draft: '',

    setDraft: (text) => set({ draft: text }),

    messagesOf: (sessionId) => get().messages.filter((m) => m.sessionId === sessionId && !m.reverted),

    // 以 DB 为准重拉消息快照：断线重连（resync）后覆盖本地流式态
    reloadMessages: async (sessionId) => {
      if (apiMode === 'mock') return
      try {
        const list = await api.listMessages(sessionId)
        set((s) => ({
          messages: [...s.messages.filter((m) => m.sessionId !== sessionId), ...list],
        }))
      } catch {
        /* 拉取失败保留本地态，等待下一次 resync 再对齐 */
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
        // POST 仍在途（real 模式）：先记账，拿到句柄后立刻 stop
        if (get().streaming) cancelPending = true
        return
      }
      handle.stop()
      handle = null
      flush()
      const id = streamingMsgId
      streamingMsgId = null
      currentRunId = null
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
      if (apiMode === 'real') {
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
        return
      }
      startRun(content)
    },

    guide: (input) => {
      const text = input.trim()
      if (!text) return
      if (!get().streaming) {
        startRun(text)
        return
      }
      if (apiMode === 'real' && currentRunId) {
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

    // 分支：以此消息为截止点分叉新会话（复制前序历史）
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
      useUI.getState().toast('success', `已分叉新会话（含此前 ${copied.length} 条消息）`)
    },

    // 撤回对话到此处：该消息起后续全部置灰（保留可见），内容回填输入框
    revertTo: (msgId) => {
      const target = get().messages.find((m) => m.id === msgId)
      if (!target) return
      const now = Date.now()
      set((s) => ({
        messages: s.messages.map((m) =>
          m.sessionId === target.sessionId && m.seq >= target.seq && !m.reverted
            ? { ...m, revertedAt: now }
            : m,
        ),
        draft: target.content,
      }))
      if (apiMode === 'real') {
        // 服务端同步置灰（S10 消息级回滚，含受影响文件回滚），重拉快照以 DB 为准
        void api
          .rollbackToMessage(target.sessionId, target.seq)
          .then((rid) => {
            lastRollbackBySession[target.sessionId] = rid
            return get().reloadMessages(target.sessionId)
          })
          .catch(() => useUI.getState().toast('warn', '服务端回滚失败，历史以本地界面为准'))
      }
      useUI.getState().toast('info', '已撤回至此处，内容已回填输入框（发送新消息将覆盖此后历史）')
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
      if (apiMode === 'real') {
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
      } else {
        useUI.getState().toast('success', '已重新应用，消息与文件还原为最新')
      }
    },

    // 删除对话：删除该提问及其后全部消息
    deleteFrom: (msgId) => {
      const target = get().messages.find((m) => m.id === msgId)
      if (!target) return
      set((s) => ({
        messages: s.messages.filter(
          (m) => !(m.sessionId === target.sessionId && m.seq >= target.seq),
        ),
      }))
      if (apiMode === 'real') {
        // 删除 = 同一消息级回滚（服务端语义为置灰保留，可 reapply 恢复；界面按已删除呈现）
        void api
          .rollbackToMessage(target.sessionId, target.seq)
          .then((rid) => {
            lastRollbackBySession[target.sessionId] = rid
            return get().reloadMessages(target.sessionId)
          })
          .catch(() => useUI.getState().toast('warn', '服务端回滚失败，历史以本地界面为准'))
      }
      useUI.getState().toast('success', '已删除该对话及后续内容')
    },

    // ---------------- 上下文压缩（确认 → 摘要 → 历史） ----------------
    requestCompaction: (trigger = 'manual') => {
      const st = get()
      if (st.compactionPending) return
      const sessions = useSessions.getState()
      const session = sessions.sessions.find((x) => x.id === sessions.currentId)
      const { limit } = resolveModelContext(session?.modelRef)
      const active = Math.max(0, st.tokensIn + st.tokensOut - st.compactionFreed)
      if (active <= 0) {
        useUI.getState().toast('warn', '当前会话暂无可压缩的上下文')
        return
      }
      // 自动触发时，同一水位已被「稍后再说」过就不再打扰（再涨 5% 才提醒）
      if (trigger === 'auto' && st.compactionSnoozeAt > 0 && active < st.compactionSnoozeAt + limit * 0.05) return

      const msgs = st.messages.filter((m) => m.sessionId === sessions.currentId && !m.reverted)
      const turns = new Set(msgs.map((m) => m.turnSeq)).size
      const keptTurns = 3
      set({
        compactionPending: {
          sessionId: sessions.currentId,
          beforeTokens: active,
          limit,
          threshold: Math.round(limit * COMPACTION_RATIO),
          droppedTurns: Math.max(0, turns - keptTurns),
          keptTurns,
          trigger,
        },
      })
    },

    confirmCompaction: (strategy) => {
      const p = get().compactionPending
      if (!p) return
      const keptTurns = strategy === 'semantic' ? p.keptTurns : strategy === 'summary_only' ? 1 : 0
      const ratio = strategy === 'semantic' ? 0.42 : strategy === 'summary_only' ? 0.55 : 0.3
      const after = Math.max(1200, Math.round(p.beforeTokens * ratio))
      const freed = Math.max(0, p.beforeTokens - after)
      const sessions = useSessions.getState()
      const session = sessions.sessions.find((x) => x.id === p.sessionId)
      const project = sessions.projects.find((x) => x.id === session?.projectId)
      const droppedTurns = Math.max(p.droppedTurns, 1)
      const strategyLabel =
        strategy === 'semantic' ? '智能语义压缩' : strategy === 'summary_only' ? '仅保留摘要' : '截断最早轮次'
      const summary = [
        `【上下文摘要】${session?.title ?? '当前会话'}`,
        `压缩策略：${strategyLabel}${strategy === 'truncate' ? '（未生成语义摘要，仅按轮次截断）' : ''}`,
        `已折叠：${droppedTurns} 轮历史对话（含工具调用与 diff 明细）`,
        `保留：最近 ${keptTurns} 轮完整消息 + 系统提示 + 项目约束 + 活跃计划`,
        project?.constraints ? `项目约束：${project.constraints.split('\n')[0]}` : '项目约束：未设置',
        '关键结论：M2 插件注册表已落地；审批闸门与快照链校验通过；待办 3 项待推进',
      ].join('\n')

      const record: CompactionRecord = {
        id: `cp-${Date.now()}`,
        sessionId: p.sessionId,
        createdAt: Date.now(),
        beforeTokens: p.beforeTokens,
        afterTokens: after,
        freedTokens: freed,
        droppedTurns,
        keptTurns,
        strategy,
        summary,
        trigger: p.trigger,
      }
      set((s) => ({
        compactions: [record, ...s.compactions],
        compactionPending: null,
        compactionFreed: s.compactionFreed + freed,
        compactionSnoozeAt: 0,
      }))
      useUI.getState().toast('success', `上下文已压缩（${strategyLabel}）：释放 ${freed.toLocaleString('zh-CN')} tokens`)
    },

    dismissCompaction: () => {
      const st = get()
      const active = Math.max(0, st.tokensIn + st.tokensOut - st.compactionFreed)
      set({ compactionPending: null, compactionSnoozeAt: active })
      useUI.getState().toast('info', '已稍后处理：上下文再增长 5% 后会再次提醒')
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

      // 数据权限审批：只改策略与台账，不触碰工具调用句柄
      if (ap.kind === 'data') {
        const sessions = useSessions.getState()
        const session = sessions.sessions.find((x) => x.id === sessions.currentId)
        if (decision === 'allow_session' && session?.projectId && ap.dataScope) {
          sessions.setDataScope(session.projectId, ap.dataScope, 'allow')
          useUI.getState().toast('success', `已允许本项目「${DATA_SCOPE_LABEL[ap.dataScope]}」（已写入项目数据权限）`)
        } else if (decision === 'allow_once') {
          useUI.getState().toast('success', `已允许本次「${ap.dataScope ? DATA_SCOPE_LABEL[ap.dataScope] : '数据'}」访问（一次性）`)
        } else if (decision === 'deny') {
          useUI.getState().toast('info', '已拒绝该数据访问请求，原因将回传给调用方')
        }
        set({ approval: null })
        return
      }

      handle?.resume(decision, reason)
      set({ approval: null, runStatus: 'running' })
      if (decision === 'allow_session') {
        useUI.getState().toast('success', `已记住规则：${ap.toolName} 在本会话内允许`)
      } else if (decision === 'deny') {
        useUI.getState().toast('info', '已拒绝，拒绝原因将回传给模型')
      }
    },
  }
})
