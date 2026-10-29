import { useEffect, useRef, useState } from 'react'
import { Activity, Zap } from 'lucide-react'
import { useSse, SSE_PHASE_META } from '../../stores/sseStore'
import { Button, cn } from '../../components/ui'

const dotCls: Record<string, string> = {
  ok: 'bg-ok',
  warn: 'bg-warn',
  accent2: 'bg-accent2',
  err: 'bg-err',
}

/**
 * SSE 断线续传可见态（对齐客户端状态机 open → backoff → resync → open）：
 * 顶栏会话标题行内嵌状态胶囊，点击弹出订阅详情（游标 / 缓冲 / 重连 / 断开原因），
 * 并提供「模拟断线重连」演示（正式实现为 EventSource + lastEventId 游标 + 指数退避）。
 */
export function SseStatus() {
  const sse = useSse()
  const [open, setOpen] = useState(false)
  const [pos, setPos] = useState<{ left: number; top: number } | null>(null)
  const rootRef = useRef<HTMLDivElement>(null)
  const panelRef = useRef<HTMLDivElement>(null)

  const meta = SSE_PHASE_META[sse.phase]

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

  const toggle = () => {
    if (open) {
      setOpen(false)
      return
    }
    const r = rootRef.current?.getBoundingClientRect()
    if (r) {
      const left = Math.max(8, Math.min(r.left, window.innerWidth - 332))
      setPos({ left, top: r.bottom + 8 })
    }
    setOpen(true)
  }

  const pending = Math.max(0, sse.totalSeq - sse.lastSeq)

  return (
    <div className="relative shrink-0" ref={rootRef}>
      <button
        type="button"
        onClick={toggle}
        title={`实时事件流：${meta.label}（点击查看订阅详情）`}
        className="inline-flex items-center gap-1.5 px-1.5 py-0.5 rounded-md text-[10.5px] text-inkdim hover:text-ink hover:bg-panel2/70 transition-colors"
      >
        <span className="relative flex h-1.5 w-1.5 shrink-0">
          {(sse.phase === 'backoff' || sse.phase === 'connecting') && (
            <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-warn opacity-75" />
          )}
          <span className={cn('relative inline-flex rounded-full h-1.5 w-1.5', dotCls[meta.tone])} />
        </span>
        <span className="hidden sm:inline">{meta.label}</span>
      </button>

      {open && pos && (
        <div
          ref={panelRef}
          className="fixed z-50 w-[324px] max-h-[min(70vh,480px)] overflow-y-auto rounded-2xl border border-edge bg-panel shadow-modal p-3.5 text-[12px] slide-up"
          style={{ left: pos.left, top: pos.top }}
        >
          <div className="flex items-center justify-between pb-2.5 border-b border-edge/60">
            <div className="flex items-center gap-1.5">
              <Activity size={14} className="text-accent shrink-0" />
              <span className="font-semibold text-ink text-[12.5px] tracking-tight">实时事件流（SSE）</span>
            </div>
            <span
              className={cn(
                'px-1.5 py-0.5 rounded-md text-[10px] font-medium border',
                meta.tone === 'ok' && 'bg-ok/15 text-ok border-ok/30',
                meta.tone === 'warn' && 'bg-warn/15 text-warn border-warn/30',
                meta.tone === 'accent2' && 'bg-accent2/15 text-accent2 border-accent2/30',
                meta.tone === 'err' && 'bg-err/15 text-err border-err/30',
              )}
            >
              {meta.label}
            </span>
          </div>

          <div className="mt-2.5 text-[10.5px] text-inkfaint leading-relaxed">{meta.hint}</div>

          <div className="mt-2.5 space-y-1.5 text-[11px] text-inkdim border-t border-edge/40 pt-2.5 font-mono">
            <Row label="事件游标:" value={`${sse.lastSeq} / ${sse.totalSeq}`} />
            <Row label="待补事件:" value={pending > 0 ? `${pending} 条（resync 补拉）` : '0（已对齐）'} tone={pending > 0 ? 'accent2' : undefined} />
            <Row label="订阅缓冲:" value={`${sse.bufferUsed} / ${sse.bufferCap}`} />
            <Row label="累计重连:" value={`${sse.reconnects} 次`} />
            <Row
              label="上次断开:"
              value={sse.lastDrop ? new Date(sse.lastDrop.at).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }) : '—'}
            />
          </div>

          {sse.lastDrop && (
            <div className="mt-2 rounded-lg border border-edge bg-panel2/40 p-2.5 text-[10.5px] text-inkfaint leading-relaxed">
              <div className="text-inkdim font-medium">断开原因：{sse.lastDrop.reason}</div>
              <div className="mt-1 font-mono">
                退避 {sse.lastDrop.backoffMs}ms → 重连 → 补拉 {sse.lastDrop.resynced} 条里程碑事件 → 恢复订阅
              </div>
            </div>
          )}

          <div className="mt-3 pt-2.5 border-t border-edge/40 flex items-center gap-2">
            <Button
              size="sm"
              variant="outline"
              className="gap-1.5"
              disabled={sse.demoRunning || sse.phase !== 'open'}
              onClick={() => sse.simulateDrop()}
              title="演示断线 → 指数退避 → 重连 → 按游标补同步的全过程"
            >
              <Zap size={12} className="text-warn" />
              <span>{sse.demoRunning ? '断线重连演示中…' : '模拟断线重连'}</span>
            </Button>
            <span className="text-[10px] text-inkfaint leading-relaxed">
              回到前台会立即重连（visibilitychange）
            </span>
          </div>

          <div className="mt-2.5 pt-2 border-t border-edge/40 text-[10px] text-inkfaint leading-relaxed">
            铁律：消息最终内容以数据库为准；流式 delta 只是预览，resync 后以消息快照对齐。
          </div>
        </div>
      )}
    </div>
  )
}

function Row({ label, value, tone }: { label: string; value: string; tone?: 'accent2' }) {
  return (
    <div className="flex items-center justify-between gap-2">
      <span className="text-inkfaint shrink-0">{label}</span>
      <span className={cn('font-medium text-right', tone === 'accent2' ? 'text-accent2' : 'text-ink')}>{value}</span>
    </div>
  )
}
