import { useMemo, useState, useRef, useEffect } from 'react'
import { Brain, Layers, History, Sparkles } from 'lucide-react'
import { useChat } from '../../stores/chatStore'
import { useSessions } from '../../stores/sessionStore'
import { resolveModelContext, formatTokens } from '../../lib/context'
import { cn } from '../../components/ui'

const strategyLabel = (s: 'semantic' | 'summary_only' | 'truncate') =>
  s === 'semantic' ? '智能语义压缩' : s === 'summary_only' ? '仅保留摘要' : '截断最早轮次'

// 内联上下文仪表（对齐桌面端 ContextUsageGauge）：
// 触发钮 = SVG 环形圆圈进度 + 百分比 + 「上下文」；点击弹出「上下文窗口负载」详情浮层
// （状态徽标 + 模型 + 活跃 tokens/上限 + 带 75% 压缩线的进度条 + 剩余/阈值/距压缩 + 压缩历史）。
export function ContextGauge() {
  const { tokensIn, tokensOut, cachedTokens, compactionFreed, compactions, compactionPending, requestCompaction } = useChat()
  const { sessions, currentId } = useSessions()
  const s = sessions.find((x) => x.id === currentId)
  const [open, setOpen] = useState(false)
  // 浮层用 fixed 定位脱离输入卡片的 overflow-hidden 裁剪
  const [pos, setPos] = useState<{ left: number; bottom: number } | null>(null)
  const rootRef = useRef<HTMLDivElement>(null)
  const panelRef = useRef<HTMLDivElement>(null)

  const toggle = () => {
    if (open) {
      setOpen(false)
      return
    }
    const r = rootRef.current?.getBoundingClientRect()
    if (r) {
      const left = Math.max(8, Math.min(r.left, window.innerWidth - 328))
      setPos({ left, bottom: window.innerHeight - r.top + 8 })
    }
    setOpen(true)
  }

  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node
      if (panelRef.current?.contains(t)) return
      if (rootRef.current?.contains(t)) return
      setOpen(false)
    }
    const onResize = () => setOpen(false)
    window.addEventListener('mousedown', onDown)
    window.addEventListener('resize', onResize)
    return () => {
      window.removeEventListener('mousedown', onDown)
      window.removeEventListener('resize', onResize)
    }
  }, [open])

  const { limit, modelName, providerName } = useMemo(() => resolveModelContext(s?.modelRef), [s?.modelRef])

  // 活跃上下文 = 累计输入输出 - 已压缩释放量（压缩不改「会话累计消耗」语义）
  const activeTokens = Math.max(0, tokensIn + tokensOut - compactionFreed)
  const pct = limit > 0 ? Math.min(100, Math.round((activeTokens / limit) * 1000) / 10) : 0
  const remaining = Math.max(0, limit - activeTokens)
  const threshold = Math.round(limit * 0.75)
  const toCompaction = Math.max(0, threshold - activeTokens)
  const cacheRate = tokensIn > 0 ? Math.round((cachedTokens / tokensIn) * 1000) / 10 : 0

  // 压缩历史（按当前会话过滤）
  const sessionCompactions = compactions.filter((c) => c.sessionId === currentId)
  const freedTotal = sessionCompactions.reduce((a, c) => a + c.freedTokens, 0)
  const lastCompaction = sessionCompactions[0]

  // 桌面端状态分段：≥75% 接近压缩阈值（critical）、≥60% 负载适中（moderate）、其余空间充裕（safe）
  const status: 'safe' | 'moderate' | 'critical' = pct >= 75 ? 'critical' : pct >= 60 ? 'moderate' : 'safe'
  const ringCls = status === 'critical' ? 'text-err' : status === 'moderate' ? 'text-warn' : 'text-ok'
  const barCls = status === 'critical' ? 'bg-err' : status === 'moderate' ? 'bg-warn' : 'bg-ok'
  const badge =
    status === 'critical' ? (
      <span className="px-1.5 py-0.5 rounded-md text-[10px] bg-err/15 text-err font-medium animate-pulse">接近压缩阈值</span>
    ) : status === 'moderate' ? (
      <span className="px-1.5 py-0.5 rounded-md text-[10px] bg-warn/15 text-warn font-medium">负载适中</span>
    ) : (
      <span className="px-1.5 py-0.5 rounded-md text-[10px] bg-ok/15 text-ok font-medium">空间充裕</span>
    )

  // SVG 环形进度参数（对齐桌面端：r=6.2 → 周长 ≈ 38.96）
  const CIRCLE_R = 6.2
  const CIRCLE_C = 2 * Math.PI * CIRCLE_R
  const offset = CIRCLE_C * (1 - Math.min(pct, 100) / 100)

  return (
    <div className="relative shrink-0" ref={rootRef}>
      <button
        type="button"
        className={cn(
          'inline-flex items-center gap-1.5 px-2 py-1 rounded-lg text-[11px] font-mono transition-all duration-150',
          open
            ? 'bg-panel3 text-ink ring-1 ring-accent/40'
            : 'bg-panel2/80 hover:bg-panel3 text-ink',
        )}
        onClick={toggle}
        title="上下文窗口负载（点击查看详情 / 压缩历史）"
      >
        <svg className="w-3.5 h-3.5 -rotate-90 shrink-0" viewBox="0 0 16 16">
          <circle cx="8" cy="8" r={CIRCLE_R} fill="none" stroke="currentColor" className="text-edge" strokeWidth="2.4" />
          <circle
            cx="8"
            cy="8"
            r={CIRCLE_R}
            fill="none"
            stroke="currentColor"
            className={cn(ringCls, 'transition-all duration-500 ease-out')}
            strokeWidth="2.4"
            strokeDasharray={CIRCLE_C}
            strokeDashoffset={offset}
            strokeLinecap="round"
          />
        </svg>
        <span className="font-semibold text-ink tracking-tight">{pct}%</span>
        <span className="text-[10px] text-inkfaint hidden sm:inline">上下文</span>
      </button>

      {open && pos && (
        <div
          ref={panelRef}
          className="fixed z-50 w-[320px] max-h-[min(72vh,540px)] overflow-y-auto rounded-2xl border border-edge bg-panel p-3.5 text-[12px] slide-up"
          style={{ left: pos.left, bottom: pos.bottom }}
        >
          {/* 标题栏 */}
          <div className="flex items-center justify-between pb-2.5 border-b border-edge/60">
            <div className="flex items-center gap-1.5">
              <Brain size={14} className="text-accent2 shrink-0" />
              <span className="font-semibold text-ink text-[13px] tracking-tight">上下文窗口负载</span>
            </div>
            {badge}
          </div>

          {/* 模型与厂商 */}
          <div className="flex items-center justify-between text-[11px] text-inkdim mt-2.5 font-mono">
            <span className="truncate max-w-[280px]" title={providerName ? `${providerName}/${modelName}` : modelName}>
              模型: <span className="text-ink font-medium">{providerName ? `${providerName}/${modelName}` : modelName}</span>
            </span>
          </div>

          {/* Token 数值与比例 */}
          <div className="mt-2.5 flex items-baseline justify-between font-mono">
            <div>
              <span className="text-[17px] font-bold text-ink tracking-tight">{activeTokens.toLocaleString('zh-CN')}</span>
              <span className="text-[11px] text-inkfaint ml-1">tokens</span>
            </div>
            <div className="text-[12px] text-inkdim">
              上限 {formatTokens(limit)}（<span className={cn('font-semibold', status === 'critical' ? 'text-err' : 'text-ink')}>{pct}%</span>）
            </div>
          </div>

          {/* 线性水平进度条，附带 75% 压缩触发线 */}
          <div className="w-full bg-panel2 h-2 rounded-full overflow-hidden relative mt-2 border border-edge/60">
            <div className={cn('h-full rounded-full transition-all duration-300', barCls)} style={{ width: `${Math.min(pct, 100)}%` }} />
            <div className="absolute top-0 bottom-0 w-[1.5px] bg-err/90 z-10 shadow-sm" style={{ left: '75%' }} title="75% 智能语义压缩触发线" />
          </div>

          {/* 详细指标明细 */}
          <div className="mt-3 space-y-1.5 text-[11px] text-inkdim border-t border-edge/40 pt-2.5 font-mono">
            <Row label="剩余可用空间:" value={`${remaining.toLocaleString('zh-CN')} tokens`} />
            <Row label="智能压缩阈值 (75%):" value={formatTokens(threshold)} />
            <Row
              label="距触发自动压缩:"
              value={toCompaction > 0 ? `剩余 ${toCompaction.toLocaleString('zh-CN')} tokens` : '已达阈值 / 待确认压缩'}
              tone={toCompaction === 0 ? 'err' : undefined}
            />
            {lastCompaction ? (
              <Row
                label="历史压缩状态:"
                value={`已压缩 ${sessionCompactions.length} 次 · 累计释放 ${formatTokens(freedTotal)}`}
                tone="accent2"
              />
            ) : (
              <Row label="历史压缩状态:" value="未发生压缩 (历史完整)" />
            )}
            {cacheRate > 0 && <Row label="缓存命中:" value={`${formatTokens(cachedTokens)} · ${cacheRate.toFixed(1)}%`} tone="accent2" />}
          </div>

          {/* 压缩历史 + 手动入口 */}
          <div className="mt-2.5 pt-2 border-t border-edge/40">
            <div className="flex items-center justify-between gap-2 mb-1.5">
              <span className="inline-flex items-center gap-1 text-[11px] text-inkdim font-medium">
                <History size={11} className="text-inkfaint" />
                压缩历史
              </span>
              <button
                className={cn(
                  'inline-flex items-center gap-1 px-1.5 py-0.5 rounded-md text-[11px] transition-colors',
                  compactionPending
                    ? 'text-inkfaint cursor-not-allowed'
                    : 'text-accent hover:bg-accent/10',
                )}
                disabled={Boolean(compactionPending)}
                onClick={() => requestCompaction('manual')}
                title="手动发起一次上下文压缩（需在确认条中确认）"
              >
                <Sparkles size={10} />
                {compactionPending ? '待确认中' : '压缩上下文'}
              </button>
            </div>

            {sessionCompactions.length === 0 ? (
              <div className="text-[11px] text-inkfaint leading-relaxed">
                尚未发生压缩。达到 75% 会自动请求确认，也可手动压缩。
              </div>
            ) : (
              <div className="space-y-1">
                {sessionCompactions.slice(0, 3).map((c) => (
                  <div key={c.id} className="rounded-md bg-panel2/50 border border-edge/60 px-2 py-1">
                    <div className="flex items-center justify-between font-mono text-[11px]">
                      <span className="text-inkdim inline-flex items-center gap-1">
                        <Layers size={9} className="text-accent2" />
                        {formatTokens(c.beforeTokens)} → {formatTokens(c.afterTokens)}
                      </span>
                      <span className="text-ok">-{formatTokens(c.freedTokens)}</span>
                    </div>
                    <div className="text-[10px] text-inkfaint mt-0.5">
                      {new Date(c.createdAt).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })} ·{' '}
                      {strategyLabel(c.strategy)} · 保留 {c.keptTurns} 轮
                    </div>
                  </div>
                ))}
                {sessionCompactions.length > 3 && (
                  <div className="text-[10px] text-inkfaint">另有 {sessionCompactions.length - 3} 次压缩记录…</div>
                )}
              </div>
            )}
          </div>

          <div className="mt-2.5 pt-2 border-t border-edge/40 text-[10px] text-inkfaint leading-relaxed">
            未发生预算标注 ≈；已发生用量以 Provider <code className="font-mono bg-panel2 px-1 py-0.5 rounded text-accent">usage</code> 为准。
          </div>
        </div>
      )}
    </div>
  )
}

function Row({ label, value, tone }: { label: string; value: string; tone?: 'err' | 'accent2' }) {
  return (
    <div className="flex items-center justify-between gap-2">
      <span className="text-inkfaint shrink-0">{label}</span>
      <span className={cn('font-medium text-right', tone === 'err' ? 'text-err' : tone === 'accent2' ? 'text-accent2' : 'text-ink')}>
        {value}
      </span>
    </div>
  )
}
