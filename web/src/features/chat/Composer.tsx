import { useRef, useState } from 'react'
import { Paperclip, Coins, Zap, File as FileIcon, Image as ImageIcon, Send, Square, X, Clock, WifiOff } from 'lucide-react'
import { useChat } from '../../stores/chatStore'
import { useSessions } from '../../stores/sessionStore'
import { Button, cn } from '../../components/ui'
import { ContextGauge } from './ContextGauge'

interface PendingAtt {
  id: string
  name: string
  size: number
  isImage: boolean
}

const fmtTokens = (n: number) =>
  n >= 1_000_000 ? `${(n / 1_000_000).toFixed(2)}M` : n >= 10_000 ? `${(n / 1000).toFixed(1)}k` : n.toLocaleString('zh-CN')

const fmtSize = (n: number) =>
  n >= 1024 * 1024 ? `${(n / 1024 / 1024).toFixed(1)}MB` : n >= 1024 ? `${(n / 1024).toFixed(0)}KB` : `${n}B`

export function Composer() {
  const { send, stop, streaming, queued, removeQueued, draft, setDraft, tokensIn, tokensOut, cachedTokens } = useChat()
  const { workspaces, activeWorkspaceId } = useSessions()
  const ref = useRef<HTMLTextAreaElement>(null)
  const fileRef = useRef<HTMLInputElement>(null)
  const [atts, setAtts] = useState<PendingAtt[]>([])

  // 离线工作空间只读：可查看历史，不支持发起对话
  const activeWs = workspaces.find((w) => w.id === activeWorkspaceId)
  const offline = Boolean(activeWs && !activeWs.online)

  const total = tokensIn + tokensOut
  const cacheRate = tokensIn > 0 ? Math.round((cachedTokens / tokensIn) * 1000) / 10 : 0

  const submit = () => {
    if (offline) return
    const t = draft.trim()
    if (!t) return
    send(t)
    setDraft('')
    setAtts([])
    if (ref.current) {
      ref.current.style.height = 'auto'
    }
  }

  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      submit()
    }
  }

  const grow = () => {
    const el = ref.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, 160)}px`
  }

  const onPickFiles = (e: React.ChangeEvent<HTMLInputElement>) => {
    const files = e.target.files
    if (!files || files.length === 0) return
    const next = Array.from(files).map((f) => ({
      id: `${Date.now()}-${f.name}-${f.size}`,
      name: f.name,
      size: f.size,
      isImage: f.type.startsWith('image/'),
    }))
    setAtts((prev) => [...prev, ...next])
    e.target.value = ''
  }

  return (
    <div className="px-3 sm:px-5 pb-3 pt-1 select-none">
      {/* 待执行队列 */}
      {queued.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-1.5 slide-up">
          {queued.map((q, i) => (
            <span
              key={i}
              className="inline-flex items-center gap-1.5 rounded-lg border border-accent/30 bg-accent/10 px-2.5 py-1 text-[12px] text-accent font-medium shadow-subtle"
            >
              <Clock size={11} className="opacity-70" />
              <span className="text-[10px] opacity-75">待执行</span>
              <span className="max-w-[180px] truncate">{q}</span>
              <button
                className="w-4 h-4 rounded hover:bg-accent/20 flex items-center justify-center transition-colors text-accent hover:text-ink"
                onClick={() => removeQueued(i)}
                title="移除该待执行任务"
              >
                <X size={11} />
              </button>
            </span>
          ))}
        </div>
      )}

      {/* 离线工作空间：仅可查看历史对话，禁止发起新对话 */}
      {offline && (
        <div className="flex items-start gap-1.5 rounded-xl border border-warn/30 bg-warn/10 px-3 py-2 text-[11.5px] text-warn leading-relaxed">
          <WifiOff size={13} className="shrink-0 mt-0.5" />
          <span>
            「{activeWs?.name}」当前离线，仅可查看历史对话记录，暂不支持发起对话。
          </span>
        </div>
      )}

      {/* 主输入卡片：清晰线框，无阴影 */}
      <div
        className={cn(
          'rounded-2xl border bg-panel transition-all duration-150 overflow-hidden',
          streaming
            ? 'border-accent'
            : 'border-edge hover:border-edge2 focus-within:border-accent',
        )}
      >
        {/* 待发附件条 */}
        {atts.length > 0 && (
          <div className="flex items-center gap-1.5 px-3 pt-2.5 pb-2 overflow-x-auto no-scrollbar border-b border-edge bg-panel2">
            {atts.map((a) => (
              <span
                key={a.id}
                className="group inline-flex items-center gap-1.5 shrink-0 max-w-[220px] rounded-lg border border-edge bg-panel px-2.5 py-1 text-ink"
              >
                {a.isImage ? (
                  <ImageIcon size={13} className="text-accent shrink-0" />
                ) : (
                  <FileIcon size={13} className="text-accent shrink-0" />
                )}
                <span className="text-[12px] truncate font-medium" title={a.name}>{a.name}</span>
                <span className="text-[10px] text-inkfaint font-mono shrink-0">{fmtSize(a.size)}</span>
                <button
                  className="w-4 h-4 rounded flex items-center justify-center text-inkfaint hover:text-err hover:bg-err/10 transition-colors ml-0.5"
                  title="移除附件"
                  onClick={() => setAtts((p) => p.filter((x) => x.id !== a.id))}
                >
                  <X size={11} />
                </button>
              </span>
            ))}
          </div>
        )}

        <textarea
          ref={ref}
          disabled={offline}
          className="w-full min-h-[50px] max-h-[160px] px-3.5 pt-3 pb-2 bg-transparent outline-none focus:outline-none focus:ring-0 resize-none text-[14px] leading-relaxed text-ink placeholder:text-inkfaint border-none disabled:cursor-not-allowed disabled:opacity-60"
          placeholder={
            offline
              ? '该工作空间离线，仅可查看历史对话'
              : streaming ? '运行中：输入指令将自动加入待执行队列…' : '输入指令，Enter 发送 / Shift+Enter 换行'
          }
          value={draft}
          onChange={(e) => { setDraft(e.target.value); grow() }}
          onKeyDown={onKeyDown}
          rows={1}
        />

        {/* 输入框底栏工具行 */}
        <div className="flex items-center justify-between gap-2 px-2.5 pb-2 pt-1">
          <div className="flex items-center gap-1.5 min-w-0 flex-1">
            <input ref={fileRef} type="file" multiple className="hidden" onChange={onPickFiles} />
            <button
              type="button"
              onClick={() => fileRef.current?.click()}
              disabled={offline}
              className="w-8 h-8 shrink-0 rounded-lg inline-flex items-center justify-center text-inkdim hover:text-accent hover:bg-panel2 transition-colors disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-inkdim"
              title={offline ? '工作空间离线，无法添加附件' : '添加图片或文件附件'}
              aria-label="添加附件"
            >
              <Paperclip size={15} />
            </button>

            <ContextGauge />

            <button
              type="button"
              className="inline-flex items-center gap-1.5 px-2 py-1 rounded-md text-[11px] font-mono text-inkdim hover:text-ink hover:bg-panel2 transition-all whitespace-nowrap"
              title={`会话累计消耗: ${total.toLocaleString('zh-CN')} tokens\n输入: ${tokensIn.toLocaleString('zh-CN')}（缓存命中: ${cachedTokens.toLocaleString('zh-CN')} · ${cacheRate.toFixed(1)}%）\n输出: ${tokensOut.toLocaleString('zh-CN')}`}
            >
              <Coins size={12} className="text-amber-400 shrink-0" />
              <span className="font-medium text-inkdim">{fmtTokens(total)}</span>
              <span className="hidden sm:inline text-[10px] text-inkfaint">tokens</span>
              {cacheRate > 0 && (
                <span className="ml-0.5 inline-flex items-center gap-0.5 rounded bg-accent2/10 px-1 py-[0.5px] text-[10px] font-medium text-accent2">
                  <Zap size={9} className="shrink-0" />
                  {cacheRate.toFixed(1)}%
                </span>
              )}
            </button>
          </div>

          <div className="shrink-0">
            {streaming ? (
              <Button size="sm" variant="danger" onClick={stop} className="gap-1.5 px-3">
                <Square size={11} fill="currentColor" />
                <span>停止</span>
              </Button>
            ) : (
              <Button size="sm" variant="primary" onClick={submit} disabled={offline} className="gap-1.5 px-3.5">
                <Send size={12} />
                <span>{offline ? '离线' : '发送'}</span>
              </Button>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
