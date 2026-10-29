import { useState, useRef, useEffect } from 'react'
import {
  Plus, ChevronDown, ChevronRight, Pin, Folder,
  MoreHorizontal, MessageSquare, Sliders, Settings, Pencil,
  Archive, Trash2, FolderKanban, CalendarClock, Blocks, SquarePen
} from 'lucide-react'
import { useSessions, wsOfProject, wsOfSession } from '../stores/sessionStore'
import { useUI } from '../stores/uiStore'
import { useSchedule } from '../stores/scheduleStore'
import { Button, cn } from '../components/ui'
import { PromptModal, type PromptField } from '../components/PromptModal'
import { ProjectModal } from '../features/projects/ProjectModal'
import { WorkspaceSwitcher } from '../components/WorkspaceSwitcher'

interface MenuItem {
  label: string
  icon?: React.ReactNode
  danger?: boolean
  onClick: () => void
}

interface PromptState {
  title: string
  confirmText?: string
  fields: PromptField[]
  onSubmit: (v: Record<string, string>) => void
}

/** 每个分组默认展示的对话条数（对齐桌面端：超出收进「更多」） */
const SESSION_PREVIEW = 5

export function Sidebar({ onNavigate }: { onNavigate?: () => void }) {
  const {
    sessions, projects, currentId, select, create, rename, remove, archive, unarchive,
    createProject, renameProject, removeProject, toggleProjectPin,
    workspaces, activeWorkspaceId,
  } = useSessions()
  const { toast, setAdvanced, setSettings, scheduledOpen, pluginsOpen, setScheduled, setPlugins } = useUI()
  const enabledTasks = useSchedule((s) => s.tasks.filter((t) => t.enabled).length)
  const [menuFor, setMenuFor] = useState<string | null>(null)
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({})
  // 每组默认只展示最近 SESSION_PREVIEW 条对话，超出收进「更多」
  const [showAllSessions, setShowAllSessions] = useState<Record<string, boolean>>({})
  const [settingsFor, setSettingsFor] = useState<string | null>(null)
  const [prompt, setPrompt] = useState<PromptState | null>(null)

  // 仅展示当前工作空间下的项目与会话（工作空间是项目/会话的归属边界）
  const activeWs = workspaces.find((w) => w.id === activeWorkspaceId)
  const wsProjects = projects.filter((p) => wsOfProject(p) === activeWorkspaceId)
  const active = sessions.filter(
    (s) => !s.archivedAt && wsOfSession(s, projects) === activeWorkspaceId,
  )
  const pinnedProjects = wsProjects.filter((p) => p.pinned)
  const otherProjects = wsProjects.filter((p) => !p.pinned)

  // 纯对话（未归属项目）分组同样按 5 条折叠
  const looseSessions = active.filter((s) => !s.projectId)
  const looseExpanded = Boolean(showAllSessions.__loose__)
  const looseVisible = looseExpanded ? looseSessions : looseSessions.slice(0, SESSION_PREVIEW)
  const looseHidden = Math.max(0, looseSessions.length - SESSION_PREVIEW)

  const go = (fn: () => void) => { fn(); onNavigate?.() }
  /** 选中/新建会话：对话区常驻，无需切换页面（仅关闭移动端抽屉） */
  const goChat = (fn: () => void) => { fn(); onNavigate?.() }
  const closeMenu = () => setMenuFor(null)

  // ---------------- 菜单 / 弹窗构造 ----------------
  const sessionMenu = (s: { id: string; title: string; projectId?: string }): MenuItem[] => [
    {
      label: '重命名',
      icon: <Pencil size={12} className="text-inkdim" />,
      onClick: () => setPrompt({
        title: '重命名会话',
        confirmText: '保存',
        fields: [{ key: 'title', label: '会话标题', value: s.title }],
        onSubmit: (v) => { rename(s.id, v.title.trim()); setPrompt(null); toast('success', '会话已重命名') },
      }),
    },
    {
      label: '归档',
      icon: <Archive size={12} className="text-inkdim" />,
      onClick: () => { archive(s.id); toast('info', '会话已归档，可点击侧栏底部「高级 > 归档会话」恢复') }
    },
    {
      label: '删除',
      icon: <Trash2 size={12} className="text-err" />,
      danger: true,
      onClick: () => { remove(s.id); toast('success', '会话已删除') },
    },
  ]

  const projectMenu = (p: { id: string; name: string; path: string; pinned?: boolean }): MenuItem[] => [
    {
      label: '项目设置',
      icon: <Settings size={12} className="text-inkdim" />,
      onClick: () => setSettingsFor(p.id)
    },
    {
      label: '重命名',
      icon: <Pencil size={12} className="text-inkdim" />,
      onClick: () => setPrompt({
        title: '重命名项目',
        confirmText: '保存',
        fields: [{ key: 'name', label: '项目名称', value: p.name }],
        onSubmit: (v) => { renameProject(p.id, v.name); setPrompt(null); toast('success', '项目已重命名') },
      }),
    },
    {
      label: p.pinned ? '取消固定' : '固定到顶部',
      icon: <Pin size={12} className={p.pinned ? 'text-accent' : 'text-inkdim'} />,
      onClick: () => { toggleProjectPin(p.id); toast('success', p.pinned ? '已取消固定' : '已固定到顶部') }
    },
    {
      label: '移除项目',
      icon: <Trash2 size={12} className="text-err" />,
      danger: true,
      onClick: () => { removeProject(p.id); toast('success', '项目已移除，其下会话已移回「纯对话」') },
    },
  ]

  const newProject = () => setPrompt({
    title: '新建项目',
    confirmText: '创建',
    fields: [
      { key: 'name', label: '项目名称', value: '', placeholder: '如 harness_go' },
      { key: 'path', label: '工作区目录（可选）', value: '', placeholder: 'F:/WorkSpace/Other/harness_go', hint: '仅作项目标识，不会访问本地文件' },
    ],
    onSubmit: (v) => {
      createProject(v.name, v.path ?? '')
      setPrompt(null)
      toast('success', `项目「${v.name.trim()}」已创建`)
    },
  })

  return (
    <div className="flex flex-col h-full bg-[#fafbfc] dark:bg-panel border-r border-edge select-none">
      {/* 顶部工作空间切换器（服务端内置空间 + 客户端接入空间） */}
      <WorkspaceSwitcher />

      {/* 顶部三段纵向导航：新建对话 / 定时任务 / 插件市场，三行同构（各占一行，样式一致） */}
      <div className="p-2.5 pb-1.5 shrink-0 flex flex-col gap-0.5">
        <NavRow
          icon={<SquarePen size={13} />}
          label="新建对话"
          active={false}
          onClick={() => goChat(() => create())}
        />
        <NavRow
          icon={<CalendarClock size={13} />}
          label="定时任务"
          active={scheduledOpen}
          badge={enabledTasks > 0 ? String(enabledTasks) : undefined}
          onClick={() => go(() => setScheduled(true))}
        />
        <NavRow
          icon={<Blocks size={13} />}
          label="插件市场"
          active={pluginsOpen}
          onClick={() => go(() => setPlugins(true))}
        />
      </div>

      {/* 离线工作空间提示：仅可查看历史对话 */}
      {activeWs && !activeWs.online && (
        <div className="mx-2.5 mt-1.5 rounded-lg border border-warn/30 bg-warn/10 px-2 py-1.5 text-[11px] text-warn leading-relaxed shrink-0">
          该工作空间离线，仅可查看历史对话
        </div>
      )}

      {/* 会话与项目列表 */}
      <div className="flex-1 overflow-y-auto px-2 py-1.5 space-y-3">
        {/* 项目分组 */}
        <div>
          <div className="flex items-center justify-between px-2 py-1 text-inkdim">
            <div className="flex items-center gap-1.5">
              <FolderKanban size={13} className="text-inkfaint" />
              <span className="text-[12px] font-medium">项目</span>
              <span className="text-[10px] font-mono text-inkfaint">({wsProjects.length})</span>
            </div>
            <button
              className="w-5 h-5 rounded hover:bg-panel2 flex items-center justify-center text-inkfaint hover:text-accent transition-colors"
              title="新建项目"
              onClick={newProject}
            >
              <Plus size={13} />
            </button>
          </div>

          <div className="space-y-0.5 mt-0.5">
            {[...pinnedProjects, ...otherProjects].map((p) => {
              const list = active.filter((s) => s.projectId === p.id)
              const isCollapsed = Boolean(collapsed[p.id])
              const expanded = Boolean(showAllSessions[p.id])
              const visible = expanded ? list : list.slice(0, SESSION_PREVIEW)
              const hidden = Math.max(0, list.length - SESSION_PREVIEW)
              return (
                <div key={p.id} className="mb-1">
                  <div className="relative flex items-center gap-1 px-1.5 py-1 rounded-lg hover:bg-panel2/80 group transition-colors">
                    <button
                      className="text-inkfaint hover:text-ink w-4 h-4 flex items-center justify-center shrink-0"
                      title={isCollapsed ? '展开' : '折叠'}
                      onClick={() => setCollapsed((c) => ({ ...c, [p.id]: !isCollapsed }))}
                    >
                      {isCollapsed ? <ChevronRight size={12} /> : <ChevronDown size={12} />}
                    </button>
                    <button
                      className="flex-1 min-w-0 flex items-center gap-1.5 text-left text-[12px] text-inkdim font-medium truncate hover:text-ink"
                      title={p.path || p.name}
                      onClick={() => setCollapsed((c) => ({ ...c, [p.id]: !isCollapsed }))}
                    >
                      {p.pinned ? (
                        <Pin size={11} className="text-accent shrink-0 -rotate-45" />
                      ) : (
                        <Folder size={12} className="text-inkfaint shrink-0" />
                      )}
                      <span className="truncate">{p.name}</span>
                    </button>

                    {/* 操作按钮组：移动端与桌面端均常驻显示 */}
                    <div className="flex items-center gap-0.5 shrink-0">
                      <button
                        className="w-5 h-5 rounded hover:bg-panel3 flex items-center justify-center text-inkdim hover:text-accent transition-colors"
                        title="新建对话（归入该项目）"
                        onClick={() => goChat(() => create(p.id))}
                      >
                        <Plus size={12} />
                      </button>
                      <button
                        className={cn(
                          'w-5 h-5 rounded hover:bg-panel3 flex items-center justify-center text-inkdim hover:text-ink transition-colors',
                          menuFor === p.id ? 'text-ink bg-panel3' : 'text-inkdim hover:text-ink'
                        )}
                        title="更多操作"
                        onClick={() => setMenuFor(menuFor === p.id ? null : p.id)}
                      >
                        <MoreHorizontal size={13} />
                      </button>
                    </div>
                    {menuFor === p.id && <Menu items={projectMenu(p)} onClose={closeMenu} />}
                  </div>

                  {!isCollapsed && (
                    <div className="pl-3.5 space-y-0.5 my-0.5">
                      {visible.map((s) => (
                        <SessionRow
                          key={s.id}
                          s={s}
                          activeId={currentId}
                          onSelect={() => goChat(() => select(s.id))}
                          menuOpen={menuFor === s.id}
                          onMenu={() => setMenuFor(menuFor === s.id ? null : s.id)}
                          items={sessionMenu(s)}
                          onClose={closeMenu}
                        />
                      ))}
                      {hidden > 0 && (
                        <button
                          className="w-full flex items-center gap-1 px-2 py-1 rounded-md text-[11px] text-inkfaint hover:text-accent hover:bg-panel2/60 transition-colors"
                          title={expanded ? '收起多余对话' : `展开其余 ${hidden} 条对话`}
                          onClick={() => setShowAllSessions((c) => ({ ...c, [p.id]: !c[p.id] }))}
                        >
                          {expanded ? <ChevronDown size={11} className="shrink-0" /> : <ChevronRight size={11} className="shrink-0" />}
                          <span>{expanded ? '收起' : `更多 ${hidden} 条`}</span>
                          <span className="ml-auto font-mono text-[10px] opacity-70">{list.length}</span>
                        </button>
                      )}
                      {list.length === 0 && (
                        <div className="text-[11px] text-inkfaint py-1 px-2 italic">暂无对话</div>
                      )}
                    </div>
                  )}
                </div>
              )
            })}
            {wsProjects.length === 0 && (
              <div className="text-[11px] text-inkfaint py-1.5 px-2 italic">
                该工作空间暂无项目，点击 + 新建
              </div>
            )}
          </div>
        </div>

        {/* 纯对话分组 */}
        <div>
          <div className="flex items-center gap-1.5 px-2 py-1 text-inkdim">
            <MessageSquare size={13} className="text-inkfaint" />
            <span className="text-[12px] font-medium">纯对话</span>
            <span className="text-[10px] font-mono text-inkfaint">({looseSessions.length})</span>
          </div>
          <div className="space-y-0.5 mt-0.5">
            {looseVisible.map((s) => (
              <SessionRow
                key={s.id}
                s={s}
                activeId={currentId}
                onSelect={() => goChat(() => select(s.id))}
                menuOpen={menuFor === s.id}
                onMenu={() => setMenuFor(menuFor === s.id ? null : s.id)}
                items={sessionMenu(s)}
                onClose={closeMenu}
              />
            ))}
            {looseHidden > 0 && (
              <button
                className="w-full flex items-center gap-1 px-2 py-1 rounded-md text-[11px] text-inkfaint hover:text-accent hover:bg-panel2/60 transition-colors"
                title={looseExpanded ? '收起多余对话' : `展开其余 ${looseHidden} 条对话`}
                onClick={() => setShowAllSessions((c) => ({ ...c, __loose__: !c.__loose__ }))}
              >
                {looseExpanded ? <ChevronDown size={11} className="shrink-0" /> : <ChevronRight size={11} className="shrink-0" />}
                <span>{looseExpanded ? '收起' : `更多 ${looseHidden} 条`}</span>
                <span className="ml-auto font-mono text-[10px] opacity-70">{looseSessions.length}</span>
              </button>
            )}
            {looseSessions.length === 0 && (
              <div className="text-[11px] text-inkfaint py-1 px-2 italic">暂无对话</div>
            )}
          </div>
        </div>
      </div>

      {/* 底部功能区：高级 / 设置 */}
      <div className="shrink-0 border-t border-edge/50 p-2 flex gap-1.5 bg-panel2/20">
        <Button
          variant="ghost"
          size="sm"
          className="flex-1 justify-center gap-1.5 text-inkdim hover:text-ink text-[12px]"
          onClick={() => { setAdvanced(true); onNavigate?.() }}
        >
          <Sliders size={13} />
          <span>高级</span>
        </Button>
        <Button
          variant="ghost"
          size="sm"
          className="flex-1 justify-center gap-1.5 text-inkdim hover:text-ink text-[12px]"
          onClick={() => { setSettings(true); onNavigate?.() }}
        >
          <Settings size={13} />
          <span>设置</span>
        </Button>
      </div>

      <PromptModal
        open={Boolean(prompt)}
        title={prompt?.title ?? ''}
        confirmText={prompt?.confirmText}
        fields={prompt?.fields ?? []}
        onCancel={() => setPrompt(null)}
        onSubmit={(v) => prompt?.onSubmit(v)}
      />
      <ProjectModal
        project={settingsFor ? (projects.find((p) => p.id === settingsFor) ?? null) : null}
        onClose={() => setSettingsFor(null)}
      />
    </div>
  )
}

function NavRow({ icon, label, active, badge, onClick }: {
  icon: React.ReactNode
  label: string
  active: boolean
  badge?: string
  onClick: () => void
}) {
  return (
    <button
      onClick={onClick}
      title={label}
      className={cn(
        'w-full h-8 flex items-center gap-2 px-2.5 rounded-lg text-[12.5px] transition-all duration-150 select-none',
        active
          ? 'bg-white dark:bg-panel2 text-accent font-medium shadow-[0_1px_3px_-1px_rgba(0,0,0,0.12)]'
          : 'text-inkdim hover:text-ink hover:bg-panel2/70',
      )}
    >
      <span className={cn('shrink-0', active ? 'text-accent' : 'text-inkfaint')}>{icon}</span>
      <span className="flex-1 text-left truncate">{label}</span>
      {badge && <span className="font-mono text-[10px] text-inkfaint">{badge}</span>}
    </button>
  )
}

function Menu({ items, onClose }: { items: MenuItem[]; onClose: () => void }) {
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const handleDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        onClose()
      }
    }
    window.addEventListener('mousedown', handleDown)
    return () => window.removeEventListener('mousedown', handleDown)
  }, [onClose])

  return (
    <div
      ref={ref}
      className="absolute right-1 top-7 z-30 w-36 rounded-xl border border-edge bg-panel shadow-modal py-1 fade-in"
      onClick={(e) => e.stopPropagation()}
    >
      {items.map((it) => (
        <button
          key={it.label}
          className={cn(
            'w-full text-left px-2.5 py-1.5 text-[12px] flex items-center gap-2 hover:bg-panel2 transition-colors',
            it.danger ? 'text-err hover:bg-err/10' : 'text-ink',
          )}
          onClick={() => { it.onClick(); onClose() }}
        >
          {it.icon && <span className="shrink-0">{it.icon}</span>}
          <span>{it.label}</span>
        </button>
      ))}
    </div>
  )
}

function SessionRow({ s, activeId, onSelect, menuOpen, onMenu, items, onClose }: {
  s: { id: string; title: string; running?: boolean; unread?: boolean; accessMode: string }
  activeId: string
  onSelect: () => void
  menuOpen: boolean
  onMenu: () => void
  items: MenuItem[]
  onClose: () => void
}) {
  const isActive = activeId === s.id
  return (
    <div
      className={cn(
        'relative group flex items-center gap-2 px-2.5 py-1.5 rounded-xl transition-all duration-200 cursor-pointer select-none',
        isActive
          ? 'bg-white dark:bg-panel2 text-ink font-medium shadow-[0_2px_8px_-2px_rgba(0,0,0,0.08),0_1px_3px_-1px_rgba(0,0,0,0.04)] dark:shadow-[0_4px_12px_-2px_rgba(0,0,0,0.5),0_1px_3px_-1px_rgba(0,0,0,0.3)] -translate-y-[1px]'
          : 'text-inkdim hover:text-ink hover:bg-panel2/60 hover:-translate-y-[1px]',
      )}
      onClick={onSelect}
    >
      {s.running ? (
        <span className="relative flex h-1.5 w-1.5 shrink-0">
          <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-ok opacity-75" />
          <span className="relative inline-flex rounded-full h-1.5 w-1.5 bg-ok" />
        </span>
      ) : isActive ? (
        <span className="w-1.5 h-1.5 rounded-full bg-accent shrink-0 shadow-[0_0_6px_rgba(59,130,246,0.6)]" />
      ) : (
        <span className="w-1.5 h-1.5 rounded-full bg-edge2 shrink-0 group-hover:bg-inkfaint transition-colors" />
      )}

      <span className="flex-1 text-left text-[12px] truncate leading-snug">
        {s.title}
      </span>

      {s.unread && <span className="w-1.5 h-1.5 rounded-full bg-accent shrink-0" />}

      <button
        className={cn(
          'w-5 h-5 rounded hover:bg-panel3 flex items-center justify-center text-inkdim hover:text-ink shrink-0 transition-colors',
          menuOpen ? 'text-ink bg-panel3' : 'text-inkfaint hover:text-ink',
        )}
        title="更多操作"
        onClick={(e) => { e.stopPropagation(); onMenu() }}
      >
        <MoreHorizontal size={13} />
      </button>

      {menuOpen && <Menu items={items} onClose={onClose} />}
    </div>
  )
}
