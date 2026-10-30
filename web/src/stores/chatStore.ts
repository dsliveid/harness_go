import { create } from 'zustand'
import type {
  Approval, CompactionPending, CompactionRecord, CompactionStrategy, DataScope, Message, Run, ToolCall,
} from '../types'
import { longSessionMessages, messages as mockMessages } from '../mock/data'
import { startMockRun, type Decision, type MockRunHandle } from '../mock/stream'
import { COMPACTION_RATIO, resolveModelContext } from '../lib/context'
import { DATA_SCOPE_LABEL } from '../lib/dataScopes'
import { useSessions } from './sessionStore'
import { useUI } from './uiStore'

// 模块级流式缓冲：delta 累积后按 50ms 批量提交，避免逐 token setState
let handle: MockRunHandle | null = null
let pendingText = ''
let flushTimer: ReturnType<typeof setTimeout> | null = null
let streamingMsgId: string | null = null
let currentRunId: string | null = null
let stepStartAt = 0

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

  const patchTool = (callId: string, patch: Partial<ToolCall>, appendResult?: string) => {
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
                }
              : c,
          ),
        }
      }),
    }))
  }

  const startRun = (input: string) => {
    const sessionId = useSessions.getState().currentId
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

    handle = startMockRun({
      sessionId,
      runId,
      input,
      onEvent: (e) => {
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
            const queued = get().queued
            if (queued.length > 0) {
              const next = queued[0]
              set({ queued: queued.slice(1) })
              setTimeout(() => startRun(next), 260)
            }
            break
          }
        }
      },
    })
  }

  return {
    messages: [...mockMessages, ...longSessionMessages],
    runStatus: 'running',
    approval: null,
    queued: [],
    streaming: false,
    reasoning: '',
    tokensIn: 18420,
    tokensOut: 1280,
    cachedTokens: 15980,
    lastError: null,
    compactions: [],
    compactionPending: null,
    compactionFreed: 0,
    compactionSnoozeAt: 0,
    draft: '',

    setDraft: (text) => set({ draft: text }),

    messagesOf: (sessionId) => get().messages.filter((m) => m.sessionId === sessionId && !m.reverted),

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
      if (!handle) return
      handle.stop()
      handle = null
      flush()
      const id = streamingMsgId
      streamingMsgId = null
      currentRunId = null
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
      startRun(content)
    },

    guide: (input) => {
      const text = input.trim()
      if (!text) return
      if (!get().streaming) {
        startRun(text)
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
      useUI.getState().toast('success', '已重新应用，消息与文件还原为最新')
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
