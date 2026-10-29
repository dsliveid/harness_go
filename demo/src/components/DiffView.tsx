import { useState } from 'react'
import { FileCode, RotateCcw } from 'lucide-react'
import type { DiffHunk, FileDiff } from '../types'
import { Button, cn } from './ui'

export function DiffView({
  diff, onRevertHunk, compact,
}: { diff: FileDiff; onRevertHunk?: (hunkId: string) => void; compact?: boolean }) {
  const [mode, setMode] = useState<'unified' | 'split'>('unified')

  return (
    <div className="rounded-xl border border-edge overflow-hidden bg-panel">
      <div className="flex items-center justify-between px-3 h-8 border-b border-edge bg-panel2/80 select-none">
        <div className="flex items-center gap-2 min-w-0">
          <FileCode size={13} className="text-accent shrink-0" />
          <span className="font-mono text-[12px] font-medium text-ink truncate" title={diff.path}>{diff.path}</span>
          <span className="text-[11px] text-ok font-mono font-medium px-1 rounded bg-ok/10 shrink-0">+{diff.additions}</span>
          <span className="text-[11px] text-err font-mono font-medium px-1 rounded bg-err/10 shrink-0">−{diff.deletions}</span>
        </div>
        <div className="flex items-center gap-1 shrink-0 p-0.5 rounded-lg bg-panel border border-edge/60">
          <button
            className={cn('text-[11px] px-2 py-0.5 rounded-md font-medium transition-all', mode === 'unified' ? 'bg-accent/15 text-accent' : 'text-inkfaint hover:text-ink')}
            onClick={() => setMode('unified')}
          >
            统一
          </button>
          <button
            className={cn('text-[11px] px-2 py-0.5 rounded-md font-medium transition-all', mode === 'split' ? 'bg-accent/15 text-accent' : 'text-inkfaint hover:text-ink')}
            onClick={() => setMode('split')}
          >
            并排
          </button>
        </div>
      </div>

      <div className="overflow-x-auto">
        {diff.hunks.map((h) => (
          <div key={h.id} className="border-b border-edge/60 last:border-b-0">
            <HunkHeader hunk={h} onRevert={onRevertHunk} />
            {mode === 'unified' ? <Unified hunk={h} compact={compact} /> : <Split hunk={h} />}
          </div>
        ))}
      </div>
    </div>
  )
}

function HunkHeader({ hunk, onRevert }: { hunk: DiffHunk; onRevert?: (id: string) => void }) {
  return (
    <div className="flex items-center justify-between gap-2 px-3 py-1 bg-panel2/40 border-b border-edge/40">
      <span className="font-mono text-[11px] text-inkfaint truncate">{hunk.header}</span>
      {onRevert && (
        <Button size="sm" variant={hunk.reverted ? 'soft' : 'ghost'} disabled={hunk.reverted} onClick={() => onRevert(hunk.id)} className="gap-1 h-6 text-[11px] px-2">
          <RotateCcw size={10} />
          <span>{hunk.reverted ? '已回滚' : '回滚此 hunk'}</span>
        </Button>
      )}
    </div>
  )
}

function Unified({ hunk, compact }: { hunk: DiffHunk; compact?: boolean }) {
  const rows = [
    ...hunk.before.filter((l) => l.type !== 'add').map((l) => ({ ...l, show: true })),
    ...hunk.after.filter((l) => l.type === 'add').map((l) => ({ ...l, show: true })),
  ]
  return (
    <div className="font-mono text-[12px] leading-[1.65]">
      {rows.slice(0, compact ? 18 : undefined).map((l, i) => (
        <div key={i} className={cn('flex items-center transition-colors', l.type === 'add' && 'bg-ok/[0.12] text-ok', l.type === 'del' && 'bg-err/[0.12] text-err')}>
          <span className="w-10 shrink-0 text-right pr-2.5 text-inkfaint select-none text-[11px] font-mono">{l.no ?? ''}</span>
          <span className={cn('w-4 shrink-0 select-none font-bold', l.type === 'add' ? 'text-ok' : l.type === 'del' ? 'text-err' : 'text-inkfaint')}>
            {l.type === 'add' ? '+' : l.type === 'del' ? '−' : ' '}
          </span>
          <span className="whitespace-pre pr-3 text-ink">{l.text}</span>
        </div>
      ))}
    </div>
  )
}

function Split({ hunk }: { hunk: DiffHunk }) {
  const max = Math.max(hunk.before.length, hunk.after.length)
  return (
    <div className="grid grid-cols-2 font-mono text-[12px] leading-[1.65] divide-x divide-edge min-w-[560px]">
      <div>
        <div className="px-3 py-0.5 text-[10px] uppercase font-mono text-inkfaint bg-panel2/60 border-b border-edge/40">变更前</div>
        {Array.from({ length: max }).map((_, i) => {
          const l = hunk.before[i]
          return (
            <div key={i} className={cn('flex items-center', l?.type === 'del' && 'bg-err/[0.12]')}>
              <span className="w-10 shrink-0 text-right pr-2.5 text-inkfaint select-none text-[11px]">{l?.no ?? ''}</span>
              <span className="w-4 shrink-0 text-err font-bold">{l?.type === 'del' ? '−' : ' '}</span>
              <span className="whitespace-pre pr-2 text-ink">{l?.text ?? ''}</span>
            </div>
          )
        })}
      </div>
      <div>
        <div className="px-3 py-0.5 text-[10px] uppercase font-mono text-inkfaint bg-panel2/60 border-b border-edge/40">变更后</div>
        {Array.from({ length: max }).map((_, i) => {
          const l = hunk.after[i]
          return (
            <div key={i} className={cn('flex items-center', l?.type === 'add' && 'bg-ok/[0.12]')}>
              <span className="w-10 shrink-0 text-right pr-2.5 text-inkfaint select-none text-[11px]">{l?.no ?? ''}</span>
              <span className="w-4 shrink-0 text-ok font-bold">{l?.type === 'add' ? '+' : ' '}</span>
              <span className="whitespace-pre pr-2 text-ink">{l?.text ?? ''}</span>
            </div>
          )
        })}
      </div>
    </div>
  )
}
