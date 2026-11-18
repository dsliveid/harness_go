import { useEffect, useState } from 'react'
import { api } from '../../lib/api'
import type { PlanMode, SOPCatalog, SOPCore, SOPItem } from '../../lib/apiTypes'
import { Badge, Button, Switch } from '../../components/ui'
import { cn } from '../../components/ui'
import { ShieldCheck, RotateCcw, Copy, Check, Layers } from 'lucide-react'

type Mode = 'builtin' | 'custom'

/**
 * 工程作业程序目录：按层级分组渲染 + 逐条启停 + 规划模式切换。
 *
 * 为什么单独抽组件：这一段的真源与上方「核心 SOP 编辑器」完全不同
 * （代码真源 vs 用户正文），共用 state 只会让两处互相污染。
 */
function SopCatalogSection({
  catalog,
  msg,
  saving,
  planModes,
  onToggle,
  onEnableAll,
  onChangePlanMode,
}: {
  catalog: SOPCatalog | null
  msg: { tone: 'ok' | 'err'; text: string } | null
  saving: boolean
  planModes: Array<{ id: PlanMode; label: string; hint: string }>
  onToggle: (item: SOPItem) => void
  onEnableAll: () => void
  onChangePlanMode: (next: PlanMode) => void
}) {
  if (!catalog) {
    return (
      <div className="pt-2 border-t border-edge/60">
        <div className="text-[12px] text-inkdim">正在读取工程规范目录…</div>
      </div>
    )
  }

  const items = catalog.items
  const hardCount = items.filter((i) => i.is_hard_rule).length
  const enabledCount = items.length - catalog.disabled.length
  // 按层级分组（服务端已按出现顺序返回，这里只做视觉分组，不重排语义）
  const phases = Array.from(new Set(items.map((i) => i.phase))).sort((a, b) => a - b)

  return (
    <div className="flex flex-col gap-3 pt-3 border-t border-edge/60">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="text-[13px] font-medium flex items-center gap-2">
            <Layers size={15} className="text-accent" />
            工程作业程序（原生 SOP）
          </div>
          <div className="text-[12px] text-inkdim mt-1 leading-relaxed max-w-[560px]">
            这部分由服务端代码定义（随版本升级），界面只决定「启用 / 关闭」。关闭后模型在该维度解除硬性约束，
            <span className="text-warn">下一轮对话立即生效</span>。
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <Badge tone={catalog.disabled.length > 0 ? 'yellow' : 'green'}>
            已启用 {enabledCount} / {items.length}
          </Badge>
          {catalog.disabled.length > 0 && (
            <Button size="sm" variant="ghost" onClick={onEnableAll} disabled={saving} className="text-accent">
              全部启用
            </Button>
          )}
        </div>
      </div>

      {/* 规划模式：决定「方案先行」规则渲染哪个变体（会影响模型是否停下来等确认） */}
      <div className="flex flex-col gap-2">
        <div className="text-[12px] font-semibold text-accent px-0.5">规划模式</div>
        <div className="grid grid-cols-1 sm:grid-cols-3 gap-2">
          {planModes.map((m) => {
            const active = catalog.plan_mode === m.id
            return (
              <button
                key={m.id}
                onClick={() => onChangePlanMode(m.id)}
                disabled={saving}
                className={cn(
                  'text-left p-2.5 rounded-xl border transition-colors',
                  active ? 'border-accent/60 bg-accent/10' : 'border-edge bg-panel2/30 hover:border-edge/80',
                )}
              >
                <div className={cn('text-[12px] font-medium', active ? 'text-accent' : '')}>{m.label}</div>
                <div className="text-[11px] text-inkfaint mt-0.5 leading-relaxed">{m.hint}</div>
              </button>
            )
          })}
        </div>
      </div>

      {msg && (
        <div className={cn('text-[11px]', msg.tone === 'ok' ? 'text-ok' : 'text-danger')}>{msg.text}</div>
      )}

      {hardCount > 0 && (
        <div className="text-[11px] text-inkfaint">
          其中 {hardCount} 条为硬性安全规范（开关置灰），不可关闭。
        </div>
      )}

      {phases.map((phase) => {
        const group = items.filter((i) => i.phase === phase)
        return (
          <div key={phase} className="flex flex-col gap-2">
            <div className="flex items-center gap-2 text-[12px] font-semibold text-accent px-0.5">
              <span className="w-2 h-2 rounded-full bg-accent" />
              <span>{group[0].phase_label}</span>
            </div>
            {group.map((item) => {
              const enabled = item.is_hard_rule || !catalog.disabled.includes(item.id)
              return (
                <div
                  key={item.id}
                  className={cn(
                    'border rounded-xl p-3 flex items-start justify-between gap-3 transition-colors',
                    enabled ? 'border-edge bg-panel2/30' : 'border-edge/50 opacity-75',
                  )}
                >
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-2 flex-wrap">
                      <code className="text-[13px] font-mono font-medium bg-panel2 px-1.5 py-0.5 rounded border border-edge/60">
                        {item.id}
                      </code>
                      <span className="text-[12px] font-medium">{item.name}</span>
                      <span
                        className={cn(
                          'text-[11px] px-1.5 py-0.5 rounded-md border font-medium',
                          CATEGORY_CLS[item.category] ?? 'text-inkdim bg-panel2 border-edge/60',
                        )}
                      >
                        {item.category_label}
                      </span>
                      <span className="text-[11px] px-1.5 py-0.5 rounded-md border border-edge/60 text-inkfaint">
                        {item.scope_label}
                      </span>
                    </div>
                    <div className="text-[12px] text-inkdim mt-1.5 leading-relaxed">{item.description}</div>
                    <div className="text-[11px] text-inkfaint mt-1.5 flex items-start gap-1">
                      <span>↳</span>
                      <span>{item.disable_effect}</span>
                    </div>
                    {/* 变体对比：同一规则在主 / 子 / 各模式下措辞不同，界面如实展示 */}
                    {item.variants.length > 1 && (
                      <div className="mt-1.5 flex items-center gap-1 flex-wrap">
                        {item.variants.map((v) => (
                          <span
                            key={v.label}
                            className="text-[10px] px-1.5 py-0.5 rounded border border-edge/60 text-inkfaint"
                          >
                            {v.label}
                          </span>
                        ))}
                      </div>
                    )}
                  </div>
                  <div className="flex items-center gap-2 shrink-0 pt-0.5">
                    <span
                      className={cn(
                        'text-[12px] select-none whitespace-nowrap',
                        enabled ? 'text-ok font-medium' : 'text-inkdim',
                      )}
                    >
                      {item.is_hard_rule ? '常驻' : enabled ? '启用' : '禁用'}
                    </span>
                    <Switch
                      checked={enabled}
                      onChange={() => onToggle(item)}
                      disabled={item.is_hard_rule || saving}
                      title={item.is_hard_rule ? `${item.name}（硬性规范，不可关闭）` : `${enabled ? '禁用' : '启用'} ${item.name}`}
                    />
                  </div>
                </div>
              )
            })}
          </div>
        )
      })}
    </div>
  )
}

const CATEGORY_CLS: Record<string, string> = {
  thinking: 'text-cyan-400 bg-cyan-500/12 border-cyan-500/30',
  workflow: 'text-warn bg-warn/12 border-warn/30',
  orchestration: 'text-accent2 bg-accent2/12 border-accent2/30',
  quality: 'text-accent bg-accent/12 border-accent/30',
  memory: 'text-ok bg-ok/12 border-ok/30',
  safety: 'text-danger bg-danger/12 border-danger/30',
}

/** 规划模式选项（决定「方案先行」规则渲染哪个变体）。 */
const PLAN_MODES: Array<{ id: PlanMode; label: string; hint: string }> = [
  { id: 'standard', label: '标准模式', hint: '大改动才落计划，不做强门禁' },
  { id: 'always_plan', label: 'Always Plan', hint: '生成计划后必须停下等确认' },
  { id: 'always_proceed', label: 'Always Proceed', hint: '生成计划后自动推进施工' },
]

/**
 * Agent SOP 设置面板。
 *
 * 两部分（真源不同，绝不互相覆盖）：
 *   ① 核心 SOP（agent-core）：用户可编辑正文，落 data/sop/agent-core.md，受 token 预算约束；
 *   ② 工程作业程序（/sop/catalog）：**代码即真源**，界面只渲染与逐条启停 + 切规划模式。
 *
 * 红线：切到「系统内置」页签**绝不删除**自定义文件 —— 否则手滑一次就丢失调教成果。
 */
export function SopPanel() {
  const [core, setCore] = useState<SOPCore | null>(null)
  const [mode, setMode] = useState<Mode>('builtin')
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<{ tone: 'ok' | 'err'; text: string } | null>(null)
  const [copied, setCopied] = useState(false)

  // 工程作业程序目录（服务端单一真源；前端不硬编码任何规则文案）
  const [catalog, setCatalog] = useState<SOPCatalog | null>(null)
  const [catalogMsg, setCatalogMsg] = useState<{ tone: 'ok' | 'err'; text: string } | null>(null)
  const [savingCatalog, setSavingCatalog] = useState(false)

  const load = async () => {
    try {
      const res = await api.getSOPCore()
      setCore(res)
      setMode(res.overridden ? 'custom' : 'builtin')
      setDraft(res.overridden ? res.content : res.builtin_content)
    } catch (e) {
      setMsg({ tone: 'err', text: e instanceof Error ? e.message : '读取核心 SOP 失败' })
    }
  }

  const loadCatalog = async () => {
    try {
      setCatalog(await api.getSOPCatalog())
    } catch (e) {
      setCatalogMsg({ tone: 'err', text: e instanceof Error ? e.message : '读取工程规范目录失败' })
    }
  }

  useEffect(() => {
    void load()
    void loadCatalog()
  }, [])

  if (!core) {
    return (
      <div className="flex flex-col gap-4">
        <div className="text-[12px] text-inkdim">正在读取核心 SOP…</div>
      </div>
    )
  }

  const estTokens = (s: string) => {
    let cjk = 0
    let other = 0
    for (const ch of s) {
      const c = ch.codePointAt(0) ?? 0
      if ((c > 0x2e80 && c <= 0x9fff) || (c >= 0xff00 && c <= 0xffef)) cjk++
      else if (!/\s/.test(ch)) other++
    }
    return cjk + Math.ceil(other / 4)
  }

  const tokens = estTokens(draft)
  const overBudget = core.budget > 0 && tokens > core.budget

  const switchMode = (next: Mode) => {
    setMsg(null)
    setMode(next)
    // 切页签只切换**视图**，不写不删 —— 自定义内容原封不动留在服务端。
    setDraft(next === 'custom' ? core.content : core.builtin_content)
  }

  const save = async () => {
    setBusy(true)
    setMsg(null)
    try {
      const res = await api.putSOPCore(draft)
      setCore(res)
      setMode('custom')
      setMsg({ tone: 'ok', text: '已保存，下一轮对话立即生效（无需重启）' })
    } catch (e) {
      setMsg({ tone: 'err', text: e instanceof Error ? e.message : '保存失败' })
    } finally {
      setBusy(false)
    }
  }

  const reset = async () => {
    if (!window.confirm('将删除你的自定义内容，恢复为出厂内置。此操作不可撤销，确认继续？')) return
    setBusy(true)
    setMsg(null)
    try {
      const res = await api.resetSOPCore()
      setCore(res)
      setMode('builtin')
      setDraft(res.builtin_content)
      setMsg({ tone: 'ok', text: '已重置为出厂内置' })
    } catch (e) {
      setMsg({ tone: 'err', text: e instanceof Error ? e.message : '重置失败' })
    } finally {
      setBusy(false)
    }
  }

  const copyAll = async () => {
    try {
      await navigator.clipboard.writeText(mode === 'custom' ? draft : core.builtin_content)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      setMsg({ tone: 'err', text: '复制失败，请手动选择文本复制' })
    }
  }

  // 保存启停 + 规划模式：整单提交（服务端先校验后落库，绝不半生效）。
  const saveCatalog = async (nextDisabled: string[], nextMode: PlanMode) => {
    setSavingCatalog(true)
    setCatalogMsg(null)
    try {
      const res = await api.putSOPCatalog({ disabled: nextDisabled, plan_mode: nextMode })
      setCatalog(res)
      setCatalogMsg({ tone: 'ok', text: '已保存，下一轮对话立即生效（无需重启）' })
    } catch (e) {
      setCatalogMsg({ tone: 'err', text: e instanceof Error ? e.message : '保存失败' })
      // 失败即回读真实状态，避免界面停留在「假已保存」的乐观态。
      void loadCatalog()
    } finally {
      setSavingCatalog(false)
    }
  }

  const toggleItem = (item: SOPItem) => {
    if (!catalog || item.is_hard_rule) return
    const off = catalog.disabled.includes(item.id)
    const nextDisabled = off
      ? catalog.disabled.filter((id) => id !== item.id)
      : [...catalog.disabled, item.id]
    void saveCatalog(nextDisabled, catalog.plan_mode)
  }

  const enableAll = () => {
    if (!catalog) return
    void saveCatalog([], catalog.plan_mode)
  }

  return (
    <div className="flex flex-col gap-4">

      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="text-[13px] font-medium flex items-center gap-2">
            <ShieldCheck size={15} className="text-accent" />
            Agent 核心 SOP（agent-core）
          </div>
          <div className="text-[12px] text-inkdim mt-1 leading-relaxed max-w-[560px]">
            这是 Agent 的基础行为规范，直接作为系统提示注入每轮对话。调教满意后可用「复制全文」贴回源码固化，不满意可一键重置出厂。
          </div>
        </div>
        <Badge tone={mode === 'custom' ? 'yellow' : 'green'}>
          {mode === 'custom' ? '自定义' : '系统内置'}
        </Badge>
      </div>

      {/* 模式切换 */}
      <div className="flex items-center gap-1.5 p-1 rounded-xl bg-panel2/60 border border-edge/80 w-fit">
        {(['builtin', 'custom'] as Mode[]).map((m) => (
          <button
            key={m}
            onClick={() => switchMode(m)}
            className={cn(
              'px-3 py-1.5 rounded-lg text-[12px] transition-colors',
              mode === m ? 'bg-accent/15 text-accent font-medium' : 'text-inkdim hover:text-ink',
            )}
          >
            {m === 'builtin' ? '系统内置' : '自定义'}
          </button>
        ))}
      </div>

      {/* 编辑器 */}
      <div className="flex flex-col gap-2">
        <div className="flex items-center justify-between text-[11px] text-inkfaint">
          <span>
            {mode === 'builtin' ? '出厂内置（只读）' : '自定义内容（可编辑）'}
            {core.overridden && mode === 'builtin' ? ' · 当前生效的是自定义版本' : ''}
          </span>
          <span className={cn(overBudget ? 'text-danger font-medium' : '')}>
            ≈{tokens} / {core.budget > 0 ? core.budget : '不限'} token
          </span>
        </div>
        <textarea
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          readOnly={mode === 'builtin'}
          spellCheck={false}
          className={cn(
            'w-full h-[320px] p-3 rounded-xl border bg-panel2/40 font-mono text-[12px] leading-relaxed resize-y',
            'focus:outline-none focus:border-accent/50',
            mode === 'builtin' ? 'border-edge/60 text-inkdim cursor-default' : 'border-edge/80',
            overBudget && mode === 'custom' ? 'border-danger/60' : '',
          )}
        />
        {overBudget && mode === 'custom' && (
          <div className="text-[11px] text-danger">
            超出总预算，保存会被服务端拒绝（不会截断）。请精炼内容，或调大 config.sop.core_max_tokens。
          </div>
        )}
      </div>

      {msg && (
        <div className={cn('text-[11px]', msg.tone === 'ok' ? 'text-ok' : 'text-danger')}>{msg.text}</div>
      )}

      {/* 操作区 */}
      <div className="flex items-center gap-2 flex-wrap">
        {mode === 'custom' && (
          <Button size="sm" onClick={save} disabled={busy || overBudget || !draft.trim()}>
            {busy ? '保存中…' : '保存'}
          </Button>
        )}
        {mode === 'builtin' && (
          <Button size="sm" onClick={() => switchMode('custom')}>
            切换为自定义编辑
          </Button>
        )}
        <Button size="sm" variant="ghost" onClick={copyAll} className="gap-1.5">
          {copied ? <Check size={13} /> : <Copy size={13} />}
          {copied ? '已复制' : '复制全文'}
        </Button>
        {core.overridden && (
          <Button size="sm" variant="ghost" onClick={reset} disabled={busy} className="text-danger gap-1.5">
            <RotateCcw size={13} />
            重置为内置
          </Button>
        )}
      </div>

      <SopCatalogSection
        catalog={catalog}
        msg={catalogMsg}
        saving={savingCatalog}
        planModes={PLAN_MODES}
        onToggle={toggleItem}
        onEnableAll={enableAll}
        onChangePlanMode={(next) => {
          if (!catalog || catalog.plan_mode === next) return
          void saveCatalog(catalog.disabled, next)
        }}
      />
    </div>
  )
}
