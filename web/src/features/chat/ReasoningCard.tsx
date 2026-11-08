import { useEffect, useRef, useState } from 'react'
import { Brain, ChevronDown, ChevronRight } from 'lucide-react'
import { cn } from '../../components/ui'

/**
 * 思考过程卡片（reasoning / reasoning_content）。
 *
 * 折叠状态机与 ToolCard / ProcessDrawer 完全统一：
 * - 运行中（streaming=true）自动展开，带打字光标；
 * - 进入终态自动收起，标题栏保留字数摘要（可随时点开回看）；
 * - 用户手动切换后以用户为准，直到 streaming 再次跃迁才复位。
 *
 * 数据来源：Message.reasoning（实时来自 SSE `message.reasoning` 累积，刷新/重连来自 DB 快照），
 * 不做任何伪造——文本为空时组件整体不渲染。
 */
export function ReasoningCard({
  text,
  streaming = false,
  defaultTitle = '思考过程',
}: {
  text?: string
  streaming?: boolean
  defaultTitle?: string
}) {
  const body = (text ?? '').trim()
  const [userOpen, setUserOpen] = useState<boolean | null>(null)
  const open = userOpen ?? streaming
  const prevStreaming = useRef(streaming)
  useEffect(() => {
    const was = prevStreaming.current
    prevStreaming.current = streaming
    // 流式结束：复位用户干预 → 回到跟随状态 → 自动收起
    if (was && !streaming) setUserOpen(null)
  }, [streaming])

  const bodyRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = bodyRef.current
    if (!el) return
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 48
    if (nearBottom) el.scrollTop = el.scrollHeight
  }, [body])

  if (!body) return null

  return (
    <div className="rounded-xl border border-edge bg-panel overflow-hidden transition-all duration-150">
      <button
        type="button"
        className="w-full flex items-center gap-2 px-3 py-2 text-left bg-panel2/40 hover:bg-panel2 transition-colors select-none"
        onClick={() => setUserOpen(!open)}
        title={open ? '收起思考过程' : '展开思考过程'}
      >
        <span className="text-inkfaint w-4 h-4 flex items-center justify-center shrink-0">
          {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
        </span>
        <Brain size={12} className={cn('shrink-0', streaming ? 'text-accent' : 'text-inkfaint')} />
        <span className={cn('text-[12px] font-medium shrink-0', streaming ? 'text-accent' : 'text-inkdim')}>
          {streaming ? '思考中…' : defaultTitle}
        </span>
        {!streaming && (
          <span className="text-[10.5px] font-mono text-inkfaint shrink-0">
            {body.length.toLocaleString('zh-CN')} 字
          </span>
        )}
        <span className="flex-1" />
        {streaming && (
          <span className="flex items-center gap-1.5 text-[11px] text-accent font-medium shrink-0">
            <span className="relative flex h-1.5 w-1.5 shrink-0">
              <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-accent opacity-75" />
              <span className="relative inline-flex rounded-full h-1.5 w-1.5 bg-accent" />
            </span>
          </span>
        )}
      </button>

      {open && (
        <div className="border-t border-edge bg-panel">
          <div
            ref={bodyRef}
            className="px-3.5 py-2.5 text-[12.5px] leading-relaxed text-inkdim whitespace-pre-wrap break-words max-h-72 overflow-auto border-l-2 border-accent/50"
          >
            {body}
            {streaming && <span className="inline-block w-1.5 h-3 bg-accent align-text-bottom ml-0.5 animate-pulse" />}
          </div>
        </div>
      )}
    </div>
  )
}
