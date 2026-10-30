import { useState, type ReactNode } from 'react'
import { Monitor, Smartphone, Globe, Server, Wifi, WifiOff, Trash2, Lock, Check } from 'lucide-react'
import { useSessions, wsOfProject, wsOfSession } from '../../stores/sessionStore'
import { useUI } from '../../stores/uiStore'
import { Button, Empty, cn } from '../../components/ui'
import type { WorkspaceKind } from '../../types'

const KIND_META: Record<WorkspaceKind, { label: string; icon: ReactNode }> = {
  server: { label: '服务端', icon: <Server size={13} className="text-sky-400 shrink-0" /> },
  desktop: { label: '桌面端', icon: <Monitor size={13} className="text-emerald-400 shrink-0" /> },
  mobile: { label: '移动端', icon: <Smartphone size={13} className="text-purple-400 shrink-0" /> },
  browser: { label: '浏览器端', icon: <Globe size={13} className="text-accent shrink-0" /> },
}

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

// 工作空间管理面板（高级 → 工作空间）：查看已接入空间、移除客户端主动接入的空间
export function WorkspacePanel() {
  const { workspaces, projects, sessions, activeWorkspaceId, setWorkspace, removeWorkspace } = useSessions()
  const { toast } = useUI()
  const [confirmId, setConfirmId] = useState<string | null>(null)

  const onRemove = (id: string, name: string) => {
    if (confirmId !== id) {
      setConfirmId(id)
      toast('info', `再次点击「确认移除」以移除「${name}」`)
      window.setTimeout(() => setConfirmId((cur) => (cur === id ? null : cur)), 8000)
      return
    }
    const ok = removeWorkspace(id)
    setConfirmId(null)
    toast(ok ? 'success' : 'error', ok ? `已移除「${name}」及其项目与会话` : '内置服务端空间不可移除')
  }

  if (workspaces.length === 0) {
    return <Empty icon={<Server size={22} className="text-inkdim" />} title="暂无工作空间" hint="服务端内置空间应始终存在" />
  }

  return (
    <div className="space-y-3">
      <div className="rounded-xl border border-edge bg-panel2/50 p-3 text-[12px] text-inkfaint leading-relaxed space-y-1">
        <div>· 工作空间 = 服务端大脑 + 该空间绑定的肢体（客户端可执行能力）。</div>
        <div>· <span className="text-inkdim">服务端工作空间</span>为内置默认空间，永远在线、不可移除。</div>
        <div>· 其他工作空间由桌面端 / 移动端<span className="text-inkdim">主动接入</span>产生，名称可在接入时自定义。</div>
        <div>· 移除后该空间的项目与会话一并下线（原型行为），离线空间仅保留历史查看能力。</div>
      </div>

      <div className="space-y-2">
        {workspaces.map((w) => {
          const isActive = w.id === activeWorkspaceId
          const pCount = projects.filter((p) => wsOfProject(p) === w.id).length
          const sCount = sessions.filter((s) => wsOfSession(s, projects) === w.id).length
          const meta = KIND_META[w.kind]
          return (
            <div
              key={w.id}
              className={cn(
                'rounded-xl border bg-panel p-3 shadow-subtle transition-all',
                isActive ? 'border-accent/50' : 'border-edge hover:border-edge2',
              )}
            >
              <div className="flex items-center gap-2.5">
                <div className="w-7 h-7 rounded-lg bg-panel2 flex items-center justify-center shrink-0">
                  {meta.icon}
                </div>

                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-1.5 min-w-0">
                    <span className="text-[13px] font-semibold text-ink truncate">{w.name}</span>
                    {w.builtin && (
                      <span className="shrink-0 inline-flex items-center gap-0.5 px-1 py-[1px] rounded bg-panel3 text-[9.5px] font-mono text-inkdim">
                        <Lock size={8} />内置
                      </span>
                    )}
                    {isActive && (
                      <span className="shrink-0 inline-flex items-center gap-0.5 px-1 py-[1px] rounded bg-accent-soft text-[9.5px] font-medium text-accent">
                        <Check size={8} />当前
                      </span>
                    )}
                  </div>
                  <div className="flex items-center gap-1.5 mt-0.5 text-[11px] text-inkfaint flex-wrap">
                    <span className="font-mono">{meta.label}</span>
                    <span className="opacity-40">·</span>
                    {w.online ? (
                      <span className="inline-flex items-center gap-0.5 text-ok/90">
                        <Wifi size={9} />在线
                      </span>
                    ) : (
                      <span className="inline-flex items-center gap-0.5">
                        <WifiOff size={9} />离线 · {fmtAgo(w.lastSeenAt)}活跃
                      </span>
                    )}
                    <span className="opacity-40">·</span>
                    <span>{w.limbCount} 肢体</span>
                    <span className="opacity-40">·</span>
                    <span>{pCount} 项目 / {sCount} 对话</span>
                    {w.clientId && (
                      <>
                        <span className="opacity-40">·</span>
                        <span className="font-mono">{w.clientId}</span>
                      </>
                    )}
                  </div>
                </div>

                <div className="flex items-center gap-1.5 shrink-0">
                  {!isActive && (
                    <Button
                      size="sm"
                      variant="outline"
                      className="h-7 px-2 text-[11px]"
                      onClick={() => { setWorkspace(w.id); toast('success', `已切换到「${w.name}」`) }}
                    >
                      切换
                    </Button>
                  )}
                  {w.builtin ? (
                    <span className="text-[10.5px] text-inkfaint px-1" title="内置空间不可移除">不可移除</span>
                  ) : (
                    <Button
                      size="sm"
                      variant={confirmId === w.id ? 'danger' : 'ghost'}
                      className={cn('h-7 px-2 text-[11px] gap-1', confirmId === w.id ? '' : 'text-inkdim hover:text-err')}
                      onClick={() => onRemove(w.id, w.name)}
                      title="移除该客户端接入的工作空间"
                    >
                      <Trash2 size={11} />
                      {confirmId === w.id ? '确认移除' : '移除'}
                    </Button>
                  )}
                </div>
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )
}
