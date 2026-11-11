import { useEffect, useRef, useState } from 'react'
import { Check, ChevronDown, Link2, Wifi, WifiOff } from 'lucide-react'
import { useSessions, wsOfProject, wsOfSession } from '../stores/sessionStore'
import { useUI } from '../stores/uiStore'
import { cn } from './ui'
import type { Workspace, WorkspaceKind } from '../types'

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
    setWorkspace,
  } = useSessions()
  const { toast } = useUI()
  const [open, setOpen] = useState(false)
  const [pos, setPos] = useState<{ top: number; left: number; width: number } | null>(null)
  const wrapRef = useRef<HTMLDivElement>(null)

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

  const onPick = (w: Workspace) => {
    setWorkspace(w.id)
    // 提示与能力策略严格对齐：零工具空间「可以对话、只是没有工具」，不是只读历史。
    if (w.toolPolicy === 'none') {
      toast('info', `已切换到「${w.name}」· ${w.policyReason ?? '该空间没有可用设备，可对话但无法调用工具'}`)
    } else {
      toast('success', `已切换到「${w.name}」`)
    }
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
            工作空间 ({workspaces.length})
          </div>

          <div className="max-h-[300px] overflow-y-auto px-1">
            {workspaces.map((w) => {
              const isActive = w.id === activeWorkspaceId
              const pCount = projects.filter((p) => wsOfProject(p) === w.id).length
              const sCount = sessions.filter((s) => wsOfSession(s, projects) === w.id).length
              return (
                <button
                  key={w.id}
                  onClick={() => onPick(w)}
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
                    <StatusText w={w} />
                    <span className="opacity-40">·</span>
                    <span>
                      {pCount} 项目 / {sCount} 对话
                    </span>
                  </div>
                  {/* 设备绑定情况：哪个设备绑定的，一眼可见（内置空间不重复展示） */}
                  {!w.builtin && (
                    <div className="flex items-center gap-1 mt-0.5 pl-3 text-[10px] text-inkfaint">
                      <Link2 size={8} className="shrink-0 opacity-70" />
                      {w.deviceId ? (
                        <>
                          <span>绑定：</span>
                          <span className="text-inkdim">{w.deviceName || w.deviceId}</span>
                          {w.deviceKind && <span className="font-mono opacity-60">· {w.deviceKind}</span>}
                        </>
                      ) : (
                        <span>未绑定设备</span>
                      )}
                    </div>
                  )}
                  {/* 无工具能力时给出原因：用户能明白「能聊但调不了工具」，而不是困惑按钮失效 */}
                  {w.toolPolicy !== 'full' && (
                    <div className="mt-0.5 pl-3 text-[10px] text-warn/90 leading-relaxed">
                      {w.policyReason ?? (w.toolPolicy === 'readonly' ? '仅支持只读操作与对话' : '仅可对话，无法调用工具')}
                    </div>
                  )}
                </button>
              )
            })}
          </div>

          <div className="mt-1.5 pt-2 px-2.5 border-t border-edge/70">
            <div className="text-[10.5px] text-inkfaint leading-relaxed">
              工作空间与设备严格一对一：配对设备后服务端自动创建其空间。
              设备绑定/解绑请到「设置 → 高级 → 工作空间」。
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

/**
 * 单行状态文案：把「在线 / 离线 / 设备已删除 / 设备已解绑 / 未绑定设备」三种语义分开。
 *
 * 🚨 关键：离线（可恢复）与已删除/已解绑（需重新绑定）绝不能用同一句「离线」，
 * 否则用户会一直等一台永远不会回来的设备。
 */
function StatusText({ w }: { w: Workspace }) {
  if (w.builtin) {
    return (
      <span className="text-ok/90 inline-flex items-center gap-0.5">
        <Wifi size={9} />在线
      </span>
    )
  }
  if (w.deviceMissing) {
    return (
      <span className="text-danger/90 inline-flex items-center gap-0.5">
        <WifiOff size={9} />设备已删除
      </span>
    )
  }
  if (w.deviceRevoked) {
    return (
      <span className="text-danger/90 inline-flex items-center gap-0.5">
        <WifiOff size={9} />设备已解绑
      </span>
    )
  }
  if (!w.bound) {
    return (
      <span className="inline-flex items-center gap-0.5">
        <WifiOff size={9} />未绑定设备
      </span>
    )
  }
  if (!w.online) {
    return (
      <span className="inline-flex items-center gap-0.5">
        <WifiOff size={9} />离线 · {fmtAgo(w.lastSeenAt)}
      </span>
    )
  }
  return (
    <span className="text-ok/90 inline-flex items-center gap-0.5">
      <Wifi size={9} />{w.deviceName ? `${w.deviceName} 在线` : '在线'}
    </span>
  )
}
