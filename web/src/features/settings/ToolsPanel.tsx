import { useEffect, useState } from 'react'
import { toolList, toolInstallInfo } from '../../lib/toolCatalog'
import { toggleDisabled, useSettings } from '../../stores/settingsStore'
import { usePlugins } from '../../stores/pluginStore'
import { useUI } from '../../stores/uiStore'
import { Badge, Button, Switch } from '../../components/ui'
import type { DeviceNode, Risk, ToolGroup, ToolInstallMeta } from '../../types'
import { cn } from '../../components/ui'
import { supportsClient, unsupportedReason } from '../../lib/platform'
import { api } from '../../lib/api'
import type { ToolSnapshot, ToolTier } from '../../lib/apiTypes'
import { ToolInjectionPanel } from './ToolInjectionPanel'

// ---------------- 分区元信息（顺序即渲染顺序，数量统计按「已启用 n / 总数 m」） ----------------
const TOOL_GROUPS: Array<{ key: ToolGroup; label: string; hint: string; dot: string; text: string }> = [
  { key: 'file', label: '文件与命令', hint: '文件读写 · 代码检索 · 终端命令执行', dot: 'bg-accent', text: 'text-accent' },
  { key: 'net', label: '网络与多模态', hint: '网页抓取 · 图片落盘校验与元信息读取（生成/识别未接线）', dot: 'bg-ok', text: 'text-ok' },
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

function ToolItem({ name, enabled, onToggle, client, serverMode }: {
  name: string
  enabled: boolean
  onToggle: () => void
  client: DeviceNode
  /** 服务端节点：内置工具只有启用/停用，没有安装/卸载（编译期注册，无独立制品） */
  serverMode?: boolean
}) {
  const t = toolList.find((x) => x.name === name)!
  const meta = toolInstallInfo(t.name, t.group)
  const { installOnClient, uninstallOnClient } = usePlugins()
  const toast = useUI((s) => s.toast)
  const [busy, setBusy] = useState(false)
  const installed = client.installed.includes(t.name)
  const compatible = supportsClient(meta.platforms, client.os)

  // 安装/卸载都走真实端点（PUT /devices/{did}/plugins）：失败必须明示，不能静默或假装装好。
  const run = (fn: () => Promise<void>, ok: string, label: string) => {
    setBusy(true)
    fn()
      .then(() => toast('success', ok))
      .catch((e: unknown) => toast('error', `${label}失败：${e instanceof Error ? e.message : String(e)}`))
      .finally(() => setBusy(false))
  }

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
          {serverMode
            ? <Badge tone="gray">随服务端内置</Badge>
            : installed
              ? <Badge tone="green">已安装</Badge>
              : meta.installable
                ? <Badge tone="yellow">未安装</Badge>
                : <Badge tone="gray">不可安装</Badge>}
        </div>
        <div className="text-[12px] text-inkdim mt-1.5 leading-relaxed">{t.description}</div>
        <div className="mt-1.5 flex flex-wrap items-center gap-2 text-[11px] text-inkfaint font-mono">
          <PlatformBadges meta={meta} client={client} />
          <span>客户端依赖：{meta.deps.length ? meta.deps.join(' · ') : '无'}</span>
          {meta.note && <span className="font-sans">{meta.note}</span>}
        </div>
      </div>
      <div className="flex items-center gap-2 shrink-0 pt-0.5">
        {!serverMode && meta.installable && (
          installed ? (
            <Button
              size="sm"
              variant="ghost"
              disabled={busy}
              className="text-inkdim hover:text-err"
              title={`从 ${client.name} 卸载`}
              onClick={() => run(() => uninstallOnClient(t.name), `已从 ${client.name} 卸载 ${t.name}`, '卸载')}
            >
              卸载
            </Button>
          ) : (
            <Button
              size="sm"
              variant="primary"
              disabled={!compatible || busy}
              title={!compatible ? unsupportedReason(meta.platforms, client) : `安装到 ${client.name}（该端本地执行）`}
              onClick={() => run(() => installOnClient(t.name), `${t.name} 已安装到 ${client.name}`, '安装')}
            >
              {busy ? '安装中…' : compatible ? '安装到此端' : '该端不支持'}
            </Button>
          )
        )}
        {/* 客户端节点：纯服务端能力也要说明原因，绝不渲染成点了没反应的假按钮 */}
        {!serverMode && !meta.installable && (
          <span className="text-[11px] text-inkfaint" title={meta.note}>
            纯服务端能力 · 客户端经 API 调用
          </span>
        )}
        {serverMode && (
          <span
            className="text-[11px] text-inkfaint"
            title="内置工具随服务端发布，编译期注册：不提供安装/卸载，只能启用或停用"
          >
            随服务端内置 · 仅启停
          </span>
        )}
        <span className={cn('text-[12px] select-none whitespace-nowrap', enabled ? 'text-ok font-medium' : 'text-inkdim')}>
          {enabled ? '启用' : '停用'}
        </span>
        <Switch checked={enabled} onChange={onToggle} title={`${enabled ? '停用' : '启用'} ${t.name}`} />
      </div>
    </div>
  )
}

// ---------------- 服务端工具快照（A1 接真） ----------------
// 为什么能接真：lib/api.ts 的 getTools() / patchToolTiers() 已走 request() 直连 A-10 三端点，
// 返回 ToolSnapshot（total / 三档 tier / cost_* / limits / warnings / overrides），此处不做任何本地估算。
// 为什么只能部分对齐：后端只有 core / deferred / on_demand 三档 tier，**没有**「启用/禁用」布尔，
// 因此下方工具卡片的启停开关仍是 settingsStore 的本地态（不写服务端）；只有 tier 变更才回写服务端。
const TIER_META: Array<{ key: ToolTier; label: string; hint: string }> = [
  { key: 'core', label: 'core 常驻', hint: '每次请求都注入完整 schema' },
  { key: 'deferred', label: 'deferred 延迟', hint: '只注入索引，按需回捞定义' },
  { key: 'on_demand', label: 'on_demand 按需', hint: '仅列举，不进入注入预算' },
]

const fmtNum = (n: number) => n.toLocaleString('en-US')

function ServerToolSnapshot() {
  const toast = useUI((s) => s.toast)
  const [snap, setSnap] = useState<ToolSnapshot | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState<string | null>(null)

  useEffect(() => {
    let alive = true
    api
      .getTools()
      .then((s) => {
        if (!alive) return
        setSnap(s)
        setError(null)
      })
      .catch((e: unknown) => {
        // 不求兜底成假值：失败就如实展示错误
        if (alive) setError(e instanceof Error ? e.message : String(e))
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [])

  const changeTier = (name: string, tier: ToolTier) => {
    setSaving(name)
    api
      .patchToolTiers({ [name]: tier })
      .then((s) => {
        setSnap(s)
        toast('success', `已将 ${name} 调整为 ${tier}`)
      })
      .catch((e: unknown) => toast('error', `调整失败：${e instanceof Error ? e.message : String(e)}`))
      .finally(() => setSaving(null))
  }

  return (
    <div className="rounded-xl border border-edge bg-panel2/30 p-3 space-y-2.5">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-[12px] font-semibold">服务端工具快照</span>
        <Badge tone="blue">GET /tools · 真实</Badge>
        {loading && <span className="text-[11px] text-inkfaint">读取中…</span>}
        {error && <span className="text-[11px] text-err">读取失败：{error}</span>}
      </div>

      {!snap && !loading && !error && <div className="text-[11px] text-inkfaint">服务端未返回工具快照</div>}

      {snap && (
        <>
          <div className="grid grid-cols-2 sm:grid-cols-4 gap-2">
            <div className="rounded-lg border border-edge px-2 py-1.5">
              <div className="text-[10px] text-inkfaint">注册工具总数</div>
              <div className="text-[14px] font-medium">{fmtNum(snap.total)}</div>
            </div>
            <div className="rounded-lg border border-edge px-2 py-1.5">
              <div className="text-[10px] text-inkfaint">注入成本 / 预算</div>
              <div className="text-[14px] font-medium">
                {fmtNum(snap.cost_total)}
                <span className="text-[11px] text-inkfaint"> / {fmtNum(snap.limits.core_soft + snap.limits.deferred_soft)}</span>
              </div>
            </div>
            <div className="rounded-lg border border-edge px-2 py-1.5">
              <div className="text-[10px] text-inkfaint">core / deferred 成本</div>
              <div className="text-[14px] font-medium">
                {fmtNum(snap.cost_core)} <span className="text-inkfaint">/</span> {fmtNum(snap.cost_deferred)}
              </div>
            </div>
            <div className="rounded-lg border border-edge px-2 py-1.5">
              <div className="text-[10px] text-inkfaint">插件来源 / tier 覆盖</div>
              <div className="text-[14px] font-medium">
                {fmtNum(snap.plugins.length)} <span className="text-inkfaint">/</span> {fmtNum(Object.keys(snap.overrides).length)}
              </div>
            </div>
          </div>

          {snap.warnings.length > 0 && (
            <div className="rounded-lg border border-warn/40 bg-warn/[0.06] px-2 py-1.5 text-[11px] text-warn space-y-0.5">
              {snap.warnings.map((w, i) => <div key={i}>· {w}</div>)}
            </div>
          )}

          <div className="space-y-2">
            {TIER_META.map((t) => {
              const list = snap[t.key]
              return (
                <div key={t.key}>
                  <div className="flex items-center gap-2">
                    <span className="text-[11px] font-medium">{t.label}</span>
                    <Badge tone={list.length ? 'green' : 'gray'}>{list.length} 个</Badge>
                    <span className="text-[10px] text-inkfaint">{t.hint}</span>
                  </div>
                  {list.length === 0 ? (
                    <div className="text-[11px] text-inkfaint pl-1">（服务端该档为空）</div>
                  ) : (
                    <div className="mt-1 flex flex-wrap gap-1.5">
                      {list.map((tool) => (
                        <span
                          key={tool.name}
                          className="inline-flex items-center gap-1 rounded border border-edge bg-panel2 px-1.5 py-0.5 text-[10px] font-mono"
                          title={`namespace=${tool.namespace} · risk=${tool.risk} · executor=${tool.executor} · cost=${tool.cost_tokens} · health=${tool.health}`}
                        >
                          {tool.name}
                          <span className="text-inkfaint">·{tool.cost_tokens}</span>
                          <select
                            className="bg-transparent text-[10px] outline-none cursor-pointer disabled:opacity-50"
                            value={tool.tier}
                            disabled={saving !== null}
                            onChange={(e) => changeTier(tool.name, e.target.value as ToolTier)}
                            title="调整服务端 tier（PATCH /tools/tiers）"
                          >
                            {TIER_META.map((o) => <option key={o.key} value={o.key}>{o.key}</option>)}
                          </select>
                        </span>
                      ))}
                    </div>
                  )}
                </div>
              )
            })}
          </div>

          <div className="text-[10px] text-inkfaint font-mono">
            generated_at: {snap.generated_at}
          </div>
        </>
      )}
    </div>
  )
}

export function ToolsPanel({ client, query = '' }: { client: DeviceNode; query?: string }) {
  const { settings, setDisabledTools } = useSettings()
  const { toolSnapshot, setServerToolEnabled, loadRemoteCapabilities } = usePlugins()
  const toast = useUI((s) => s.toast)
  const d = settings
  const kw = query.trim().toLowerCase()
  const match = (s: string) => !kw || s.toLowerCase().includes(kw)

  // 服务端节点：启停必须走服务端真实端点（内置工具编译期注册，只有可用性开关）。
  // 客户端节点：维持 settingsStore 的本地态（客户端本机能力未上报到服务端）。
  const serverMode = client.platform === 'server'
  const disabledOnServer = new Set(toolSnapshot?.disabled ?? [])
  const isEnabled = (name: string) =>
    serverMode ? !disabledOnServer.has(name) : !d.disabledTools.includes(name)
  const toggle = (name: string) => {
    if (!serverMode) {
      setDisabledTools(toggleDisabled(d.disabledTools, name))
      return
    }
    const next = !isEnabled(name)
    setServerToolEnabled(name, next)
      .then(() => toast('success', `${next ? '已启用' : '已停用'} ${name}（下一轮对话生效）`))
      .catch((e: unknown) => toast('error', `操作失败：${e instanceof Error ? e.message : String(e)}`))
  }
  const enableAll = () => {
    if (!serverMode) {
      setDisabledTools([])
      return
    }
    const disabled = toolSnapshot?.disabled ?? []
    if (disabled.length === 0) return
    void Promise.all(disabled.map((n) => setServerToolEnabled(n, true)))
      .then(() => toast('success', `已启用 ${disabled.length} 个内置工具`))
      .catch((e: unknown) => toast('error', `操作失败：${e instanceof Error ? e.message : String(e)}`))
  }

  // 进入面板即拉取真值：服务端节点要 disabled 清单，客户端节点要真实 installed 清单
  // （两者同源，都来自 loadRemoteCapabilities 的一次并发拉取）。
  useEffect(() => {
    void loadRemoteCapabilities()
  }, [loadRemoteCapabilities])

  const disabledCount = serverMode ? (toolSnapshot?.disabled?.length ?? 0) : d.disabledTools.length
  const installableCount = toolList.filter((t) => {
    const meta = toolInstallInfo(t.name, t.group)
    return meta.installable && supportsClient(meta.platforms, client.os)
  }).length
  const installedCount = toolList.filter((t) => client.installed.includes(t.name)).length

  return (
    <div className="flex flex-col gap-4">
      <ServerToolSnapshot />
      <ToolInjectionPanel />
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="text-[13px] font-medium">系统内置工具</div>
          <div className="text-[12px] text-inkdim mt-0.5 leading-relaxed">
            {serverMode
              ? '内置工具随服务端发布（编译期注册），不提供安装/卸载，只能启用或停用。停用的工具不进注入集合、模型无法调用；下一轮对话生效，重启后仍保持。'
              : `禁用的工具将不会注册到 Agent 工具列表，模型无法调用。当前客户端（${client.name}）可安装 ${installableCount} / ${toolList.length} 个，已安装 ${installedCount} 个；
            平台支持按 移动端 / PC 端（Windows、Linux）分别判定。`}
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
                  serverMode={serverMode}
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
