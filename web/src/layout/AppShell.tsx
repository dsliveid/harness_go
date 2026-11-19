import { useEffect, useRef } from 'react'
import { Sparkles } from 'lucide-react'
import { useUI } from '../stores/uiStore'
import { useSessions } from '../stores/sessionStore'
import { useSettings } from '../stores/settingsStore'
import { useBreakpoint } from './useBreakpoint'
import { Sidebar } from './Sidebar'
import { TopBar } from './TopBar'
import { ChatView } from '../features/chat/ChatView'
import { MarketModal } from '../features/market/MarketPage'
import { ScheduledModal } from '../features/scheduled/ScheduledPage'
import { SettingsModal } from '../features/settings/SettingsModal'
import { AdvancedModal } from '../features/settings/AdvancedModal'
import { SidePanel } from '../features/advanced/SidePanel'
import { Drawer, ToastHost, cn } from '../components/ui'
import { applyUpdate, initPwa, usePwa } from '../lib/pwa'
import { HostSettingsFab } from './HostSettingsFab'
import { SessionEventsClient } from '../lib/sessionEvents'
import { toUiSession } from '../lib/api'

// 【PWA 降级】离线横幅已移除：登录后服务端执行的工具型应用，离线壳价值低；
// 仅保留「新版本已就绪」更新提示与主屏安装能力。
function UpdateBanner() {
  const { updateReady } = usePwa()
  if (!updateReady) return null
  return (
    <div className="shrink-0 select-none">
      <div className="px-3.5 py-1.5 bg-accent/10 border-b border-accent/30 text-[12px] text-accent flex items-center gap-2">
        <Sparkles size={13} className="shrink-0" />
        <span>新版本已就绪</span>
        <button className="underline font-medium hover:text-ink transition-colors ml-1" onClick={applyUpdate}>立即更新</button>
      </div>
    </div>
  )
}

// 移动端底部导航已移除：统一使用侧栏导航（移动端侧栏以抽屉形式弹出）

export function AppShell() {
  useBreakpoint()
  const { breakpoint, sidebarOpen, setSidebar, theme, panel } = useUI()
  const loadSessions = useSessions((s) => s.load)
  const loadProjects = useSessions((s) => s.loadProjects)
  const loadProviders = useSettings((s) => s.loadProviders)
  useEffect(() => {
    void initPwa()
  }, [])

  // 进入工作台即从主库拉取会话列表、项目列表与厂商配置（real 模式；mock 模式为空操作）。
  // 项目必须早于会话落地，否则会话归属（projectId）会因项目列表为空而反查失败、掉进纯对话。
  useEffect(() => {
    void (async () => {
      await loadProjects()
      await loadSessions()
      void loadProviders()
    })()
  }, [loadSessions, loadProjects, loadProviders])

  useEffect(() => {
    const root = document.documentElement
    root.classList.toggle('dark', theme === 'dark')
    root.classList.toggle('light', theme === 'light')
  }, [theme])

  // 会话列表实时订阅（/api/v1/sessions/events）：跨端同步「列表 + 每个会话的状态」。
  //
  // 在此之前只有「打开中的会话内消息」会自动刷新，左侧列表与运行中脉冲点要靠手动
  // 刷新页面才更新。这里补上缺失的那条通道。
  //
  // ⚠️ 依赖 sessionsReady：必须等 load() 拿到首份列表后再建连，
  // 否则快照帧可能与首屏 load() 的结果交错覆盖（load 里的 create() 会自动建一条
  // 空会话，若它晚于快照落地，就会把快照里已有的会话挤掉）。
  const sessionsReady = useSessions((s) => s.currentId !== '' || s.sessions.length > 0)
  const clientRef = useRef<SessionEventsClient | null>(null)
  // 领域列表重拉防抖计时器（projects / workspaces 各自独立）
  const domainTimers = useRef<{ projects?: ReturnType<typeof setTimeout>; workspaces?: ReturnType<typeof setTimeout> }>({})
  useEffect(() => {
    if (!sessionsReady) return
    if (clientRef.current) return // 幂等：已建连不重复建
    const client = new SessionEventsClient({
      onSnapshot: (all) => {
        const st = useSessions.getState()
        // 服务端视图 → UI 会话：复用 toUiSession 保证 running/archivedAt 的映射口径
        // 与 /sessions 完全一致（两处各写一套必然漂移）。
        const list = all.map((s) => toUiSession(s as Parameters<typeof toUiSession>[0]))
        st.applyServerSnapshot(list)
      },
      onSessionUpdated: (s) => {
        const st = useSessions.getState()
        st.applyServerPatch(toUiSession(s as Parameters<typeof toUiSession>[0]))
      },
      onSessionRemoved: (id) => {
        useSessions.getState().applyServerRemoval(id)
      },
      onDomainChanged: (domain) => {
        // 防抖 300ms：一次操作可能连带多个变更（如删除工作空间会级联迁移项目 → 两个信号），
        // 且多端短时间内的连续改动不应产生一串全量请求。
        //
        // 为什么是 300ms 而不是会话那种 1 秒：项目/工作空间变更**由用户手动触发**，
        // 保存后就盯着界面看，1 秒延迟会被感知为「点了没反应」。
        const existing = domainTimers.current[domain]
        if (existing) clearTimeout(existing)
        domainTimers.current[domain] = setTimeout(() => {
          domainTimers.current[domain] = undefined
          const st = useSessions.getState()
          // 🚨 必须调既有 load* 而不是自己 set 数据：
          // loadProjects 内含「服务端字段 + 本地 workspaceId 合并」逻辑，
          // 自己拼会丢掉前端独有的 workspaceId → 项目归属丢失。
          if (domain === 'projects') void st.loadProjects()
          else void st.loadWorkspaces()
        }, 300)
      },
    })
    client.connect()
    clientRef.current = client
    return () => {
      client.dispose()
      clientRef.current = null
      // 清理未触发的防抖计时器（否则卸载后仍会执行一次无效请求）
      for (const key of ['projects', 'workspaces'] as const) {
        const t = domainTimers.current[key]
        if (t) clearTimeout(t)
        domainTimers.current[key] = undefined
      }
    }
  }, [sessionsReady])

  // 断点变化时同步侧栏开合：移动端默认收起（抽屉形式），桌面/平板默认展开
  useEffect(() => {
    setSidebar(breakpoint !== 'mobile')
  }, [breakpoint, setSidebar])

  // 【布局定稿】右侧主内容恒为对话区：定时任务 / 插件市场 一律以弹窗覆盖，不替换对话区，
  // 因此打开/关闭这些弹窗不会影响对话的流式输出、输入草稿与滚动位置。
  const content = <ChatView />
  const isMobile = breakpoint === 'mobile'

  // 统一骨架：侧栏 + 主内容（三端同构；差异仅在侧栏展示形态）
  const sidebar = <Sidebar onNavigate={isMobile ? () => setSidebar(false) : undefined} />

  return (
    <div className="app-shell flex bg-bg text-ink">
      {isMobile ? (
        <Drawer open={sidebarOpen} onClose={() => setSidebar(false)} side="left" width="w-[86%] max-w-[320px]">
          {sidebar}
        </Drawer>
      ) : (
        <div className={cn('shrink-0 h-full overflow-hidden transition-[width] duration-200', sidebarOpen ? 'w-[264px]' : 'w-0')}>
          <div className="w-[264px] h-full">{sidebar}</div>
        </div>
      )}
      <div className="flex-1 flex flex-col min-w-0 h-full">
        <TopBar />
        <UpdateBanner />
        <div className="flex-1 flex min-h-0">
          <div className="flex-1 min-w-0 min-h-0">{content}</div>
          {panel !== 'none' && <SidePanel />}
        </div>
      </div>
      <SettingsModal />
      <AdvancedModal />
      <ScheduledModal />
      <MarketModal />
      <ToastHost />
      {/* 契约三：客户端壳内的「返回宿主设置」浮标（纯浏览器不渲染）。 */}
      <HostSettingsFab />
    </div>
  )
}
