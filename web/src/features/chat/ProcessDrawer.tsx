import { useState } from 'react'
import { ChevronDown, ChevronRight, Cpu, Clock, Zap } from 'lucide-react'
import type { Message } from '../../types'
import { MarkdownLite } from '../../components/MarkdownLite'
import { ToolCard } from '../../components/ToolCard'

export function formatStepMs(ms?: number): string {
  if (!ms || ms <= 0) return ''
  if (ms < 60_000) return ms < 1000 ? `${Math.round(ms)}ms` : `${(ms / 1000).toFixed(1)}s`
  const mins = Math.floor(ms / 60_000)
  const secs = Math.floor((ms % 60_000) / 1000)
  return secs > 0 ? `${mins}分${secs}秒` : `${mins}分`
}

export function formatStepTokens(n?: number): string {
  if (!n || n <= 0) return ''
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M tok`
  if (n >= 10_000) return `${(n / 1000).toFixed(1)}k tok`
  return `${n} tok`
}

/**
 * 执行过程抽屉（参照桌面端双轨容器模型）：
 * - 过程轨：本轮所有中间步骤（工具调用 / 思考）收拢于此抽屉
 * - 交付轨：最终答复独立于抽屉之外（由 MessageList 渲染）
 * - 纯实体背景呈现，移动端无任何模糊遮罩与半透发虚
 */
export function ProcessDrawer({ steps, running, reasoning }: {
  steps: Message[]
  running: boolean
  reasoning?: string
}) {
  const stepCount = steps.length
  // 运行中默认展开；结束后自动收起；用户手动切换后以用户为准
  const [userOpen, setUserOpen] = useState<boolean | null>(null)
  const open = userOpen ?? running

  if (stepCount === 0) return null

  const totalMs = steps.reduce((a, s) => a + (s.durationMs ?? 0), 0)
  const totalTok = steps.reduce((a, s) => a + (s.tokens ?? 0), 0)

  return (
    <div className="rounded-xl border border-edge bg-panel overflow-hidden transition-all duration-150">
      <button
        type="button"
        className="w-full flex items-center gap-2.5 px-3.5 py-2.5 text-left bg-panel2/60 hover:bg-panel2 transition-colors select-none"
        onClick={() => setUserOpen(!open)}
      >
        <span className="text-inkfaint w-4 h-4 flex items-center justify-center shrink-0">
          {open ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
        </span>
        <div className="flex items-center gap-1.5 shrink-0">
          <Cpu size={13} className="text-accent" />
          <span className="text-[13px] text-inkdim font-medium">执行过程</span>
          <span className="text-[12px] font-semibold text-ink font-mono">({stepCount} 个步骤)</span>
        </div>

        <span className="flex-1" />

        {running ? (
          <span className="flex items-center gap-1.5 text-[12px] text-accent font-medium shrink-0">
            <span className="relative flex h-1.5 w-1.5 shrink-0">
              <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-accent opacity-75" />
              <span className="relative inline-flex rounded-full h-1.5 w-1.5 bg-accent" />
            </span>
            <span>正在执行第 {stepCount} 步…</span>
          </span>
        ) : (
          <div className="flex items-center gap-2.5 text-[11px] text-inkfaint font-mono shrink-0">
            {totalTok > 0 && (
              <span className="flex items-center gap-1">
                <Zap size={11} className="text-amber-400" />
                <span>{formatStepTokens(totalTok)}</span>
              </span>
            )}
            {totalMs > 0 && (
              <span className="flex items-center gap-1">
                <Clock size={11} />
                <span>{formatStepMs(totalMs)}</span>
              </span>
            )}
          </div>
        )}
      </button>

      {open && (
        <div className="border-t border-edge px-3.5 py-3 space-y-3 bg-panel">
          {running && reasoning && (
            <div className="text-[13px] text-inkfaint border-l-2 border-accent pl-3 py-1 bg-panel2 rounded-r-lg">
              <span className="text-[10px] uppercase font-mono tracking-wider text-accent font-semibold">思考中</span>
              <div className="mt-1 leading-relaxed text-inkdim">{reasoning}</div>
            </div>
          )}

          {steps.map((s, i) => (
            <div key={s.id} className="space-y-1.5">
              <div className="flex items-center gap-2.5 text-[11px] text-inkfaint select-none">
                <span className="font-mono font-medium text-inkdim">步骤 {i + 1}/{stepCount}</span>
                {s.durationMs !== undefined && s.durationMs > 0 && (
                  <span className="font-mono">{formatStepMs(s.durationMs)}</span>
                )}
                {s.tokens !== undefined && s.tokens > 0 && (
                  <span className="font-mono">{formatStepTokens(s.tokens)}</span>
                )}
              </div>

              {s.toolCalls && s.toolCalls.length > 0 && (
                <div className="space-y-1.5">
                  {s.toolCalls.map((c) => <ToolCard key={c.id} call={c} />)}
                </div>
              )}

              {s.content && (
                <div className="text-[13px] leading-relaxed text-ink bg-panel2 p-3 rounded-xl">
                  <MarkdownLite text={s.content} />
                  {s.streaming && <span className="inline-block w-1.5 h-3.5 bg-accent align-text-bottom ml-1 animate-pulse" />}
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
