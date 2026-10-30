import { useEffect, useMemo, useRef, useState } from 'react'
import { useChat } from '../../stores/chatStore'
import { useSessions } from '../../stores/sessionStore'
import { MarkdownLite } from '../../components/MarkdownLite'
import { VirtualList } from '../../components/VirtualList'
import { cn } from '../../components/ui'
import { useUI } from '../../stores/uiStore'
import { Check, Clock, Copy, GitBranch, GitCompare, RotateCcw, RotateCw, Trash2, Zap, Sparkles, MessageSquare, Terminal, FileCode2 } from 'lucide-react'
import { ProcessDrawer, formatStepMs, formatStepTokens } from './ProcessDrawer'
import type { Message } from '../../types'

interface TurnGroup {
  turnSeq: number
  items: Message[]
}

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
  const { streaming, reasoning, fork, revertTo, reapply, deleteFrom } = useChat()
  const currentId = useSessions((s) => s.currentId)
  const messages = useChat((s) => s.messagesOf(currentId))

  return (
    <MessageThread
      messages={messages}
      streaming={streaming}
      reasoning={reasoning}
      actions={{ fork, revertTo, reapply, deleteFrom }}
    />
  )
}

/** 通用对话线程：主对话与子进程详情浮窗复用同一渲染体系（轮次/双轨/执行过程/指标/消息操作） */
export function MessageThread({
  messages,
  streaming = false,
  reasoning = '',
  actions,
  showCopy = true,
  scrollCls,
  emptyHint,
  plain = false,
}: MessageThreadProps) {
  const scrollerRef = useRef<HTMLDivElement>(null)
  const [nearBottom, setNearBottom] = useState(true)
  const { setDraft } = useChat()

  // 切换数据源：重置贴底并跳到底部
  useEffect(() => {
    setNearBottom(true)
    const el = scrollerRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [messages[0]?.id])

  useEffect(() => {
    const el = scrollerRef.current
    if (!el) return
    const onScroll = () => {
      const v = el.scrollHeight - el.scrollTop - el.clientHeight < 140
      setNearBottom((prev) => (prev === v ? prev : v))
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [])

  const turns = useMemo<TurnGroup[]>(() => {
    const out: TurnGroup[] = []
    for (const m of messages) {
      const last = out[out.length - 1]
      if (last && last.turnSeq === m.turnSeq) last.items.push(m)
      else out.push({ turnSeq: m.turnSeq, items: [m] })
    }
    return out
  }, [messages])

  const lastMsg = messages[messages.length - 1]
  const followKey = `${messages.length}:${lastMsg?.content.length ?? 0}:${streaming ? 1 : 0}`

  const suggestions = [
    { icon: <FileCode2 size={13} className="text-accent" />, text: '帮我查看工具注册表实现，增加同名冲突检测' },
    { icon: <Terminal size={13} className="text-amber-400" />, text: '运行构建脚本并检查有哪些未通过的类型定义' },
    { icon: <Sparkles size={13} className="text-accent2" />, text: '分析当前项目结构并梳理能力扩展路线图' },
  ]

  if (messages.length === 0) {
    return (
      <div className="flex-1 flex flex-col items-center justify-center p-6 select-none fade-in">
        <div className="w-12 h-12 rounded-2xl bg-gradient-to-tr from-accent/20 to-accent2/20 border border-edge flex items-center justify-center text-accent shadow-subtle mb-3">
          <Sparkles size={22} />
        </div>
        <h3 className="text-[15px] font-semibold text-ink tracking-tight">开启一段新对话</h3>
        <p className="text-[13px] text-inkfaint mt-1.5 max-w-[340px] text-center leading-relaxed">
          {emptyHint ?? '随时下发指令、提问代码实现或让 Agent 协助执行自动化重构任务'}
        </p>

        {!emptyHint && (
          <div className="mt-5 w-full max-w-[460px] space-y-2">
            <div className="text-[11px] font-medium text-inkdim flex items-center gap-1.5 px-1">
              <MessageSquare size={11} className="text-inkfaint" />
              <span>常用指令示例（点击填入输入框）：</span>
            </div>
            {suggestions.map((s, idx) => (
              <button
                key={idx}
                type="button"
                onClick={() => setDraft(s.text)}
                className="w-full text-left p-2.5 rounded-xl border border-edge bg-panel2/40 hover:bg-panel2/80 hover:border-edge2 transition-all flex items-center gap-2.5 group cursor-pointer active:scale-[0.99]"
              >
                <span className="p-1.5 rounded-lg bg-panel2 shrink-0 group-hover:scale-105 transition-transform">
                  {s.icon}
                </span>
                <span className="text-[13px] text-inkdim group-hover:text-ink transition-colors flex-1 truncate font-medium">
                  {s.text}
                </span>
                <span className="text-[11px] text-accent font-medium shrink-0">
                  填入 ↵
                </span>
              </button>
            ))}
          </div>
        )}
      </div>
    )
  }

  // plain：直排渲染全部轮次
  if (plain) {
    return (
      <div ref={scrollerRef} className={scrollCls ? cn('flex-1 overflow-y-auto', scrollCls) : 'flex-1 overflow-y-auto px-3 sm:px-6 py-5'}>
        <div className="max-w-[860px] mx-auto space-y-2">
          {turns.map((t, i) => (
            <TurnBlock
              key={t.turnSeq}
              turn={t}
              isLast={i === turns.length - 1}
              running={streaming}
              reasoning={reasoning}
              actions={actions}
              showCopy={showCopy}
            />
          ))}
        </div>
      </div>
    )
  }

  return (
    <div ref={scrollerRef} className={scrollCls ? cn('flex-1 overflow-y-auto', scrollCls) : 'flex-1 overflow-y-auto px-3 sm:px-6 py-5'}>
      <div className="max-w-[860px] mx-auto">
        <VirtualList<TurnGroup>
          items={turns}
          scrollerRef={scrollerRef}
          estimateSize={() => 240}
          getKey={(t) => String(t.turnSeq)}
          stickToBottom={nearBottom}
          followKey={followKey}
          renderItem={(t, index) => (
            <TurnBlock
              turn={t}
              isLast={index === turns.length - 1}
              running={streaming}
              reasoning={reasoning}
              actions={actions}
              showCopy={showCopy}
            />
          )}
        />
      </div>
    </div>
  )
}

/**
 * 单轮对话块（参照桌面端双轨容器模型）：
 * 「第 N 轮」分隔条 → 用户提问 → 执行过程抽屉（N 个步骤）→ 最终交付答复
 */
function TurnBlock({ turn, isLast, running, reasoning, actions, showCopy }: {
  turn: TurnGroup
  isLast: boolean
  running: boolean
  reasoning: string
  actions: ThreadActions
  showCopy: boolean
}) {
  const users = turn.items.filter((m) => m.role === 'user')
  const assistants = turn.items.filter((m) => m.role === 'assistant')
  const hasTools = (m: Message) => (m.toolCalls?.length ?? 0) > 0
  const hasReasoning = (m: Message) => Boolean(m.reasoning?.trim())

  const active = running && isLast

  let process: Message[] = []
  let deliverable: Message | null = null
  if (assistants.length === 1) {
    const only = assistants[0]
    if (hasTools(only) || hasReasoning(only)) process = [only]
    else deliverable = only
  } else if (assistants.length > 1) {
    const last = assistants[assistants.length - 1]
    if (!hasTools(last)) {
      deliverable = last
      process = assistants.slice(0, -1)
    } else {
      process = assistants
    }
  }

  const showReasoningInline = active && reasoning.length > 0 && process.length === 0
  const reverted = turn.items.some((m) => m.revertedAt)

  return (
    <div className="pb-8">
      <div className={cn('space-y-3', reverted && 'opacity-60')}>
        {users.map((m) => <MessageItem key={m.id} msg={m} actions={actions} />)}

        {process.length > 0 && (
          <ProcessDrawer steps={process} running={active} reasoning={active ? reasoning : undefined} />
        )}

        {deliverable && <MessageItem key={deliverable.id} msg={deliverable} actions={actions} />}

        {/* 轮次底部指标栏 */}
        <TurnFooterBar turn={turn} streaming={active} actions={actions} showCopy={showCopy} />

        {showReasoningInline && (
          <div className="text-[13px] text-inkfaint border-l-2 border-accent/60 pl-3 py-1 bg-panel2/40 rounded-r-lg">
            <span className="text-[10px] uppercase font-mono tracking-wider text-accent font-semibold">思考中</span>
            <div className="mt-1 leading-relaxed text-inkdim">{reasoning}</div>
          </div>
        )}
      </div>
    </div>
  )
}

/** 轮次底部指标栏（对齐桌面端）：复制、分支、本轮改动、整轮耗时、Token 消耗 */
function TurnFooterBar({ turn, streaming, actions, showCopy }: { turn: TurnGroup; streaming: boolean; actions: ThreadActions; showCopy: boolean }) {
  const { toast, setPanel } = useUI()
  const [copied, setCopied] = useState(false)
  const items = turn.items
  const assistants = items.filter((m) => m.role === 'assistant')
  const lastAssistant = assistants[assistants.length - 1]
  const copyText = lastAssistant?.content ?? ''
  const totalTokens = items.reduce((a, m) => a + (m.tokens ?? 0), 0)
  const totalPrompt = items.reduce((a, m) => a + (m.promptTokens ?? 0), 0)
  const totalCached = items.reduce((a, m) => a + (m.cachedTokens ?? 0), 0)
  const rateBase = totalPrompt > 0 ? totalPrompt : totalTokens
  const cacheRate = rateBase > 0 ? Math.round((totalCached / rateBase) * 1000) / 10 : 0
  const totalMs = items.reduce((a, m) => a + (m.durationMs ?? 0), 0)
  const editFiles = items
    .flatMap((m) => m.toolCalls ?? [])
    .filter((c) => (c.name === 'write_file' || c.name === 'edit_file') && c.status === 'ok')

  if (!copyText && totalTokens === 0 && editFiles.length === 0) return null

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

      {totalMs > 0 && (
        <span className="inline-flex items-center gap-1 font-mono text-inkfaint cursor-default select-none px-1.5 py-0.5" title="整轮执行总耗时（含全部步骤与工具执行）">
          <Clock size={11} className="opacity-75" />
          <span>{formatStepMs(totalMs)}</span>
        </span>
      )}

      {totalTokens > 0 && (
        <span className="inline-flex items-center gap-1 font-mono text-inkfaint cursor-default select-none shrink-0 px-1.5 py-0.5" title={`本轮累计消耗 ${totalTokens} tokens（含全部步骤）`}>
          <Zap size={11} className="text-amber-400" />
          <span>{formatStepTokens(totalTokens)}</span>
        </span>
      )}

      {totalCached > 0 && (
        <span
          className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded-md bg-accent2/10 text-accent2 font-medium select-none cursor-default shrink-0 text-[11px]"
          title={`本轮累计消耗 ${totalTokens.toLocaleString()} tokens（含全部步骤）\n输入: ${totalPrompt.toLocaleString()}（缓存命中: ${totalCached.toLocaleString()} · ${cacheRate.toFixed(1)}%）· 输出: ${(totalTokens - totalPrompt).toLocaleString()}`}
        >
          <Zap size={10} />
          <span>{cacheRate.toFixed(1)}% 缓存</span>
        </span>
      )}
    </div>
  )
}

function MessageItem({ msg, actions }: { msg: Message; actions: ThreadActions }) {
  if (msg.role === 'user') {
    const reverted = Boolean(msg.revertedAt)
    const actBtn = 'w-7 h-7 rounded-lg hover:bg-panel2 flex items-center justify-center transition-all shrink-0 active:scale-95'
    return (
      <div className="group flex justify-end items-start gap-2">
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
                  title="删除对话：删除该提问及其后全部内容"
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
                  title="从此提问节点分叉新对话（复制前序历史）"
                  onClick={() => actions.fork?.(msg.id)}
                >
                  <GitBranch size={13} />
                </button>
              )}
              {actions.revertTo && (
                <button
                  className={`${actBtn} text-inkdim hover:text-warn hover:bg-panel2`}
                  title="撤回对话到此处（后续所有对话与执行过程置灰；发送新消息将覆盖此后历史）"
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
            reverted && 'opacity-80 border-dashed border-warn/40 bg-warn/10'
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
    <div className="flex flex-col gap-1.5 py-1">
      {msg.content && (
        <div className="text-[14px] leading-relaxed text-ink">
          <MarkdownLite text={msg.content} />
          {msg.streaming && <span className="inline-block w-1.5 h-3.5 bg-accent align-text-bottom ml-1 rounded-sm animate-pulse" />}
        </div>
      )}

      {msg.streaming && !msg.content && (
        <div className="flex items-center gap-1.5 text-inkfaint text-[12px] py-1">
          <span className="w-1.5 h-1.5 rounded-full bg-accent inline-block typing-dot" />
          <span className="w-1.5 h-1.5 rounded-full bg-accent inline-block typing-dot" />
          <span className="w-1.5 h-1.5 rounded-full bg-accent inline-block typing-dot" />
          <span className="ml-1 text-[12px] font-medium text-inkdim">思考中…</span>
        </div>
      )}
    </div>
  )
}
