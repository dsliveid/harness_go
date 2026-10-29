import { toolList, toolInstallInfo } from '../../mock/settingsData'
import { toggleDisabled, useSettings } from '../../stores/settingsStore'
import { usePlugins } from '../../stores/pluginStore'
import { useUI } from '../../stores/uiStore'
import { Badge, Button, Switch } from '../../components/ui'
import type { DeviceNode, Risk, ToolGroup, ToolInstallMeta } from '../../types'
import { cn } from '../../components/ui'
import { supportsClient, unsupportedReason } from '../../lib/platform'

// ---------------- 分区元信息（顺序即渲染顺序，数量统计按「已启用 n / 总数 m」） ----------------
const TOOL_GROUPS: Array<{ key: ToolGroup; label: string; hint: string; dot: string; text: string }> = [
  { key: 'file', label: '文件与命令', hint: '文件读写 · 代码检索 · 终端命令执行', dot: 'bg-accent', text: 'text-accent' },
  { key: 'net', label: '网络与多模态', hint: '网页抓取 · 图片生成与视觉识别', dot: 'bg-ok', text: 'text-ok' },
  { key: 'plan', label: '任务与计划', hint: '任务清单与计划文档中台', dot: 'bg-warn', text: 'text-warn' },
  { key: 'memory', label: '长期记忆', hint: '项目长期认知记忆读写', dot: 'bg-accent2', text: 'text-accent2' },
  { key: 'agent', label: '子 Agent 编排', hint: '子 Agent 与子进程的派生、等待、停止与恢复', dot: 'bg-accent', text: 'text-accent' },
  { key: 'decision', label: '决策模型', hint: '二元判断 · 多选一 · 量化评分', dot: 'bg-ok', text: 'text-ok' },
  { key: 'skill', label: '自演化引擎（技能库）', hint: '项目技能的查询、固化与执行', dot: 'bg-accent2', text: 'text-accent2' },
]

function RiskBadge({ risk }: { risk: Risk }) {
  const map: Record<Risk, { tone: 'blue' | 'yellow' | 'red' | 'gray'; text: string }> = {
    readonly: { tone: 'blue', text: '只读' },
    write: { tone: 'yellow', text: '写入' },
    command: { tone: 'yellow', text: '执行' },
    danger: { tone: 'red', text: '危险' },
  }
  const m = map[risk]
  return <Badge tone={m.tone}>{m.text}</Badge>
}

function CategoryBadge({ category }: { category: 'builtin' | 'skill' }) {
  return category === 'skill'
    ? <Badge tone="purple">自演化引擎</Badge>
    : <Badge tone="blue">系统内置</Badge>
}

function InstallBadge({ meta }: { meta: ToolInstallMeta }) {
  return meta.installable
    ? <Badge tone="green">可安装 · {meta.mode}</Badge>
    : <Badge tone="gray">不可安装</Badge>
}

/** 平台支持标签：Windows / Linux / 移动端；当前端不支持的一档划掉 */
function PlatformBadges({ meta, client }: { meta: ToolInstallMeta; client: DeviceNode }) {
  const items: Array<{ key: 'windows' | 'linux' | 'mobile'; label: string; ok: boolean }> = [
    { key: 'windows', label: 'Windows', ok: meta.platforms.windows },
    { key: 'linux', label: 'Linux', ok: meta.platforms.linux },
    { key: 'mobile', label: '移动端', ok: meta.platforms.mobile },
  ]
  const current = client.platform === 'mobile' ? 'mobile' : client.os
  return (
    <span className="inline-flex items-center gap-1 flex-wrap">
      <span className="text-[10px] text-inkfaint">支持</span>
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

function ToolItem({ name, enabled, onToggle, client }: {
  name: string
  enabled: boolean
  onToggle: () => void
  client: DeviceNode
}) {
  const t = toolList.find((x) => x.name === name)!
  const meta = toolInstallInfo(t.name, t.group)
  const { installOnClient, uninstallOnClient } = usePlugins()
  const toast = useUI((s) => s.toast)
  const installed = client.installed.includes(t.name)
  const installing = client.installing.includes(t.name)
  const compatible = supportsClient(meta.platforms, client.os)

  return (
    <div className={cn(
      'border rounded-xl p-3 flex items-start justify-between gap-3 transition-colors',
      enabled ? 'border-edge bg-panel2/30' : 'border-edge/50 opacity-75',
    )}>
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2 flex-wrap">
          <code className="text-[13px] font-mono font-medium bg-panel2 px-1.5 py-0.5 rounded border border-edge/60">{t.name}</code>
          <span className="text-[12px] font-medium">{t.title}</span>
          <RiskBadge risk={t.risk} />
          <CategoryBadge category={t.category} />
          <InstallBadge meta={meta} />
          {installed && <Badge tone="green">本端已安装</Badge>}
        </div>
        <div className="text-[12px] text-inkdim mt-1.5 leading-relaxed">{t.description}</div>
        <div className="mt-1.5 flex flex-wrap items-center gap-2 text-[11px] text-inkfaint font-mono">
          <PlatformBadges meta={meta} client={client} />
          <span>客户端依赖：{meta.deps.length ? meta.deps.join(' · ') : '无'}</span>
          {meta.note && <span className="font-sans">{meta.note}</span>}
        </div>
      </div>
      <div className="flex items-center gap-2 shrink-0 pt-0.5">
        {meta.installable && (
          installed ? (
            <Button
              size="sm"
              variant="ghost"
              className="text-inkdim hover:text-err"
              title={`从 ${client.name} 卸载`}
              onClick={() => { uninstallOnClient(t.name); toast('info', `已从 ${client.name} 卸载 ${t.name}`) }}
            >
              卸载
            </Button>
          ) : (
            <Button
              size="sm"
              variant="outline"
              disabled={!compatible || installing}
              title={!compatible ? unsupportedReason(meta.platforms, client) : `在当前客户端（${client.name}）本地安装`}
              onClick={() => { installOnClient(t.name); toast('info', `正在安装 ${t.name} 到 ${client.name}`) }}
            >
              {installing ? '安装中…' : compatible ? '安装到此端' : '该端不支持'}
            </Button>
          )
        )}
        <span className={cn('text-[12px] select-none whitespace-nowrap', enabled ? 'text-ok font-medium' : 'text-inkdim')}>
          {enabled ? '启用' : '禁用'}
        </span>
        <Switch checked={enabled} onChange={onToggle} title={`${enabled ? '禁用' : '启用'} ${t.name}`} />
      </div>
    </div>
  )
}

export function ToolsPanel({ client, query = '' }: { client: DeviceNode; query?: string }) {
  const { settings, setDisabledTools } = useSettings()
  const d = settings
  const kw = query.trim().toLowerCase()
  const match = (s: string) => !kw || s.toLowerCase().includes(kw)
  const isEnabled = (name: string) => !d.disabledTools.includes(name)
  const toggle = (name: string) => setDisabledTools(toggleDisabled(d.disabledTools, name))
  const enableAll = () => setDisabledTools([])

  const disabledCount = d.disabledTools.length
  const installableCount = toolList.filter((t) => {
    const meta = toolInstallInfo(t.name, t.group)
    return meta.installable && supportsClient(meta.platforms, client.os)
  }).length
  const installedCount = toolList.filter((t) => client.installed.includes(t.name)).length

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="text-[13px] font-medium">系统内置工具</div>
          <div className="text-[12px] text-inkdim mt-0.5 leading-relaxed">
            禁用的工具将不会注册到 Agent 工具列表，模型无法调用。
            当前客户端（{client.name}）可安装 {installableCount} / {toolList.length} 个，已安装 {installedCount} 个；
            平台支持按 移动端 / PC 端（Windows、Linux）分别判定。
          </div>
        </div>
        <div className="flex items-center gap-2.5 shrink-0">
          <Badge tone={disabledCount > 0 ? 'yellow' : 'green'}>已启用 {toolList.length - disabledCount} / {toolList.length}</Badge>
          {disabledCount > 0 && (
            <Button size="sm" variant="ghost" onClick={enableAll} className="text-accent">全部启用</Button>
          )}
        </div>
      </div>

      <div className="flex flex-col gap-5">
        {TOOL_GROUPS.map((g) => {
          const tools = toolList.filter((t) => t.group === g.key && match(`${t.name} ${t.title} ${t.description}`))
          if (tools.length === 0) return null
          const enabledCount = tools.filter((t) => isEnabled(t.name)).length
          return (
            <div key={g.key} className="flex flex-col gap-2">
              <div className="flex items-center gap-2 text-[12px] font-semibold px-0.5">
                <span className={cn('w-2 h-2 rounded-full shrink-0', g.dot)} />
                <span className={g.text}>{g.label}</span>
                <Badge tone={enabledCount === tools.length ? 'green' : 'yellow'}>
                  已启用 {enabledCount} / {tools.length}
                </Badge>
                <span className="text-[11px] font-normal text-inkfaint truncate">{g.hint}</span>
              </div>
              {tools.map((t) => (
                <ToolItem
                  key={t.name}
                  name={t.name}
                  enabled={isEnabled(t.name)}
                  onToggle={() => toggle(t.name)}
                  client={client}
                />
              ))}
            </div>
          )
        })}
        {toolList.every((t) => !match(`${t.name} ${t.title} ${t.description}`)) && (
          <div className="text-[12px] text-inkfaint text-center py-6">没有匹配的内置工具</div>
        )}
      </div>

      <div className="text-[11px] text-inkfaint leading-relaxed">
        平台说明：当前端为 {client.name}，可安装能力见上方「支持」徽标；
        子 Agent / 子进程编排等纯服务端能力任何客户端都不安装，由客户端通过 API 代理调用。
      </div>
    </div>
  )
}
