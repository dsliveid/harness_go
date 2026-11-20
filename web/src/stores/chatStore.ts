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
// 运行阶段类型与加载动画共用一份定义（LiveProgress 是「加载中」视觉的唯一真源），
// 避免 store 与视图各写一套阶段枚举导致文案漂移。
import type { RunPhase } from '../features/chat/LiveProgress'

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
/**
 * 本轮是否为「未选择模型」的纯保存发送（2026-11）。
 *
 * 语义：消息正常上屏并落库，但本轮不产生回复。收口时必须把它上屏的
 * 空 assistant 气泡摘掉，否则用户会永远看到一个空白转圈气泡
 * ——「发送成功但没有回复」必须表现为「没有气泡」，而不是「空回复」。
 */
let noModelRun = false
/** 当前被跟踪 Run 所属会话（切会话后仍要继续跟踪原 Run，故与 currentId 分开记） */
let currentRunSessionId: string | null = null
/**
 * 乐观壳 id → 服务端真实 id 的别名表（同一会话的两个名字）。
 *
 * 为什么需要：新建会话时本地先造壳 `s-<ts>`，POST 后才拿到真实 id 并调用 migrateUsage。
 * 而流式 Run 的归属记录（currentRunSessionId）与 SSE 订阅闭包捕获的都是**壳 id** ——
 * 🚨 绝不能把它改写成真实 id：makeStreamEventHandler 的归属守卫正拿它比对，
 * 一改就会把所有后续事件判定为「别的会话的残留」而全部丢弃（Run 变成假死）。
 * 所以只能在**比较时**归一化（见 sameSessionId）。
 */
const sessionAlias: Record<string, string> = {}
/** 把会话 id 归一化到它已知的最终名字（无别名则原样返回）。 */
const aliasOf = (id: string | null): string => (id ? sessionAlias[id] ?? id : '')
/**
 * 两个会话 id 是否指向**同一个会话**（兼容「壳 id ↔ 真实 id」的两种写法）。
 *
 * 归属守卫必须用它而不是 `===`：否则壳 id 迁徙成真实 id 后，
 * `isStreamingCurrentSession()` 会误判「当前会话没在跑」→ 用户在本会话再发一条
 * 就会被当成新 Run（服务端原 Run 仍在跑 → 同会话两个 Run）。
 */
const sameSessionId = (a: string | null, b: string | null): boolean =>
  Boolean(a) && Boolean(b) && (a === b || aliasOf(a) === aliasOf(b))
let stepStartAt = 0
/**
 * 本 Run 是否已收到过**服务端下发的步级用量**（message.end 的 token/prompt/cached）。
 *
 * 存在原因：`step`（message.end）会把当前步结算并再开一条新的空 assistant 消息，
 * 于是 Run 结束时 settleRun 结算的是那条「末步空消息」。若此时再用「整轮合计」
 * 兜底填充，轮次指标栏就会出现「各步之和 + 整轮合计」的重复计数（缓存率甚至 >100%）。
 * 只有整轮**从未**收到过步级用量（旧后端 / 上游不回 usage）时才允许轮级兜底。
 */
let sawStepUsage = false
/**
 * 本 Run 已通过**步级 message.end 实时入账**的用量累计。
 *
 * 存在原因（双计防护）：各步用量此前只在整轮结束的 done 一次性入账，导致
 * 「对话进行中 token 与缓存命中率不动」；改为逐步入账后，done 携带的
 * **整轮合计**若再全额累加就是双计（缓存率甚至可能 >100%）。
 * 收口时按「整轮合计 − 已实时入账部分」补差额，既保证旧后端
 * （只发 done、不发步级用量）行为不变，也保证新后端不重复计数。
 */
let runRealtime: SessionUsage = { tokensIn: 0, tokensOut: 0, cachedTokens: 0 }
/** 每会话最近一次消息级回滚的事件 id（real 模式 reapply 依据；S10 语义，刷新后从服务端 rollbacks 列表不可得时降级为不可重放） */
const lastRollbackBySession: Record<string, string> = {}
/**
 * 同一会话「正在加载消息」的 in-flight Promise 表（ensureMessages 去重用）。
 *
 * 为什么不进 state：它只是并发去重句柄（副作用态），
 * 进 state 会让每次 set 都 diff 一个 Promise 对象（无意义）。
 */
const inflightLoads = new Map<string, Promise<boolean>>()

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

/**
 * 把某会话里「进行中」的工具卡片就地收口为 cancelled。
 *
 * 为什么需要（2026-11 缺陷）：工具卡片状态有两条来源 ——
 *   ① SSE `tool.result`（实时路径）；
 *   ② 历史回灌 `toUiMessages`：只有存在对应 tool 消息时才判终态，否则一律 `pending`。
 * 用户在命令执行中点了「停止」/ 进程崩溃时 tool 消息不会落库，于是刷新后
 * 第 ② 条路径把卡片渲染成「等待/执行中」，而服务端 Run 早已结束 —— 用户以为命令还在跑。
 *
 * 判据是服务端权威事实：该会话**没有进行中的 Run** ⇒ 没有任何工具可以还在跑。
 */
function settleStaleToolCalls(sessionId: string) {
  const live = (s: ToolCall['status']) => s === 'pending' || s === 'running'
  const has = useChat.getState().messages.some(
    (m) => m.sessionId === sessionId && m.toolCalls?.some((c) => live(c.status)),
  )
  if (!has) return
  useChat.setState((s) => ({
    messages: s.messages.map((m) =>
      m.sessionId !== sessionId || !m.toolCalls?.some((c) => live(c.status))
        ? m
        : {
            ...m,
            toolCalls: m.toolCalls.map((c) =>
              live(c.status)
                ? { ...c, status: 'cancelled' as ToolCall['status'], result: c.result ?? '已停止（无进行中的运行）' }
                : c,
            ),
          },
    ),
  }))
  syncWorkspaceFromMessages()
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

/**
 * 已由**服务端汇总**回填、但尚未被本地实时累加覆盖的会话（P1-2 反双计兜底）。
 *
 * 为什么必须有这份记账：回填值 = 库里**截至目前**的累计量，可能已含某个正在跑的
 * Run 已落库的前几步用量；而 resumeRun 接回该 Run 后，`runRealtime` 从接回点归零，
 * 收口时 settleRun 会把「整轮合计 − 接回后实时入账」全额补进账本 —— 那部分「接回前
 * 已落库的用量」就与回填值重复了。故接回在跑 Run 时**撤销**该会话的回填，退化为
 * 全零起算（与改造前一致），由差额补记把整轮合计完整重建一遍。
 */
const backfilledSessions = new Set<string>()

/** 把一次 Run 的用量累加进该会话账本，并返回累加后的最新值。 */
function accumulateUsage(sessionId: string, delta: SessionUsage): SessionUsage {
  // 本地一旦开始累加，账本即由本地权威接管：撤销「回填未覆盖」标记，
  // 后续 resumeRun / activateSession 不再把服务端汇总当作可比基准。
  const key = aliasOf(sessionId)
  backfilledSessions.delete(key)
  const prev = usageBySession[key] ?? { tokensIn: 0, tokensOut: 0, cachedTokens: 0 }
  const next: SessionUsage = {
    tokensIn: prev.tokensIn + delta.tokensIn,
    tokensOut: prev.tokensOut + delta.tokensOut,
    cachedTokens: prev.cachedTokens + delta.cachedTokens,
  }
  usageBySession[key] = next
  return next
}

/** 读取某会话的用量水位（无记录 = 全零，绝不借用别的会话数值）。 */
function usageOf(sessionId: string): SessionUsage {
  return usageBySession[aliasOf(sessionId)] ?? { tokensIn: 0, tokensOut: 0, cachedTokens: 0 }
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
  /**
   * 当前 Run 的阶段（决定「加载中」文案）。
   *
   * posting = POST /runs 在途（服务端在建库/落库/记忆召回/SOP读盘/工具检索）
   * connecting = run_id 已拿到、SSE 正在建连
   * waiting = 连接已开、等模型首字（TTFT）
   *
   * 分三段是本次「界面无反应」修复的关键：用户能看见阶段在推进，
   * 而不是对着一个永不变化的「思考中」怀疑卡死。
   */
  runPhase: RunPhase
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
  /**
   * 已从服务端成功加载过消息的会话（加载账本）。
   *
   * 为什么进 state 而不是模块变量：MessageList 的「选中即有内容」不变量 effect
   * 必须能在「加载成功但消息为空」时也感知到变化（空会话的消息数组恒等空数组，
   * 仅靠 messages 变化不会触发重渲染 → effect 会无限重试）。
   */
  loadedSessions: Record<string, true>
  /**
   * 首屏被服务端截断的会话（hasMore=true）。
   *
   * 存在意义（2026-11）：「对话完成后结果不显示」的直接成因之一就是前端
   * 静默拿到被截断的 100 条而没有任何信号。现在服务端会回 has_more，
   * 这里如实记账，供 UI 显示「还有更早的消息」并触发 loadEarlierMessages。
   * 拿不到 hasMore=false 就不记账 —— 绝不用「条数等于 limit」来猜。
   */
  hasMoreSessions: Record<string, true>
  /**
   * 向上增量加载更早的消息（只补头部，绝不覆盖尾部）。
   *
   * 语义：取当前本地最小 seq 之前的一页，**前置**拼到该会话消息数组前面。
   * 与 reloadMessages 的「整段替换」是两件事，不可合并：
   * 整段替换会把用户正在看的尾部内容一起换掉（流式期间尤其危险）。
   */
  loadEarlierMessages: (sessionId: string) => Promise<boolean>
  /** 以 DB 为准重拉某会话的消息快照（real 模式；断线重连/resync 时对齐）；返回是否成功 */
  reloadMessages: (sessionId: string) => Promise<boolean>
  /**
   * 「选中即有内容」的结构性保证：确保该会话的消息已从服务端加载过。
   *
   * 存在意义（2026-11 用户反馈：刷新后左侧默认选中第一条对话，右侧却是空白，
   * 必须手动点一次对话内容才出现）：
   * 此前消息加载只挂在三个零散入口（load 的 void reloadMessages / select /
   * applyServerSnapshot），任何一条路径失败或遗漏（首屏 POST 未完成、切空间回落、
   * 别端删除后回落 currentId……）都会留下「选中但空白」的死状态 ——
   * 而且失败被 catch 静默吞掉，之后没有任何自愈通道，只有用户手点一次
   * （重跑 reloadMessages）才恢复。
   *
   * 本方法配合 MessageList 的不变量 effect：只要 currentId 有值且该会话未加载成功过，
   * 就自动加载一次（成功打标、失败不打标以便下次重试，并用 in-flight 去重防止风暴）。
   */
  ensureMessages: (sessionId: string) => Promise<boolean>
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
  /**
   * 「当前选中的会话是否真有进行中的 Run」—— 会话归属守卫的**唯一**判据（P0-2）。
   *
   * 为什么不能只看 `get().streaming`：streaming / runStatus 是**全局单份展示态**，
   * 而「被跟踪的 Run」的归属是 `currentRunId` + `currentRunSessionId`。A 会话的 Run 在跑时
   * 用户切到/新建 B 会话，streaming 仍可能是 true（跨会话续跑是既有正确行为），
   * 若据此判定「B 也在跑」→ B 的消息会被当成引导发给 **A 的 runId**：
   * 消息写进错的会话库、B 永远收不到回复、A 的历史被污染（缓存命中率随之暴跌）。
   *
   * 集中到一处判断，避免 send/guide/任何后续入口各写一套而漂移。
   */
  const isStreamingCurrentSession = () =>
    get().streaming
    && currentRunId !== null
    && sameSessionId(currentRunSessionId, useSessions.getState().currentId)

  /**
   * 把本地「被跟踪 Run」的展示态归位（**只影响展示，绝不中断服务端 Run**）。
   *
   * 服务端 Run 继续跑（跨会话续跑是既有正确行为），切回该会话时由 resumeRun 重新接回。
   * 收口落点（currentRunSessionId）必须一并清空：留着它会让该 Run 的残留事件继续
   * 通过 makeStreamEventHandler 的归属守卫写入全局 UI 态（串台）。
   */
  const resetLocalRunTracking = () => {
    flush()
    set({
      streaming: false,
      runStatus: 'completed',
      reasoning: '',
      approval: null,
      runPhase: 'idle' as RunPhase,
    })
    streamingMsgId = null
    currentRunId = null
    currentRunSessionId = null
    handle = null
    // 切换会话即放弃上一轮的收口语义标记（未选模型的空 reply 摘壳已无对象）
    noModelRun = false
    useSse.getState().detachRun()
  }

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
   * 把**单步** message.end 的真实用量实时记入会话账本，并（若该会话仍是当前会话）
   * 立即刷新输入框上的 token / 缓存命中率展示。
   *
   * 口径（与 settleRun 的轮级记账严格互补，绝不双计）：
   * - 输入（tokensIn）= promptTokens（缓存命中率的分母）；输出 = tokens − promptTokens；
   * - 缓存命中 = cachedTokens。
   * - 服务端对某步未下发 prompt_tokens 时（旧后端 / 上游不回 usage），整步跳过实时记账，
   *   让该步用量留待 done 的轮级合计兜底 —— 宁可晚一点，也不能编造输入/输出拆分。
   *
   * 为什么不直接信任 e.tokens 拆分：`token_count` 有时只是输出量（服务端口径为
   * 输入+输出，但历史数据/第三方端点不保证）。优先用 prompt_tokens + tokens 反推：
   * 有 promptTokens 就按 `输出 = tokens - promptTokens`（不小于 0）；
   * 无 promptTokens 时退化为「整步都算输出」，避免把输出误记成输入导致缓存率失真。
   */
  const creditStepUsage = (sessionId: string, e: { tokens?: number; promptTokens?: number; cachedTokens?: number }) => {
    if (e.promptTokens === undefined && e.cachedTokens === undefined) return
    const prompt = e.promptTokens ?? 0
    // token_count 缺失时 api.ts 会归一成 0，故必须按「>0 才采信」处理，
    // 否则 0 - prompt 会被截成 0 输出，整步输出量凭空丢失。
    const total = e.tokens && e.tokens > 0 ? e.tokens : prompt
    const completion = Math.max(0, total - prompt)
    const delta: SessionUsage = { tokensIn: prompt, tokensOut: completion, cachedTokens: e.cachedTokens ?? 0 }
    if (delta.tokensIn === 0 && delta.tokensOut === 0 && delta.cachedTokens === 0) return
    // 记「本轮已实时入账多少」：settleRun 用它把 done 的整轮合计折算成差额
    runRealtime = {
      tokensIn: runRealtime.tokensIn + delta.tokensIn,
      tokensOut: runRealtime.tokensOut + delta.tokensOut,
      cachedTokens: runRealtime.cachedTokens + delta.cachedTokens,
    }
    const usage = accumulateUsage(sessionId, delta)
    // 只有该会话仍是当前会话时才刷新展示值，避免 A 会话的步用量污染 B 会话的水位条
    // （sameSessionId 兼容壳 id ↔ 真实 id，否则迁徙后本会话的水位条会停止刷新）
    if (sameSessionId(useSessions.getState().currentId, sessionId)) {
      set({ tokensIn: usage.tokensIn, tokensOut: usage.tokensOut, cachedTokens: usage.cachedTokens })
    }
  }

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
      //
      // 🚨 差额补记（防双计）：各步 message.end 已通过 creditStepUsage **实时**入账，
      // done 携带的是**整轮合计**，全额再加一次必然双计（多步轮次缓存率甚至 >100%）。
      // 故这里只补「整轮合计 − 本轮已实时入账」的差额：
      //   - 新后端（逐步下发 usage）：差额 = 0 或极少残差，展示早已是终值，不跳变；
      //   - 旧后端（只发 done）：差额 = 整轮合计，行为与改造前完全一致。
      const delta: SessionUsage = {
        tokensIn: Math.max(0, opts.usage.tokensIn - runRealtime.tokensIn),
        tokensOut: Math.max(0, opts.usage.tokensOut - runRealtime.tokensOut),
        cachedTokens: Math.max(0, (opts.usage.cachedTokens ?? 0) - runRealtime.cachedTokens),
      }
      const usage = accumulateUsage(sessionId, delta)
      const isCurrent = sameSessionId(useSessions.getState().currentId, sessionId)
      usagePatch = {
        tokensIn: isCurrent ? usage.tokensIn : get().tokensIn,
        tokensOut: isCurrent ? usage.tokensOut : get().tokensOut,
        cachedTokens: isCurrent ? usage.cachedTokens : get().cachedTokens,
      }
    }
    set((s) => ({
      messages: s.messages
        .map((m) => {
          if (m.id !== id) return m
          // 末步空壳（message.end 之后新开的 assistant 壳，内容/工具都为空）
          // 不是一次真实步骤：绝不能给它记耗时或用量，否则整轮耗时会被
          // 「末步到 Run 结束的零头」虚增，token 也会重复计数。
          const isRealStep = Boolean(m.content.trim()) || (m.toolCalls?.length ?? 0) > 0 || m.tokens !== undefined
          return {
            ...m,
            streaming: false,
            durationMs: m.durationMs ?? (isRealStep ? nowSettle - stepStartAt : undefined),
            // 🚨 绝不用「本轮合计」覆盖消息级用量（重复计数，勿回退）：
            // 每一步的 message.end 已各自下发该步的 token/prompt/cached，
            // 轮次底部指标栏是对各步求和。若这里再把这**整轮合计**写到
            // 末步消息上，多步轮次会变成「各步之和 + 整轮合计」，
            // token 量与缓存命中率双双虚高（缓存率甚至可能 >100%）。
            // 仅当整轮**从未**收到步级用量（旧后端 / 上游不回 usage）时，
            // 才用轮级合计给这条末步消息兜底。
            ...(opts.usage && isRealStep && !sawStepUsage && !m.tokens
              ? {
                  tokens: opts.usage.tokensOut + opts.usage.tokensIn,
                  promptTokens: opts.usage.tokensIn,
                  cachedTokens: opts.usage.cachedTokens,
                }
              : {}),
          }
        })
        // 未选择模型的纯保存发送：本轮没有回复，把乐观上屏的空 assistant 气泡摘掉。
        // 只认「确实为空」：万一服务端/历史回灌了正文（不该发生），保留内容更安全。
        .filter((m) => !(noModelRun && m.id === id && !m.content && !(m.toolCalls?.length ?? 0))),
      streaming: false,
      runStatus,
      reasoning: '',
      approval: null,
      // 收口清掉阶段：否则「加载中」文案会在 Run 结束后继续显示（假运行态）
      runPhase: 'idle' as RunPhase,
      ...(opts.error ? { lastError: opts.error } : {}),
      ...usagePatch,
    }))
    if (opts.toast) useUI.getState().toast('warn', opts.toast)
    // 未选模型：消息已保存（用户消息即刻落库可见），明确告知「本轮无回复」，
    // 避免用户对着空对话以为是发送失败。
    // 仅在成功收口时提示：失败/取消已各自有错误提示，再叠一条「已保存」会自相矛盾。
    if (noModelRun) {
      noModelRun = false
      if (runStatus === 'completed') {
        useUI.getState().toast('info', '消息已保存；当前会话未选择模型，本轮不会产生回复')
      }
    }
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

  /**
   * 终态收口前先重拉消息快照（2026-11「对话完成后结果不显示」的收口侧修复）。
   *
   * # 为什么必须先重拉，而不是直接 settleRun
   *
   * 服务端落库顺序与事件推送顺序**不完全同步**：
   *   - 用户点停止 → RunService.StopRun 同步迁移终态并推送 run.status(cancelled)，
   *     而引擎 goroutine 手里那条「正在执行的工具结果」可能晚几十毫秒才落库
   *     （事故会话实测：runs.ended_at 已 cancelled 53 秒后 messages 还在增长）；
   *   - 引擎在 tool 结果落库后才发 message.end，但 SSE 慢消费者/断线补发可能丢帧。
   *
   * 旧实现在收到终态事件时**直接** settleRun，于是本地 messages 永远停在
   * 「最后一条 delta 的预览」，服务端已落库的 tool 结果与中止前答复永远补不上 ——
   * 用户看到的就是「对话结束了，结果却没显示出来」。
   *
   * # 为什么 finally 而不是 await 后 return
   *
   * 重拉失败**绝不能**让 UI 卡在「假运行态」（气泡转圈 + 顶栏已连接）。
   * 收口是刚性要求，重拉只是锦上添花，因此 finally 里无条件收口。
   */
  const settleAfterReload = (
    runStatus: Run['status'],
    opts: { error?: string; toast?: string; usage?: { tokensIn: number; tokensOut: number; cachedTokens?: number } } = {},
  ) => {
    const sid = currentRunSessionId
    if (!sid) {
      settleRun(runStatus, opts)
      return
    }
    void get()
      .reloadMessages(sid)
      .catch(() => false)
      .finally(() => settleRun(runStatus, opts))
  }

  const makeStreamEventHandler = (runSessionId: string) => (e: StreamEvent) => {
        // 事件隔离守卫：本地同一时刻只跟踪一个 Run（streamingMsgId / pendingText 是模块级单份）。
        // 用户切走该会话（或恢复了别的会话的 Run）后，本 Run 的残留事件不得再写入全局 UI 态，
        // 否则会把 A 会话的增量/收口错误地打到 B 会话的消息与状态上。
        if (!sameSessionId(currentRunSessionId, runSessionId)) return
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
            // 工具调用一起来就刷新运行态：todo / create_plan 的 args 此刻已完整，
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
            // 记录「本轮确实收到过服务端用量」：settleRun 据此决定是否允许轮级兜底，
            // 避免「各步之和 + 整轮合计」的重复计数。
            if (e.tokens !== undefined || e.promptTokens !== undefined || e.cachedTokens !== undefined) {
              sawStepUsage = true
            }
            // 🚨 实时入账（修复「对话中 token / 缓存命中率不刷新，要等整轮结束才动」）：
            // message.end 已经带着该步**真实**用量，此刻就记进会话账本并刷新输入框，
            // 绝不再攒到整轮 done 才更新 —— 多步长任务否则全程显示上一轮的旧数字。
            creditStepUsage(sessionId, e)
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
                        // 步耗时优先用服务端下发的 duration_ms（startedAt→流读完，
                        // 与审计日志同源，刷新后从 usage_json 回放也能复现）；
                        // 仅当服务端未下发时才回退本地计时（旧后端兼容）。
                        durationMs: e.durationMs ?? now2 - stepStartAt,
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
            // Run 非成功终态（failed）必须完整收口，否则 UI 会停在
            // 「气泡转圈 + 顶栏已连接」的假运行态。收口动作统一走 settleRun（单点）。
            // 先重拉快照：失败前已落库的工具结果/答复必须可见（见 settleAfterReload 注释）。
            settleAfterReload('failed', { error: `${e.code}: ${e.message}`, toast: e.message })
            break
          }
          case 'cancelled': {
            // 用户停止 / 设备离线中断 / 被接管：**不是错误**。
            // 不弹红条、不写 lastError，只如实收口为 cancelled，并把「已跑到哪一步」补上。
            settleAfterReload('cancelled', {})
            // 「已停止」这类中性结果用 info 提示，避免被误读成失败
            if (e.reason === 'user_stopped') useUI.getState().toast('info', '已停止本轮对话')
            else if (e.message) useUI.getState().toast('warn', e.message)
            break
          }
          case 'done': {
            // 成功终态：用量记进本 Run 所属会话的账本后收口（单点 settleRun）。
            // 同样先重拉：末步 message.end 与 SSE 帧都不如 DB 权威。
            settleAfterReload('completed', {
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
        if (currentRunId !== opts.runId || !sameSessionId(currentRunSessionId, opts.sessionId)) return
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
    const session = useSessions.getState().sessions.find((s) => s.id === sessionId)
    // 离线守卫（口径 1，2026-11）：只要归属执行面离线，**任何**发起新 Run 的入口一律拒绝
    // —— send / retry / editResend / fork / 待执行队列 全都汇聚到本函数，故在此统一拦截。
    //
    // 🚨 宽限期不豁免：宽限期只覆盖「进行中 Run 的续跑」，不授权新的发起入口。
    // 服务端 CreateRunWithSystem 仍独立 409（device.offline），此处是体验层短路
    // （让用户立刻看到原因，而不是等请求返回）。
    const sessState = useSessions.getState()
    // 归属优先取**会话自身**的 workspaceId（服务端权威），其次当前激活空间。
    // 只看 activeWorkspaceId 会漏判「会话归属其它空间」的情形。
    const wsId = session?.workspaceId ?? sessState.activeWorkspaceId
    const curWs = sessState.workspaces.find((w) => w.id === wsId)
    if (curWs?.canChat === false) {
      useUI.getState().toast(
        'warn',
        curWs.policyReason ?? '当前工作空间的设备不在线，对话已暂停（仅可查看历史）',
      )
      return
    }
    // 未选择模型**不再拦截发送**（2026-11 口径）：
    // 消息照常上屏 + 落库（刷新后仍在），只是本轮不产生回复。
    // 服务端 CreateRunWithSystem 对这一情形直接建 completed 的 Run 并发终态事件，
    // 前端由 settleRun 收口并把空 assistant 气泡摘掉（见 noModelRun 标记）。
    // 🚨 仍不做「默认模型」隐式回退：旧逻辑会静默落到 default/fake 回显模型，
    // 用户以为在聊天其实是在跟假模型对答。
    const noModel = !session?.modelRef
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
    // 发消息即置顶（2026-11 用户要求「按对话最新时间倒序」）：
    // 服务端 updated_at 由 TouchMessage 异步回写（最终一致），本地先按事实推时间，
    // 列表立刻把这条对话挪到最前；随后 SSE session.updated 帧会用服务端权威值覆盖。
    useSessions.getState().bumpSession(sessionId, now)
    // 未选模型：本轮只是把消息保存下来（不产生回复），收口时据此摘掉空 reply 气泡。
    noModelRun = noModel
    // 新一轮开始：清空「是否收到过步级用量」标记（每轮独立判定）
    sawStepUsage = false
    // 新一轮开始：实时入账累计归零（settleRun 的差额补记以本轮的实时入账为基准）
    runRealtime = { tokensIn: 0, tokensOut: 0, cachedTokens: 0 }
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
      // 三阶段推进：posting →（拿到 run_id）connecting →（SSE open）waiting
      // 注意：**不再**在这里记「运行开始时刻」——执行时长一律以本轮 user 消息的
      // createdAt 为基准（见 LiveProgress 的计时基准铁律），避免切对话后时长重开。
      runPhase: 'posting' as RunPhase,
    }))

    const onStreamEvent = makeStreamEventHandler(sessionId)

    // Run 在服务端执行（本地 runId 仅用于消息壳，真实 id 以服务端返回为准）
    cancelPending = false
    // 订阅开始：顶栏如实进入「连接中」，此后由 SseClient 状态机接管
    useSse.getState().attachRun(runId)
    // 模型引用：可能为空（未选择模型 → 服务端只保存消息、本轮不回复）
    const modelRef = session?.modelRef || undefined
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
          // 连接已开 → 进入「等模型首字」阶段（静默期的最后一段）
          if (state === 'open' && get().runPhase === 'connecting') {
            set({ runPhase: 'waiting' as RunPhase })
          }
          // 服务端返回真实 runId 后回填订阅目标（本地壳 id 与真实 id 不同）
          if (state === 'open' && currentRunId && useSse.getState().runId !== currentRunId) {
            useSse.getState().retargetRun(currentRunId)
          }
        },
        onSseEventSeq: (seq) => useSse.getState().noteEvent(seq),
      })
      .then((h) => {
        // 收口竞态守卫：终态可能在本次回调之前就已到达（settleRun 已清指针）。
        // 此时只回填 id 会留下指向已结束 Run 的幽灵句柄；SSE 侧由服务端
        // CloseRun → resync 自行收尾，这里直接放弃装配即可（**不要**调 h.stop()，
        // 对已终态 Run 调停止端点只会换来一次无意义的 409 拒绝）。
        if (currentRunId === null) {
          // 这轮已收口：期间排队的「停止」意图一并作废，别漏给下一次正常对话
          cancelPending = false
          return
        }
        handle = h
        currentRunId = h.runId
        // 自动标题回填（2026-11 缺陷：发任何消息对话名仍叫「新对话」）：
        // 服务端在创建 Run 前已用首条消息正文生成标题并落库，但 202 回包只有
        // run_id/status，不带 title —— 不主动 GET 一次，本地就永远停在乐观壳的
        // 「新对话」，直到用户刷新页面才「突然」变成正文。失败静默，不打扰用户。
        void useSessions.getState().refreshTitle(sessionId)
        // POST 已返回（run_id 到手）、SSE 已开始建连 → 静默期进入第二段。
        // 只从 posting 推进，不覆盖已到 waiting 的状态（SSE 若已 open，onSseState 会先接管）。
        if (get().runPhase === 'posting') set({ runPhase: 'connecting' as RunPhase })
        // 只回填 id，不重置已建立的连接阶段
        if (useSse.getState().runId && useSse.getState().runId !== h.runId) useSse.getState().retargetRun(h.runId)
        if (cancelPending) {
          cancelPending = false
          // 走完整 stop() 而不是只 h.stop()：本地状态（进行中的工具卡片、流式消息、
          // runPhase）必须与服务端同步收口，否则「点了停止→界面仍显示执行中」。
          get().stop()
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
    loadedSessions: {},
    hasMoreSessions: {},
    runStatus: 'completed',
    approval: null,
    queued: [],
    streaming: false,
    reasoning: '',
    tokensIn: 0,
    tokensOut: 0,
    cachedTokens: 0,
    lastError: null,
    runPhase: 'idle' as RunPhase,
    draft: '',

    setDraft: (text) => set({ draft: text }),

    messagesOf: (sessionId) => get().messages.filter((m) => m.sessionId === sessionId && !m.reverted),

    // 以 DB 为准重拉消息快照：断线重连（resync）后覆盖本地流式态。
    // 返回值 = 是否真正成功拉到快照（失败时不打「已加载」标，由 ensureMessages 重试）。
    reloadMessages: async (sessionId) => {
      if (!sessionId) return false
      try {
        // 🚨 必须显式传 limit（2026-11「对话完成后结果不显示」根因）：
        // 服务端默认 limit=100，事故会话 258 条 → 首屏只拿到前 100 条、
        // 最终结论（seq258 的 1668 字答复）永远不可见。
        // 这里取服务端上限（499，由 handler 的「多取一条判 has_more」决定），
        // 超过上限的超长会话由 hasMore 告知，由 UI 走向上增量加载补齐。
        const page = await api.listMessages(sessionId, { limit: 499 })
        const list = page.items
        set((s) => {
          // 🚨 缺陷修复（2026-11，用户反馈「首次发送后消息又没了」）：
          // startRun 会在 POST 前把 userMsg + 空 assistant 壳乐观上屏，而 `onResync`
          // 与 `resumeRun` 都会调用本方法做全量替换。首次发送时服务端可能**还没写完**
          // 用户消息、assistant 消息更是要等流结束才落库 —— 整段替换会把刚上屏的消息
          // 凭空抹掉，用户看到「发送 → 消息消失 → 一片空白」，体感就是彻底没反应。
          //
          // 保留规则（只认 seq，不认 id：服务端消息 id 与本地乐观 id 必然不同）：
          //   DB 里没有该 seq，说明这条乐观消息尚未落库，必须保留。
          //   一旦服务端落库（seq 被占用），自然被 DB 版本取代，无需手工去重。
          const others = s.messages.filter((m) => m.sessionId !== sessionId)
          const serverSeqs = new Set(list.map((m) => m.seq))
          const keepLocal =
            // sameSessionId 兼容壳 id ↔ 真实 id（该会话迁徙后仍要保住未落库的乐观消息）
            s.streaming && sameSessionId(currentRunSessionId, sessionId)
              ? s.messages.filter((m) => m.sessionId === sessionId && !serverSeqs.has(m.seq))
              : []
          return {
            messages: [...others, ...list, ...keepLocal],
            // 加载成功才打标（含「空会话」：空也是一种确定的已加载事实）
            loadedSessions: { ...s.loadedSessions, [sessionId]: true },
            // 截断记账：只有服务端明确说还有更早的才记，绝不由「条数 == limit」猜测。
            hasMoreSessions: page.hasMore
              ? { ...s.hasMoreSessions, [sessionId]: true }
              : Object.fromEntries(Object.entries(s.hasMoreSessions).filter(([k]) => k !== sessionId)),
          }
        })
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
        return true
      } catch {
        /* 拉取失败保留本地态，等待下一次 resync / ensureMessages 再对齐 */
        return false
      }
    },

    /**
     * 向上增量加载更早的消息（只补头部，绝不覆盖尾部）。
     *
     * 与 reloadMessages 的区别（不可合并，合并会出缺陷）：
     *   - reloadMessages：resync/终态后「以 DB 为准整段替换」，会把尾部也换掉；
     *   - 本方法：仅当 hasMoreSessions 为真时才可调用，取「本地最小 seq 之前」一页并**前置**，
     *     用户正在看的尾部内容（正在流式的答复）绝不受影响。
     *
     * 服务端 messages 接口是 after_seq 语义（返回 seq > afterSeq 的一页），
     * 因此「更早的一页」= after_seq 取 0 到本地最小 seq 之间的窗口。这里采用
     * 直接取 limit=499 从 0 开始的一页并在本地做 head 拼接的去重（按 seq 屏蔽已有条目）。
     */
    loadEarlierMessages: async (sessionId) => {
      if (!sessionId) return false
      if (!get().hasMoreSessions[sessionId]) return false
      try {
        const mine = get().messages.filter((m) => m.sessionId === sessionId)
        const minSeq = mine.reduce((a, m) => Math.min(a, m.seq), Number.MAX_SAFE_INTEGER)
        // after_seq 语义是「取 seq 更大的一页」，所以更早的那页要从头取，再按 seq 过滤掉已在本地的。
        const page = await api.listMessages(sessionId, { limit: 499 })
        const earlier = page.items.filter((m) => m.seq < minSeq)
        set((s) => {
          const existing = new Set(s.messages.filter((m) => m.sessionId === sessionId).map((m) => m.seq))
          const fresh = earlier.filter((m) => !existing.has(m.seq))
          if (fresh.length === 0) {
            // 拿不到更早的消息：撤掉标记，避免按钮一直转圈（如实反馈，不假装还有）
            return { hasMoreSessions: Object.fromEntries(Object.entries(s.hasMoreSessions).filter(([k]) => k !== sessionId)) }
          }
          return {
            messages: [...s.messages.filter((m) => m.sessionId !== sessionId), ...fresh, ...s.messages.filter((m) => m.sessionId === sessionId)],
          }
        })
        return true
      } catch {
        return false
      }
    },

    /**
     * 「选中即有内容」的结构性兜底（详见接口处注释）。
     *
     * 三条纪律：
     *  ① 已成功加载过 → 直接返回 true（不重复打网络）；
     *  ② 同一会话并发调用 → 复用同一条 in-flight Promise（防止 effect 抖动打出请求风暴）；
     *  ③ 失败不打标、不抛错（返回 false），让调用方在依赖变化时自然重试。
     */
    ensureMessages: async (sessionId) => {
      if (!sessionId) return false
      if (get().loadedSessions[sessionId]) return true
      const inflight = inflightLoads.get(sessionId)
      if (inflight) return inflight
      const task = get()
        .reloadMessages(sessionId)
        .finally(() => inflightLoads.delete(sessionId))
      inflightLoads.set(sessionId, task)
      return task
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
      // 仅当本地正在跟踪的**就是该会话**的 Run 时才跳过；跟踪别的会话不影响本会话恢复。
      // 比较统一走 sameSessionId（兼容「壳 id ↔ 真实 id」：该会话迁徙后仍算同一个）。
      if (get().streaming && currentRunId
        && sameSessionId(useSessions.getState().currentId, sessionId)
        && sameSessionId(currentRunSessionId, sessionId)) return
      // 切到另一个会话：先把上一会话的「进行中」本地态归位（服务端 Run 不受影响，仍继续跑；
      // 切回时靠本方法重新接回）。否则输入框会误显「运行中」、顶栏残留连接态。
      if (get().streaming && currentRunSessionId && !sameSessionId(currentRunSessionId, sessionId)) {
        resetLocalRunTracking()
      }
      let active: ActiveRunDTO | null = null
      try {
        active = await api.getActiveRun(sessionId)
      } catch {
        return // 查询失败不打扰用户：保持空闲态，绝不展示假运行
      }
      if (!active?.run_id) {
        // 服务端已无进行中 Run，但历史回灌里仍有 pending 的工具卡片（停止/崩溃时
        // tool 消息没落库 → toUiMessages 只能标 pending）→ 卡片会永远显示「等待/执行中」。
        // 权威事实是「该会话没有在跑的 Run」，故就地收口为 cancelled。
        settleStaleToolCalls(sessionId)
        return
      }
      // 查询期间用户可能已自行发起新对话（同样的会话）——此时本地是更新的真相，不覆盖
      if (get().streaming && currentRunId && sameSessionId(currentRunSessionId, sessionId)) return

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
      // 🚨 反双计兜底（P1-2）：本会话若已用服务端汇总做过回填，接回在跑 Run 时必须撤销。
      // 回填值 = 库里截至目前累计量，可能已含本 Run 已落库的前几步用量；而 runRealtime 从
      // 接回点归零 → settleRun 会把「整轮合计 − 接回后实时入账」全额补记，与回填值重复。
      // 撤销后退化为全零起算（与改造前一致），由差额补记把整轮合计完整重建一遍。
      const usageKey = aliasOf(sessionId)
      if (backfilledSessions.has(usageKey)) {
        backfilledSessions.delete(usageKey)
        usageBySession[usageKey] = { tokensIn: 0, tokensOut: 0, cachedTokens: 0 }
        if (sameSessionId(useSessions.getState().currentId, sessionId)) {
          set({ tokensIn: 0, tokensOut: 0, cachedTokens: 0 })
        }
      }
      // 接回在跑的 Run：同样是新一轮跟踪，重置步级用量标记
      sawStepUsage = false
      // 接回在跑的 Run：已过去的步骤由 DB 快照承载，实时入账从接回点重新起算
      runRealtime = { tokensIn: 0, tokensOut: 0, cachedTokens: 0 }
      stepStartAt = now
      pendingText = ''
      pendingReasoning = ''
      set((s) => ({
        streaming: true,
        runStatus: active?.status === 'waiting_approval' ? 'waiting_approval' : 'running',
        reasoning: '',
        lastError: null,
        // 恢复的 Run 早已过了 POST/建连阶段，直接进「等模型输出」。
        // 注意：执行时长**不在这里记时**——视图层一律以本轮 user 消息的
        // createdAt 为基准，所以刷新恢复后时长依然接着走，不会从 0 重开。
        runPhase: 'waiting' as RunPhase,
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
            if (!sameSessionId(currentRunSessionId, sessionId) || !get().streaming) return
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

    /**
     * 切换会话：展示态按会话归属复位 + 把 token 水位切到该会话自己的账本（并在需要时回填）。
     *
     * 三件事，顺序不可颠倒：
     *  ① **归属复位（P0-2）**：切到的会话不是「正在被跟踪的那个 Run」的会话 →
     *     本地展示态归位为空闲。🚨 只归位展示，**绝不** stop/中断服务端那个 Run
     *     （跨会话续跑是既有正确行为；切回来时 resumeRun 会重新接回）。
     *     不做这步的话，A 会话 Run 在跑时切到 B，B 的输入框会误显「运行中」，
     *     进而让 send() 把 B 的消息当引导发给 A 的 runId（串台）。
     *  ② 账本切换：无记录 = 全零，绝不借用别的会话数值。
     *  ③ 用量回填（P1-2）：本地账本无该会话记录时，用服务端汇总做**初始水位**。
     */
    activateSession: (sessionId) => {
      // 归属复位（P0-2）：切到的不是「被跟踪 Run 所在会话」→ 展示态归位为空闲。
      // 🚨 这里**只归位展示**，绝不 stop/中断服务端那个 Run（跨会话续跑是既有正确行为）；
      // 切回该会话时由 resumeRun 重新接回。用 sameSessionId 而非 ===，兼容壳 id ↔ 真实 id。
      if (get().streaming && !sameSessionId(currentRunSessionId, sessionId)) {
        resetLocalRunTracking()
      }
      const key = aliasOf(sessionId)
      const u = usageOf(sessionId)
      // 账本切换先落地（同步）：activateSession 的语义就是「展示值现在指向该会话」，
      // 故与改造前一致地无条件刷新，避免调用方（sessionStore.syncUsage 走动态 import，
      // 回调时刻 currentId 可能还没切过来）判断出偏差导致水位条停在上一会话。
      set({ tokensIn: u.tokensIn, tokensOut: u.tokensOut, cachedTokens: u.cachedTokens })
      if (!key) return
      // 🚨 反双计策略（P1-2 铁律）：
      // 服务端汇总只在「账本里**尚无**该会话记录」时用作初始水位，此后
      // creditStepUsage（逐步实时累加）与 settleRun（差额补记）照旧在账本上递增。
      // 已有记录就完全跳过网络回填 —— 本地账本是更新的真相，用服务端汇总覆盖会
      // 把本轮已入账的量抹掉（下次 done 差额补记再叠回来就是双计）。
      if (usageBySession[key]) return
      void api
        .getSessionUsage(key)
        .then((dto) => {
          // 失败/404（后端未上线该端点）→ 保持既有全零行为，界面不受影响
          if (!dto) return
          // 请求期间本会话可能已产生实时入账（用户秒发了一条），此时本地账本优先
          if (usageBySession[key]) return
          // 该会话正被跟踪（刷新后 resumeRun 已接回在跑的 Run）：库里的汇总可能
          // 已含本轮部分用量，叠加实时累加会双计 → 放弃本次回填（退化为全零起算，
          // 与改造前一致，绝不虚高）。
          if (get().streaming && sameSessionId(currentRunSessionId, key)) return
          if (dto.tokens_in === 0 && dto.tokens_out === 0 && dto.cached_tokens === 0) return
          usageBySession[key] = {
            tokensIn: dto.tokens_in,
            tokensOut: dto.tokens_out,
            cachedTokens: dto.cached_tokens,
          }
          backfilledSessions.add(key)
          // 只有该会话仍是当前选中会话时才刷新展示（切走后不覆盖别人的水位条）
          if (sameSessionId(useSessions.getState().currentId, key)) {
            set({ tokensIn: dto.tokens_in, tokensOut: dto.tokens_out, cachedTokens: dto.cached_tokens })
          }
        })
        .catch(() => {
          /* 回填是锦上添花：失败静默，绝不阻断切换会话 */
        })
    },

    /**
     * 会话 id 迁徙（乐观壳 → 服务端真实 id）：账本跟着搬，展示值若正指向旧 id 也一并刷新。
     *
     * 🚨 流式态**不得**被迁徙打断（P0-2）：这里只登记别名（sessionAlias），
     * 绝不改写 currentRunSessionId —— 那是 SSE 事件归属守卫的比对基准，改了会让
     * 在跑的 Run 的全部后续事件被丢弃（假死）。归属比较统一走 sameSessionId。
     */
    migrateUsage: (fromId, toId) => {
      if (!fromId || !toId || fromId === toId) return
      // 登记别名：此后 fromId 与 toId 视为同一会话（账本键也会归一化到 toId）
      sessionAlias[fromId] = toId
      const prev = usageBySession[fromId]
      if (prev) {
        usageBySession[toId] = prev
        delete usageBySession[fromId]
      }
      if (backfilledSessions.has(fromId)) {
        backfilledSessions.delete(fromId)
        backfilledSessions.add(toId)
      }
      if (sameSessionId(useSessions.getState().currentId, toId)) {
        const u = usageOf(toId)
        set({ tokensIn: u.tokensIn, tokensOut: u.tokensOut, cachedTokens: u.cachedTokens })
      }
    },

    send: (input) => {
      const text = input.trim()
      if (!text) return
      // 会话归属守卫（P0-2）：只有**当前会话**确有进行中的 Run 才转引导。
      // 仅看全局 streaming 会把 B 会话的消息当引导发给 A 的 runId（串台）。
      if (isStreamingCurrentSession()) {
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
      // 主动停止：清掉「未选模型」标记，避免污染下一次正常对话的收口判定
      noModelRun = false
      // 服务端 Run 真实停止（打 stop 端点）
      handle.stop()
      handle = null
      flush()
      const id = streamingMsgId
      const stoppedRunId = currentRunId
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
        // 主动停止归零阶段：否则「加载中」文案会一直留着
        runPhase: 'idle' as RunPhase,
        messages: s.messages.map((m) => {
          // ① 结算流式消息（停转圈 + 记耗时）
          const settled =
            m.id === id ? { ...m, streaming: false, durationMs: Date.now() - stepStartAt } : m
          // ② 收口「进行中」的工具卡片（2026-11 修复）：
          //    停止后 SSE 已断开，绝不会再收到 tool.result —— 后端已把 tool_events 收口为
          //    error（RunService.StopRun → FailOpenByRun），前端必须同口径落定，
          //    否则卡片永远显示「执行中…」，用户以为命令还在跑。
          if (!settled.toolCalls?.length) return settled
          // 归属判定必须是「或」：本地乐观消息壳的 runId 是 `run-<ts>`，服务端真实 runId
          // 回填在 currentRunId 与后续 step 消息上，两者并存 → 只判其一会漏掉另一半。
          const mine = settled.id === id || (Boolean(stoppedRunId) && settled.runId === stoppedRunId)
          if (!mine) return settled
          return {
            ...settled,
            toolCalls: settled.toolCalls.map((c) =>
              c.status === 'pending' || c.status === 'running'
                ? { ...c, status: 'cancelled' as ToolCall['status'], result: c.result ?? '已停止（用户中断）' }
                : c,
            ),
          }
        }),
      }))
      // 命令行面板与工具卡片同源，同步收口一次
      syncWorkspaceFromMessages()
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
      // 会话归属守卫（P0-2，与 send 共用同一个判据）：
      // 若正在跑的 Run 属于**别的会话**（用户已切走/新建了会话），这条输入不是引导，
      // 必须在本会话发起新 Run。否则 sendGuidance 会把消息写进错会话的库：
      // B 永远收不到回复，A 的历史被污染（缓存命中随之暴跌）。
      if (!isStreamingCurrentSession()) {
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
          // 0014 起数据权限已落服务端（PUT /projects/{pid}/data-grants/{scope}），
          // 刷新/换端/重启均保留，故文案不再声称「仅本地、会失效」。
          useUI
            .getState()
            .toast(
              'info',
              `本项目「${DATA_SCOPE_LABEL[ap.dataScope]}」已设为允许：已保存到服务端，可在项目设置 → 数据权限中回收`,
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
