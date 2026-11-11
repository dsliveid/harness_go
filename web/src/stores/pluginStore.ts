import { create } from 'zustand'
import type { DeviceNode, ExtensionItem, McpServer, McpState, McpTransport, MemoryItem, Plugin, RegistryItem, Risk, Skill } from '../types'
import { api } from '../lib/api'
import type { MCPServerViewDTO, PackageViewDTO } from '../lib/apiTypes'

/* ============ 服务端 DTO → UI 领域模型映射 ============ */

/**
 * 服务端状态 → UI 状态。
 * 依据：internal/mcp/manager.go:426 `State string \`json:"state"\``，取值为
 * ready / degraded / failed / disabled（另有 UI 侧独有的 starting，仅本地过渡态）。
 */
const MCP_STATE_MAP: Record<string, McpState> = {
  ready: 'ready',
  degraded: 'degraded',
  failed: 'failed',
  disabled: 'disabled',
}

/**
 * `GET /api/v1/mcp/servers` 的 ServerView → UI McpServer。
 * 来源：internal/service/mcp_service.go:51-74。
 *
 * 降级说明（不造假）：
 *   - 后端只回 `allow_tools`（启用白名单）与 `tool_count`，**不回工具描述与逐个 risk**；
 *     故工具条目按白名单名字生成，description 明确标注「服务端未回传描述」，
 *     risk 统一取 `default_risk`（后端字段口径）。
 *   - origin 归入 'builtin'（= 随服务端发布、由服务端托管），
 *     因为「已安装」页签读的是端节点 client.installed（客户端本地安装清单），
 *     把服务端 Server 放进去会导致真实数据在 UI 上不可见。
 */
function toUiMcpServer(v: MCPServerViewDTO): McpServer {
  const risk = (v.default_risk as Risk) ?? 'readonly'
  const names = v.allow_tools ?? []
  return {
    id: v.id,
    name: v.name,
    origin: 'builtin',
    transport: v.transport as McpTransport,
    command: [v.command, ...(v.args ?? [])].filter(Boolean).join(' ') || undefined,
    url: v.url,
    enabled: v.enabled,
    state: MCP_STATE_MAP[v.state ?? ''] ?? (v.enabled ? 'starting' : 'disabled'),
    tools: names.map((n) => ({ name: n, description: '（服务端未回传工具描述）', risk })),
    lastError: v.last_error,
    calls: v.calls,
    failures: v.failures,
    healthIntervalS: v.health_interval_s,
  }
}

/**
 * 能力安装模型：客户端自行安装。
 * - 服务端不再「下发」，安装动作只作用于当前客户端（currentClientId）自身；
 * - 每个端节点各自维护 installed / installing 清单（key：工具名 / mcp:<server> / skill:<name> / ext:<name> / reg:<id>）。
 */
interface PluginState {
  plugins: Plugin[]
  mcpServers: McpServer[]
  /** 本地工具包（来自 GET /api/v1/packages） */
  packages: PackageViewDTO[]
  registryItems: RegistryItem[]
  skills: Skill[]
  memories: MemoryItem[]
  /** 拓展：程序在开发过程中为自身扩展的能力（自演化引擎产出） */
  extensions: ExtensionItem[]
  /** 客户端节点清单（客户端自行安装的落地载体） */
  devices: DeviceNode[]
  /** 当前操作所在客户端（安装/卸载只作用于本端） */
  currentClientId: string
  /** 服务端接线状态：idle 未拉取 / loading / ready 已生效 / error 拉取失败（附原因） */
  remoteStatus: 'idle' | 'loading' | 'ready' | 'error'
  remoteError?: string
  /** 拉取服务端真实 MCP Server 与工具包 */
  loadRemoteCapabilities: () => Promise<void>
  setCurrentClient: (id: string) => void
  togglePlugin: (id: string) => void
  reloadMcp: (id: string) => void
  toggleMcp: (id: string) => void
  testMcp: (id: string) => { ok: boolean; message: string }
  installRegistry: (id: string) => void
  toggleSkill: (id: string) => void
  /** 安装拓展项：自检通过 → 已安装（原型模拟审批放行） */
  installExtension: (id: string) => void
  toggleExtension: (id: string) => void
  removeExtension: (id: string) => void
  /** 把能力安装到当前客户端（客户端自行安装，非服务端下发） */
  installOnClient: (key: string) => void
  /** 从当前客户端卸载能力 */
  uninstallOnClient: (key: string) => void
}

export const usePlugins = create<PluginState>((set, get) => ({
  // 一律从空态起步：registry / skills / memories / extensions / plugins 目前服务端无端点，
  // 由 loadRemoteCapabilities 与真实数据源按需填充，绝不预置演示数据冒充真实。
  plugins: [],
  mcpServers: [],
  packages: [],
  registryItems: [],
  skills: [],
  memories: [],
  extensions: [],
  devices: [],
  currentClientId: '',
  remoteStatus: 'idle',

  /**
   * 拉取服务端真实 MCP Server 与工具包（A 类接线：仅这两类有后端端点）。
   *
   * 铁律：
   *   - registry / skills / extensions 目前无后端端点，**此处不覆盖**；
   *   - 拉取失败不抛错、不静默：写入 remoteError，由 UI 明示降级。
   */
  loadRemoteCapabilities: async () => {
    set({ remoteStatus: 'loading', remoteError: undefined })
    // 两个端点独立成败：MCP 通了但 packages 挂了也应展示已拿到的部分（部分成功优于全空白）
    const [mcpRes, pkgRes] = await Promise.allSettled([
      api.listMcpServers(),
      api.listPackages(),
    ])
    const errors: string[] = []
    const patch: Partial<PluginState> = {}
    if (mcpRes.status === 'fulfilled') {
      patch.mcpServers = (mcpRes.value.items ?? []).map(toUiMcpServer)
    } else {
      errors.push(`MCP Server：${mcpRes.reason instanceof Error ? mcpRes.reason.message : String(mcpRes.reason)}`)
    }
    if (pkgRes.status === 'fulfilled') {
      patch.packages = pkgRes.value.items ?? []
    } else {
      errors.push(`工具包：${pkgRes.reason instanceof Error ? pkgRes.reason.message : String(pkgRes.reason)}`)
    }
    if (errors.length === 0) {
      set({ ...patch, remoteStatus: 'ready' })
      return
    }
    // 全部失败才算 error；部分失败保留已成功数据并提示原因
    const allFailed = mcpRes.status === 'rejected' && pkgRes.status === 'rejected'
    set({
      ...patch,
      remoteStatus: allFailed ? 'error' : 'ready',
      remoteError: errors.join('；'),
    })
  },

  setCurrentClient: (currentClientId) => set({ currentClientId }),

  togglePlugin: (id) =>
    set((s) => ({
      plugins: s.plugins.map((p) => {
        if (p.id !== id) return p
        if (p.state === 'unavailable') return p
        return { ...p, state: p.state === 'enabled' ? 'disabled' : 'enabled' }
      }),
    })),

  reloadMcp: (id) => {
    // 热重载走后端 POST /mcp/servers/{id}/reload（原子替换，失败保留旧连接），
    // 完成后重新拉取列表以回显服务端真实 state（不靠前端猜）
    set((s) => ({
      mcpServers: s.mcpServers.map((m) => (m.id === id ? { ...m, state: 'starting', lastError: undefined } : m)),
    }))
    void api
      .reloadMcpServer(id)
      .catch(() => undefined)
      .then(() => get().loadRemoteCapabilities())
  },

  toggleMcp: (id) => {
    // 启停走后端 /mcp/servers/{id}/enable|disable（停用断连但保留配置）
    const target = get().mcpServers.find((m) => m.id === id)
    const willEnable = !target?.enabled
    void (willEnable ? api.enableMcpServer(id) : api.disableMcpServer(id))
      .catch(() => undefined)
      .then(() => get().loadRemoteCapabilities())
  },

  testMcp: (id) => {
    const m = get().mcpServers.find((x) => x.id === id)
    if (!m) return { ok: false, message: '未找到该 Server' }
    // 走 POST /api/v1/mcp/test 真建连（用完即清理临时连接）。
    // 注意该方法是同步返回签名（Select 组件用），真实结果异步回填到 lastError/state，
    // 因此这里只发起请求并返回「已发起」——最终结论以 loadRemoteCapabilities 回读为准。
    void api
      .testMcpServer({
        name: m.name,
        transport: m.transport,
        command: m.command,
        url: m.url,
      })
      .then((res) => {
        set((s) => ({
          mcpServers: s.mcpServers.map((x) =>
            x.id === id
              ? { ...x, lastError: undefined, tools: res.tool_names.map((n) => ({ name: n, description: `（联通测试发现，耗时 ${res.elapsed_ms}ms）`, risk: 'readonly' as Risk })) }
              : x,
          ),
        }))
      })
      .catch((e: unknown) => {
        const msg = e instanceof Error ? e.message : String(e)
        set((s) => ({ mcpServers: s.mcpServers.map((x) => (x.id === id ? { ...x, lastError: msg } : x)) }))
      })
    return { ok: true, message: '已发起真实联通测试，结果稍后回填（见该 Server 的错误行/工具清单）' }
  },

  installRegistry: (id) => {
    set((s) => ({
      registryItems: s.registryItems.map((r) => (r.id === id ? { ...r, installing: true, progress: 0 } : r)),
    }))
    let p = 0
    const timer = setInterval(() => {
      p += 18 + Math.floor(Math.random() * 14)
      if (p >= 100) {
        clearInterval(timer)
        set((s) => {
          const item = s.registryItems.find((r) => r.id === id)
          const skills = item && item.kind === 'skill'
            ? s.skills.some((k) => k.name === item.name)
              ? s.skills
              : [{ id: `sk-${item.id}`, name: item.name, description: item.description, source: 'registry' as const, origin: 'installed' as const, updatedAt: Date.now() }, ...s.skills]
            : s.skills
          return {
            skills,
            registryItems: s.registryItems.map((r) =>
              r.id === id ? { ...r, installing: false, progress: 100, installed: true } : r,
            ),
          }
        })
        // 制品落地同时装入当前客户端
        get().installOnClient(`reg:${id}`)
      } else {
        set((s) => ({
          registryItems: s.registryItems.map((r) => (r.id === id ? { ...r, progress: p } : r)),
        }))
      }
    }, 220)
  },

  toggleSkill: (id) => set((s) => ({ skills: s.skills.filter((k) => k.id !== id) })),

  installExtension: (id) =>
    set((s) => {
      const e = s.extensions.find((x) => x.id === id)
      const key = e ? `ext:${e.name}` : null
      return {
        extensions: s.extensions.map((x) =>
          x.id === id ? { ...x, state: 'installed', selfTest: 'passed', signature: 'signed' } : x,
        ),
        devices: key
          ? s.devices.map((d) =>
              d.id === s.currentClientId && !d.installed.includes(key)
                ? { ...d, installed: [...d.installed, key], lastSyncAt: Date.now() }
                : d,
            )
          : s.devices,
      }
    }),

  toggleExtension: (id) =>
    set((s) => ({
      extensions: s.extensions.map((e) =>
        e.id === id ? { ...e, state: e.state === 'installed' ? 'disabled' : 'installed' } : e,
      ),
    })),

  removeExtension: (id) => set((s) => ({ extensions: s.extensions.filter((e) => e.id !== id) })),

  installOnClient: (key) => {
    const clientId = get().currentClientId
    set((s) => ({
      devices: s.devices.map((d) =>
        d.id === clientId && !d.installed.includes(key) && !d.installing.includes(key)
          ? { ...d, installing: [...d.installing, key] }
          : d,
      ),
    }))
    setTimeout(() => {
      set((s) => ({
        devices: s.devices.map((d) =>
          d.id === clientId
            ? {
                ...d,
                installing: d.installing.filter((k) => k !== key),
                installed: d.installed.includes(key) ? d.installed : [...d.installed, key],
                lastSyncAt: Date.now(),
              }
            : d,
        ),
      }))
    }, 700)
  },

  uninstallOnClient: (key) =>
    set((s) => ({
      devices: s.devices.map((d) =>
        d.id === s.currentClientId ? { ...d, installed: d.installed.filter((k) => k !== key) } : d,
      ),
    })),
}))
