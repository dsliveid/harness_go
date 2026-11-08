import { useMemo, useState, useRef, useEffect } from 'react'
import { Brain } from 'lucide-react'
import { useChat } from '../../stores/chatStore'
import { useSessions } from '../../stores/sessionStore'
import { resolveModelContext, formatTokens } from '../../lib/context'
import { cn } from '../../components/ui'

// 内联上下文仪表（对齐桌面端 ContextUsageGauge）：
// 触发钮 = SVG 环形圆圈进度 + 百分比 + 「上下文」；点击弹出「上下文窗口负载」详情浮层
// （状态徽标 + 模型 + 活跃 tokens/上限 + 进度条 + 剩余/缓存命中）。
// 说明：上下文「智能压缩」交互已整体下线（后端无压缩能力，保留即静默失败），
// 本组件只展示 Provider usage 返回的真实 token 水位（tokensIn/tokensOut/cachedTokens）。
//
// 口径：水位按**当前会话**记账（chatStore 的 usageBySession 账本），新开对话从 0 起算、
// 切回旧会话恢复其自身水位；页面刷新后内存账本清空，展示回到 0（delta 不落库是既定铁律，
// 服务端只有 sessions.total_tokens 合计，无法拆出输入/输出/缓存，故不做近似回填以免伪造数据）。
export function ContextGauge() {
  const { tokensIn, tokensOut, cachedTokens } = useChat()
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

  // 活跃上下文 = Provider usage 累计的输入 + 输出 tokens（真实水位，无压缩抵扣）
  const activeTokens = Math.max(0, tokensIn + tokensOut)
  const pct = limit > 0 ? Math.min(100, Math.round((activeTokens / limit) * 1000) / 10) : 0
  const remaining = Math.max(0, limit - activeTokens)
  const cacheRate = tokensIn > 0 ? Math.round((cachedTokens / tokensIn) * 1000) / 10 : 0

  // 桌面端状态分段：≥75% 负载偏高（critical）、≥60% 负载适中（moderate）、其余空间充裕（safe）
  const status: 'safe' | 'moderate' | 'critical' = pct >= 75 ? 'critical' : pct >= 60 ? 'moderate' : 'safe'
  const ringCls = status === 'critical' ? 'text-err' : status === 'moderate' ? 'text-warn' : 'text-ok'
  const barCls = status === 'critical' ? 'bg-err' : status === 'moderate' ? 'bg-warn' : 'bg-ok'
  const badge =
    status === 'critical' ? (
      <span className="px-1.5 py-0.5 rounded-md text-[10px] bg-err/15 text-err font-medium">负载偏高</span>
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
        title="上下文窗口负载（点击查看详情）"
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

          {/* 线性水平进度条 */}
          <div className="w-full bg-panel2 h-2 rounded-full overflow-hidden relative mt-2 border border-edge/60">
            <div className={cn('h-full rounded-full transition-all duration-300', barCls)} style={{ width: `${Math.min(pct, 100)}%` }} />
          </div>

          {/* 详细指标明细（均来自 Provider usage 真实事件） */}
          <div className="mt-3 space-y-1.5 text-[11px] text-inkdim border-t border-edge/40 pt-2.5 font-mono">
            <Row label="剩余可用空间:" value={`${remaining.toLocaleString('zh-CN')} tokens`} />
            <Row label="累计输入:" value={`${formatTokens(tokensIn)} tokens`} />
            <Row label="累计输出:" value={`${formatTokens(tokensOut)} tokens`} />
            {cacheRate > 0 && <Row label="缓存命中:" value={`${formatTokens(cachedTokens)} · ${cacheRate.toFixed(1)}%`} tone="accent2" />}
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
