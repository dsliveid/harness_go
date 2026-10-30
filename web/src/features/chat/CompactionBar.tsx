import { useState } from 'react'
import { Layers, ChevronDown, Check, Scissors, Sparkles, FileText, Clock } from 'lucide-react'
import { useChat } from '../../stores/chatStore'
import { useSessions } from '../../stores/sessionStore'
import { Badge, Button, cn } from '../../components/ui'
import { formatTokens } from '../../lib/context'
import type { CompactionStrategy } from '../../types'

const STRATEGIES: Array<{ key: CompactionStrategy; label: string; desc: string; icon: React.ReactNode }> = [
  { key: 'semantic', label: '智能语义压缩', desc: '生成摘要，保留最近 3 轮完整消息', icon: <Sparkles size={12} /> },
  { key: 'summary_only', label: '仅保留摘要', desc: '摘要 + 最近 1 轮，压缩比更高', icon: <FileText size={12} /> },
  { key: 'truncate', label: '截断最早轮次', desc: '不生成摘要，直接丢弃最早轮次', icon: <Scissors size={12} /> },
]

/** 压缩确认条：需人工确认后才真正丢上下文，避免自动压缩吃掉关键细节 */
export function CompactionBar() {
  const { compactionPending: p, confirmCompaction, dismissCompaction } = useChat()
  const currentId = useSessions((s) => s.currentId)
  const [strategy, setStrategy] = useState<CompactionStrategy>('semantic')
  const [detail, setDetail] = useState(false)

  if (!p || p.sessionId !== currentId) return null

  const pct = p.limit > 0 ? Math.round((p.beforeTokens / p.limit) * 1000) / 10 : 0
  const isAuto = p.trigger === 'auto'
  const afterEstimate = Math.max(
    1200,
    Math.round(p.beforeTokens * (strategy === 'semantic' ? 0.42 : strategy === 'summary_only' ? 0.55 : 0.3)),
  )

  return (
    <div
      className={cn(
        'mx-3 sm:mx-5 mb-2.5 rounded-2xl border bg-panel p-3.5 slide-up',
        isAuto ? 'border-warn/40' : 'border-accent/35',
      )}
    >
      <div className="flex items-center gap-2 flex-wrap">
        <div
          className={cn(
            'w-6 h-6 rounded-lg flex items-center justify-center shrink-0',
            isAuto ? 'bg-warn/15 text-warn' : 'bg-accent/15 text-accent',
          )}
        >
          <Layers size={14} />
        </div>
        <span className="text-[13px] font-semibold text-ink">上下文压缩确认</span>
        <Badge tone={isAuto ? 'yellow' : 'blue'}>{isAuto ? '已达 75% 阈值' : '手动触发'}</Badge>
        <span className="font-mono text-[12px] text-inkdim">
          {formatTokens(p.beforeTokens)} / {formatTokens(p.limit)}（{pct}%）
        </span>
        <span className="flex-1" />
        <button
          className="w-6 h-6 rounded-md flex items-center justify-center text-inkfaint hover:text-ink hover:bg-panel2 transition-colors"
          title="稍后再说"
          onClick={dismissCompaction}
        >
          <ChevronDown size={14} />
        </button>
      </div>

      <div className="mt-2 text-[13px] text-ink leading-relaxed">
        压缩只改变模型看到的上下文；原始消息仍完整保存在数据库，可随时回看与回溯。
      </div>

      {/* 策略选择 */}
      <div className="mt-2.5 grid grid-cols-1 sm:grid-cols-3 gap-1.5">
        {STRATEGIES.map((it) => {
          const active = strategy === it.key
          return (
            <button
              key={it.key}
              onClick={() => setStrategy(it.key)}
              className={cn(
                'text-left rounded-xl border px-2.5 py-2 transition-all duration-150',
                active
                  ? 'border-accent/50 bg-accent/[0.08]'
                  : 'border-edge bg-panel2/40 hover:border-edge2 hover:bg-panel2',
              )}
            >
              <div className="flex items-center gap-1.5">
                <span className={active ? 'text-accent' : 'text-inkfaint'}>{it.icon}</span>
                <span className={cn('text-[12px] font-medium', active ? 'text-ink' : 'text-inkdim')}>{it.label}</span>
                {active && <Check size={12} className="text-accent ml-auto shrink-0" />}
              </div>
              <div className="text-[11px] text-inkfaint mt-1 leading-relaxed">{it.desc}</div>
            </button>
          )
        })}
      </div>

      {/* 预估结果 */}
      <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] font-mono text-inkdim">
        <span>
          预计：{formatTokens(p.beforeTokens)} → <span className="text-ok font-medium">{formatTokens(afterEstimate)}</span>
        </span>
        <span className="text-inkfaint">|</span>
        <span>折叠 {Math.max(p.droppedTurns, 1)} 轮 · 保留 {strategy === 'semantic' ? p.keptTurns : strategy === 'summary_only' ? 1 : 0} 轮</span>
      </div>

      {detail && (
        <div className="mt-2 rounded-lg border border-edge bg-panel2/40 p-2.5 text-[11px] text-inkdim leading-relaxed space-y-1">
          <div>· 折叠范围：除最近若干轮之外的历史消息（含工具调用入参与 diff 明细）</div>
          <div>· 保留内容：系统提示 + 项目约束 + 活跃计划 + 最近轮次完整消息</div>
          <div>· 摘要写入 <code className="font-mono text-accent">compaction_records</code>，可作为新上下文前缀复用</div>
          <div>· 压缩不改动「会话累计消耗」，仅降低活跃上下文占用</div>
        </div>
      )}

      <div className="mt-3 pt-2.5 border-t border-edge/60 flex flex-wrap items-center gap-2">
        <Button size="sm" variant="primary" onClick={() => confirmCompaction(strategy)} className="gap-1.5">
          <Sparkles size={13} />
          <span>立即压缩</span>
        </Button>
        <Button size="sm" variant="outline" onClick={dismissCompaction}>稍后再说</Button>
        <Button size="sm" variant="ghost" className="gap-1.5 text-inkdim" onClick={() => setDetail((v) => !v)}>
          <FileText size={12} />
          <span>{detail ? '收起详情' : '压缩详情'}</span>
        </Button>
      </div>
    </div>
  )
}

/** 压缩摘要条：压缩完成后在消息区顶部展示最近一次摘要，可展开查看全文 */
export function CompactionNotice() {
  const { compactions } = useChat()
  const currentId = useSessions((s) => s.currentId)
  const [open, setOpen] = useState(false)

  const latest = compactions.find((c) => c.sessionId === currentId)
  if (!latest) return null

  const strategyLabel =
    latest.strategy === 'semantic' ? '智能语义压缩' : latest.strategy === 'summary_only' ? '仅保留摘要' : '截断最早轮次'

  return (
    <div className="mx-3 sm:mx-5 mt-2 rounded-xl border border-accent2/35 bg-accent2/[0.06] overflow-hidden">
      <button
        className="w-full flex items-center gap-2 px-3 py-2 text-left hover:bg-accent2/[0.09] transition-colors"
        onClick={() => setOpen((v) => !v)}
        title={open ? '收起摘要' : '展开摘要'}
      >
        <Layers size={13} className="text-accent2 shrink-0" />
        <span className="text-[12px] font-medium text-ink">上下文已压缩</span>
        <span className="font-mono text-[11px] text-inkdim">
          {formatTokens(latest.beforeTokens)} → {formatTokens(latest.afterTokens)}
        </span>
        <Badge tone="green">释放 {formatTokens(latest.freedTokens)}</Badge>
        <span className="hidden sm:inline-flex items-center gap-1 text-[11px] text-inkfaint">
          <Clock size={10} />
          {new Date(latest.createdAt).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })} · {strategyLabel}
        </span>
        <span className="flex-1" />
        <ChevronDown size={13} className={cn('text-inkfaint transition-transform', open && 'rotate-180')} />
      </button>
      {open && (
        <div className="px-3 pb-2.5 pt-0.5 border-t border-accent2/20">
          <pre className="mt-2 whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed text-inkdim">{latest.summary}</pre>
        </div>
      )}
    </div>
  )
}
