/**
 * 对话日志面板（LLM 调用流水查询）。
 *
 * 与「日志查询」页签的本质区别（务必不要在后续维护中混淆）：
 *   - 日志查询 → GET /api/v1/logs → data/app.log 的 **Agent 运维文本日志**
 *     （启动、panic、请求访问流水，人读的 slog 文本）
 *   - 本面板 → GET /api/v1/audit/llm-calls → logvault 天库的 **结构化对话审计**
 *     （模型、耗时、token、报文快照，机器查）
 *
 * 设计要点（对应 docs/日志与审计查询方案.md 的分层）：
 *   1. 列表跨会话：session_id 为空 = 全局查询（后端 LLMQuery.SessionID 空即不过滤）
 *   2. 点行看详情：概览 / 报文 / 工具 / 原始 四个子页签
 *   3. 诚实降级：preview 档不存正文时必须明说原因，不显示空白让人以为坏了
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import { api } from '../../lib/api'
import { Badge, cn } from '../../components/ui'
import { RealModeNotice } from '../../components/RealModeNotice'
import type { AuditLlmCallItem, AuditToolEventItem } from '../../lib/apiTypes'

type RangeKey = '1h' | 'today' | '7d' | '30d'
type DetailTab = 'overview' | 'payload' | 'tools' | 'raw'

const RANGES: Array<{ key: RangeKey; label: string; ms: number | null }> = [
  { key: '1h', label: '近 1 小时', ms: 3600_000 },
  { key: 'today', label: '今天', ms: null }, // 特殊：按当日 00:00 起算
  { key: '7d', label: '近 7 天', ms: 7 * 86400_000 },
  { key: '30d', label: '近 30 天', ms: 30 * 86400_000 },
]

/** 时间范围 → Unix 毫秒窗口（后端 parseRange 要求毫秒）。 */
function rangeWindow(key: RangeKey): { from: number; to: number } {
  const now = Date.now()
  if (key === 'today') {
    const start = new Date()
    start.setHours(0, 0, 0, 0)
    return { from: start.getTime(), to: now }
  }
  const ms = RANGES.find((r) => r.key === key)?.ms ?? 86400_000
  return { from: now - ms, to: now }
}

const fmtTime = (ms: number) => {
  if (!ms) return '—'
  const d = new Date(ms)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

const fmtNum = (n: number) => (n >= 10_000 ? `${(n / 1000).toFixed(1)}k` : String(n))

function StatusDot({ status }: { status: string }) {
  const ok = status === 'ok'
  return (
    <span
      className={cn(
        'inline-block w-1.5 h-1.5 rounded-full shrink-0',
        ok ? 'bg-ok' : 'bg-err',
      )}
      title={status || 'unknown'}
    />
  )
}

function Chip({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'h-7 px-2.5 rounded-lg text-[12px] whitespace-nowrap transition-all duration-150 select-none border',
        active
          ? 'bg-accent/15 text-accent font-medium border-accent/30'
          : 'text-inkdim hover:text-ink hover:bg-panel2 border-transparent',
      )}
    >
      {children}
    </button>
  )
}

const searchCls =
  'h-7 rounded-lg border border-edge bg-panel2/60 px-2.5 text-[12px] text-ink placeholder:text-inkfaint focus:outline-none focus:border-accent/50'

export function LlmLogsPanel() {
  const [range, setRange] = useState<RangeKey>('today')
  const [status, setStatus] = useState<'all' | 'ok' | 'error'>('all')
  const [keyword, setKeyword] = useState('')
  const [rows, setRows] = useState<AuditLlmCallItem[]>([])
  const [total, setTotal] = useState(0)
  const [snapshotMode, setSnapshotMode] = useState<string>('')
  const [retentionDays, setRetentionDays] = useState<number | undefined>(undefined)
  const [earliestDay, setEarliestDay] = useState<string>('')
  const [loading, setLoading] = useState(false)
  const [loadErr, setLoadErr] = useState('')
  const [selected, setSelected] = useState<AuditLlmCallItem | null>(null)
  const [detailTab, setDetailTab] = useState<DetailTab>('overview')
  const [payload, setPayload] = useState<{ text: string; hint: string }>({ text: '', hint: '' })
  const [toolRows, setToolRows] = useState<AuditToolEventItem[]>([])
  const [detailLoading, setDetailLoading] = useState(false)

  // 列表查询：条件变化即重新拉取（含 300ms 去抖，避免逐字符打服务端）
  useEffect(() => {
    let alive = true
    const timer = window.setTimeout(() => {
      setLoading(true)
      const win = rangeWindow(range)
      const kw = keyword.trim()
      void api
        .getAuditLlmCalls({
          from: win.from,
          to: win.to,
          status: status === 'all' ? undefined : status,
          keyword: kw || undefined,
          limit: 100,
        })
        .then((res) => {
          if (!alive) return
          setRows(res.items ?? [])
          setTotal(typeof res.total === 'number' ? res.total : 0)
          setSnapshotMode(res.snapshot_mode || '')
          setRetentionDays(typeof res.retention_days === 'number' ? res.retention_days : undefined)
          setEarliestDay(res.earliest_available_day || '')
          setLoadErr('')
        })
        .catch((e: unknown) => {
          if (!alive) return
          setRows([])
          setTotal(0)
          setLoadErr(e instanceof Error ? e.message : '对话日志查询失败')
        })
        .finally(() => {
          if (alive) setLoading(false)
        })
    }, 300)
    return () => {
      alive = false
      window.clearTimeout(timer)
    }
  }, [range, status, keyword])

  // 详情：报文正文（仅 full 档有）+ 该 run 的工具调用
  const loadDetail = useCallback((row: AuditLlmCallItem, tab: DetailTab) => {
    setDetailLoading(true)
    if (tab === 'payload') {
      void api
        .getAuditLlmCalls({ id: row.id, from: row.created_at, to: row.created_at + 1 })
        .then((res) => setPayload({ text: res.payload ?? '', hint: res.payload_hint ?? '' }))
        .catch((e: unknown) =>
          setPayload({ text: '', hint: e instanceof Error ? e.message : '正文读取失败' }),
        )
        .finally(() => setDetailLoading(false))
      return
    }
    if (tab === 'tools') {
      void api
        .getAuditToolEvents({ session_id: row.session_id, run_id: row.run_id, limit: 50 })
        .then((res) => setToolRows(res.items ?? []))
        .catch(() => setToolRows([]))
        .finally(() => setDetailLoading(false))
      return
    }
    setDetailLoading(false)
  }, [])

  const openRow = (row: AuditLlmCallItem) => {
    setSelected(row)
    setDetailTab('overview')
    setPayload({ text: '', hint: '' })
    setToolRows([])
  }

  const switchTab = (tab: DetailTab) => {
    setDetailTab(tab)
    if (selected) loadDetail(selected, tab)
  }

  const retentionText = useMemo(() => {
    if (retentionDays != null && earliestDay) {
      return `对话日志在线保留 ${retentionDays} 天（最早可查 ${earliestDay} ~ 今天），超期自动清理。`
    }
    if (retentionDays != null) return `对话日志在线保留 ${retentionDays} 天，超期自动清理。`
    return '保留策略未知（后端未返回 retention_days）'
  }, [retentionDays, earliestDay])

  const modeLabel =
    snapshotMode === 'full' ? '完整' : snapshotMode === 'preview' ? '预览' : snapshotMode === 'off' ? '关闭' : '未知'

  return (
    <div className="flex flex-col gap-3">
      <div>
        <div className="text-[13px] font-medium">对话日志</div>
        <div className="text-[12px] text-inkdim mt-0.5 leading-relaxed">
          每次大模型调用的结构化审计：模型、耗时、token 用量与报文快照。数据来自天分片审计库
          （<span className="font-mono">/api/v1/audit/llm-calls</span>），跨会话可查，与「日志查询」页签的 Agent 运维日志是两套数据源。
        </div>
      </div>

      <RealModeNotice text="本面板读取 logvault 天库的结构化审计副本。⚠️ 两个诚实边界：① 当前为「预览」档时只保存脱敏摘要与校验值，**不保存报文正文**，详情里的「报文」页签会说明原因而非显示空白；② 审计端点要求 admin 权限（日志含他人会话内容），无权限时请求会被拒绝。" />

      {/* 保留策略 + 当前档位 */}
      <div
        className={cn(
          'rounded-xl border px-3 py-2 text-[12px] leading-relaxed flex items-start gap-2',
          retentionDays != null ? 'border-edge bg-panel2/40 text-inkdim' : 'border-warn/40 bg-warn/[0.06] text-warn',
        )}
      >
        <span className="shrink-0">{retentionDays != null ? '保留策略' : '⚠️'}</span>
        <span className="min-w-0">
          {retentionText}
          <span className="ml-2 opacity-80">
            当前报文档位：<span className="font-mono">{modeLabel}</span>
            {snapshotMode === 'preview' && '（不落正文，详情「报文」页签不可用）'}
          </span>
        </span>
      </div>

      {/* 筛选 */}
      <div className="flex flex-col gap-2 rounded-xl border border-edge bg-panel2/25 px-3 py-2.5">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[12px] text-inkfaint shrink-0">时间</span>
          <div className="flex items-center gap-1">
            {RANGES.map((r) => (
              <Chip key={r.key} active={range === r.key} onClick={() => setRange(r.key)}>
                {r.label}
              </Chip>
            ))}
          </div>
        </div>
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[12px] text-inkfaint shrink-0">状态</span>
          <div className="flex items-center gap-1">
            <Chip active={status === 'all'} onClick={() => setStatus('all')}>全部</Chip>
            <Chip active={status === 'ok'} onClick={() => setStatus('ok')}>成功</Chip>
            <Chip active={status === 'error'} onClick={() => setStatus('error')}>失败</Chip>
          </div>
          <input
            className={`${searchCls} sm:ml-auto`}
            placeholder="搜索报文摘要 / 模型…"
            value={keyword}
            onChange={(e) => setKeyword(e.target.value)}
          />
        </div>
      </div>

      {/* 列表 */}
      <div className="rounded-xl border border-edge bg-panel2/20 overflow-hidden">
        <div className="flex items-center gap-2 px-3 py-2 border-b border-edge/70 text-[11px] text-inkfaint">
          <span>命中 {total} 条</span>
          {loading && <span>· 加载中…</span>}
          {loadErr && <span className="text-err">· {loadErr}</span>}
        </div>
        {rows.length === 0 && !loading && !loadErr && (
          <div className="px-3 py-6 text-center text-[12px] text-inkfaint">
            该时段无调用记录（或日志保留期已过）
          </div>
        )}
        <div className="max-h-64 overflow-y-auto">
          {rows.map((r) => (
            <button
              key={r.id}
              type="button"
              onClick={() => openRow(r)}
              className={cn(
                'w-full text-left px-3 py-2 border-b border-edge/40 last:border-b-0 transition-colors',
                selected?.id === r.id ? 'bg-accent/10' : 'hover:bg-panel2/60',
              )}
            >
              <div className="flex items-center gap-2 text-[12px]">
                <StatusDot status={r.status} />
                <span className="font-mono text-inkfaint shrink-0">{fmtTime(r.created_at)}</span>
                <span className="font-medium truncate">{r.model || '(未记录模型)'}</span>
                <span className="text-inkfaint shrink-0">{r.duration_ms}ms</span>
                <span className="text-inkfaint shrink-0 ml-auto">
                  {fmtNum(r.prompt_tokens)}↑ {fmtNum(r.completion_tokens)}↓
                </span>
              </div>
              <div className="text-[11px] text-inkfaint truncate mt-0.5 font-mono">
                {r.session_id || '—'} / {r.run_id || '—'}
                {r.error_message ? ` · ${r.error_message}` : ''}
              </div>
            </button>
          ))}
        </div>
      </div>

      {/* 详情 */}
      {selected && (
        <div className="rounded-xl border border-edge bg-panel2/20 overflow-hidden">
          <div className="flex items-center gap-1 px-2 py-1.5 border-b border-edge/70 flex-wrap">
            {(
              [
                { key: 'overview', label: '概览' },
                { key: 'payload', label: '报文' },
                { key: 'tools', label: '工具' },
                { key: 'raw', label: '原始' },
              ] as Array<{ key: DetailTab; label: string }>
            ).map((t) => (
              <Chip key={t.key} active={detailTab === t.key} onClick={() => switchTab(t.key)}>
                {t.label}
              </Chip>
            ))}
            <span className="text-[11px] text-inkfaint ml-auto font-mono truncate">{selected.id}</span>
          </div>

          <div className="px-3 py-2.5 text-[12px]">
            {detailLoading && <div className="text-inkfaint">加载中…</div>}

            {!detailLoading && detailTab === 'overview' && (
              <div className="grid grid-cols-2 sm:grid-cols-3 gap-x-4 gap-y-1.5">
                {[
                  ['状态', selected.status],
                  ['模型', selected.model || '—'],
                  ['厂商', selected.provider || '—'],
                  ['端点', selected.endpoint || '—'],
                  ['提示 token', String(selected.prompt_tokens)],
                  ['补全 token', String(selected.completion_tokens)],
                  ['缓存 token', String(selected.cached_tokens)],
                  ['首 token 延迟', `${selected.ttft_ms}ms`],
                  ['总耗时', `${selected.duration_ms}ms`],
                  ['吞吐', `${selected.tokens_per_sec.toFixed(1)} tok/s`],
                  ['会话', selected.session_id || '—'],
                  ['Run', selected.run_id || '—'],
                  ['Trace', selected.trace_id || '—'],
                  ['报文大小', `${selected.payload_bytes} B`],
                  ['正文可取', selected.payload_available ? '是' : '否'],
                ].map(([k, v]) => (
                  <div key={k} className="min-w-0">
                    <span className="text-inkfaint">{k}：</span>
                    <span className="font-mono break-all">{v}</span>
                  </div>
                ))}
                {selected.error_message && (
                  <div className="col-span-full text-err">
                    错误：{selected.error_code ? `[${selected.error_code}] ` : ''}
                    {selected.error_message}
                  </div>
                )}
              </div>
            )}

            {!detailLoading && detailTab === 'payload' && (
              <div className="flex flex-col gap-2">
                {payload.hint && (
                  <div className="rounded-lg border border-warn/40 bg-warn/[0.06] text-warn px-2.5 py-2 text-[11px] leading-relaxed">
                    {payload.hint}
                  </div>
                )}
                {payload.text ? (
                  <pre className="max-h-64 overflow-auto rounded-lg bg-panel2/60 p-2 text-[11px] font-mono whitespace-pre-wrap break-all">
                    {payload.text}
                  </pre>
                ) : (
                  !payload.hint && (
                    <div className="text-inkfaint">
                      无正文可显示（当前档位不落正文，或该记录正文已被保留期清理）。
                    </div>
                  )
                )}
              </div>
            )}

            {!detailLoading && detailTab === 'tools' && (
              <div className="flex flex-col gap-1.5">
                {toolRows.length === 0 && <div className="text-inkfaint">该 Run 无工具调用记录</div>}
                {toolRows.map((t) => (
                  <div key={t.id} className="rounded-lg border border-edge/60 px-2.5 py-1.5">
                    <div className="flex items-center gap-2">
                      <StatusDot status={t.status} />
                      <span className="font-medium">{t.tool_name}</span>
                      <span className="text-inkfaint text-[11px]">{t.namespace}</span>
                      <span className="text-inkfaint text-[11px] ml-auto">{t.duration_ms}ms</span>
                    </div>
                    {t.params_preview && (
                      <div className="text-[11px] text-inkdim font-mono truncate mt-0.5">{t.params_preview}</div>
                    )}
                    {t.error && <div className="text-[11px] text-err mt-0.5">{t.error}</div>}
                  </div>
                ))}
              </div>
            )}

            {!detailLoading && detailTab === 'raw' && (
              <pre className="max-h-64 overflow-auto rounded-lg bg-panel2/60 p-2 text-[11px] font-mono whitespace-pre-wrap break-all">
                {JSON.stringify(selected, null, 2)}
              </pre>
            )}
          </div>
        </div>
      )}
    </div>
  )
}
