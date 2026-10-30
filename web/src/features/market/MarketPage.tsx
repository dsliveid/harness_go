import { useState } from 'react'
import { Blocks, Wrench, Cpu, Sparkles, Boxes, Monitor, Smartphone, Wifi, WifiOff, Check } from 'lucide-react'
import { usePlugins } from '../../stores/pluginStore'
import { useUI } from '../../stores/uiStore'
import { Badge, Modal, Tabs, cn } from '../../components/ui'
import { RealModeNotice } from '../../components/RealModeNotice'
import { ToolsPane, McpPane, SkillsPane, ExtensionsPane } from '../plugins/PluginsPage'
import { clientPlatformLabel, osLabel } from '../../lib/platform'
import type { DeviceNode } from '../../types'

// ============ 插件市场（弹窗） ============
// 工具 / MCP / 技能 / 拓展 四块能力统一收拢到本弹窗（原「高级 → 工具/MCP/技能」已移除）。
// 安装模型：客户端自行安装 —— 顶部选定「当前客户端」，本页所有安装/卸载只作用于该端自身。
// 平台区分：PC 端分 Windows / Linux，另有移动端（iOS / Android），不兼容的能力直接禁用安装。
// 【布局定稿】市场以覆盖式弹窗呈现，不再替换右侧主内容；弹窗高度固定，仅内部列表滚动。

type MarketTab = 'tool' | 'mcp' | 'skill' | 'extension'

const ICON_OF_OS = { windows: Monitor, linux: Monitor, ios: Smartphone, android: Smartphone, web: Blocks } as const

function ClientChip({ client, active, onClick }: { client: DeviceNode; active: boolean; onClick: () => void }) {
  const Icon = ICON_OF_OS[client.os] ?? Monitor
  return (
    <button
      onClick={onClick}
      title={`以「${client.name}」的身份浏览与安装（安装动作只作用于本端）`}
      className={cn(
        'flex items-center gap-2 rounded-xl border px-2.5 py-1.5 text-left transition-all select-none shrink-0',
        active
          ? 'border-accent/50 bg-accent/10 shadow-subtle'
          : 'border-edge bg-panel hover:border-edge2 hover:bg-panel2/60',
      )}
    >
      <span className={cn('shrink-0', active ? 'text-accent' : 'text-inkfaint')}>
        <Icon size={14} />
      </span>
      <span className="min-w-0">
        <span className={cn('flex items-center gap-1 text-[12px] font-medium truncate', active ? 'text-accent' : 'text-ink')}>
          {client.name}
          {active && <Check size={10} className="shrink-0" />}
        </span>
        <span className="flex items-center gap-1 text-[10.5px] text-inkfaint">
          <span className={cn('w-1.5 h-1.5 rounded-full shrink-0', client.online ? 'bg-ok' : 'bg-inkfaint')} />
          {clientPlatformLabel(client)} · {osLabel(client.os)}
          <span className="opacity-60">已装 {client.installed.length}</span>
          {client.installing.length > 0 && <span className="text-warn">安装中 {client.installing.length}</span>}
        </span>
      </span>
    </button>
  )
}

export function MarketModal() {
  const { devices, currentClientId, setCurrentClient } = usePlugins()
  const { pluginsOpen, setPlugins, toast } = useUI()
  const [tab, setTab] = useState<MarketTab>('tool')

  const clients = devices.filter((d) => d.platform !== 'server')
  const client = devices.find((d) => d.id === currentClientId) ?? clients[0]
  const server = devices.find((d) => d.platform === 'server')

  return (
    <Modal
      open={pluginsOpen}
      onClose={() => setPlugins(false)}
      title={
        <span className="flex items-center gap-2">
          <Blocks size={15} className="text-accent" />
          <span>插件市场</span>
          <Badge tone="blue">工具 · MCP · 技能 · 拓展</Badge>
        </span>
      }
      width="max-w-4xl"
      height="h-[min(780px,92dvh)] sm:h-[min(780px,86dvh)]"
    >
      <div className="flex flex-col h-full min-h-0">
        <RealModeNotice text="插件市场仍是本地演示数据：市场包、签名校验与安装下发属阶段 E/F，尚未接入服务端。" />
        {/* 固定头：当前客户端 + 一级分类（不随列表滚动、不改变弹窗高度） */}
        <div className="shrink-0 border-b border-edge/60 bg-panel2/30 px-4 pt-2.5 pb-2">
          <div className="flex items-center gap-2 flex-wrap mb-1.5">
            <span className="text-[12px] font-semibold text-ink">当前客户端</span>
            <span className="text-[11px] text-inkfaint">安装与卸载只作用于所选客户端本机</span>
            <span className="flex-1" />
            {server && (
              <span className="hidden sm:inline-flex items-center gap-1 text-[11px] text-inkfaint" title="服务端不再下发能力，仅提供调度与 API 代理">
                <Wifi size={10} />
                {server.name}：仅调度与代理
              </span>
            )}
          </div>

          <div className="flex items-stretch gap-2 overflow-x-auto no-scrollbar pb-0.5">
            {clients.map((c) => (
              <ClientChip
                key={c.id}
                client={c}
                active={c.id === client.id}
                onClick={() => {
                  setCurrentClient(c.id)
                  toast('info', `已切换到 ${c.name}，安装将作用于本端`)
                }}
              />
            ))}
          </div>

          <div className="mt-1.5 flex items-center gap-1.5 text-[11px] text-inkfaint flex-wrap">
            {client.online ? (
              <span className="inline-flex items-center gap-1 text-ok/90"><Wifi size={10} />在线</span>
            ) : (
              <span className="inline-flex items-center gap-1"><WifiOff size={10} />离线（安装任务排队，上线后执行）</span>
            )}
            <span className="opacity-40">·</span>
            <span>平台判定：{clientPlatformLabel(client)} · {osLabel(client.os)}</span>
            <span className="opacity-40">·</span>
            <span>PC 端区分 Windows / Linux，移动端另计一档</span>
          </div>

          <div className="mt-2">
            <Tabs
              items={[
                { key: 'tool', label: <span className="flex items-center gap-1.5"><Wrench size={13} className="text-emerald-400" /><span>工具</span></span> },
                { key: 'mcp', label: <span className="flex items-center gap-1.5"><Cpu size={13} className="text-accent" /><span>MCP</span></span> },
                { key: 'skill', label: <span className="flex items-center gap-1.5"><Sparkles size={13} className="text-accent2" /><span>技能</span></span> },
                { key: 'extension', label: <span className="flex items-center gap-1.5"><Boxes size={13} className="text-cyan-400" /><span>拓展</span></span> },
              ]}
              value={tab}
              onChange={(v) => setTab(v as MarketTab)}
            />
          </div>
        </div>

        {/* 滚动区：四个 pane 内容各自滚动，弹窗高度保持不变 */}
        <div className="flex-1 overflow-y-auto min-h-0 px-4 py-3">
          {tab === 'tool' && <ToolsPane client={client} />}
          {tab === 'mcp' && <McpPane client={client} />}
          {tab === 'skill' && <SkillsPane client={client} />}
          {tab === 'extension' && <ExtensionsPane client={client} />}
        </div>
      </div>
    </Modal>
  )
}
