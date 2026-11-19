import { useEffect, useMemo, useRef, useState } from 'react'
import { useChat } from '../../stores/chatStore'
import { useSessions } from '../../stores/sessionStore'
import { MarkdownLite } from '../../components/MarkdownLite'
import { VirtualList, type ScrollTarget } from '../../components/VirtualList'
import { cn } from '../../components/ui'
import { useUI } from '../../stores/uiStore'
import { ArrowDown, Check, Clock, Copy, GitBranch, GitCompare, RotateCcw, RotateCw, Sparkles, Trash2, Zap } from 'lucide-react'
import { ProcessDrawer, formatStepMs, formatStepTokens } from './ProcessDrawer'
import { ReasoningCard } from './ReasoningCard'
import { formatDuration, RunProgressInline, useLiveElapsed, type RunPhase } from './LiveProgress'
import type { Message } from '../../types'

interface TurnGroup {
  turnSeq: number
  items: Message[]
}

/**
 * 「选中即有内容」不变量的失败重试上限。
 *
 * 3 次 + 800ms/1600ms/3200ms 退避：覆盖服务端重启、网络瞬断这类可恢复失败，
 * 又不至于把某个「服务端确实没有该会话」的 404 变成永久的请求风暴。
 * 用尽后不再自动重试 —— 但用户手动点一次对话（select）仍会重新拉取，
 * 保留人工自救通道。
 */
const MAX_LOAD_RETRY = 3

/**
 * 线程级对话操作集：
 * 主对话接 chatStore；子进程/浮窗对话按需传入（不传的操作不渲染对应按钮）。
 */
export interface ThreadActions {
  fork?: (msgId: string) => void
  revertTo?: (msgId: string) => void
  reapply?: (msgId: string) => void
  deleteFrom?: (msgId: string) => void
}

interface MessageThreadProps {
  messages: Message[]
  streaming?: boolean
  reasoning?: string
  /** 运行阶段：决定「加载中」文案（posting/connecting/waiting） */
  runPhase?: RunPhase
  actions: ThreadActions
  /** 传 false 隐藏复制按钮（子进程详情等浮窗对话） */
  showCopy?: boolean
  /** 滚动容器样式覆盖 */
  scrollCls?: string
  /** 无消息时的空态提示 */
  emptyHint?: string
  /** plain 直排模式：浮窗内少量轮次不虚拟化（滚动容器多为固定高容器，直排规避测量竞态） */
  plain?: boolean
}

export function MessageList() {
  const { streaming, reasoning, fork, revertTo, reapply, deleteFrom, runPhase, ensureMessages, resumeRun } = useChat()
  const currentId = useSessions((s) => s.currentId)
  const messages = useChat((s) => s.messagesOf(currentId))
  const loaded = useChat((s) => (currentId ? Boolean(s.loadedSessions[currentId]) : true))
  const [retryTick, setRetryTick] = useState(0)
  // 每个会话各自的重试计数（切会话即归零，不会因为上一条会话重试用尽而放弃新会话）
  const retryRef = useRef<{ id: string; n: number }>({ id: '', n: 0 })

  // 【不变量：只要左侧处于选中态，右侧必须必然有内容】
  //
  // 2026-11 缺陷：刷新后左侧默认选中第一条对话，右侧却是空白，必须手动点一次才出现内容。
  // 根因是消息加载只挂在零散入口（load 的 void reloadMessages / select / 快照），
  // 任一入口失败或时序错过就留下「选中但空白」的死状态，且失败被静默吞掉、无自愈通道。
  //
  // 这里把「选中 → 有内容」提升为渲染层不变量：currentId 有值而该会话尚未成功加载过，
  // 就自动加载（并顺带接回服务端可能仍在跑的 Run）。加载成功由 loadedSessions 打标；
  // 失败则按指数退避有限次重试 —— 绝不静默停在空白（这正是不变量该守住的东西）。
  useEffect(() => {
    if (!currentId || loaded) return
    let alive = true
    let timer: ReturnType<typeof setTimeout> | null = null
    void ensureMessages(currentId).then((ok) => {
      if (!alive) return
      if (ok) {
        void resumeRun(currentId)
        return
      }
      if (retryRef.current.id !== currentId) retryRef.current = { id: currentId, n: 0 }
      if (retryRef.current.n >= MAX_LOAD_RETRY) return
      retryRef.current.n += 1
      timer = setTimeout(() => {
        if (alive) setRetryTick((v) => v + 1)
      }, 800 * 2 ** (retryRef.current.n - 1))
    })
    return () => {
      alive = false
      if (timer) clearTimeout(timer)
    }
  }, [currentId, loaded, ensureMessages, resumeRun, retryTick])

  return (
    <div className="flex-1 min-h-0 flex flex-col">
      <MessageThread
        messages={messages}
        streaming={streaming}
        reasoning={reasoning}
        runPhase={runPhase}
        actions={{ fork, revertTo, reapply, deleteFrom }}
      />
    </div>
  )
}

/** 通用对话线程：主对话与子进程详情浮窗复用同一渲染体系（轮次/双轨/执行过程/指标/消息操作） */
export function MessageThread({
  messages,
  streaming = false,
  reasoning = '',
  runPhase = 'waiting',
  actions,
  showCopy = true,
  scrollCls,
  emptyHint,
  plain = false,
}: MessageThreadProps) {
  const scrollerRef = useRef<HTMLDivElement>(null)
  const [nearBottom, setNearBottom] = useState(true)
  const hasMessages = messages.length > 0

  // ---------------- 受控定位（右侧方案面板「定位出处」的消费端） ----------------
  const focusMessage = useUI((s) => s.focusMessage)
  const clearFocusMessage = useUI((s) => s.clearFocusMessage)
  const toast = useUI((s) => s.toast)
  const [scrollTarget, setScrollTarget] = useState<ScrollTarget | null>(null)
  /** 高亮目标：id + nonce（nonce 保证对同一消息重复定位时高亮能再次闪烁） */
  const [highlight, setHighlight] = useState<{ id: string; nonce: number } | null>(null)

  // 切换数据源：重置贴底并跳到底部
  useEffect(() => {
    setNearBottom(true)
    const el = scrollerRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [messages[0]?.id])

  // 滚动监听：**必须以 hasMessages 为依赖**（原实现依赖 []，若挂载时消息为空则
  // scrollerRef.current 为 null → 监听器永不挂上 → nearBottom 恒为 true，
  // 「回到最新答复」按钮永远不会出现）。
  useEffect(() => {
    const el = scrollerRef.current
    if (!el) return
    const onScroll = () => {
      const v = el.scrollHeight - el.scrollTop - el.clientHeight < 140
      setNearBottom((prev) => (prev === v ? prev : v))
    }
    // 初次判定：进入会话时通常已贴底，同步一次真实位置
    onScroll()
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [hasMessages, plain])

  const turns = useMemo<TurnGroup[]>(() => {
    const out: TurnGroup[] = []
    for (const m of messages) {
      const last = out[out.length - 1]
      if (last && last.turnSeq === m.turnSeq) last.items.push(m)
      else out.push({ turnSeq: m.turnSeq, items: [m] })
    }
    return out
  }, [messages])

  /** 定位请求 nonce 发生器：与 uiStore 的 focusMessage.nonce 独立自增，互不干扰 */
  const localNonce = useRef(0)
  /** 纠正性滚动的取消器（用户一旦主动滚动/触摸即作废，绝不再抢视口） */
  const correctionRef = useRef<(() => void) | null>(null)

  /**
   * 消费「定位出处」请求：消息 id → 轮次索引 → 虚拟列表滚动。
   *
   * 🚨 绝不能沿用老版本 `document.querySelector([data-message-id]) + scrollIntoView`：
   * 主对话是 @tanstack/react-virtual 虚拟列表，未进入视口的轮次**不在 DOM 里**，
   * 查询恒失败（表现为「点了定位毫无反应」）。必须走数据 → 索引 → scrollToIndex。
   */
  useEffect(() => {
    // 仅主对话（非 plain 浮窗）响应；子进程详情浮窗不应该被主对话的定位请求劫持
    if (plain || !focusMessage) return
    const idx = turns.findIndex((t) => t.items.some((m) => m.id === focusMessage.messageId))
    if (idx < 0) {
      // 目标已被过滤（如撤回）或用户已切走会话 → 如实提示，绝不静默失败
      toast('warn', '未在当前对话中找到该消息位置（可能已撤回或已切换会话）')
      clearFocusMessage()
      return
    }
    localNonce.current += 1
    const nonce = localNonce.current
    setScrollTarget({ index: idx, nonce, align: 'center' })
    setHighlight({ id: focusMessage.messageId, nonce })
    // 定位期间必须关掉贴底，否则流式增量会让 VirtualList 立刻把视口拽回底部
    setNearBottom(false)
    clearFocusMessage()

    // ---- 纠正性滚动（一次性）----
    // 时序坑：若目标在「执行过程」抽屉内部，抽屉是在滚动之后才展开的
    // （ProcessDrawer 的 expandSignal effect → 再渲染 → 高度变大），
    // 首次 scrollToIndex 用的是展开前的高度 → 落点会偏。故 320ms 后按同一 index 再滚一次。
    // 但用户此刻若已自行滚动/触摸，说明他已在阅读，绝不能再抢视口 → 立即取消。
    correctionRef.current?.()
    const el = scrollerRef.current
    let fired = false
    let timer = 0
    // 取消/收尾统一出口：既清定时器也摘监听（once:true 若未被触发则不会自动摘除，
    // 反复定位会累积监听器 —— 这是必须显式 remove 的原因）。
    const finish = (runCorrection: boolean) => {
      if (fired) return
      fired = true
      window.clearTimeout(timer)
      el?.removeEventListener('wheel', onUserInput)
      el?.removeEventListener('touchstart', onUserInput)
      el?.removeEventListener('keydown', onUserInput)
      correctionRef.current = null
      if (runCorrection) {
        localNonce.current += 1
        setScrollTarget({ index: idx, nonce: localNonce.current, align: 'center' })
      }
    }
    function onUserInput() { finish(false) }
    timer = window.setTimeout(() => finish(true), 320)
    correctionRef.current = () => finish(false)
    const opts = { passive: true } as const
    el?.addEventListener('wheel', onUserInput, opts)
    el?.addEventListener('touchstart', onUserInput, opts)
    el?.addEventListener('keydown', onUserInput)
  }, [focusMessage, turns, plain, toast, clearFocusMessage])

  // 卸载时清掉挂起的纠正滚动，避免在已卸载组件上 setState
  useEffect(() => () => { correctionRef.current?.() }, [])

  // 高亮 2.4s 后自动消退（对齐老版本 ring 高亮 2s 的手感，略放长便于视线捕捉）
  useEffect(() => {
    if (!highlight) return
    const t = setTimeout(() => setHighlight(null), 2400)
    return () => clearTimeout(t)
  }, [highlight?.nonce])

  /** 「回到最新答复」：置贴底 + 滚到最后一轮 */
  const jumpToLatest = () => {
    setNearBottom(true)
    if (plain) {
      // plain（浮窗少量轮次直排，全量在 DOM 中）→ 直接平滑滚底即可
      const el = scrollerRef.current
      if (el) el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' })
      return
    }
    localNonce.current += 1
    setScrollTarget({ index: Math.max(0, turns.length - 1), nonce: localNonce.current, align: 'end' })
  }

  const lastMsg = messages[messages.length - 1]
  const followKey = `${messages.length}:${lastMsg?.content.length ?? 0}:${streaming ? 1 : 0}`
  const showJumpToLatest = hasMessages && !nearBottom

  if (messages.length === 0) {
    return (
      <div className="flex-1 flex flex-col items-center justify-center p-6 select-none fade-in">
        <div className="w-12 h-12 rounded-2xl bg-gradient-to-tr from-accent/20 to-accent2/20 border border-edge flex items-center justify-center text-accent shadow-subtle mb-3">
          <Sparkles size={22} />
        </div>
        <h3 className="text-[15px] font-semibold text-ink tracking-tight">开启一段新对话</h3>
        {/* 仅浮窗等场景（传了 emptyHint）才显示补充说明；主对话空态保持极简，不放说明与常用指令 */}
        {emptyHint && (
          <p className="text-[13px] text-inkfaint mt-1.5 max-w-[340px] text-center leading-relaxed">
            {emptyHint}
          </p>
        )}
      </div>
    )
  }

  // plain：直排渲染全部轮次
  if (plain) {
    return (
      <div className="relative flex-1 min-h-0 flex flex-col">
        <div ref={scrollerRef} className={scrollCls ? cn('flex-1 overflow-y-auto', scrollCls) : 'flex-1 overflow-y-auto px-3 sm:px-6 py-5'}>
          <div className="max-w-[860px] mx-auto space-y-2">
            {turns.map((t, i) => (
              <TurnBlock
                key={t.turnSeq}
                turn={t}
                isLast={i === turns.length - 1}
                running={streaming}
                reasoning={reasoning}
                runPhase={runPhase}
                actions={actions}
                showCopy={showCopy}
                highlight={highlight}
              />
            ))}
          </div>
        </div>
        {showJumpToLatest && <JumpToLatestButton onClick={jumpToLatest} />}
      </div>
    )
  }

  return (
    <div className="relative flex-1 min-h-0 flex flex-col">
      <div ref={scrollerRef} className={scrollCls ? cn('flex-1 overflow-y-auto', scrollCls) : 'flex-1 overflow-y-auto px-3 sm:px-6 py-5'}>
        <div className="max-w-[860px] mx-auto">
          <VirtualList<TurnGroup>
            items={turns}
            scrollerRef={scrollerRef}
            estimateSize={() => 240}
            getKey={(t) => String(t.turnSeq)}
            stickToBottom={nearBottom}
            followKey={followKey}
            scrollTarget={scrollTarget}
            renderItem={(t, index) => (
              <TurnBlock
                turn={t}
                isLast={index === turns.length - 1}
                running={streaming}
                reasoning={reasoning}
                runPhase={runPhase}
                actions={actions}
                showCopy={showCopy}
                highlight={highlight}
              />
            )}
          />
        </div>
      </div>
      {showJumpToLatest && <JumpToLatestButton onClick={jumpToLatest} />}
    </div>
  )
}

/**
 * 「回到最新答复」浮钮（对齐老版本 ChatView.tsx:466 的「最新答复」胶囊）。
 *
 * 定位：贴在滚动容器右下角。容器是 relative，故用 absolute；z-20 保证压在消息之上、
 * 但不抢 SideRail（z-30）与 SidePanel 的层级。
 */
function JumpToLatestButton({ onClick }: { onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'absolute right-3 sm:right-5 bottom-4 z-20',
        'inline-flex items-center gap-1.5 px-3 py-1.5 rounded-full',
        'bg-panel2/95 hover:bg-panel border border-edge hover:border-accent/50 shadow-lg backdrop-blur-sm',
        'text-[12px] font-medium text-inkdim hover:text-ink transition-all cursor-pointer select-none',
        'active:scale-95 fade-in',
      )}
      title="回到最新一轮答复（并恢复运行中自动跟随）"
    >
      <ArrowDown size={13} className="text-accent shrink-0" />
      <span>最新答复</span>
    </button>
  )
}

/**
 * 单轮对话块（参照桌面端双轨容器模型）：
 * 「第 N 轮」分隔条 → 用户提问 → 执行过程抽屉（N 个步骤）→ 最终交付答复
 */
function TurnBlock({ turn, isLast, running, reasoning, runPhase, actions, showCopy, highlight }: {
  turn: TurnGroup
  isLast: boolean
  running: boolean
  reasoning: string
  runPhase: RunPhase
  actions: ThreadActions
  showCopy: boolean
  /** 全局定位高亮态：本轮回命中时用于展开抽屉 + 高亮目标消息 */
  highlight?: { id: string; nonce: number } | null
}) {
  const users = turn.items.filter((m) => m.role === 'user')
  const assistants = turn.items.filter((m) => m.role === 'assistant')
  /**
   * 本轮开始时刻 = 本轮 user 消息的 createdAt（对齐老版本 types.ts:888-890）。
   *
   * 🚨 铁律：绝不能换成 store 里的全局「运行开始时刻」。全局字段在停止 Run、
   * 切换会话、resumeRun 时都会被重置为「此刻」，导致秒表从 0 重开；
   * 而消息 createdAt 是服务端落库的不可变事实 —— 切走再切回、刷新页面，
   * 读到的都是同一个值，秒表自然接着走，不需要任何额外状态同步。
   */
  const turnStartedAt = users[0]?.createdAt ?? assistants[0]?.createdAt ?? 0
  const hasTools = (m: Message) => (m.toolCalls?.length ?? 0) > 0
  const hasReasoning = (m: Message) => Boolean(m.reasoning?.trim())
  // 过程步骤的判据：有工具调用，或「只有思考过程、尚无正文」的纯思考轮。
  // 注意：带正文的答复即便带 thinking，也必须留在交付轨，否则思考卡片会把最终答复藏进过程抽屉。
  const isProcessStep = (m: Message) => hasTools(m) || (hasReasoning(m) && !m.content.trim())

  const active = running && isLast

  let process: Message[] = []
  let deliverable: Message | null = null
  // A1（对齐老版本）：最后一步仍带工具调用（正在执行 或 中途停止）时，
  // 它既进抽屉展示过程，也作交付轨展示其总结文本（交付轨渲染前会剥空过程字段）。
  // 仅在有正文时才发交付，避免空白气泡。
  const canAlsoDeliver = (m: Message) => !active && Boolean(m.content.trim())
  if (assistants.length === 1) {
    const only = assistants[0]
    if (isProcessStep(only)) {
      process = [only]
      if (canAlsoDeliver(only)) deliverable = only
    } else deliverable = only
  } else if (assistants.length > 1) {
    const last = assistants[assistants.length - 1]
    if (!isProcessStep(last)) {
      deliverable = last
      process = assistants.slice(0, -1)
    } else {
      process = assistants
      // A1：中途停止时最后一步仍带工具调用 → 同时发交付轨（展示其总结文本）
      if (canAlsoDeliver(last)) deliverable = last
    }
  }

  /**
   * 交付轨兜底（2026-11「对话完成后结果没显示出来」的最直接成因）。
   *
   * # 缺陷现场（事故会话 122559a3 / 9ee34e00）
   *
   * 93 条 assistant 里 **86 条正文为空**（只有 reasoning + tool_calls）。
   * 旧判据 `canAlsoDeliver = 有正文`，于是这 86 条全被塞进「执行过程」抽屉，
   * 交付轨渲染为 null —— 用户跑完 15 分钟只看到「第 1 轮」分隔条和一个折叠的
   * 过程卡片，**正文区一片空白**，体感就是「什么都没出来」。
   *
   * # 为什么兜底必须存在（而不是「模型本来就该给结论」）
   *
   * 收敛指令确实要求模型给结论，但**不能把可读性押在模型一定听话上**：
   * 用户中途停止、步数耗尽、上游断流都会让最后一步停在「带工具无正文」的形态。
   * 这时如实展示「本轮没有文字结论，共 N 步」是唯一不撒谎的做法 ——
   * 既不伪造正文，也不让整轮彻底隐身。
   */
  const needFallback = !active && process.length > 0 && !deliverable
  const fallbackStepCount = process.length
  // 兜底态必须自动展开抽屉：否则用户仍需手点一次才知道「跑过什么」
  const autoExpandSignal = needFallback ? -(turn.turnSeq + 1) : undefined

  const lastAssistantReasoning = assistants[assistants.length - 1]?.reasoning ?? ''
  const reverted = turn.items.some((m) => m.revertedAt)

  // 中途停止/最后一步仍带工具调用时，最后一步既要进抽屉（看过程），又要作交付轨（看总结文本）。
  // 对齐老版本 ExecutionProcessBlock 的做法，但**必须剥空过程字段**再交给交付轨：
  // 老版本 emit deliverable 时强制 `reasoning: null, toolEvents: [], toolCalls: []`，
  // 而本仓库此前直接把原始 Message 传进 MessageItem → 同一条消息的思考与工具卡片
  // 会在「执行过程卡片外面」再显示一份。
  const deliverableForRender = deliverable
    ? { ...deliverable, reasoning: undefined, toolCalls: undefined }
    : null

  /**
   * 本轮的定位命中情况（供「定位出处」联动）：
   * - hitInProcess：目标消息在抽屉内部 → 必须强制展开抽屉，否则用户看到空轮次
   * - 目标在交付轨/用户消息上时无需展开，靠 MessageItem 自身高亮即可
   *
   * nonce 作为 expandSignal 直传：每次定位都产生新 nonce，故重复定位同一轮也会再次展开。
   */
  const hitId = highlight?.id
  const hitInProcess = Boolean(hitId) && process.some((m) => m.id === hitId)
  const expandSignal = hitInProcess ? highlight!.nonce : autoExpandSignal

  return (
    <div className="pb-8">
      <div className={cn('space-y-3', reverted && 'opacity-60')}>
        {users.map((m) => (
          <MessageItem
            key={m.id}
            msg={m}
            actions={actions}
            highlighted={Boolean(hitId) && hitId === m.id}
          />
        ))}

        {process.length > 0 && (
          <ProcessDrawer
            steps={process}
            running={active}
            reasoning={active ? reasoning : undefined}
            expandSignal={expandSignal}
          />
        )}

        {deliverableForRender && (
          <MessageItem
            key={deliverableForRender.id}
            msg={deliverableForRender}
            actions={actions}
            highlighted={Boolean(hitId) && hitId === deliverableForRender.id}
            // 只有「本轮的进行中答复」才带阶段与秒表：历史轮次给 undefined，
            // 避免给已完成的旧消息挂上一个永不停止的计时器。
            runPhase={active ? runPhase : undefined}
            turnStartedAt={active ? turnStartedAt : undefined}
          />
        )}

        {/*
          交付轨兜底条：本轮全部步骤都没有正文时，正文区会完全空白。
          这里如实告知「跑到哪一步、为什么没有文字结论」，并已自动展开过程抽屉。
          🚨 绝不编造答复文案 —— 只陈述事实（步数 + 需要去哪里看细节）。
        */}
        {needFallback && (
          <div className="rounded-lg border border-edge bg-panel2/60 px-3 py-2 text-[12px] text-inkdim leading-relaxed">
            本轮已执行 <span className="text-ink font-medium">{fallbackStepCount}</span> 个步骤，
            但未产生文字结论（可能被中途停止、步数耗尽或上游断流）。
            详细执行过程已在上方展开，可逐步查看工具调用与输出。
          </div>
        )}

        {/* 轮次底部指标栏 */}
        <TurnFooterBar
          turn={turn}
          streaming={active}
          actions={actions}
          showCopy={showCopy}
          turnStartedAt={turnStartedAt}
        />
      </div>
    </div>
  )
}

/** 轮次底部指标栏（对齐桌面端）：复制、分支、本轮改动、整轮耗时、Token 消耗 */
function TurnFooterBar({ turn, streaming, actions, showCopy, turnStartedAt }: {
  turn: TurnGroup
  streaming: boolean
  actions: ThreadActions
  showCopy: boolean
  /** 本轮开始时刻（本轮 user 消息 createdAt）：运行中秒表的基准 */
  turnStartedAt: number
}) {
  const { toast, setPanel } = useUI()
  const [copied, setCopied] = useState(false)
  // 运行中实时跳动（对齐老版本 MessageItem.tsx:1101-1115 的「正在执行... 21分5秒」）
  const liveElapsed = useLiveElapsed(turnStartedAt, streaming)
  const items = turn.items
  const assistants = items.filter((m) => m.role === 'assistant')
  const lastAssistant = assistants[assistants.length - 1]
  const copyText = lastAssistant?.content ?? ''
  const totalTokens = items.reduce((a, m) => a + (m.tokens ?? 0), 0)
  const totalPrompt = items.reduce((a, m) => a + (m.promptTokens ?? 0), 0)
  const totalCached = items.reduce((a, m) => a + (m.cachedTokens ?? 0), 0)
  // 缓存命中率分母**只认输入 token**（Provider usage 语义：cached 是 prompt 的子集）。
  // 无输入用量时不得退化为「总 tokens」当分母——那会把输出也算进分母，
  // 命中率被系统性低估（如 100 输入全命中 + 900 输出会显示 10% 而非 100%）。
  const hasPromptUsage = totalPrompt > 0
  // 是否「上游确实汇报过缓存用量」：cachedTokens 为 undefined 表示该端点根本不返回
  // 缓存明细。此时强行展示「0.0% 缓存」会让用户误以为「缓存完全没命中」，
  // 而事实是「无此能力」——两种语义必须区分开（原型 mock 恒有该字段）。
  const hasCacheInfo = items.some((m) => m.cachedTokens !== undefined)
  const cacheRate = hasPromptUsage ? Math.round((totalCached / totalPrompt) * 1000) / 10 : 0
  // 输出 token 只能由「总量 − 输入」推算，且输入未知时不可推算（否则显示成总量）
  const totalOutput = hasPromptUsage ? Math.max(0, totalTokens - totalPrompt) : 0
  const totalMs = items.reduce((a, m) => a + (m.durationMs ?? 0), 0)
  const editFiles = items
    .flatMap((m) => m.toolCalls ?? [])
    .filter((c) => (c.name === 'write_file' || c.name === 'edit_file') && c.status === 'ok')

  // 🚨 运行中不能提前返回：`streaming` 时即便正文/tokens 都还是空（首字未到），
  // 也必须渲染出底部的「正在执行... X秒」——这正是用户最需要看到进度的静默期。
  // 老版本 MessageItem.tsx:1101 的 isRunningTurn 分支同样不受空内容影响。
  if (!streaming && !copyText && totalTokens === 0 && editFiles.length === 0) return null

  const itemCls = 'inline-flex items-center gap-1.5 px-2 py-0.5 rounded-md hover:bg-panel2 text-inkdim hover:text-ink transition-all shrink-0 cursor-pointer select-none active:scale-95'

  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5 text-[11px] pt-1">
      {!streaming && showCopy && copyText && (
        <button
          className={itemCls}
          title="复制回复正文"
          onClick={() => {
            navigator.clipboard?.writeText(copyText).then(
              () => { setCopied(true); setTimeout(() => setCopied(false), 1500); toast('success', '已复制到剪贴板') },
              () => toast('warn', '复制失败'),
            )
          }}
        >
          {copied ? <Check size={12} className="text-ok" /> : <Copy size={12} />}
          <span>{copied ? '已复制' : '复制'}</span>
        </button>
      )}

      {!streaming && actions.fork && lastAssistant && (
        <button
          className={`${itemCls} hover:text-accent`}
          title="以此轮为截止点分叉新会话"
          onClick={() => actions.fork?.(lastAssistant.id)}
        >
          <GitBranch size={12} />
          <span>分支</span>
        </button>
      )}

      {editFiles.length > 0 && (
        <button
          className="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-md bg-amber-500/10 text-amber-500 hover:bg-amber-500/20 font-medium transition-all shrink-0 cursor-pointer active:scale-95"
          title="查看本轮修改产生的文件变更快照"
          onClick={() => setPanel('snapshot')}
        >
          <GitCompare size={12} />
          <span>本轮改动（{editFiles.length}个文件）</span>
        </button>
      )}

      {/* 整轮耗时：运行中实时跳动，结束后显示静态总耗时（对齐老版本 1101-1147） */}
      {streaming ? (
        <span
          className="inline-flex items-center gap-1 text-accent font-mono cursor-default select-none animate-pulse shrink-0 px-1.5 py-0.5"
          title="整轮执行持续耗时：从发送消息起，持续跨越所有工具执行与计划步骤"
        >
          <Clock size={11} className="animate-spin" />
          <span>正在执行... {formatDuration(liveElapsed) || '0.1s'}</span>
        </span>
      ) : totalMs > 0 ? (
        <span
          className="inline-flex items-center gap-1 font-mono text-inkfaint cursor-default select-none px-1.5 py-0.5"
          title="本轮模型调用累计耗时（各步耗时之和，不含工具执行时间；服务端计时，刷新后仍可复现）"
        >
          <Clock size={11} className="opacity-75" />
          <span>{formatStepMs(totalMs)}</span>
        </span>
      ) : null}

      {totalTokens > 0 && (
        <span
          className="inline-flex items-center gap-1 font-mono text-inkfaint cursor-default select-none shrink-0 px-1.5 py-0.5"
          title={[
            `本轮累计消耗 ${totalTokens.toLocaleString('zh-CN')} tokens（含全部步骤）`,
            ...(hasPromptUsage
              ? [`输入: ${totalPrompt.toLocaleString('zh-CN')} · 输出: ${totalOutput.toLocaleString('zh-CN')}`]
              : []),
            ...(hasCacheInfo && hasPromptUsage
              ? [`缓存命中: ${totalCached.toLocaleString('zh-CN')}（${cacheRate.toFixed(1)}%）`]
              : []),
            ...(totalMs > 0 ? [`模型调用累计耗时: ${formatStepMs(totalMs)}`] : []),
          ].join('\n')}
        >
          <Zap size={11} className="text-amber-400" />
          <span>{formatStepTokens(totalTokens)}</span>
        </span>
      )}

      {/* 缓存命中率：**只要上游汇报过缓存用量就展示**（含 0%）。
          旧实现在 totalCached === 0 时整块隐藏，导致「缓存确实没命中」与
          「本端点不提供缓存明细」两种语义无法区分，用户以为功能没做。 */}
      {hasCacheInfo && hasPromptUsage && (
        <span
          className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded-md bg-accent2/10 text-accent2 font-medium select-none cursor-default shrink-0 text-[11px]"
          title={`缓存命中率 = 缓存命中 tokens / 输入 tokens\n本轮命中 ${totalCached.toLocaleString('zh-CN')} / 输入 ${totalPrompt.toLocaleString('zh-CN')}，输出 ${totalOutput.toLocaleString('zh-CN')}`}
        >
          <Zap size={10} />
          <span>{cacheRate.toFixed(1)}% 缓存</span>
        </span>
      )}
    </div>
  )
}

function MessageItem({ msg, actions, runPhase, turnStartedAt, highlighted }: {
  msg: Message
  actions: ThreadActions
  /** 仅「本轮进行中的答复」传入；历史消息不传（否则会挂上永不停止的计时器） */
  runPhase?: RunPhase
  /** 本轮开始时刻（本轮 user 消息 createdAt，不可变事实）：秒表基准 */
  turnStartedAt?: number
  /** 「定位出处」命中高亮：ring 高亮 1 次（由 MessageList 的 2.4s 定时器自动清除） */
  highlighted?: boolean
}) {
  // 锚点属性：id 供外部（如未来的深链）定位，data-message-id 供调试与自动化校验。
  // 🚨 注意：虚拟列表下未渲染的轮次并没有这个 DOM，所以**内部定位逻辑不得依赖它**
  // （见 MessageThread 里「数据 → 索引 → scrollToIndex」的注释）；此处仅供可见项使用。
  const anchorProps = { id: `msg-${msg.id}`, 'data-message-id': msg.id } as const
  const ringCls = 'ring-2 ring-accent ring-offset-2 ring-offset-panel transition-all duration-500'

  if (msg.role === 'user') {
    const reverted = Boolean(msg.revertedAt)
    const actBtn = 'w-7 h-7 rounded-lg hover:bg-panel2 flex items-center justify-center transition-all shrink-0 active:scale-95'
    return (
      <div className="group flex justify-end items-start gap-2" {...anchorProps}>
        {/* 操作列（用户消息前方：移动端与桌面端均常驻显示） */}
        <div className="flex items-center gap-0.5 mt-2 shrink-0">
          {reverted ? (
            <>
              {actions.reapply && (
                <button
                  className={`${actBtn} text-ok hover:text-ok bg-ok/10 border border-ok/20`}
                  title="重新应用（所有消息和文件还原为最新）"
                  onClick={() => actions.reapply?.(msg.id)}
                >
                  <RotateCw size={13} />
                </button>
              )}
              {actions.deleteFrom && (
                <button
                  className={`${actBtn} text-inkdim hover:text-err hover:bg-err/10`}
                  title="删除对话：删除该提问及其后全部内容（服务端仅置灰不再参与上下文，未物理删除，可重新应用恢复）"
                  onClick={() => actions.deleteFrom?.(msg.id)}
                >
                  <Trash2 size={13} />
                </button>
              )}
            </>
          ) : (
            <>
              {actions.fork && (
                <button
                  className={`${actBtn} text-inkdim hover:text-accent hover:bg-panel2`}
                  title="从此提问节点分叉新对话（新建会话；前序历史仅本端副本，服务端不复制消息）"
                  onClick={() => actions.fork?.(msg.id)}
                >
                  <GitBranch size={13} />
                </button>
              )}
              {actions.revertTo && (
                <button
                  className={`${actBtn} text-inkdim hover:text-warn hover:bg-panel2`}
                  title="撤回对话到此处（此后消息置灰并同步服务端回滚；发送新消息将覆盖此后历史）"
                  onClick={() => actions.revertTo?.(msg.id)}
                >
                  <RotateCcw size={13} />
                </button>
              )}
            </>
          )}
        </div>

        <div className="flex flex-col items-end gap-1 max-w-[92%] sm:max-w-[80%] min-w-0">
          <div className={cn(
            'w-full text-left rounded-2xl rounded-tr-sm bg-panel2 border border-edge px-4 py-2.5 text-ink',
            reverted && 'opacity-80 border-dashed border-warn/40 bg-warn/10',
            highlighted && ringCls,
          )}>
            {reverted && (
              <div className="flex items-center gap-1.5 text-[11px] text-warn bg-warn/15 border border-warn/30 px-2 py-0.5 rounded-md w-fit mb-2 select-none">
                <RotateCcw size={11} />
                <span>已撤回至此处（发送新消息将覆盖此后历史，或点击左侧重新应用恢复）</span>
              </div>
            )}
            <div className="text-[14px] leading-relaxed text-ink whitespace-pre-wrap break-words">{msg.content}</div>
          </div>
        </div>
      </div>
    )
  }

  // assistant（交付轨）
  return (
    <div
      className={cn('flex flex-col gap-1.5 py-1 rounded-xl', highlighted && ringCls)}
      {...anchorProps}
    >
      {msg.reasoning && msg.reasoning.trim() && (
        <ReasoningCard text={msg.reasoning} streaming={Boolean(msg.streaming && !msg.content)} />
      )}

      {msg.content && (
        // leading 走 1.7 与 .md 段统一（原 leading-relaxed=1.625 与正文 1.7 不一致，
        // 会让流式光标所在行与正文行距错位）
        <div className="text-[14px] leading-[1.7] text-ink">
          <MarkdownLite text={msg.content} />
          {msg.streaming && <span className="inline-block w-1.5 h-3.5 bg-accent align-text-bottom ml-1 rounded-sm animate-pulse" />}
        </div>
      )}

      {msg.streaming && !msg.content && (
        // 「消息已上屏 → 下方加载中动画」（对齐老版本 animate-bounce 三点错峰）。
        // 阶段文案随 runPhase 推进，并把已等待时长打在文案里 —— 时间在动 = 程序活着。
        <RunProgressInline
          running={Boolean(msg.streaming)}
          startedAt={turnStartedAt ?? 0}
          phase={runPhase ?? 'waiting'}
        />
      )}
    </div>
  )
}
