import { useEffect, type ReactNode } from 'react'
import { Cpu, Globe, Shield, BarChart3, FileText, MonitorSmartphone } from 'lucide-react'
import { useUI } from '../../stores/uiStore'
import { useSettings } from '../../stores/settingsStore'
import { Button, Modal, ScrollFade } from '../../components/ui'
import { cn } from '../../components/ui'
import { ModelSettingsSection } from './ModelSettingsSection'
import { NetworkPanel } from './NetworkPanel'
import { SopPanel } from './SopPanel'
import { LogsPanel, TokenStatsPanel } from './StatsLogsPanel'
import { DevicePanel } from '../devices/DevicePanel'

type Tab = 'models' | 'network' | 'devices' | 'sop' | 'tokenstats' | 'logs'

const NAV_MAIN: Array<{ key: Tab; label: string; icon: ReactNode }> = [
  { key: 'models', label: '模型设置', icon: <Cpu size={14} className="text-accent shrink-0" /> },
  { key: 'network', label: '网络代理', icon: <Globe size={14} className="text-sky-400 shrink-0" /> },
  { key: 'devices', label: '设备', icon: <MonitorSmartphone size={14} className="text-emerald-400 shrink-0" /> },
  { key: 'sop', label: 'Agent SOP', icon: <Shield size={14} className="text-purple-400 shrink-0" /> },
]

const NAV_EXTRA: Array<{ key: Tab; label: string; icon: ReactNode }> = [
  { key: 'tokenstats', label: 'Token 统计', icon: <BarChart3 size={14} className="text-warn shrink-0" /> },
  { key: 'logs', label: '日志查询', icon: <FileText size={14} className="text-accent shrink-0" /> },
]

// 程序设置弹窗：对齐桌面端 SettingsModal（模型/工具/网络/SOP + Token 统计/日志查询）。
export function SettingsModal() {
  const { settingsOpen, settingsTab, setSettings, toast } = useUI()
  const { beginEdit, endEdit } = useSettings()

  useEffect(() => {
    if (settingsOpen) beginEdit()
  }, [settingsOpen, beginEdit])

  const close = (commit: boolean) => {
    endEdit(commit)
    setSettings(false)
    if (commit) toast('success', '设置已保存（原型模拟）')
  }

  if (!settingsOpen) return null
  const tab = settingsTab

  const menuCls = (active: boolean) =>
    cn(
      // 移动端为横向滚动菜单：按钮不参与收缩、文字强制单行，避免「模型设置」被挤成两行
      'shrink-0 whitespace-nowrap justify-center sm:justify-start sm:w-full text-center sm:text-left px-3 py-2 rounded-xl text-[13px] font-medium flex items-center gap-2.5 transition-all select-none',
      active
        ? 'bg-accent/15 text-accent border border-accent/25 shadow-subtle'
        : 'text-inkdim hover:text-ink hover:bg-panel2 border border-transparent',
    )

  return (
    <Modal
      open={settingsOpen}
      onClose={() => close(false)}
      title="程序设置"
      width="max-w-4xl"
      height="h-[min(620px,92dvh)] sm:h-[min(620px,86dvh)]"
      footer={
        <>
          <Button variant="ghost" onClick={() => close(false)}>取消</Button>
          <Button variant="primary" onClick={() => close(true)}>保存修改</Button>
        </>
      }
    >
      <div className="flex flex-col sm:flex-row h-full min-h-0">
        <aside className="sm:w-44 border-b sm:border-b-0 sm:border-r border-edge/80 p-2.5 shrink-0 min-w-0 bg-panel2/20">
          <ScrollFade className="flex sm:flex-col gap-1">
            {NAV_MAIN.map((it) => (
              <button key={it.key} className={menuCls(tab === it.key)} onClick={() => setSettings(true, it.key)}>
                {it.icon}
                <span>{it.label}</span>
              </button>
            ))}
            <div className="hidden sm:block border-t border-edge/60 my-1.5" />
            {NAV_EXTRA.map((it) => (
              <button key={it.key} className={menuCls(tab === it.key)} onClick={() => setSettings(true, it.key)}>
                {it.icon}
                <span>{it.label}</span>
              </button>
            ))}
          </ScrollFade>
        </aside>

        <div className="flex-1 min-w-0 min-h-0 overflow-y-auto p-4 sm:p-5">
          {tab === 'models' && <ModelSettingsSection />}
          {tab === 'network' && <NetworkPanel />}
          {tab === 'devices' && <DevicePanel />}
          {tab === 'sop' && <SopPanel />}
          {tab === 'tokenstats' && <TokenStatsPanel />}
          {tab === 'logs' && <LogsPanel />}
        </div>
      </div>
    </Modal>
  )
}
