import { useEffect, useMemo, useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { Badge, cn } from '../../components/ui'
import type { LogEntry } from '../../types'
import { api } from '../../lib/api'
import type { SnapshotMode, TokenStatApiRow } from '../../lib/apiTypes'

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
// 数据源：GET /api/v1/stats/tokens（走 rollup_hourly 预聚合，不扫明细表，30 天量级通常 <20ms）。
// 数字全部为服务端真实统计，前端不做任何换算；历史「前端演示数据」面板已随双模式下线一并删除。
const RANGE_MS: Record<TimeRange, number | null> = { today: 24 * 3_600_000, '7d': 7 * 24 * 3_600_000, '30d': 30 * 24 * 3_600_000, all: null }

export function TokenStatsPanel() {
  const [range, setRange] = useState<TimeRange>('today')
  const [groupBy, setGroupBy] = useState<'model' | 'provider' | 'day'>('model')
  const [apiRows, setApiRows] = useState<TokenStatApiRow[]>([])
  const [summary, setSummary] = useState<{ calls: number; prompt: number; completion: number; cached: number; hit: number } | null>(null)
  const [loading, setLoading] = useState(false)
  const [loadErr, setLoadErr] = useState('')

  useEffect(() => {
    let alive = true
    const ms = RANGE_MS[range]
    const to = Date.now()
    const from = ms ? to - ms : to - 30 * 24 * 3_600_000
    setLoading(true)
    void api
      .getTokenStats({ from, to, group_by: groupBy })
      .then((res) => {
        if (!alive) return
        setApiRows(res.items ?? [])
        setSummary({
          calls: res.total_calls ?? 0,
          prompt: res.prompt_tokens ?? 0,
          completion: res.completion_tokens ?? 0,
          cached: res.cached_tokens ?? 0,
          hit: res.cache_hit_rate ?? 0,
        })
        setLoadErr('')
      })
      .catch((e: unknown) => {
        if (!alive) return
        setApiRows([])
        setSummary(null)
        setLoadErr(e instanceof Error ? e.message : '用量查询失败')
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [range, groupBy])

  return (
    <div className="flex flex-col gap-4">
      <div>
        <div className="text-[13px] font-medium">Token 用量统计</div>
        <div className="text-[12px] text-inkdim mt-0.5 leading-relaxed">
          按模型 / 厂商 / 天聚合的调用次数与 token 用量。数据来自天分片审计库的小时级预聚合。
        </div>
      </div>

        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[12px] text-inkfaint shrink-0">范围</span>
          <div className="flex items-center gap-1">
            {TIME_RANGES.map((r) => (
              <Chip key={r.key} active={range === r.key} onClick={() => setRange(r.key)}>{r.label}</Chip>
            ))}
          </div>
          <span className="text-[12px] text-inkfaint shrink-0 ml-2">分组</span>
          <div className="flex items-center gap-1">
            {([['model', '按模型'], ['provider', '按厂商'], ['day', '按天']] as const).map(([k, label]) => (
              <Chip key={k} active={groupBy === k} onClick={() => setGroupBy(k)}>{label}</Chip>
            ))}
          </div>
        </div>

        {loadErr && (
          <div className="rounded-xl border border-err/40 bg-err/[0.06] px-3 py-2 text-[12px] text-err">
            {loadErr}
          </div>
        )}

        {summary && (
          <div className="grid grid-cols-2 sm:grid-cols-4 gap-2">
            {[
              ['调用次数', String(summary.calls)],
              ['提示 token', fmt(summary.prompt)],
              ['补全 token', fmt(summary.completion)],
              ['缓存命中率', `${(summary.hit * 100).toFixed(1)}%`],
            ].map(([k, v]) => (
              <div key={k} className="rounded-xl border border-edge bg-panel2/25 px-3 py-2">
                <div className="text-[11px] text-inkfaint">{k}</div>
                <div className="text-[15px] font-medium font-mono mt-0.5">{v}</div>
              </div>
            ))}
          </div>
        )}

        <div className="rounded-xl border border-edge overflow-hidden">
          <div className="grid grid-cols-5 gap-2 px-3 py-2 border-b border-edge/70 text-[11px] text-inkfaint">
            <span>{groupBy === 'day' ? '日期' : groupBy === 'provider' ? '厂商' : '模型'}</span>
            <span className="text-right">调用</span>
            <span className="text-right">提示</span>
            <span className="text-right">补全</span>
            <span className="text-right">缓存</span>
          </div>
          {loading && <div className="px-3 py-3 text-center text-[12px] text-inkfaint">查询中…</div>}
          {!loading && apiRows.length === 0 && (
            <div className="px-3 py-6 text-center text-[12px] text-inkfaint">
              该时段无调用记录（或日志保留期已过）
            </div>
          )}
          {apiRows.map((r) => (
            <div key={r.key} className="grid grid-cols-5 gap-2 px-3 py-1.5 border-b border-edge/40 last:border-b-0 text-[12px]">
              <span className="truncate font-mono">{r.key || '—'}</span>
              <span className="text-right font-mono text-inkdim">{r.calls}</span>
              <span className="text-right font-mono text-inkdim">{fmt(r.prompt_tokens)}</span>
              <span className="text-right font-mono text-inkdim">{fmt(r.completion_tokens)}</span>
              <span className="text-right font-mono text-inkdim">{fmt(r.cached_tokens)}</span>
            </div>
          ))}
        </div>
    </div>
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

/** 日志级别说明（鼠标悬停提示，说明每档会看到什么）。 */
const LEVEL_HINTS: Record<'debug' | 'info' | 'warn' | 'error', string> = {
  debug: '调试：含最详细内部流水，仅供排查',
  info: '信息：启动装配、请求访问等常规流水',
  warn: '警告（默认）：只保留必要的告警与错误',
  error: '错误：仅记录失败与异常',
}

/** 报文抓取三档（settings 键 log.snapshot_mode）。量化口径由后端约定给出，前端只做展示。 */
const SNAPSHOT_MODES: Array<{ key: SnapshotMode; label: string; desc: string; danger?: boolean }> = [
  { key: 'off', label: '关闭', desc: '不落报文快照 · 日增约 50MB' },
  { key: 'preview', label: '预览', desc: '落截断预览 · 日增约 500MB（30 天约 15GB）' },
  { key: 'full', label: '完整', desc: '落完整报文 · 日增约 5GB（30 天约 150GB）', danger: true },
]

/** 时间范围 → 服务端 from/to（RFC3339）。'全部' 不下发时间窗，由后端按保留策略兜底。 */
function rangeToWindow(range: TimeRange): { from?: string; to?: string } {
  if (range === 'all') return {}
  if (range === 'today') {
    const start = new Date()
    start.setHours(0, 0, 0, 0)
    return { from: start.toISOString(), to: new Date().toISOString() }
  }
  const win = TIME_RANGES.find((r) => r.key === range)?.ms ?? null
  if (!win) return {}
  return { from: new Date(Date.now() - win).toISOString(), to: new Date().toISOString() }
}

/** 行内时间：优先从日志文本首部解析真实时间戳（服务端 items 只给 {level,text}）。 */
const LOG_TIME_RE = /^(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}:\d{2})/
function parseLogTime(text: string): number {
  const m = LOG_TIME_RE.exec(text)
  if (!m) return 0
  const t = Date.parse(`${m[1]}T${m[2]}`)
  return Number.isNaN(t) ? 0 : t
}

/** traceId 形态（长十六进制串）——用于决定关键字输入走 traceId 还是 keyword 通道。 */
const TRACE_ID_RE = /^[0-9a-f]{16,}$/i

function normalizeLevel(raw: string): LogEntry['level'] {
  const lv = (raw || '').toLowerCase()
  if (lv.includes('err') || lv === 'fatal' || lv === 'panic') return 'error'
  if (lv.includes('warn')) return 'warn'
  return 'info'
}

export function LogsPanel() {
  // level / 时间范围 / keyword 全部作为 **服务端 query 参数** 下发给 GET /api/v1/logs，
  // 前端不再对一次性 500 行做本地过滤（那会让第 500 行以外的日志永远搜不到）。
  const [logEntries, setLogEntries] = useState<LogEntry[]>([])
  const [level, setLevel] = useState<LevelFilter>('all')
  const [range, setRange] = useState<TimeRange>('today')
  const [keyword, setKeyword] = useState('')
  const [expandedId, setExpandedId] = useState<string | null>(null)

  // 服务端返回的保留策略 / 抓取档位（真值来源，缺字段就显示"未知"，绝不硬编码天数）
  const [retentionDays, setRetentionDays] = useState<number | undefined>(undefined)
  const [earliestDay, setEarliestDay] = useState<string | undefined>(undefined)
  const [snapshotMode, setSnapshotMode] = useState<string>('')
  const [total, setTotal] = useState<number | undefined>(undefined)
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(false)
  const [loadErr, setLoadErr] = useState('')
  const [patching, setPatching] = useState(false)
  const [patchMsg, setPatchMsg] = useState<{ ok: boolean; text: string } | null>(null)

  // 日志级别（Agent 自身日志的热调）：来自 GET /logs/config 的 level 字段
  const [logLevel, setLogLevel] = useState<string>('')
  const [levelPatching, setLevelPatching] = useState(false)
  const [levelMsg, setLevelMsg] = useState<{ ok: boolean; text: string } | null>(null)

  // 首屏读取当前真实生效的级别（后端返回 LevelVar 的值，不是猜测值）
  useEffect(() => {
    let alive = true
    void api
      .getLogConfig()
      .then((cfg) => {
        if (alive) setLogLevel((cfg.level || '').toLowerCase())
      })
      .catch(() => {
        if (alive) setLogLevel('')
      })
    return () => {
      alive = false
    }
  }, [])

  const applyLogLevel = (next: string) => {
    if (next === logLevel) return
    setLevelPatching(true)
    setLevelMsg(null)
    void api
      .patchSettings({ 'log.level': next })
      .then(() => {
        setLogLevel(next)
        setLevelMsg({
          ok: true,
          text: `已设为 ${next.toUpperCase()}：控制台与日志文件同步生效，无需重启`,
        })
      })
      .catch((e: unknown) => {
        setLevelMsg({ ok: false, text: `设置失败：${e instanceof Error ? e.message : '服务端拒绝'}` })
      })
      .finally(() => setLevelPatching(false))
  }

  // 服务端过滤：任一条件变化就重新请求（含 300ms 去抖，避免逐字符打服务端）
  useEffect(() => {
    let alive = true
    const timer = window.setTimeout(() => {
      setLoading(true)
      const kw = keyword.trim()
      // 关键字框兼容 traceId：形如长十六进制串时走 traceId 通道，否则走 keyword 全文通道
      const asTrace = TRACE_ID_RE.test(kw)
      const win = rangeToWindow(range)
      void api
        .getLogs({
          limit: 500,
          level: level === 'all' ? undefined : level,
          from: win.from,
          to: win.to,
          traceId: asTrace ? kw : undefined,
          keyword: !asTrace && kw ? kw : undefined,
        })
        .then((res) => {
          if (!alive) return
          const items = res.items ?? []
          setLogEntries(
            items.map((l, i) => ({
              id: `srv-${i}`,
              time: parseLogTime(l.text),
              level: normalizeLevel(l.level),
              source: 'server',
              text: l.text,
            })),
          )
          setTotal(typeof res.total === 'number' ? res.total : undefined)
          setHasMore(Boolean(res.has_more))
          setRetentionDays(typeof res.retention_days === 'number' ? res.retention_days : undefined)
          setEarliestDay(res.earliest_available_day || undefined)
          setSnapshotMode(res.snapshot_mode || '')
          setLoadErr('')
        })
        .catch((e: unknown) => {
          if (!alive) return
          setLogEntries([])
          setTotal(undefined)
          setHasMore(false)
          setLoadErr(e instanceof Error ? e.message : '日志查询失败')
        })
        .finally(() => {
          if (alive) setLoading(false)
        })
    }, 300)
    return () => {
      alive = false
      window.clearTimeout(timer)
    }
  }, [level, range, keyword])

  const applySnapshotMode = (next: SnapshotMode) => {
    if (next === snapshotMode) return
    if (next === 'full' && !window.confirm(
      '确认切换到「完整」报文抓取？\n\n完整档会落盘全部请求/响应报文，日增约 5GB，30 天累积约 150GB。\n一旦产生即为真实磁盘占用，切回低档不会自动清理已有数据。',
    )) return
    setPatching(true)
    setPatchMsg(null)
    // 值必须是 JSON 字符串（带引号），后端按 json.RawMessage 存库
    void api
      .patchSettings({ 'log.snapshot_mode': next })
      .then(() => {
        setSnapshotMode(next)
        setPatchMsg({
          ok: true,
          text: `已切换为「${SNAPSHOT_MODES.find((m) => m.key === next)?.label}」，仅对之后的新调用生效`,
        })
      })
      .catch((e: unknown) => {
        setPatchMsg({ ok: false, text: `切换失败：${e instanceof Error ? e.message : '服务端拒绝'}` })
      })
      .finally(() => setPatching(false))
  }

  // level / range / keyword 已由服务端过滤，直接返回服务端结果。
  // （原「来源」筛选已移除：source 恒为 'server'，只有一个选项的控件属于误导性复杂度。）
  const list = useMemo(() => logEntries, [logEntries])

  const retentionText = retentionDays != null && earliestDay
    ? `日志在线保留 ${retentionDays} 天（最早可查 ${earliestDay} ~ 今天），超期自动清理。`
    : '保留策略未知'
  const activeMode = SNAPSHOT_MODES.find((m) => m.key === snapshotMode)

  return (
    <div className="flex flex-col gap-3">
      <div>
        <div className="text-[13px] font-medium">日志查询</div>
        <div className="text-[12px] text-inkdim mt-0.5 leading-relaxed">
          运行日志与运行时事件，按级别 / 来源 / 时间范围 / 关键字组合过滤；点击行展开详情（traceId + 上下文说明）。
        </div>
      </div>

      {/* 保留策略说明条：N 天与最早可查日全部来自后端响应，取不到就明说"未知" */}
      <div
        className={cn(
          'rounded-xl border px-3 py-2 text-[12px] leading-relaxed flex items-start gap-2',
          retentionDays != null
            ? 'border-edge bg-panel2/40 text-inkdim'
            : 'border-warn/40 bg-warn/[0.06] text-warn',
        )}
        data-testid="logs-retention-bar"
      >
        <span className="shrink-0">{retentionDays != null ? '保留策略' : '⚠️'}</span>
        <span className="min-w-0">
          {retentionText}
          {retentionDays == null && (
            <span className="text-[11px] opacity-80">
              （后端未返回 retention_days / earliest_available_day；本面板不臆造保留天数）
            </span>
          )}
        </span>
      </div>

      {/* 日志级别调节（Agent 自身日志：控制台与 data/app.log 同源，一次调整同时生效） */}
      <div className="rounded-xl border border-edge bg-panel2/20 px-3 py-2.5 flex flex-col gap-2" data-testid="logs-level">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[12px] text-inkfaint shrink-0">日志级别</span>
          <div className="flex items-center gap-1">
            {(['debug', 'info', 'warn', 'error'] as const).map((lv) => (
              <button
                key={lv}
                type="button"
                disabled={levelPatching}
                onClick={() => applyLogLevel(lv)}
                className={cn(
                  'h-7 px-2.5 rounded-lg text-[12px] whitespace-nowrap transition-all duration-150 select-none border disabled:opacity-50',
                  logLevel === lv
                    ? 'bg-accent/15 text-accent font-medium border-accent/30 shadow-subtle'
                    : 'text-inkdim hover:text-ink hover:bg-panel2 border-transparent',
                )}
                title={LEVEL_HINTS[lv]}
              >
                {lv.toUpperCase()}
              </button>
            ))}
          </div>
          {logLevel === '' && <span className="text-[11px] text-warn">当前级别未知（后端未返回 level）</span>}
          {levelPatching && <span className="text-[11px] text-inkfaint">应用中…</span>}
        </div>
        <div className="text-[11px] text-inkfaint leading-relaxed">
          控制台与 <span className="font-mono">data/app.log</span> 共用同一个 logger，调整后<b>两者同步生效、无需重启</b>；
          默认 WARN 表示控制台只显示必要的警告与错误。
        </div>
        {levelMsg && (
          <div className={cn('text-[11px]', levelMsg.ok ? 'text-ok' : 'text-err')}>{levelMsg.text}</div>
        )}
      </div>

      {/* 报文抓取档位选择器 */}
      <div className="rounded-xl border border-edge bg-panel2/20 px-3 py-2.5 flex flex-col gap-2" data-testid="logs-snapshot-mode">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[12px] text-inkfaint shrink-0">报文抓取</span>
          <div className="flex items-center gap-1">
            {SNAPSHOT_MODES.map((m) => (
              <button
                key={m.key}
                type="button"
                disabled={patching}
                onClick={() => applySnapshotMode(m.key)}
                className={cn(
                  'h-7 px-2.5 rounded-lg text-[12px] whitespace-nowrap transition-all duration-150 select-none border disabled:opacity-50',
                  snapshotMode === m.key
                    ? 'bg-accent/15 text-accent font-medium border-accent/30 shadow-subtle'
                    : m.danger
                      ? 'text-err/80 hover:text-err hover:bg-err/10 border-transparent'
                      : 'text-inkdim hover:text-ink hover:bg-panel2 border-transparent',
                )}
                title={m.desc}
              >
                {m.label}
              </button>
            ))}
          </div>
          {snapshotMode === '' && (
            <span className="text-[11px] text-warn">当前档位未知（后端未返回 snapshot_mode）</span>
          )}
          {patching && <span className="text-[11px] text-inkfaint">切换中…</span>}
        </div>
        <div className="flex flex-col gap-1">
          {SNAPSHOT_MODES.map((m) => (
            <div key={m.key} className="flex items-start gap-2 text-[11px]">
              <span className={cn('font-mono shrink-0 w-16', snapshotMode === m.key ? 'text-accent' : 'text-inkfaint')}>
                {m.label}
                {snapshotMode === m.key ? ' ●' : ''}
              </span>
              <span className={m.danger ? 'text-err/90' : 'text-inkdim'}>{m.desc}</span>
            </div>
          ))}
        </div>
        {patchMsg && (
          <div className={cn('text-[11px]', patchMsg.ok ? 'text-ok' : 'text-err')}>{patchMsg.text}</div>
        )}
        {/* 常驻提示（不可省略：避免用户误期待补录/清理） */}
        <div className="text-[11px] text-inkfaint leading-relaxed border-t border-edge/60 pt-1.5">
          切换仅影响之后的新调用：不补录历史，不清理已有数据
        </div>
        {activeMode?.danger && (
          <div className="text-[11px] text-err leading-relaxed">
            ⚠️ 当前为「完整」档：30 天累积约 150GB，请确认磁盘余量。
          </div>
        )}
      </div>

      {/* 查询条件 */}
      <div className="flex flex-col gap-2 rounded-xl border border-edge bg-panel2/25 px-3 py-2.5">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[12px] text-inkfaint shrink-0">级别</span>
          <div className="flex items-center gap-1">
            {([
              // 列表已按所选级别被服务端过滤，各档明细数会互相矛盾，
              // 因此只有「全部」显示服务端 total，其余档位只显示名称。
              { key: 'all', label: `全部 (${total ?? logEntries.length})` },
              { key: 'info', label: '信息' },
              { key: 'warn', label: '警告' },
              { key: 'error', label: '错误' },
            ] as Array<{ key: LevelFilter; label: string }>).map((f) => (
              <Chip key={f.key} active={level === f.key} onClick={() => setLevel(f.key)}>{f.label}</Chip>
            ))}
          </div>
        </div>
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[12px] text-inkfaint shrink-0">时间</span>
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
        {loading && <div className="px-3 py-2 text-center text-[11px] text-inkfaint border-b border-edge/50">查询中…</div>}
        {loadErr && (
          <div className="px-3 py-2 text-center text-[11px] text-err border-b border-edge/50">
            日志查询失败：{loadErr}
          </div>
        )}
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
                <span className="text-[11px] text-inkfaint font-mono shrink-0 pt-0.5 w-[110px]">{l.time ? fmtTime(l.time) : '—'}</span>
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
                      <span className="font-mono text-inkdim">{l.time ? fmtTime(l.time) : '（日志文本无时间戳）'}</span>
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

      <div className="text-[11px] text-inkfaint leading-relaxed">
        {`本次返回 ${list.length} 条${total != null ? ` / 服务端命中 ${total} 条` : ''}${hasMore ? '（还有更多，已按 limit 截断）' : ''} · 过滤条件由服务端执行`}
        {' · '}
        {retentionText}
      </div>
    </div>
  )
}
