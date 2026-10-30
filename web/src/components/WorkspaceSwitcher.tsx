import { useEffect, useRef, useState } from 'react'
import { Check, ChevronDown, Plus, Wifi, WifiOff } from 'lucide-react'
import { useSessions, wsOfProject, wsOfSession } from '../stores/sessionStore'
import { useUI } from '../stores/uiStore'
import { Button, cn } from './ui'
import type { WorkspaceKind } from '../types'

const KIND_LABEL: Record<WorkspaceKind, string> = {
  server: '服务端',
  desktop: '桌面端',
  mobile: '移动端',
  browser: '浏览器端',
}

const KIND_OPTIONS: Array<{ value: WorkspaceKind; label: string }> = [
  { value: 'desktop', label: '桌面端' },
  { value: 'mobile', label: '移动端' },
  { value: 'browser', label: '浏览器端' },
]

/** 相对时间：离线空间展示「多久前活跃」 */
function fmtAgo(t?: number) {
  if (!t) return '未知'
  const diff = Math.max(0, Date.now() - t)
  const m = Math.floor(diff / 60_000)
  if (m < 1) return '刚刚'
  if (m < 60) return `${m} 分钟前`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时前`
  return `${Math.floor(h / 24)} 天前`
}

export function WorkspaceSwitcher() {
  const {
    workspaces, activeWorkspaceId, projects, sessions,
    setWorkspace, connectWorkspace,
  } = useSessions()
  const { toast } = useUI()
  const [open, setOpen] = useState(false)
  const [pos, setPos] = useState<{ top: number; left: number; width: number } | null>(null)
  const [newName, setNewName] = useState('')
  const [newKind, setNewKind] = useState<WorkspaceKind>('desktop')
  const wrapRef = useRef<HTMLDivElement>(null)
  const nameRef = useRef<HTMLInputElement>(null)

  const active = workspaces.find((w) => w.id === activeWorkspaceId)

  // 点击外部关闭
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false)
    }
    const onScroll = () => setOpen(false)
    window.addEventListener('mousedown', onDown)
    window.addEventListener('resize', onScroll)
    return () => {
      window.removeEventListener('mousedown', onDown)
      window.removeEventListener('resize', onScroll)
    }
  }, [open])

  // fixed 定位：避开侧栏 overflow 裁剪
  const toggle = () => {
    if (open) {
      setOpen(false)
      return
    }
    const r = wrapRef.current?.getBoundingClientRect()
    if (r) {
      const width = Math.min(272, Math.max(252, r.width))
      setPos({
        top: r.bottom + 6,
        left: Math.max(8, Math.min(r.left, window.innerWidth - width - 8)),
        width,
      })
    }
    setOpen(true)
  }

  const onPick = (id: string, online: boolean) => {
    setWorkspace(id)
    const ws = workspaces.find((w) => w.id === id)
    toast(online ? 'success' : 'info', online
      ? `已切换到「${ws?.name ?? id}」`
      : `已切换到「${ws?.name ?? id}」· 该工作空间离线，仅可查看历史对话`)
    setOpen(false)
  }

  const onConnect = () => {
    // 以 DOM 值为准兜底，避免受控 state 未同步（CDP/输入法）时丢输入
    const name = (nameRef.current?.value ?? newName).trim()
    if (!name) {
      toast('error', '请填写该客户端工作空间的名称')
      return
    }
    connectWorkspace(name, newKind)
    toast('success', `「${name}」已接入并切换为当前工作空间`)
    setNewName('')
    if (nameRef.current) nameRef.current.value = ''
    setOpen(false)
  }

  return (
    <div ref={wrapRef} className="relative h-12 px-3 flex items-center shrink-0 border-b border-edge/50">
      <button
        onClick={toggle}
        className={cn(
          'group w-full min-w-0 flex items-center gap-2 h-9 px-2 -mx-1 rounded-lg text-left transition-colors',
          open ? 'bg-panel2' : 'hover:bg-panel2/70',
        )}
        title="切换工作空间"
      >
        <span
          className={cn(
            'w-1.5 h-1.5 rounded-full shrink-0',
            active?.online ? 'bg-ok shadow-[0_0_0_3px_rgba(34,197,94,0.15)]' : 'bg-inkfaint',
          )}
        />
        <span className="flex-1 min-w-0 flex items-baseline gap-1.5">
          <span className="font-semibold text-[13.5px] tracking-tight text-ink truncate">
            {active?.name ?? '未接入工作空间'}
          </span>
          <span className="shrink-0 text-[10px] font-mono text-inkfaint">
            {active ? KIND_LABEL[active.kind] : ''}
          </span>
        </span>
        <ChevronDown
          size={13}
          className={cn('shrink-0 text-inkfaint transition-transform', open && 'rotate-180')}
        />
      </button>

      {open && pos && (
        <div
          className="fixed z-50 rounded-xl border border-edge bg-panel shadow-modal py-1.5 fade-in"
          style={{ top: pos.top, left: pos.left, width: pos.width }}
        >
          <div className="px-2.5 pb-1.5 pt-0.5 text-[10.5px] font-medium text-inkfaint uppercase tracking-wider">
            已接入工作空间 ({workspaces.length})
          </div>

          <div className="max-h-[300px] overflow-y-auto px-1">
            {workspaces.map((w) => {
              const isActive = w.id === activeWorkspaceId
              const pCount = projects.filter((p) => wsOfProject(p) === w.id).length
              const sCount = sessions.filter((s) => wsOfSession(s, projects) === w.id).length
              return (
                <button
                  key={w.id}
                  onClick={() => onPick(w.id, w.online)}
                  className={cn(
                    'w-full text-left px-2 py-1.5 rounded-lg transition-colors',
                    isActive ? 'bg-accent-soft' : 'hover:bg-panel2',
                  )}
                >
                  <div className="flex items-center gap-1.5 min-w-0">
                    <span
                      className={cn(
                        'w-1.5 h-1.5 rounded-full shrink-0',
                        w.online ? 'bg-ok' : 'bg-inkfaint/60',
                      )}
                    />
                    <span
                      className={cn(
                        'flex-1 min-w-0 truncate text-[12.5px] font-medium',
                        isActive ? 'text-accent' : 'text-ink',
                      )}
                    >
                      {w.name}
                    </span>
                    {w.builtin && (
                      <span className="shrink-0 px-1 py-[1px] rounded bg-panel3 text-[9.5px] font-mono text-inkdim">
                        内置
                      </span>
                    )}
                    {isActive && <Check size={12} className="shrink-0 text-accent" />}
                  </div>
                  <div className="flex items-center gap-1.5 mt-0.5 pl-3 text-[10.5px] text-inkfaint">
                    <span className="font-mono">{KIND_LABEL[w.kind]}</span>
                    <span className="opacity-40">·</span>
                    {w.online ? (
                      <span className="text-ok/90 inline-flex items-center gap-0.5">
                        <Wifi size={9} />在线
                      </span>
                    ) : (
                      <span className="inline-flex items-center gap-0.5">
                        <WifiOff size={9} />离线 · {fmtAgo(w.lastSeenAt)}
                      </span>
                    )}
                    <span className="opacity-40">·</span>
                    <span>
                      {pCount} 项目 / {sCount} 对话
                    </span>
                  </div>
                </button>
              )
            })}
          </div>

          <div className="mt-1.5 pt-2 px-2.5 border-t border-edge/70 space-y-1.5">
            <div className="text-[10.5px] text-inkfaint leading-relaxed">
              客户端主动接入：桌面端 / 移动端启动后在此接入，可自定义该工作空间名称。
            </div>
            <div className="flex items-center gap-1.5">
              <select
                value={newKind}
                onChange={(e) => setNewKind(e.target.value as WorkspaceKind)}
                className="h-7 shrink-0 rounded-md bg-panel2 text-[11px] text-inkdim px-1 outline-none cursor-pointer appearance-none hover:bg-panel3 transition-colors focus:ring-1 focus:ring-accent/40"
                title="接入客户端类型"
              >
                {KIND_OPTIONS.map((o) => (
                  <option key={o.value} value={o.value} className="bg-panel text-ink">
                    {o.label}
                  </option>
                ))}
              </select>
              <input
                ref={nameRef}
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                onKeyDown={(e) => { if (e.key === 'Enter') onConnect() }}
                placeholder="客户端工作空间名称"
                className="flex-1 min-w-0 h-7 px-2 rounded-md bg-panel2 text-[11.5px] text-ink placeholder:text-inkfaint outline-none focus:ring-1 focus:ring-accent/40"
              />
              <Button
                variant="primary"
                size="sm"
                className="h-7 px-2 shrink-0 gap-1 text-[11px]"
                onClick={onConnect}
              >
                <Plus size={12} />
                接入
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
