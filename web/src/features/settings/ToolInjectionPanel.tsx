import { useCallback, useEffect, useState } from 'react'
import { Layers, RefreshCw, ChevronDown, ChevronRight } from 'lucide-react'
import { api } from '../../lib/api'
import { useUI } from '../../stores/uiStore'
import { Badge, Button, cn } from '../../components/ui'
import type { ToolInjection, ToolSpecBrief } from '../../lib/apiTypes'

const fmtNum = (n: number) => n.toLocaleString('en-US')

/**
 * 工具注入明细（A-10）：展示「本轮实际注入给模型的内容与 token 预算」。
 *
 * 契约依据 `internal/tools/loader/loader.go:34` 的 `Injection` 结构体（经
 * `GET /api/v1/tools/injection` → `tools_handler.go:35` 原样返回）：
 *   core[]（toolapi.Spec 全量）/ deferred_index[]（名称+一行描述）/ on_demand[]（零注入）
 *   + deferred_hidden[] + cost_core / cost_index / cost_total + limits + warnings[]
 *
 * 与 `/tools` 快照的差别：快照是「注册了什么」，本视图是「本轮真正塞进 prompt 的」，
 * 因此 core/on_demand 展示 toolapi.Spec 的真实字段（description / executor / risk / cost_tokens）。
 * 任何字段都直接来自响应，不做前端估算；读取失败原样透传。
 */
export function ToolInjectionPanel() {
  const toast = useUI((s) => s.toast)
  const [inj, setInj] = useState<ToolInjection | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  const load = useCallback(
    async (silent = false) => {
      if (!silent) setLoading(true)
      try {
        const v = await api.getToolsInjection()
        setInj(v)
        setError(null)
      } catch (e: unknown) {
        // 不求兜底成假值：失败就如实展示错误
        const msg = e instanceof Error ? e.message : String(e)
        setInj(null)
        setError(msg)
        if (!silent) toast('error', `读取工具注入明细失败：${msg}`)
      } finally {
        if (!silent) setLoading(false)
      }
    },
    [toast],
  )

  useEffect(() => {
    void load()
  }, [load])

  const budgetCore = inj ? inj.limits.core_soft : 0
  const budgetDeferred = inj ? inj.limits.deferred_soft : 0

  return (
    <div className="rounded-xl border border-edge bg-panel2/30 p-3 space-y-2.5">
      <div className="flex items-center gap-2 flex-wrap">
        <Layers size={13} className="text-inkdim shrink-0" />
        <span className="text-[12px] font-semibold">本轮工具注入明细</span>
        <Badge tone="blue">服务端实时</Badge>
        {loading && <span className="text-[11px] text-inkfaint">读取中…</span>}
        <Button variant="ghost" size="sm" className="ml-auto" onClick={() => void load()} disabled={loading}>
          <RefreshCw size={11} className={cn('mr-1', loading && 'animate-spin')} />
          刷新
        </Button>
      </div>

      {error && <div className="text-[11px] text-err font-mono break-all">读取失败：{error}</div>}

      {!error && !inj && !loading && (
        <div className="text-[11px] text-inkfaint">服务端未返回注入视图。</div>
      )}

      {inj && (
        <>
          <div className="grid grid-cols-2 sm:grid-cols-4 gap-2">
            <Stat label="core 注入成本" value={fmtNum(inj.cost_core)} sub={`软上限 ${fmtNum(budgetCore)}`} />
            <Stat label="deferred 索引成本" value={fmtNum(inj.cost_index)} sub={`软上限 ${fmtNum(budgetDeferred)}`} />
            <Stat label="注入总成本" value={fmtNum(inj.cost_total)} sub="token" />
            <Stat
              label="条目数 core / 索引 / 按需"
              value={`${inj.core.length} / ${inj.deferred_index.length} / ${inj.on_demand.length}`}
              sub={inj.deferred_hidden?.length ? `${inj.deferred_hidden.length} 个因超限未入索引` : '无隐藏项'}
            />
          </div>

          {inj.warnings && inj.warnings.length > 0 && (
            <div className="rounded-lg border border-warn/40 bg-warn/[0.06] px-2 py-1.5 text-[11px] text-warn space-y-0.5">
              {inj.warnings.map((w, i) => <div key={i}>· {w}</div>)}
            </div>
          )}

          <SpecSection title="core · 全量注入" hint="每轮请求都带完整 schema" list={inj.core} tone="green" />

          <div>
            <div className="flex items-center gap-2">
              <span className="text-[11px] font-medium">deferred · 仅注入索引</span>
              <Badge tone={inj.deferred_index.length ? 'green' : 'gray'}>{inj.deferred_index.length} 条</Badge>
              <span className="text-[10px] text-inkfaint">只给「名称 + 一行描述」，模型按需回捞定义</span>
            </div>
            {inj.deferred_index.length === 0 ? (
              <div className="text-[11px] text-inkfaint pl-1">（本轮未注入 deferred 索引）</div>
            ) : (
              <div className="mt-1 space-y-0.5">
                {inj.deferred_index.map((d) => (
                  <div key={d.name} className="flex items-baseline gap-2 text-[10.5px]">
                    <span className="font-mono text-inkdim shrink-0">{d.name}</span>
                    <span className="text-inkfaint truncate flex-1">{d.description || '（无描述）'}</span>
                    <span className="font-mono text-inkfaint shrink-0">{d.cost_tokens} tk</span>
                  </div>
                ))}
              </div>
            )}
            {inj.deferred_hidden && inj.deferred_hidden.length > 0 && (
              <div className="mt-1 text-[10.5px] text-warn">
                超软上限未入索引：{inj.deferred_hidden.join(', ')}
              </div>
            )}
          </div>

          <SpecSection title="on_demand · 零注入" hint="不进 prompt，仅可被检索到" list={inj.on_demand} tone="gray" />
        </>
      )}
    </div>
  )
}

function Stat({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="rounded-lg border border-edge px-2 py-1.5">
      <div className="text-[10px] text-inkfaint">{label}</div>
      <div className="text-[14px] font-medium">{value}</div>
      {sub && <div className="text-[10px] text-inkfaint font-mono">{sub}</div>}
    </div>
  )
}

/** core / on_demand 区块：渲染 toolapi.Spec 的真实字段，不臆造。 */
function SpecSection({
  title,
  hint,
  list,
  tone,
}: {
  title: string
  hint: string
  list: ToolSpecBrief[]
  tone: 'green' | 'gray'
}) {
  const [open, setOpen] = useState(false)
  return (
    <div>
      <button
        type="button"
        className="flex items-center gap-2 w-full text-left"
        onClick={() => setOpen((v) => !v)}
        disabled={list.length === 0}
      >
        {open && list.length > 0 ? <ChevronDown size={12} className="shrink-0" /> : <ChevronRight size={12} className="shrink-0" />}
        <span className="text-[11px] font-medium">{title}</span>
        <Badge tone={list.length ? tone : 'gray'}>{list.length} 个</Badge>
        <span className="text-[10px] text-inkfaint">{hint}</span>
      </button>
      {open && list.length > 0 && (
        <div className="mt-1 space-y-1 pl-5">
          {list.map((t) => (
            <div key={t.name} className="text-[10.5px] leading-relaxed">
              <div className="flex items-baseline gap-1.5 flex-wrap">
                <span className="font-mono text-ink">{t.name}</span>
                <span className="text-inkfaint">·{t.namespace ?? 'builtin'}</span>
                <Badge tone={t.risk === 'readonly' ? 'green' : t.risk === 'danger' ? 'red' : 'yellow'}>
                  {t.risk}
                </Badge>
                <span className="text-inkfaint">{t.executor}</span>
                <span className="font-mono text-inkfaint">{t.cost_tokens} tk</span>
                {t.version && <span className="font-mono text-inkfaint">v{t.version}</span>}
              </div>
              {t.description && <div className="text-inkfaint line-clamp-2">{t.description}</div>}
              {t.requires && t.requires.length > 0 && (
                <div className="text-[10px] text-inkfaint">依赖：{t.requires.join(', ')}</div>
              )}
              {t.capabilities && t.capabilities.length > 0 && (
                <div className="text-[10px] text-inkfaint">能力：{t.capabilities.join(', ')}</div>
              )}
            </div>
          ))}
        </div>
      )}
      {list.length === 0 && <div className="text-[11px] text-inkfaint pl-5">（本轮该类为空）</div>}
    </div>
  )
}
