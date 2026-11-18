import { useEffect, useState } from 'react'
import { Blocks, Wrench, Cpu, Sparkles, Boxes, Monitor, Smartphone, Wifi, WifiOff, Check, Server, Globe } from 'lucide-react'
import { usePlugins } from '../../stores/pluginStore'
import { useUI } from '../../stores/uiStore'
import { Badge, Button, Modal, Tabs, cn } from '../../components/ui'
import { ToolsPane, McpPane, SkillsPane, ExtensionsPane } from '../plugins/PluginsPage'
import { clientPlatformLabel, osLabel } from '../../lib/platform'
import type { MarketRegistryDTO } from '../../lib/apiTypes'
import type { DeviceNode } from '../../types'

// ============ 插件市场（弹窗） ============
// 工具 / MCP / 技能 / 拓展 四块能力统一收拢到本弹窗（原「高级 → 工具/MCP/技能」已移除）。
// 安装模型：按「管理目标」区分 —— 服务端 / 桌面端 / 移动端都是一等公民的管理目标。
//   - 服务端自身：内置工具随程序发布（**只有启用/停用，不可安装/卸载**）；
//     MCP / 技能 / 拓展与其他端一致（服务端本就是建连与安装宿主）。
//   - 客户端（PC / 移动）：可安装/卸载到本机，服务端不下发能力。
// 平台区分：PC 端分 Windows / Linux，另有移动端（iOS / Android），不兼容的能力直接禁用安装。
// 【布局定稿】市场以覆盖式弹窗呈现，不再替换右侧主内容；弹窗高度固定，仅内部列表滚动。

type MarketTab = 'tool' | 'mcp' | 'skill' | 'extension'

const ICON_OF_OS = { windows: Monitor, linux: Monitor, ios: Smartphone, android: Smartphone, web: Server } as const

function ClientChip({ client, active, onClick }: { client: DeviceNode; active: boolean; onClick: () => void }) {
  const Icon = ICON_OF_OS[client.os] ?? Monitor
  const isServer = client.platform === 'server'
  return (
    <button
      onClick={onClick}
      title={isServer
        ? '以「服务端」的身份管理能力：内置工具仅启用/停用（随程序发布，不可装缷）'
        : `以「${client.name}」的身份浏览与安装（安装动作只作用于本端）`}
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

/**
 * 外部市场说明条。
 *
 * 用户明确要求「对接那个市场需要先明确说明」，因此这条说明常驻在弹窗顶部：
 *   ① 制品来自**外部 registry**（不是本机、不是随程序发布）；
 *   ② 服务端会下载并落盘执行，所以强制 sha256 完整性 + ed25519 验签；
 *   ③ 未配置/已禁用/拉取失败时**显示原因**，绝不用空列表冒充「市场为空」。
 */
function ExternalMarketNotice({
  market, loading, onRetry,
}: {
  market: MarketRegistryDTO | null
  loading: boolean
  onRetry: () => void
}) {
  const status = market?.status ?? 'unconfigured'
  const ok = status === 'ready'
  return (
    <div
      className={cn(
        'rounded-xl border px-3 py-2 mb-3 text-[11.5px] leading-relaxed',
        ok ? 'border-edge bg-panel2/50 text-inkfaint' : 'border-warn/40 bg-warn/[0.06] text-warn',
      )}
    >
      <div className="flex items-center gap-2 flex-wrap">
        <Globe size={13} className="shrink-0" />
        <span className="font-medium text-ink">外部市场</span>
        <Badge tone={ok ? 'green' : status === 'error' ? 'red' : 'yellow'}>
          {loading ? '校验中' : ok ? '已配置' : status === 'error' ? '拉取失败' : status === 'disabled' ? '未启用' : '未配置'}
        </Badge>
        {ok && market?.trust && <span className="text-[11px]">{market.trust}</span>}
        {ok && market?.cached && <Badge tone="gray">缓存</Badge>}
        <span className="flex-1" />
        <Button size="sm" variant="ghost" onClick={onRetry} disabled={loading}>
          {loading ? '读取中…' : '重新拉取'}
        </Button>
      </div>
      <div className="mt-1">
        制品来自<span className="font-medium">外部 registry</span>
        {ok && market?.registry_url
          ? <code className="font-mono mx-1 px-1 rounded bg-panel2">{market.registry_url}</code>
          : null}
        ：服务端会下载后落盘，因此强制 <span className="font-medium">sha256 完整性校验</span> +
        <span className="font-medium">ed25519 验签</span>；未签名且未放行的制品一律拒绝。
        与「系统内置工具」不同 —— 内置工具/工具包随服务端发布，无需下载。
        {!ok && market?.reason && <div className="mt-1 font-medium">当前不可用：{market.reason}</div>}
      </div>
    </div>
  )
}

export function MarketModal() {
  const {
    devices, currentClientId, setCurrentClient, loadRemoteCapabilities,
    marketRegistry, marketLoading, loadMarketRegistry,
  } = usePlugins()
  const { pluginsOpen, setPlugins, toast } = useUI()
  const [tab, setTab] = useState<MarketTab>('tool')

  // 打开即拉取真值：服务端节点（含内置工具停用清单）与客户端节点都由此同步；
  // 同时拉一次外部市场状态，让顶部说明条立刻显示「已配置 / 未配置 / 拉取失败」。
  useEffect(() => {
    if (pluginsOpen) {
      void loadRemoteCapabilities()
      void loadMarketRegistry()
    }
  }, [pluginsOpen, loadRemoteCapabilities, loadMarketRegistry])

  // 管理目标清单：服务端自身恒在首位（它永远存在且在线），其后是已配对的客户端。
  // 服务端不能被过滤掉：它承载内置工具（仅启停）且是 MCP / 工具包的真实安装宿主。
  const clients = [...devices].sort((a, b) => {
    const rank = (d: DeviceNode) => (d.platform === 'server' ? 0 : d.platform === 'desktop' ? 1 : 2)
    return rank(a) - rank(b)
  })
  const client = clients.find((d) => d.id === currentClientId) ?? clients[0]
  const isServer = client?.platform === 'server'
  const server = devices.find((d) => d.platform === 'server')

  // 🚨 空态必须早退（2026-11 登录后黑屏事故根因）：
  // 尚未拉取（devices 为空）时 client === undefined。本组件下方多处直接读
  // `client.online` / `client.os` / `clientPlatformLabel(client)`，一旦为 undefined 就抛
  // `TypeError: Cannot read properties of undefined (reading 'online')`
  // —— 由于该弹窗所在的组件树与主界面共享根节点，异常会让 React 卸载整棵树，
  // 表现为**登录后整页纯黑，无任何提示**。
  // 注意：正常情况下服务端节点一定会被 loadRemoteCapabilities 注入，所以这里只兜
  // 「拉取尚未完成 / 拉取失败」的瞬时态，绝不再把「没有配对客户端」当成空态（服务端即可管理）。
  if (!client) {
    return (
      <Modal
        open={pluginsOpen}
        onClose={() => setPlugins(false)}
        title={
          <span className="flex items-center gap-2">
            <Blocks size={15} className="text-accent" />
            <span>插件市场</span>
          </span>
        }
        width="max-w-xl"
        height="h-auto"
      >
        <div className="flex flex-col items-center justify-center gap-2 px-6 py-12 text-center">
          <Blocks size={26} className="text-inkfaint" />
          <div className="text-[13px] font-medium text-ink">正在读取可用管理目标…</div>
          <div className="text-[12px] text-inkfaint leading-relaxed max-w-[360px]">
            服务端能力与客户端能力都从服务端拉取。若长时间停留在此，请确认服务端已启动并已登录。
          </div>
          <Button size="sm" variant="outline" className="mt-1" onClick={() => void loadRemoteCapabilities()}>
            重试
          </Button>
        </div>
      </Modal>
    )
  }

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
        {/* 固定头：当前管理目标 + 一级分类（不随列表滚动、不改变弹窗高度） */}
        <div className="shrink-0 border-b border-edge/60 bg-panel2/30 px-4 pt-2.5 pb-2">
          <div className="flex items-center gap-2 flex-wrap mb-1.5">
            <span className="text-[12px] font-semibold text-ink">当前管理目标</span>
            <span className="text-[11px] text-inkfaint">
              {isServer
                ? '内置工具仅启用/停用（随程序发布，不可装缷）；MCP / 技能 / 拓展与其他端一致'
                : '安装与卸载只作用于所选客户端本机'}
            </span>
            <span className="flex-1" />
            {server && !isServer && (
              <span className="hidden sm:inline-flex items-center gap-1 text-[11px] text-inkfaint" title="服务端也是一类管理目标，可在左侧切换">
                <Server size={10} />
                {server.name}：可管理
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
                  toast(
                    'info',
                    c.platform === 'server'
                      ? '已切换到服务端：内置工具仅启用/停用'
                      : `已切换到 ${c.name}，安装将作用于本端`,
                  )
                }}
              />
            ))}
          </div>

          <div className="mt-1.5 flex items-center gap-1.5 text-[11px] text-inkfaint flex-wrap">
            {isServer ? (
              <span className="inline-flex items-center gap-1 text-ok/90"><Wifi size={10} />服务端在线</span>
            ) : client.online ? (
              <span className="inline-flex items-center gap-1 text-ok/90"><Wifi size={10} />在线</span>
            ) : (
              <span className="inline-flex items-center gap-1"><WifiOff size={10} />离线（安装任务排队，上线后执行）</span>
            )}
            <span className="opacity-40">·</span>
            <span>平台判定：{clientPlatformLabel(client)} · {osLabel(client.os)}</span>
            <span className="opacity-40">·</span>
            <span>
              {isServer
                ? '服务端能力随程序发布，内置工具只能启用/停用'
                : 'PC 端区分 Windows / Linux，移动端另计一档'}
            </span>
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
          <ExternalMarketNotice market={marketRegistry} loading={marketLoading} onRetry={() => void loadMarketRegistry()} />
          {tab === 'tool' && <ToolsPane client={client} />}
          {tab === 'mcp' && <McpPane client={client} />}
          {tab === 'skill' && <SkillsPane client={client} />}
          {tab === 'extension' && <ExtensionsPane client={client} />}
        </div>
      </div>
    </Modal>
  )
}
