import { useEffect, useState } from 'react'
import { api } from '../../lib/api'
import type { SOPCore } from '../../lib/apiTypes'
import { Badge, Button } from '../../components/ui'
import { cn } from '../../components/ui'
import { ShieldCheck, RotateCcw, Copy, Check } from 'lucide-react'

type Mode = 'builtin' | 'custom'

/**
 * Agent 核心 SOP 编辑器（agent-core）。
 *
 * 三态语义（由「data/sop/agent-core.md 文件是否存在」单一裁决，不存独立模式字段）：
 *   系统内置：文件不存在 → 用出厂 embed 内容，**只读预览**；
 *   自定义　：文件存在　 → 用文件内容，可编辑保存；
 *   重置　　：删除文件　 → 回落出厂（需二次确认）。
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

  useEffect(() => {
    void load()
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
    </div>
  )
}
