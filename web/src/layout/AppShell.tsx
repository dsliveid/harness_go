import { useEffect } from 'react'
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
  const loadProviders = useSettings((s) => s.loadProviders)
  useEffect(() => {
    void initPwa()
  }, [])

  // 进入工作台即从主库拉取会话列表与厂商配置（real 模式；mock 模式为空操作）
  useEffect(() => {
    void loadSessions()
    void loadProviders()
  }, [loadSessions, loadProviders])

  useEffect(() => {
    const root = document.documentElement
    root.classList.toggle('dark', theme === 'dark')
    root.classList.toggle('light', theme === 'light')
  }, [theme])

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
    </div>
  )
}
