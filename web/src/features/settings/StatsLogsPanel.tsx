import { useEffect, useMemo, useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { logEntries as mockLogEntries, tokenStats, tokenSummary } from '../../mock/settingsData'
import { Badge, cn } from '../../components/ui'
import type { LogEntry, TokenStatRow } from '../../types'
import { api, apiMode } from '../../lib/api'

const fmt = (n: number) => n >= 10_000 ? `${(n / 1000).toFixed(1)}k` : String(n)
const fmtTime = (t: number) => {
  const d = new Date(t)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

// ---------------- 查询条件控件 ----------------
type TimeRange = 'today' | '7d' | '30d' | 'all'

const TIME_RANGES: Array<{ key: TimeRange; label: string; ms: number | null }> = [
  { key: 'today', label: '今日', ms: 24 * 3_600_000 },
  { key: '7d', label: '近 7 日', ms: 7 * 24 * 3_600_000 },
  { key: '30d', label: '近 30 日', ms: 30 * 24 * 3_600_000 },
  { key: 'all', label: '全部', ms: null },
]

function Chip({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      onClick={onClick}
      className={cn(
        'h-7 px-2.5 rounded-lg text-[12px] whitespace-nowrap transition-all duration-150 select-none border',
        active ? 'bg-accent/15 text-accent font-medium border-accent/30 shadow-subtle' : 'text-inkdim hover:text-ink hover:bg-panel2 border-transparent',
      )}
    >{children}</button>
  )
}

const searchCls = 'h-8 w-full sm:w-56 px-2.5 rounded-lg bg-panel2 border border-edge text-[12px] outline-none focus:border-accent'

// ---------------- Token 统计 ----------------
// 时间范围决定聚合系数与明细过滤（原型模拟后端聚合）
const RANGE_SCALE: Record<TimeRange, number> = { today: 0.12, '7d': 1, '30d': 2.9, all: 3.4 }
const RANGE_MS: Record<TimeRange, number | null> = { today: 24 * 3_600_000, '7d': 7 * 24 * 3_600_000, '30d': 30 * 24 * 3_600_000, all: null }

export function TokenStatsPanel() {
  const [range, setRange] = useState<TimeRange>('today')
  const [modelFilter, setModelFilter] = useState('all')
  const [expanded, setExpanded] = useState<string | null>(null)

  const rows = useMemo(() => {
    const ms = RANGE_MS[range]
    const scale = RANGE_SCALE[range]
    return tokenStats
      .filter((r) => modelFilter === 'all' || r.model === modelFilter)
      .map((r: TokenStatRow) => {
        const all = r.recent ?? []
        const recent = ms ? all.filter((c) => Date.now() - c.time <= ms) : all
        return {
          ...r,
          sessions: Math.max(1, Math.round(r.sessions * scale)),
          prompt: Math.round(r.prompt * scale),
          completion: Math.round(r.completion * scale),
          cached: Math.round(r.cached * scale),
          recent,
        }
      })
  }, [range, modelFilter])

  const total = rows.reduce((a, r) => a + r.prompt + r.completion, 0)
  const totalCached = rows.reduce((a, r) => a + r.cached, 0)

  return (
    <div className="flex flex-col gap-4">
      <div>
        <div className="text-[13px] font-medium">Token 用量统计</div>
        <div className="text-[12px] text-inkdim mt-0.5 leading-relaxed">
          已发生用量为后端响应 <code className="font-mono text-[11px] bg-panel2 px-1 rounded">usage</code> 的精确值；预估列展示本地 tokenizer + 请求结构开销的校准结果。点击行展开最近调用明细。
        </div>
      </div>

      {/* 查询条件：时间范围 + 模型筛选 */}
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-[12px] text-inkfaint shrink-0">时间范围</span>
        <div className="flex items-center gap-1">
          {TIME_RANGES.map((r) => (
            <Chip key={r.key} active={range === r.key} onClick={() => setRange(r.key)}>{r.label}</Chip>
          ))}
        </div>
        <span className="text-[12px] text-inkfaint shrink-0 sm:ml-2">模型</span>
        <select className={`${searchCls} sm:w-40`} value={modelFilter} onChange={(e) => setModelFilter(e.target.value)}>
          <option value="all">全部模型</option>
          {tokenStats.map((r) => <option key={r.model} value={r.model}>{r.provider} / {r.model}</option>)}
        </select>
      </div>

      {/* 汇总卡 */}
      <div className="grid grid-cols-3 gap-2">
        {[
          { label: '区间用量', value: fmt(total) },
          { label: '缓存命中', value: fmt(totalCached) },
          { label: '缓存命中率', value: total > 0 ? `${Math.round((totalCached / (total + totalCached * 3)) * 100)}%` : '0%' },
        ].map((c) => (
          <div key={c.label} className="rounded-xl border border-edge bg-panel2/30 p-3">
            <div className="text-[11px] text-inkfaint">{c.label}</div>
            <div className="text-[16px] font-semibold mt-1">{c.value}</div>
          </div>
        ))}
      </div>

      {/* 明细表（行可展开） */}
      <div className="rounded-xl border border-edge overflow-x-auto">
        <table className="w-full text-[12px] min-w-[560px]">
          <thead>
            <tr className="text-inkfaint text-left border-b border-edge">
              <th className="px-3 py-2 font-medium">模型</th>
              <th className="px-3 py-2 font-medium">会话</th>
              <th className="px-3 py-2 font-medium text-right">输入</th>
              <th className="px-3 py-2 font-medium text-right">输出</th>
              <th className="px-3 py-2 font-medium text-right">缓存</th>
              <th className="px-3 py-2 font-medium text-right">校准系数</th>
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 && (
              <tr><td colSpan={6} className="px-3 py-6 text-center text-inkfaint">该筛选条件下暂无数据</td></tr>
            )}
            {rows.map((r) => (
              <TokenRow key={r.model} r={r} expanded={expanded === r.model} onToggle={() => setExpanded(expanded === r.model ? null : r.model)} />
            ))}
          </tbody>
        </table>
      </div>

      <div className="rounded-lg border border-edge bg-panel2/40 px-2.5 py-2 text-[11px] text-inkfaint">
        UI 铁律：已发生用量显示精确值，未发生预算显示 ≈；偏差系数超过 ±3% 的模型会在下轮校准中修正。
      </div>
    </div>
  )
}

function TokenRow({ r, expanded, onToggle }: { r: TokenStatRow & { recent: TokenStatRow['recent'] }; expanded: boolean; onToggle: () => void }) {
  return (
    <>
      <tr
        className="border-b border-edge/50 cursor-pointer hover:bg-panel2/40 transition-colors"
        onClick={onToggle}
        title={expanded ? '收起明细' : '展开最近调用明细'}
      >
        <td className="px-3 py-2">
          <div className="font-mono flex items-center gap-1.5">
            {expanded ? <ChevronDown size={12} className="text-inkfaint shrink-0" /> : <ChevronRight size={12} className="text-inkfaint shrink-0" />}
            <span>{r.model}</span>
          </div>
          <div className="text-[11px] text-inkfaint">{r.provider}</div>
        </td>
        <td className="px-3 py-2">{r.sessions}</td>
        <td className="px-3 py-2 text-right font-mono">{fmt(r.prompt)}</td>
        <td className="px-3 py-2 text-right font-mono">{fmt(r.completion)}</td>
        <td className="px-3 py-2 text-right font-mono text-inkdim">{fmt(r.cached)}</td>
        <td className={cn('px-3 py-2 text-right font-mono', Math.abs(r.calibRatio - 1) <= 0.03 ? 'text-ok' : 'text-warn')}>
          {r.calibRatio.toFixed(2)}
        </td>
      </tr>
      {expanded && (
        <tr className="border-b border-edge/50">
          <td colSpan={6} className="bg-panel2/20 px-3 py-2.5">
            <div className="text-[11px] text-inkfaint mb-1.5">最近调用明细（含缓存命中与首字延迟）</div>
            {(r.recent ?? []).length === 0 ? (
              <div className="text-[12px] text-inkfaint py-1">该时间范围内无调用记录</div>
            ) : (
              <div className="flex flex-col gap-1">
                {(r.recent ?? []).map((c, i) => (
                  <div key={i} className="flex items-center gap-3 text-[12px] rounded-lg bg-panel px-2.5 py-1.5 border border-edge/40">
                    <span className="text-inkfaint font-mono shrink-0 w-[110px]">{fmtTime(c.time)}</span>
                    <span className="font-mono shrink-0">↑ {fmt(c.prompt)}</span>
                    <span className="font-mono shrink-0">↓ {fmt(c.completion)}</span>
                    <span className="font-mono text-inkdim shrink-0">缓存 {fmt(c.cached)}</span>
                    <span className="flex-1" />
                    <span className={cn('font-mono shrink-0', c.latencyMs < 1000 ? 'text-ok' : 'text-warn')}>{(c.latencyMs / 1000).toFixed(2)}s</span>
                  </div>
                ))}
              </div>
            )}
            <div className="text-[11px] text-inkfaint mt-1.5">
              预估输入 {fmt(Math.round(r.prompt / r.calibRatio))} · 实际输入 {fmt(r.prompt)} · 偏差 {(Math.abs(1 - r.calibRatio) * 100).toFixed(1)}%
            </div>
          </td>
        </tr>
      )}
    </>
  )
}

// ---------------- 日志查询 ----------------
type LevelFilter = 'all' | LogEntry['level']
type SourceFilter = 'all' | string

const LEVEL_BADGE: Record<LogEntry['level'], { tone: 'gray' | 'yellow' | 'red'; text: string }> = {
  info: { tone: 'gray', text: '信息' },
  warn: { tone: 'yellow', text: '警告' },
  error: { tone: 'red', text: '错误' },
}

export function LogsPanel() {
  // real 模式：日志来自服务端 data/app.log 尾行；mock 模式用演示数据
  const [logEntries, setLogEntries] = useState<LogEntry[]>(apiMode === 'mock' ? mockLogEntries : [])
  const [level, setLevel] = useState<LevelFilter>('all')
  const [source, setSource] = useState<SourceFilter>('all')
  const [range, setRange] = useState<TimeRange>('today')
  const [keyword, setKeyword] = useState('')
  const [expandedId, setExpandedId] = useState<string | null>(null)

  useEffect(() => {
    if (apiMode !== 'real') return
    let alive = true
    void api
      .getLogs({ limit: 500 })
      .then((lines) => {
        if (!alive) return
        const now = Date.now()
        setLogEntries(
          lines.map((l, i) => {
            const lv = l.level.toLowerCase()
            return {
              id: `srv-${i}`,
              time: now,
              level: lv.includes('warn') ? 'warn' : lv.includes('err') ? 'error' : 'info',
              source: 'server',
              text: l.text,
            } as LogEntry
          }),
        )
      })
      .catch(() => {
        if (alive) setLogEntries([])
      })
    return () => {
      alive = false
    }
  }, [])

  const sources = useMemo(() => Array.from(new Set(logEntries.map((l) => l.source))), [logEntries])
  const list = useMemo(() => {
    const ms = TIME_RANGES.find((r) => r.key === range)?.ms ?? null
    const kw = keyword.trim().toLowerCase()
    return logEntries.filter((l) =>
      (level === 'all' || l.level === level) &&
      (source === 'all' || l.source === source) &&
      (!ms || Date.now() - l.time <= ms) &&
      (!kw || l.text.toLowerCase().includes(kw) || (l.detail ?? '').toLowerCase().includes(kw) || (l.traceId ?? '').includes(kw)),
    )
  }, [level, source, range, keyword, logEntries])

  return (
    <div className="flex flex-col gap-3">
      <div>
        <div className="text-[13px] font-medium">日志查询</div>
        <div className="text-[12px] text-inkdim mt-0.5 leading-relaxed">
          运行日志与运行时事件，按级别 / 来源 / 时间范围 / 关键字组合过滤；点击行展开详情（traceId + 上下文说明）。
        </div>
      </div>

      {/* 查询条件 */}
      <div className="flex flex-col gap-2 rounded-xl border border-edge bg-panel2/25 px-3 py-2.5">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[12px] text-inkfaint shrink-0">级别</span>
          <div className="flex items-center gap-1">
            {([
              { key: 'all', label: `全部 (${logEntries.length})` },
              { key: 'info', label: `信息 (${logEntries.filter((l) => l.level === 'info').length})` },
              { key: 'warn', label: `警告 (${logEntries.filter((l) => l.level === 'warn').length})` },
              { key: 'error', label: `错误 (${logEntries.filter((l) => l.level === 'error').length})` },
            ] as Array<{ key: LevelFilter; label: string }>).map((f) => (
              <Chip key={f.key} active={level === f.key} onClick={() => setLevel(f.key)}>{f.label}</Chip>
            ))}
          </div>
        </div>
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[12px] text-inkfaint shrink-0">来源</span>
          <select className={`${searchCls} sm:w-36`} value={source} onChange={(e) => setSource(e.target.value)}>
            <option value="all">全部来源</option>
            {sources.map((s) => <option key={s} value={s}>{s}</option>)}
          </select>
          <span className="text-[12px] text-inkfaint shrink-0 sm:ml-2">时间</span>
          <div className="flex items-center gap-1">
            {TIME_RANGES.map((r) => (
              <Chip key={r.key} active={range === r.key} onClick={() => setRange(r.key)}>{r.label}</Chip>
            ))}
          </div>
          <input
            className={`${searchCls} sm:ml-auto`}
            placeholder="搜索文本 / traceId…"
            value={keyword}
            onChange={(e) => setKeyword(e.target.value)}
          />
        </div>
      </div>

      {/* 日志列表（行可展开详情） */}
      <div className="rounded-xl border border-edge">
        {list.length === 0 && <div className="px-3 py-8 text-center text-[12px] text-inkfaint">无匹配日志（调整查询条件后重试）</div>}
        {list.map((l) => {
          const open = expandedId === l.id
          return (
            <div key={l.id} className={cn('border-b border-edge/50 last:border-0', open && 'bg-panel2/20')}>
              <button
                type="button"
                data-log-id={l.id}
                className="w-full text-left px-3 py-2 flex items-start gap-2.5 hover:bg-panel2/40 transition-colors"
                onClick={() => setExpandedId(open ? null : l.id)}
                title={open ? '收起详情' : '展开详情'}
              >
                <span className="text-inkfaint shrink-0 pt-1 w-3.5 flex items-center justify-center">
                  {open ? <ChevronDown size={11} /> : <ChevronRight size={11} />}
                </span>
                <span className="text-[11px] text-inkfaint font-mono shrink-0 pt-0.5 w-[110px]">{fmtTime(l.time)}</span>
                <Badge tone={LEVEL_BADGE[l.level].tone} className="shrink-0">{LEVEL_BADGE[l.level].text}</Badge>
                <span className="text-[11px] text-accent font-mono shrink-0 pt-0.5 w-16">{l.source}</span>
                <span className={cn('text-[12px] min-w-0', l.level === 'error' ? 'text-err' : 'text-inkdim')}>{l.text}</span>
              </button>
              {open && (
                <div className="px-3 pb-3 pt-0.5 pl-[88px]">
                  <div className="rounded-lg bg-panel border border-edge/50 px-3 py-2.5 flex flex-col gap-1.5 text-[12px]">
                    <div className="flex items-center gap-2 flex-wrap">
                      <span className="text-inkfaint">traceId</span>
                      <code className="font-mono text-inkdim bg-panel2 px-1.5 py-0.5 rounded border border-edge/60">{l.traceId ?? '—'}</code>
                      <span className="text-inkfaint ml-2">时间</span>
                      <span className="font-mono text-inkdim">{fmtTime(l.time)}</span>
                      <Badge tone={LEVEL_BADGE[l.level].tone} className="ml-auto">{LEVEL_BADGE[l.level].text}</Badge>
                    </div>
                    <div className="text-inkdim leading-relaxed">{l.detail ?? '（无详细信息）'}</div>
                  </div>
                </div>
              )}
            </div>
          )
        })}
      </div>

      <div className="text-[11px] text-inkfaint">共 {list.length} 条 / {logEntries.length} 条 · 保留近 30 日，更早日志由 WAL checkpoint 归档</div>
    </div>
  )
}
