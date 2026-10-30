import { useEffect, useRef, useState } from 'react'
import { PanelLeft, PanelLeftClose, Lock, Unlock, Moon, Sun, Settings, Bot, ChevronDown, Folder, LogOut, KeyRound } from 'lucide-react'
import { useSessions } from '../stores/sessionStore'
import { useUI } from '../stores/uiStore'
import { useSettings } from '../stores/settingsStore'
import { roleLabel, useAuth } from '../stores/authStore'
import { Button, IconButton } from '../components/ui'
import { ChangePasswordModal } from '../features/auth/ChangePasswordModal'
import { SseStatus } from '../features/chat/SseStatus'
import type { AccessMode } from '../types'

export function TopBar() {
  const { sessions, currentId, setAccessMode, setModel, workspaces, activeWorkspaceId } = useSessions()
  const { setSettings, theme, toggleTheme, sidebarOpen, toggleSidebar, toast } = useUI()
  const { user, loggedInAt, expiresAt, logout } = useAuth()
  // 厂商列表以主库为唯一真源（real 模式由 settingsStore.loadProviders 拉取；
  // mock 模式 settingsStore 内置演示数据）——不要再直接读 mock/data，否则 real 模式下
  // 选中的模型不在列表里会导致选择器显示空白。
  const providers = useSettings((st) => st.settings.providers)
  const [accountOpen, setAccountOpen] = useState(false)
  const [pwdOpen, setPwdOpen] = useState(false)
  const accountRef = useRef<HTMLDivElement>(null)
  const s = sessions.find((x) => x.id === currentId)
  const activeWs = workspaces.find((w) => w.id === activeWorkspaceId)

  useEffect(() => {
    if (!accountOpen) return
    const onDown = (e: MouseEvent) => {
      if (accountRef.current && !accountRef.current.contains(e.target as Node)) setAccountOpen(false)
    }
    window.addEventListener('mousedown', onDown)
    return () => window.removeEventListener('mousedown', onDown)
  }, [accountOpen])

  const fmtTime = (t?: number | null) =>
    t ? new Date(t).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '—'

  const models = providers.filter((p) => p.enabled).flatMap((p) => p.models.map((m) => ({ ref: `${p.id}/${m.id}`, name: m.id, limit: m.contextLimit })))
  const currentModel = models.find((m) => m.ref === s?.modelRef)
  // 会话当前 modelRef 不在已配置列表里（未配置厂商 / 厂商被禁用）时补一个占位项，
  // 否则受控 select 的 value 无匹配 option，界面上会显示为空白（real 模式未配厂商时即为空串）。
  const modelChoices =
    !s || models.some((m) => m.ref === s.modelRef)
      ? models
      : [{ ref: s.modelRef, name: s.modelRef || '默认模型', limit: 0 }, ...models]
  // 标题恒为当前会话标题（对话区常驻，定时任务/插件市场以弹窗呈现，不再改写标题）
  const pageTitle = s?.title ?? '新对话'

  return (
    <header className="h-12 px-2.5 sm:px-4 flex items-center gap-2 bg-panel border-b border-edge shrink-0 select-none z-20">
      {/* 侧栏开合开关：全断点常驻（移动端弹出抽屉，PC 端收起/展开常驻列） */}
      <IconButton
        onClick={toggleSidebar}
        title={sidebarOpen ? '收起侧栏' : '展开侧栏'}
        ariaLabel="切换侧栏"
        className="text-inkdim hover:text-ink hover:bg-panel2"
      >
        {sidebarOpen ? <PanelLeftClose size={16} /> : <PanelLeft size={16} />}
      </IconButton>

      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2 min-w-0">
          {s?.running && (
            <span className="relative flex h-2 w-2 shrink-0" title="正在运行中">
              <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-ok opacity-75" />
              <span className="relative inline-flex rounded-full h-2 w-2 bg-ok" />
            </span>
          )}
          <span className="text-[13px] font-semibold tracking-tight text-ink truncate">
            {pageTitle}
          </span>
        </div>
        {s && (
          <div className="flex items-center gap-1 text-[11px] text-inkfaint truncate mt-0.5">
            <SseStatus />
            <span className="shrink-0 max-w-[140px] truncate" title="当前工作空间">{activeWs?.name ?? '未接入空间'}</span>
            {activeWs && !activeWs.online && (
              <span className="shrink-0 text-warn font-medium">离线</span>
            )}
            <span className="shrink-0 opacity-40">·</span>
            <Folder size={10} className="shrink-0 opacity-70" />
            <span className="truncate">{s.workspacePath || '纯对话 · 未绑定工作区'}</span>
          </div>
        )}
      </div>

      {s && (
        <div className="flex items-center gap-1.5 shrink-0">
          {/* 模型选择器：轻量胶囊下拉 */}
          <div className="relative flex items-center">
            <span className="absolute left-2 pointer-events-none text-accent">
              <Bot size={13} />
            </span>
            <select
              className="h-8 pl-6 pr-6 max-w-[96px] sm:max-w-[200px] rounded-lg bg-panel2 text-[12px] font-medium text-ink outline-none cursor-pointer appearance-none hover:bg-panel3 transition-all focus:ring-1 focus:ring-accent/40"
              value={s.modelRef}
              onChange={(e) => setModel(s.id, e.target.value)}
              title="会话级模型（切换不影响其他会话）"
            >
              {modelChoices.map((m) => (
                <option key={m.ref} value={m.ref} className="bg-panel text-ink py-1">
                  {m.name}
                </option>
              ))}
            </select>
            <span className="absolute right-2 pointer-events-none text-inkfaint">
              <ChevronDown size={11} />
            </span>
          </div>

          {currentModel && (
            <span className="hidden sm:inline-flex px-1.5 py-0.5 rounded-md bg-panel2 text-[10px] font-mono text-inkfaint" title={`上下文窗口上限: ${currentModel.limit.toLocaleString()} tokens`}>
              {Math.round(currentModel.limit / 1000)}k
            </span>
          )}

          {/* 访问模式切换 */}
          <IconButton
            onClick={() => setAccessMode(s.id, (s.accessMode === 'confirm' ? 'full' : 'confirm') as AccessMode)}
            tone={s.accessMode === 'full' ? 'warn' : undefined}
            title={s.accessMode === 'full' ? '完全访问（已开启高权限，点击切回变更前确认）' : '变更前确认（写入/命令均需审批，点击切为完全访问）'}
            className={s.accessMode === 'full' ? 'text-warn bg-warn/15 border-warn/30' : 'text-inkdim hover:text-ink'}
          >
            {s.accessMode === 'full' ? <Unlock size={14} /> : <Lock size={14} />}
          </IconButton>
        </div>
      )}

      {/* 分隔线：移动端顶栏空间紧张，隐藏（视觉上也不需要） */}
      <div className="hidden sm:block h-4 w-px bg-edge mx-0.5" />

      {/* 主题与全局设置：主题开关在移动端收起（设置 → 外观 面板可切换），
          避免 360px 宽度下把会话标题挤成 0 宽 */}
      <div className="hidden sm:flex">
        <IconButton onClick={toggleTheme} title={theme === 'dark' ? '切换为浅色主题' : '切换为深色主题'} ariaLabel="切换主题">
          {theme === 'dark' ? <Moon size={15} /> : <Sun size={15} />}
        </IconButton>
      </div>
      <IconButton onClick={() => setSettings(true)} title="程序设置" ariaLabel="打开设置">
        <Settings size={15} />
      </IconButton>

      {/* 账户菜单：账号密码登录态与退出 */}
      <div className="relative flex items-center" ref={accountRef}>
        <button
          className="h-8 pl-1 pr-1.5 sm:pr-2 flex items-center gap-1.5 rounded-lg border border-edge bg-panel2/60 hover:bg-panel3 transition-colors select-none"
          onClick={() => setAccountOpen((v) => !v)}
          title={`账户：${user?.name ?? '未登录'}（点击查看登录信息）`}
          aria-label="账户菜单"
        >
          <span className="w-6 h-6 rounded-full bg-accent/15 text-accent flex items-center justify-center text-[11px] font-semibold">
            {(user?.name ?? '?').slice(0, 1).toUpperCase()}
          </span>
          <span className="hidden sm:inline text-[12px] text-inkdim font-medium max-w-[80px] truncate">{user?.name ?? '未登录'}</span>
          <ChevronDown size={11} className="text-inkfaint hidden sm:block" />
        </button>

        {accountOpen && (
          <div className="absolute right-0 top-full mt-1.5 z-40 w-60 rounded-xl border border-edge bg-panel shadow-modal p-2 fade-in">
            <div className="flex items-center gap-2 px-1 pb-2">
              <div className="w-8 h-8 rounded-full bg-accent/15 text-accent flex items-center justify-center text-[13px] font-semibold shrink-0">
                {(user?.name ?? '?').slice(0, 1).toUpperCase()}
              </div>
              <div className="min-w-0">
                <div className="text-[13px] font-medium text-ink truncate">{user?.name ?? '未登录'}</div>
                <div className="text-[11px] text-inkfaint truncate">
                  {roleLabel(user?.role)} · {user?.username ?? '—'}
                </div>
              </div>
            </div>

            <div className="border-t border-edge/60 pt-2 space-y-1 text-[11px] text-inkdim font-mono">
              <div className="flex items-center justify-between gap-2">
                <span className="text-inkfaint">登录时间</span>
                <span className="truncate">{fmtTime(loggedInAt)}</span>
              </div>
              <div className="flex items-center justify-between gap-2">
                <span className="text-inkfaint">有效期至</span>
                <span className="truncate">{fmtTime(expiresAt)}</span>
              </div>
              <div className="flex items-center justify-between gap-2">
                <span className="text-inkfaint">部署形态</span>
                <span className="truncate">本地自托管</span>
              </div>
            </div>

            <div className="mt-2 pt-2 border-t border-edge/60 flex flex-col gap-0.5">
              <Button
                size="sm"
                variant="ghost"
                className="w-full justify-start gap-1.5 text-inkdim hover:text-ink"
                onClick={() => { setAccountOpen(false); setPwdOpen(true) }}
              >
                <KeyRound size={13} />
                <span>修改密码</span>
              </Button>
              <Button
                size="sm"
                variant="ghost"
                className="w-full justify-start gap-1.5 text-inkdim hover:text-err"
                onClick={() => { logout(); toast('info', '已退出登录') }}
              >
                <LogOut size={13} />
                <span>退出登录</span>
              </Button>
            </div>
          </div>
        )}

        <ChangePasswordModal open={pwdOpen} onClose={() => setPwdOpen(false)} />
      </div>
    </header>
  )
}
