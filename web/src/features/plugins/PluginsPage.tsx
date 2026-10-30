import { useState } from 'react'
import { Search, X, Download, Trash2, Sparkles, Cpu } from 'lucide-react'
import { usePlugins } from '../../stores/pluginStore'
import { useUI } from '../../stores/uiStore'
import { Badge, Button, Empty, Progress, StateBadge, Tabs, cn } from '../../components/ui'
import type {
  DeviceNode, ExtensionItem, ExtensionKind, McpServer, PlatformSupport, RegistryItem, Risk,
} from '../../types'
import { toolInstallInfo, toolList } from '../../mock/settingsData'
import { supportsClient, unsupportedReason, clientPlatformLabel } from '../../lib/platform'
import { ToolsPanel } from '../settings/ToolsPanel'

// ============ 能力面板（客户端自行安装）============
// 口径：
//   系统内置 = 随程序发布、各端可自行安装；市场 = 远端 registry 候选池；
//   已安装   = 当前客户端真实落地清单（key 前缀解析）；拓展 = 程序自我扩展产出的能力。
// 安装动作只作用于「当前客户端」自身，服务端不再下发。

type ViewTab = 'builtin' | 'installed' | 'market' | 'extended'

export type CapabilityKind = 'tool' | 'mcp' | 'skill' | 'ext' | 'reg'

export const KEY_PREFIX: Record<Exclude<CapabilityKind, 'tool'>, string> = {
  mcp: 'mcp:', skill: 'skill:', ext: 'ext:', reg: 'reg:',
}

const KIND_TEXT: Record<CapabilityKind, string> = {
  tool: '工具', mcp: 'MCP', skill: '技能', ext: '拓展', reg: '市场制品',
}

const KIND_TONE: Record<CapabilityKind, 'blue' | 'purple' | 'cyan' | 'green' | 'gray'> = {
  tool: 'green', mcp: 'blue', skill: 'purple', ext: 'cyan', reg: 'gray',
}

// ---------------- 通用小组件 ----------------
function PageHint({ children }: { children: React.ReactNode }) {
  return (
    <div className="rounded-xl border border-edge bg-panel2/50 p-2.5 text-[12px] text-inkfaint mb-3 leading-relaxed">
      {children}
    </div>
  )
}

function SearchBar({ value, onChange, placeholder }: { value: string; onChange: (v: string) => void; placeholder: string }) {
  return (
    <div className="relative w-full">
      <span className="absolute left-3 top-1/2 -translate-y-1/2 text-inkfaint pointer-events-none">
        <Search size={14} />
      </span>
      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') onChange((e.target as HTMLInputElement).value)
        }}
        placeholder={placeholder}
        className="h-8 w-full rounded-xl bg-panel2 border border-edge pl-8.5 pr-8 text-[13px] text-ink outline-none transition-all duration-150 focus:border-accent focus:ring-1 focus:ring-accent/30 placeholder:text-inkfaint"
      />
      {value && (
        <button
          className="absolute right-2.5 top-1/2 -translate-y-1/2 w-4 h-4 rounded hover:bg-panel3 flex items-center justify-center text-inkfaint hover:text-ink transition-colors"
          title="清空"
          onClick={() => onChange('')}
        >
          <X size={12} />
        </button>
      )}
    </div>
  )
}

/** 平台支持标签：Windows / Linux / 移动端；当前端支持的一档高亮，不支持的一档划掉 */
function PlatformTags({ platforms, client }: { platforms?: PlatformSupport; client: DeviceNode }) {
  const items: Array<{ key: 'windows' | 'linux' | 'mobile'; label: string; ok: boolean }> = [
    { key: 'windows', label: 'Windows', ok: platforms ? platforms.windows : true },
    { key: 'linux', label: 'Linux', ok: platforms ? platforms.linux : true },
    { key: 'mobile', label: '移动端', ok: platforms ? platforms.mobile : true },
  ]
  const current = client.platform === 'mobile' ? 'mobile' : client.os
  return (
    <span className="inline-flex items-center gap-1 flex-wrap">
      <span className="text-[10px] text-inkfaint">{clientPlatformLabel(client)}支持</span>
      {items.map((i) => (
        <span
          key={i.key}
          className={cn(
            'px-1 py-[1px] rounded border text-[10px] font-mono leading-[14px]',
            i.ok ? 'border-edge bg-panel2 text-inkdim' : 'border-edge/40 bg-panel2/30 text-inkfaint line-through',
            i.ok && i.key === current && 'border-accent/40 bg-accent/10 text-accent',
          )}
        >
          {i.label}
        </span>
      ))}
    </span>
  )
}

/** 安装动作：客户端自行安装 / 卸载；平台不兼容则禁用 */
function InstallAction({ itemKey, platforms, client, disabled, note }: {
  itemKey: string
  platforms?: PlatformSupport
  client: DeviceNode
  disabled?: boolean
  note?: string
}) {
  const { installOnClient, uninstallOnClient } = usePlugins()
  const toast = useUI((s) => s.toast)
  const installed = client.installed.includes(itemKey)
  const installing = client.installing.includes(itemKey)
  const compatible = supportsClient(platforms, client.os)

  if (disabled) {
    return <Button size="sm" variant="ghost" disabled title={note ?? '该能力不提供独立安装'}>不可安装</Button>
  }
  if (!compatible) {
    return <Button size="sm" variant="ghost" disabled title={unsupportedReason(platforms, client)}>该端不支持</Button>
  }
  if (installed) {
    return (
      <Button
        size="sm"
        variant="ghost"
        className="text-inkdim hover:text-err"
        title={`从 ${client.name} 卸载`}
        onClick={() => {
          uninstallOnClient(itemKey)
          toast('info', `已从 ${client.name} 卸载 ${itemKey}`)
        }}
      >
        已安装 · 卸载
      </Button>
    )
  }
  return (
    <Button
      size="sm"
      variant="primary"
      disabled={installing}
      title={`在当前客户端（${client.name}）本地安装`}
      onClick={() => {
        installOnClient(itemKey)
        toast('info', `正在安装到 ${client.name}（本端安装）`)
      }}
    >
      {installing ? '安装中…' : '安装到此端'}
    </Button>
  )
}

function SourceTabs({ view, onView, counts }: {
  view: ViewTab
  onView: (v: ViewTab) => void
  counts: { builtin: number; installed: number; extended: number }
}) {
  return (
    <Tabs
      items={[
        { key: 'builtin', label: `系统内置 (${counts.builtin})` },
        { key: 'installed', label: `已安装 (${counts.installed})` },
        { key: 'market', label: '市场' },
        { key: 'extended', label: `拓展 (${counts.extended})` },
      ]}
      value={view}
      onChange={(v) => onView(v as ViewTab)}
    />
  )
}

// ---------------- 已安装清单（解析 key → 具体能力） ----------------
interface InstalledEntry {
  key: string
  kind: CapabilityKind
  name: string
  description: string
  platforms?: PlatformSupport
  version?: string
}

function resolveInstalled(client: DeviceNode, kinds: CapabilityKind[]): InstalledEntry[] {
  const { registryItems, mcpServers, skills, extensions } = usePlugins.getState()
  const out: InstalledEntry[] = []
  client.installed.forEach((key) => {
    let entry: InstalledEntry | undefined
    if (key.startsWith(KEY_PREFIX.mcp)) {
      const name = key.slice(KEY_PREFIX.mcp.length)
      const m = mcpServers.find((x) => x.name === name)
      if (m) entry = { key, kind: 'mcp', name, description: m.command ?? m.url ?? '', platforms: m.platforms }
    } else if (key.startsWith(KEY_PREFIX.skill)) {
      const name = key.slice(KEY_PREFIX.skill.length)
      const k = skills.find((x) => x.name === name)
      if (k) entry = { key, kind: 'skill', name, description: k.description, platforms: k.platforms }
    } else if (key.startsWith(KEY_PREFIX.ext)) {
      const name = key.slice(KEY_PREFIX.ext.length)
      const e = extensions.find((x) => x.name === name)
      if (e) entry = { key, kind: 'ext', name, description: e.description, version: e.version }
    } else if (key.startsWith(KEY_PREFIX.reg)) {
      const id = key.slice(KEY_PREFIX.reg.length)
      const r = registryItems.find((x) => x.id === id)
      if (r) {
        entry = {
          key, kind: 'reg', name: r.name, description: r.description,
          platforms: r.platforms, version: r.version,
          // 制品按自身 kind 归类（plugin → tool / mcp / skill）
        }
        if (r.kind === 'mcp') entry.kind = 'mcp'
        else if (r.kind === 'skill') entry.kind = 'skill'
        else entry.kind = 'tool'
      }
    } else {
      const t = toolList.find((x) => x.name === key)
      if (t) {
        entry = {
          key, kind: 'tool', name: t.name, description: t.description,
          platforms: toolInstallInfo(t.name, t.group).platforms,
        }
      }
    }
    if (entry && kinds.includes(entry.kind)) out.push(entry)
  })
  return out
}

function InstalledPane({ client, kinds, hint }: { client: DeviceNode; kinds: CapabilityKind[]; hint: React.ReactNode }) {
  const list = resolveInstalled(client, kinds)
  return (
    <>
      <PageHint>{hint}</PageHint>
      <div className="space-y-2">
        {list.map((i) => (
          <div key={i.key} className="rounded-xl border border-edge bg-panel p-3 shadow-subtle hover:border-edge2 transition-all">
            <div className="flex items-center gap-2 flex-wrap">
              <span className="text-[13px] font-medium text-ink">{i.name}</span>
              <Badge tone={KIND_TONE[i.kind]}>{KIND_TEXT[i.kind]}</Badge>
              {i.version && <Badge>{i.version}</Badge>}
              <Badge tone="green">已安装</Badge>
              <span className="flex-1" />
              <Button
                size="sm"
                variant="ghost"
                className="gap-1 text-inkdim hover:text-err"
                onClick={() => usePlugins.getState().uninstallOnClient(i.key)}
              >
                <Trash2 size={11} />
                卸载
              </Button>
            </div>
            <div className="text-[12px] text-inkfaint mt-1 leading-relaxed truncate">{i.description || '—'}</div>
            <div className="mt-1.5"><PlatformTags platforms={i.platforms} client={client} /></div>
          </div>
        ))}
        {list.length === 0 && (
          <Empty
            icon={<Download size={22} className="text-inkfaint" />}
            title={`${client.name} 暂无已安装${kinds.map((k) => KIND_TEXT[k]).join(' / ')}`}
            hint="在「系统内置」或「市场」中点击「安装到此端」即可落地到当前客户端"
          />
        )}
      </div>
    </>
  )
}

// ---------------- 市场制品卡片 ----------------
function RegistryCard({ r, client }: { r: RegistryItem; client: DeviceNode }) {
  const { installRegistry } = usePlugins()
  const toast = useUI((s) => s.toast)
  const key = `${KEY_PREFIX.reg}${r.id}`
  const installed = client.installed.includes(key)
  const compatible = supportsClient(r.platforms, client.os)

  return (
    <div className="rounded-xl border border-edge bg-panel p-3.5 shadow-subtle hover:border-edge2 transition-all">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-[13px] font-semibold text-ink">{r.name}</span>
        <Badge>{r.version}</Badge>
        <Badge tone={r.signature === 'signed' ? 'green' : 'yellow'}>{r.signature === 'signed' ? '已签名' : '未签名'}</Badge>
        <span className="flex-1" />
        {installed ? (
          <InstallAction itemKey={key} platforms={r.platforms} client={client} />
        ) : (
          <Button
            size="sm"
            variant="primary"
            disabled={!compatible || Boolean(r.installing)}
            title={!compatible ? unsupportedReason(r.platforms, client) : `下载并安装到 ${client.name}`}
            onClick={() => {
              installRegistry(r.id)
              toast('info', `开始下载 ${r.name}（${r.sizeMb}MB）并安装到 ${client.name}`)
            }}
          >
            {r.installing ? '安装中…' : compatible ? '安装到此端' : '该端不支持'}
          </Button>
        )}
      </div>
      <div className="text-[12px] text-inkdim mt-1.5 leading-relaxed">{r.description}</div>
      <div className="mt-1.5 flex flex-wrap items-center gap-2 text-[11px] text-inkfaint font-mono">
        <span>体积 {r.sizeMb}MB</span>
        <span>依赖 {r.requires.length ? r.requires.join(', ') : '无'}</span>
        <PlatformTags platforms={r.platforms} client={client} />
      </div>
      {r.installing && <Progress value={r.progress ?? 0} className="mt-2.5" />}
    </div>
  )
}

// ---------------- 工具 ----------------
export function ToolsPane({ client }: { client: DeviceNode }) {
  const { registryItems, extensions } = usePlugins()
  const [view, setView] = useState<ViewTab>('builtin')
  const [q, setQ] = useState('')
  const kw = q.trim().toLowerCase()
  const match = (s: string) => !kw || s.toLowerCase().includes(kw)

  const market = registryItems.filter((r) => r.kind === 'plugin')
  const marketList = market.filter((r) => match(`${r.name} ${r.description} ${r.version}`))
  const extAll = extensions.filter((e) => e.kind === 'tool')
  const installedCount = resolveInstalled(client, ['tool']).length
  const installableOfClient = toolList.filter((t) => {
    const meta = toolInstallInfo(t.name, t.group)
    return meta.installable && supportsClient(meta.platforms, client.os)
  }).length

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2 flex-wrap">
        <div className="flex-1 min-w-[200px]"><SearchBar value={q} onChange={setQ} placeholder="搜索工具：名称 / 描述 / 版本" /></div>
        <SourceTabs view={view} onView={setView} counts={{ builtin: toolList.length, installed: installedCount, extended: extAll.length }} />
      </div>

      {view === 'builtin' && (
        <>
          <PageHint>
            系统内置工具随程序发布，各端可<span className="text-inkdim">自行安装</span>到本机执行。当前端（{client.name}）可安装 {installableOfClient} / {toolList.length} 个；
            移动端不支持 shell 执行类工具，Windows 与 Linux 分别判定（如 bash 脚本、Chromium 依赖）。
          </PageHint>
          <ToolsPanel client={client} query={kw} />
        </>
      )}

      {view === 'installed' && (
        <InstalledPane
          client={client}
          kinds={['tool']}
          hint={<>当前客户端已落地的工具清单（内置工具 + 市场制品）。此视图只反映「{client.name}」本机状态，切换客户端即可查看其他端。</>}
        />
      )}

      {view === 'market' && (
        <>
          <PageHint>市场制品由 registry 提供（官方源强制签名，第三方源需审批）。下载后直接安装到当前客户端（{client.name}）。</PageHint>
          <div className="space-y-2">
            {marketList.map((r) => <RegistryCard key={r.id} r={r} client={client} />)}
            {marketList.length === 0 && <Empty icon={<Search size={22} className="text-inkfaint" />} title={kw ? '没有匹配的工具' : '暂无内容'} />}
          </div>
        </>
      )}

      {view === 'extended' && (
        <>
          <PageHint>
            拓展 = 程序在开发过程中为自己扩展的能力：Agent 在会话中生成实现 → 自动自检 → 用户审批 → 安装到当前客户端。
          </PageHint>
          <ExtensionList client={client} kind="tool" query={kw} />
        </>
      )}
    </div>
  )
}

// ---------------- MCP ----------------
export function McpPane({ client }: { client: DeviceNode }) {
  const { mcpServers, registryItems, extensions } = usePlugins()
  const [view, setView] = useState<ViewTab>('builtin')
  const [q, setQ] = useState('')
  const kw = q.trim().toLowerCase()
  const match = (s: string) => !kw || s.toLowerCase().includes(kw)

  const market = registryItems.filter((r) => r.kind === 'mcp')
  const marketList = market.filter((r) => match(`${r.name} ${r.description}`))
  const builtinList = mcpServers.filter((m) => m.origin === 'builtin')
    .filter((m) => match(`${m.name} ${m.command ?? ''} ${m.url ?? ''}`))
  const extAll = extensions.filter((e) => e.kind === 'mcp')
  const installedCount = resolveInstalled(client, ['mcp']).length

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2 flex-wrap">
        <div className="flex-1 min-w-[200px]"><SearchBar value={q} onChange={setQ} placeholder="搜索 MCP：名称 / 命令 / 工具" /></div>
        <SourceTabs view={view} onView={setView} counts={{ builtin: builtinList.length, installed: installedCount, extended: extAll.length }} />
      </div>

      {view === 'builtin' && (
        <>
          <PageHint>
            系统内置 MCP Server 可自行安装到客户端本地进程运行（stdio/http），避免服务端往返与数据外泄；
            stdio 类仅 PC 端（Windows / Linux）可安装，HTTP/SSE 类全端可用。
          </PageHint>
          <div className="space-y-2">
            {builtinList.map((m) => (
              <McpCard
                key={m.id}
                m={m}
                client={client}
                action={<InstallAction itemKey={`${KEY_PREFIX.mcp}${m.name}`} platforms={m.platforms} client={client} />}
              />
            ))}
            {builtinList.length === 0 && <Empty icon={<Cpu size={22} className="text-inkfaint" />} title={kw ? '没有匹配的内置 MCP' : '暂无内置 MCP Server'} />}
          </div>
        </>
      )}

      {view === 'installed' && (
        <InstalledPane
          client={client}
          kinds={['mcp']}
          hint={<>当前客户端已安装的 MCP Server（含市场制品与程序自建）。与内置工具同名时本地优先：内置版本生效，MCP 工具以 mcp:&lt;server&gt; 命名空间注入。</>}
        />
      )}

      {view === 'market' && (
        <>
          <PageHint>市场 MCP 制品：下载后安装到当前客户端（{client.name}），由该端本地托管进程/连接。</PageHint>
          <div className="space-y-2">
            {marketList.map((r) => <RegistryCard key={r.id} r={r} client={client} />)}
            {marketList.length === 0 && <Empty icon={<Search size={22} className="text-inkfaint" />} title={kw ? '没有匹配的 MCP' : '暂无内容'} />}
          </div>
        </>
      )}

      {view === 'extended' && (
        <>
          <PageHint>拓展 MCP = 程序为自己编写的 MCP Server，自检通过并经审批后可安装到当前客户端。</PageHint>
          <ExtensionList client={client} kind="mcp" query={kw} />
        </>
      )}
    </div>
  )
}

function McpCard({ m, client, action }: { m: McpServer; client: DeviceNode; action?: React.ReactNode }) {
  const conflicts = m.tools.filter((t) => t.conflictWith)
  return (
    <div className="rounded-xl border border-edge bg-panel p-3.5 shadow-subtle hover:border-edge2 transition-all">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-[13px] font-semibold text-ink">{m.name}</span>
        <Badge tone="blue">{m.transport}</Badge>
        <StateBadge state={m.state} />
        <span className="flex-1" />
        {action}
      </div>
      <div className="text-[11px] text-inkfaint font-mono mt-1.5 truncate">{m.command || m.url}</div>
      <div className="mt-1.5"><PlatformTags platforms={m.platforms} client={client} /></div>
      <div className="mt-2 flex flex-wrap gap-1.5">
        {m.tools.map((t) => (
          <span
            key={t.name}
            className={cn(
              'inline-flex items-center gap-1 rounded-md bg-panel2 border border-edge/80 px-2 py-0.5 text-[11px] text-inkdim',
              t.conflictWith && 'opacity-75 border-dashed',
            )}
            title={t.conflictWith
              ? `与内置工具 ${t.conflictWith} 同名：本地优先，内置生效；该工具仅以 mcp:${m.name} 命名空间注入`
              : undefined}
          >
            <span className="font-mono">{t.name}</span>
            <span className={cn('text-[10px]', t.risk === 'readonly' ? 'text-inkfaint' : 'text-warn')}>{t.risk}</span>
          </span>
        ))}
      </div>
      {conflicts.length > 0 && (
        <div className="mt-2 text-[11px] text-inkfaint leading-relaxed">
          与内置工具同名（{conflicts.map((t) => t.name).join(' / ')}）：<span className="text-ok">本地优先</span>
          ，内置版本生效；MCP 工具加 <code className="font-mono bg-panel2 px-1 rounded">mcp:{m.name}.</code> 前缀注入。
        </div>
      )}
    </div>
  )
}

// ---------------- 技能 ----------------
export function SkillsPane({ client }: { client: DeviceNode }) {
  const { skills, registryItems, extensions } = usePlugins()
  const [view, setView] = useState<ViewTab>('builtin')
  const [q, setQ] = useState('')
  const kw = q.trim().toLowerCase()
  const match = (s: string) => !kw || s.toLowerCase().includes(kw)

  const market = registryItems.filter((r) => r.kind === 'skill')
  const marketList = market.filter((r) => match(`${r.name} ${r.description}`))
  const builtinList = skills.filter((k) => k.origin === 'builtin').filter((k) => match(`${k.name} ${k.description}`))
  const extAll = extensions.filter((e) => e.kind === 'skill')
  const installedCount = resolveInstalled(client, ['skill']).length

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2 flex-wrap">
        <div className="flex-1 min-w-[200px]"><SearchBar value={q} onChange={setQ} placeholder="搜索技能：名称 / 描述" /></div>
        <SourceTabs view={view} onView={setView} counts={{ builtin: builtinList.length, installed: installedCount, extended: extAll.length }} />
      </div>

      {view === 'builtin' && (
        <>
          <PageHint>
            系统内置技能可把技能目录（含脚本）安装到客户端 .harness/skills/ 后本地执行。
            依赖脚本运行时（py / sh / ps1）的技能仅 PC 端可安装，移动端仅支持读取说明的纯提示型技能。
          </PageHint>
          <div className="space-y-2">
            {builtinList.map((k) => (
              <div key={k.id} className="rounded-xl border border-edge bg-panel p-3 shadow-subtle hover:border-edge2 transition-all">
                <div className="flex items-center gap-2 flex-wrap">
                  <span className="text-[13px] font-medium text-ink">{k.name}</span>
                  <Badge tone="blue">系统内置</Badge>
                  <span className="flex-1" />
                  <InstallAction itemKey={`${KEY_PREFIX.skill}${k.name}`} platforms={k.platforms} client={client} />
                </div>
                <div className="text-[12px] text-inkfaint mt-1 leading-relaxed">{k.description}</div>
                <div className="mt-1.5"><PlatformTags platforms={k.platforms} client={client} /></div>
              </div>
            ))}
            {builtinList.length === 0 && <Empty icon={<Sparkles size={22} className="text-inkfaint" />} title={kw ? '没有匹配的内置技能' : '暂无内置技能'} />}
          </div>
        </>
      )}

      {view === 'installed' && (
        <InstalledPane
          client={client}
          kinds={['skill']}
          hint={<>当前客户端已安装技能：内置技能 / 市场下载 / 拓展生成，均落地在本地 .harness/skills/。卸载后回到可重新安装状态。</>}
        />
      )}

      {view === 'market' && (
        <>
          <PageHint>技能市场：从 registry 下载并安装到当前客户端（{client.name}）。</PageHint>
          <div className="space-y-2">
            {marketList.map((r) => <RegistryCard key={r.id} r={r} client={client} />)}
            {marketList.length === 0 && <Empty icon={<Search size={22} className="text-inkfaint" />} title={kw ? '没有匹配的技能' : '暂无内容'} />}
          </div>
        </>
      )}

      {view === 'extended' && (
        <>
          <PageHint>拓展技能 = 程序把一次成功经验固化为可复用技能（save_skill 自演化），自检通过并经审批后即可安装到当前客户端。</PageHint>
          <ExtensionList client={client} kind="skill" query={kw} />
        </>
      )}
    </div>
  )
}

// ---------------- 拓展（三类汇总） ----------------
export function ExtensionsPane({ client }: { client: DeviceNode }) {
  const { extensions } = usePlugins()
  const [q, setQ] = useState('')
  const kw = q.trim().toLowerCase()
  const match = (s: string) => !kw || s.toLowerCase().includes(kw)

  const all = extensions.filter((e) => match(`${e.name} ${e.description}`))
  const drafts = all.filter((e) => e.state === 'draft')
  const installedLike = all.filter((e) => e.state !== 'draft')

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2 flex-wrap">
        <div className="flex-1 min-w-[200px]"><SearchBar value={q} onChange={setQ} placeholder="搜索拓展：名称 / 描述" /></div>
        <Badge tone="cyan">草稿 {drafts.length}</Badge>
        <Badge tone="green">已落地 {installedLike.length}</Badge>
      </div>
      <PageHint>
        拓展是程序在开发过程中为自己扩展的能力（工具 / MCP / 技能）：草稿需自检通过并经审批后才可安装到当前客户端（{client.name}）。
      </PageHint>
      <ExtensionList client={client} query={kw} />
    </div>
  )
}

// ---------------- 拓展卡片 ----------------
const RISK_TEXT: Record<Risk, { tone: 'blue' | 'yellow' | 'red'; text: string }> = {
  readonly: { tone: 'blue', text: '只读' },
  write: { tone: 'yellow', text: '写入' },
  command: { tone: 'yellow', text: '执行' },
  danger: { tone: 'red', text: '危险' },
}

const EXT_KIND_TEXT: Record<ExtensionKind, string> = { tool: '工具', mcp: 'MCP Server', skill: '技能' }

function ExtensionCard({ e, client }: { e: ExtensionItem; client: DeviceNode }) {
  const { installExtension, toggleExtension, removeExtension, uninstallOnClient } = usePlugins()
  const toast = useUI((s) => s.toast)
  const risk = RISK_TEXT[e.risk]
  const installed = e.state === 'installed'
  const disabled = e.state === 'disabled'
  const draft = e.state === 'draft'
  const testFailed = e.selfTest === 'failed'
  const key = `${KEY_PREFIX.ext}${e.name}`
  const onThisClient = client.installed.includes(key)

  return (
    <div className="rounded-xl border border-edge bg-panel p-3.5 shadow-subtle hover:border-edge2 transition-all">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-[13px] font-semibold text-ink">{e.name}</span>
        <Badge>{e.version}</Badge>
        <Badge tone="cyan">{EXT_KIND_TEXT[e.kind]}</Badge>
        <Badge tone={risk.tone}>{risk.text}</Badge>
        <Badge tone={installed ? 'green' : disabled ? 'gray' : 'yellow'}>
          {installed ? '已安装' : disabled ? '已停用' : '草稿'}
        </Badge>
        <Badge tone={e.selfTest === 'passed' ? 'green' : e.selfTest === 'pending' ? 'blue' : 'red'}>
          {e.selfTest === 'passed' ? '自检通过' : e.selfTest === 'pending' ? '自检中' : '自检失败'}
        </Badge>
        {e.signature === 'unsigned' && <Badge tone="yellow">未签名</Badge>}
        <span className="flex-1" />
        {draft && (
          <Button
            size="sm"
            variant="primary"
            disabled={testFailed}
            title={testFailed ? '自检未通过：需修复实现并重跑自检后才可安装' : `自检通过后安装到 ${client.name}`}
            onClick={() => {
              installExtension(e.id)
              toast('success', `已安装拓展${EXT_KIND_TEXT[e.kind]}「${e.name}」到 ${client.name}`)
            }}
          >
            安装到此端
          </Button>
        )}
        {installed && (
          <>
            <Button size="sm" variant="ghost" onClick={() => { toggleExtension(e.id); toast('info', `已停用「${e.name}」`) }}>停用</Button>
            {!onThisClient && (
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  usePlugins.getState().installOnClient(key)
                  toast('info', `正在安装「${e.name}」到 ${client.name}`)
                }}
              >
                安装到此端
              </Button>
            )}
            {onThisClient && (
              <Button
                size="sm"
                variant="ghost"
                className="text-inkdim hover:text-err"
                onClick={() => { uninstallOnClient(key); toast('info', `已从 ${client.name} 卸载「${e.name}」`) }}
              >
                卸载
              </Button>
            )}
          </>
        )}
        {disabled && (
          <>
            <Button size="sm" variant="outline" onClick={() => { toggleExtension(e.id); toast('success', `已启用「${e.name}」`) }}>启用</Button>
            <Button size="sm" variant="ghost" onClick={() => { removeExtension(e.id); toast('info', `已卸载「${e.name}」`) }}>移除</Button>
          </>
        )}
      </div>
      <div className="text-[12px] text-inkdim mt-1.5 leading-relaxed">{e.description}</div>
      <div className="mt-1.5 flex flex-wrap gap-2 text-[11px] text-inkfaint font-mono">
        <span>产出：{e.generatedBy}</span>
        <span>{new Date(e.createdAt).toLocaleString('zh-CN')}</span>
      </div>
    </div>
  )
}

function ExtensionList({ client, kind, query = '' }: { client: DeviceNode; kind?: ExtensionKind; query?: string }) {
  const { extensions } = usePlugins()
  const kw = query.trim().toLowerCase()
  const list = extensions.filter(
    (e) =>
      (!kind || e.kind === kind) &&
      (!kw || `${e.name} ${e.description}`.toLowerCase().includes(kw)),
  )
  if (list.length === 0) {
    return (
      <Empty
        icon={<Sparkles size={22} className="text-inkfaint" />}
        title={kw ? '没有匹配的拓展' : '暂无拓展'}
        hint="程序在开发过程中自建的能力会出现在这里：自检通过并经审批后即可安装到客户端"
      />
    )
  }
  return (
    <div className="space-y-2">
      {list.map((e) => <ExtensionCard key={e.id} e={e} client={client} />)}
    </div>
  )
}
