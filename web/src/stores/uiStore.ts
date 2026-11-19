import { create } from 'zustand'
import type { Breakpoint, ToastMsg } from '../types'

// 【布局定稿】对话区（右侧主内容）常驻，不再做整页切换：
// 「定时任务」「插件市场」改为覆盖式弹窗，打开/关闭不影响对话区的运行态、输入草稿与滚动位置。
export type SidePanel = 'none' | 'todo' | 'plan' | 'commands' | 'runs' | 'subagent' | 'snapshot' | 'file'

interface UIState {
  theme: 'dark' | 'light'
  breakpoint: Breakpoint
  sidebarOpen: boolean
  toggleSidebar: () => void
  /** 定时任务弹窗 */
  scheduledOpen: boolean
  /** 插件市场弹窗 */
  pluginsOpen: boolean
  settingsOpen: boolean
  advancedOpen: boolean
  advancedTab: 'workspace' | 'archive' | 'data' | 'theme'
  settingsTab: 'models' | 'network' | 'devices' | 'sop' | 'webhooks' | 'tokenstats' | 'llmLogs' | 'logs'
  panel: SidePanel
  /** 当前打开的子进程对话 id（panel === 'subagent'） */
  subagentId: string | null
  /** 当前打开的文件详情路径（panel === 'file'） */
  filePath: string | null
  /**
   * 主对话「定位出处」请求（一次性、非持久）。
   *
   * 为什么放在全局 store 而不是 props：发起方是右侧方案面板（features/advanced/SidePanel），
   * 消费方是对话区虚拟列表（features/chat/MessageList），二者在组件树上互不相邻
   * （SidePanel 是 overlay/侧栏，ChatView 是主内容），props 传不通。
   *
   * nonce 必须自增：用户可能对同一目标连点两次（第一次滚走了想再回来），
   * 若只传 messageId，React 的依赖比较会认为「没变化」而不再触发滚动。
   */
  focusMessage: { messageId: string; nonce: number } | null
  toasts: ToastMsg[]
  toggleTheme: () => void
  setBreakpoint: (b: Breakpoint) => void
  setScheduled: (open: boolean) => void
  setPlugins: (open: boolean) => void
  setSidebar: (v: boolean) => void
  setAdvanced: (open: boolean, tab?: UIState['advancedTab']) => void
  setSettings: (open: boolean, tab?: UIState['settingsTab']) => void
  setPanel: (p: SidePanel) => void
  /** 打开子进程对话详情浮窗（执行过程卡片入口，无返回列表） */
  setSubagent: (id: string) => void
  /** 在子进程列表面板内打开详情（支持「返回」回列表） */
  openRunsDetail: (id: string) => void
  /** 打开文件详情浮窗（接收会话内已出现过的路径） */
  openFileDetail: (path: string) => void
  /**
   * 请求主对话滚动定位到某条消息并高亮（「定位出处」）。
   *
   * 注意：调用方**应先关闭侧面板**（移动端 Drawer 会遮住对话区）；
   * 本动作只负责发出请求，不理解也不关心面板状态。
   */
  locateMessage: (messageId: string) => void
  /** 消费/清除定位请求（对话区完成滚动后调用，避免切换会话时重复定位） */
  clearFocusMessage: () => void
  toast: (type: ToastMsg['type'], text: string) => void
  dismissToast: (id: string) => void
}

export const useUI = create<UIState>((set, get) => ({
  theme: 'dark',
  breakpoint: 'desktop',
  sidebarOpen: true,
  scheduledOpen: false,
  pluginsOpen: false,
  settingsOpen: false,
  advancedOpen: false,
  advancedTab: 'workspace',
  settingsTab: 'models',
  panel: 'none',
  subagentId: null,
  filePath: null,
  focusMessage: null,
  toasts: [],

  toggleTheme: () => set((s) => ({ theme: s.theme === 'dark' ? 'light' : 'dark' })),
  setBreakpoint: (breakpoint) => set({ breakpoint }),
  setScheduled: (scheduledOpen) => set({ scheduledOpen }),
  setPlugins: (pluginsOpen) => set({ pluginsOpen }),
  setSidebar: (sidebarOpen) => set({ sidebarOpen }),
  toggleSidebar: () => set((s) => ({ sidebarOpen: !s.sidebarOpen })),
  setAdvanced: (advancedOpen, advancedTab) =>
    set((s) => ({ advancedOpen, advancedTab: advancedTab ?? s.advancedTab })),
  setSettings: (settingsOpen, settingsTab) =>
    set((s) => ({ settingsOpen, settingsTab: settingsTab ?? s.settingsTab })),
  setPanel: (panel) => set({ panel, subagentId: panel === 'subagent' ? get().subagentId : null, filePath: panel === 'file' ? get().filePath : null }),

  setSubagent: (subagentId) => set({ panel: 'subagent', subagentId, filePath: null }),

  openRunsDetail: (subagentId) => set({ panel: 'runs', subagentId, filePath: null }),

  openFileDetail: (filePath) => set({ panel: 'file', filePath, subagentId: null }),

  locateMessage: (messageId) =>
    set((s) => ({
      // nonce 单调自增：同一 id 连点两次也必然产生新的 nonce，从而重新触发滚动
      focusMessage: { messageId, nonce: (s.focusMessage?.nonce ?? 0) + 1 },
    })),

  clearFocusMessage: () => set({ focusMessage: null }),

  toast: (type, text) => {
    const id = `t-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`
    set((s) => ({ toasts: [...s.toasts, { id, type, text }] }))
    setTimeout(() => get().dismissToast(id), 3200)
  },
  dismissToast: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),
}))
