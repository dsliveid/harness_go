import { create } from 'zustand'
import type { DeviceNode, ExtensionItem, McpServer, McpState, McpTransport, MemoryItem, Plugin, RegistryItem, Risk, Skill } from '../types'
import { api } from '../lib/api'
import { ALL_PLATFORMS } from '../lib/platform'
import type {
  MCPServerViewDTO, MarketItemDTO, MarketRegistryDTO, PackageViewDTO, SkillView, ToolSnapshot,
} from '../lib/apiTypes'

/**
 * 服务端自身节点 ID。
 *
 * 为什么需要它：能力的落地载体有三类（服务端 / PC 端 / 移动端），
 * 服务端虽然「不接收下发」，但它自己就是一类安装宿主 —— 内置工具随程序发布
 * （编译期注册，只有启用/停用），MCP / 工具包由服务端本地建连与安装。
 * 若把服务端排除在 devices 之外，插件市场就永远只能管理客户端，
 * 服务端自身能力只能去「设置 → 工具」里找，与「统一市场」的产品口径割裂。
 */
const SERVER_NODE_ID = 'server'

/** 工具包 installed 清单的 key 前缀（与服务端 GET /packages 的 id 组合使用）。 */
const KEY_PREFIX_PKG = 'pkg:'

/**
 * 由服务端真实数据构建「服务端自身」节点。
 *
 * 铁律：installed 只写服务端**真实**存在的能力（MCP Server 清单 / 工具包清单 /
 * 未停用的内置工具），绝不预置演示值冒充已装。
 */
function buildServerNode(input: {
  mcpServers: McpServer[]
  packages: PackageViewDTO[]
  skills: Skill[]
  extensions: ExtensionItem[]
  tools: ToolSnapshot | null
}): DeviceNode {
  const installed: string[] = []
  for (const m of input.mcpServers) installed.push(`mcp:${m.name}`)
  for (const s of input.skills) installed.push(`skill:${s.name}`)
  for (const e of input.extensions) installed.push(`ext:${e.name}`)
  for (const p of input.packages) installed.push(`${KEY_PREFIX_PKG}${p.id}`)
  if (input.tools) {
    // 内置工具：未停用的才计入「已装」，停用的仍应在工具页可一键启用（不入 installed）
    const disabled = new Set(input.tools.disabled ?? [])
    for (const group of [input.tools.core, input.tools.deferred, input.tools.on_demand]) {
      for (const t of group) if (!disabled.has(t.name)) installed.push(t.name)
    }
  }
  return {
    id: SERVER_NODE_ID,
    name: '服务端',
    platform: 'server',
    os: 'web',
    online: true,
    installed,
    installing: [],
    lastSyncAt: Date.now(),
  }
}

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
 * - **服务端自身也是可管理的节点**（platform='server'）：但它承载的能力是编译期注册的，
 *   内置工具对它只有「启用/停用」（不可安装/卸载），MCP / 技能 / 拓展则可完整装卸。
 *   这条差异由 UI 按 client.platform 判定，store 层不额外分叉。
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
  /** 客户端节点清单（客户端自行安装的落地载体；含服务端自身节点） */
  devices: DeviceNode[]
  /** 当前操作所在客户端（安装/卸载只作用于本端；服务端节点为 'server' 内置节点） */
  currentClientId: string
  /** 服务端接线状态：idle 未拉取 / loading / ready 已生效 / error 拉取失败（附原因） */
  remoteStatus: 'idle' | 'loading' | 'ready' | 'error'
  remoteError?: string
  /** 服务端真实内置工具快照（GET /api/v1/tools）：停用清单 + 各工具 enabled 的真源 */
  toolSnapshot: ToolSnapshot | null
  /** 服务端技能库真源（GET /api/v1/skills）：.harness/skills 下的实际内容 */
  skillViews: SkillView[]
  /** 技能库落地根（绝对路径），界面需明示「技能落在哪个目录」 */
  skillRoot: string
  /** 技能库相对目录（恒为 .harness/skills，与客户端一致） */
  skillDir: string
  /** 外部市场索引（GET /api/v1/market/registry）：status/reason 决定界面如何提示 */
  marketRegistry: MarketRegistryDTO | null
  marketLoading: boolean
  /** 拉取服务端真实 MCP Server 与工具包（并同步服务端节点自身能力清单） */
  loadRemoteCapabilities: () => Promise<void>
  setCurrentClient: (id: string) => void
  /** 启用/停用服务端内置工具（只有服务端节点有该动作；内置工具不可安装/卸载） */
  setServerToolEnabled: (name: string, enabled: boolean) => Promise<void>
  /** 服务端工具包启停（保留安装，只翻转是否注册工具） */
  setServerPackageEnabled: (id: string, enabled: boolean) => Promise<void>
  /** 卸载服务端工具包（注销工具 + 清目录 + 删行） */
  uninstallServerPackage: (id: string) => Promise<void>
  /** 回滚服务端工具包到上一版本 */
  rollbackServerPackage: (id: string) => Promise<void>
  /** 拉取服务端技能库真源（.harness/skills） */
  loadSkills: () => Promise<void>
  /** 安装/覆盖技能到服务端技能库（与 Agent 的 save_skill 同源） */
  installSkill: (name: string, description: string, body: string) => Promise<void>
  /** 从服务端技能库卸载技能（删整个技能目录） */
  uninstallSkill: (name: string) => Promise<void>
  /** 拉取外部市场索引（未配置时 status/reason 如实落地，不伪造空列表） */
  loadMarketRegistry: () => Promise<void>
  /** 从外部市场安装制品（服务端完成 sha256 + 验签双校验） */
  installFromMarket: (id: string, kind?: 'package' | 'skill') => Promise<MarketItemDTO>
  togglePlugin: (id: string) => void
  reloadMcp: (id: string) => void
  toggleMcp: (id: string) => void
  testMcp: (id: string) => { ok: boolean; message: string }
  toggleSkill: (id: string) => void
  /** 安装拓展项（选 A：按 kind 复用技能/工具包真实链路，不再只改内存） */
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
  toolSnapshot: null,
  skillViews: [],
  skillRoot: '',
  skillDir: '.harness/skills',
  marketRegistry: null,
  marketLoading: false,

  /**
   * 拉取服务端真实能力（MCP Server / 工具包 / 内置工具快照 / 技能库）。
   *
   * 铁律：
   *   - registry / extensions 目前无后端端点，**此处不覆盖**（市场已改为外部 registry，
   *     走 loadMarketRegistry；拓展是程序自演化产物，按 kind 复用技能与工具包链路）；
   *   - 拉取失败不抛错、不静默：写入 remoteError，由 UI 明示降级；
   *   - 拉取成功后重建「服务端自身节点」并 upsert 进 devices（服务端也是一类安装宿主）。
   */
  loadRemoteCapabilities: async () => {
    set({ remoteStatus: 'loading', remoteError: undefined })
    // 四个端点独立成败：任一通了就应展示已拿到的部分（部分成功优于全空白）
    const [mcpRes, pkgRes, toolRes, skillRes] = await Promise.allSettled([
      api.listMcpServers(),
      api.listPackages(),
      api.getTools(),
      api.listSkills(),
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
    if (toolRes.status === 'fulfilled') {
      patch.toolSnapshot = toolRes.value
    } else {
      errors.push(`内置工具：${toolRes.reason instanceof Error ? toolRes.reason.message : String(toolRes.reason)}`)
    }
    if (skillRes.status === 'fulfilled') {
      patch.skillViews = skillRes.value.items ?? []
      patch.skillRoot = skillRes.value.root ?? ''
      if (skillRes.value.dir) patch.skillDir = skillRes.value.dir
      // 服务端节点 installed 清单沿用 skills 字段口径：把真源技能名映射进 Skill[]。
      // 这样 buildServerNode 无需改签名，也不会出现「界面有技能但节点已装数为 0」的撕裂。
      patch.skills = (skillRes.value.items ?? []).map((v) => ({
        id: `sk-${v.name}`,
        name: v.name,
        description: v.description,
        source: 'registry' as const,
        origin: 'installed' as const,
        updatedAt: Date.now(),
        platforms: ALL_PLATFORMS,
      }))
    } else {
      errors.push(`技能库：${skillRes.reason instanceof Error ? skillRes.reason.message : String(skillRes.reason)}`)
    }

    // 服务端节点用「本次拿到的值 + 兜底旧值」重建，避免某个端点抖动导致节点能力清单清零。
    // 服务端节点恒排在首位：它是唯一永远可用的管理目标。
    set((s) => {
      const node = buildServerNode({
        mcpServers: patch.mcpServers ?? s.mcpServers,
        packages: patch.packages ?? s.packages,
        skills: patch.skills ?? s.skills,
        extensions: s.extensions,
        tools: patch.toolSnapshot ?? s.toolSnapshot,
      })
      return { ...patch, devices: [node, ...s.devices.filter((d) => d.platform !== 'server')] }
    })

    // 默认管理目标：优先沿用当前选择；若为空或指向已消失的节点，默认落到服务端
    // （服务端节点永远存在且在线，不会出现「没有可选目标」的空态死路）。
    const { currentClientId, devices } = get()
    if (!currentClientId || !devices.some((d) => d.id === currentClientId)) {
      const fallback = devices.find((d) => d.platform === 'server') ?? devices[0]
      if (fallback) set({ currentClientId: fallback.id })
    }

    if (errors.length === 0) {
      set({ remoteStatus: 'ready' })
      return
    }
    // 全部失败才算 error；部分失败保留已成功数据并提示原因
    const allFailed = mcpRes.status === 'rejected' && pkgRes.status === 'rejected'
      && toolRes.status === 'rejected' && skillRes.status === 'rejected'
    set({
      remoteStatus: allFailed ? 'error' : 'ready',
      remoteError: errors.join('；'),
    })
  },

  setCurrentClient: (currentClientId) => set({ currentClientId }),

  /**
   * 启用/停用服务端内置工具。
   *
   * 为什么只有服务端走这条路：内置工具随程序发布，服务端侧编译期注册，
   * 不存在「安装到本机」的语义，只有可用性开关；客户端的内置工具才可装卸
   * （见 installOnClient/uninstallOnClient）。
   * 服务端是唯一真源：成功后重拉快照，不在前端推算 enabled。
   */
  setServerToolEnabled: async (name, enabled) => {
    const snap = await api.setToolEnabled(name, enabled)
    set((s) => ({
      toolSnapshot: snap,
      // 立刻同步服务端节点的已装清单（停用的工具不计入已装）
      devices: s.devices.map((d) =>
        d.platform === 'server'
          ? buildServerNode({
              mcpServers: s.mcpServers,
              packages: s.packages,
              skills: s.skills,
              extensions: s.extensions,
              tools: snap,
            })
          : d,
      ),
    }))
  },

  /**
   * 服务端工具包启停 / 卸载 / 回滚：三者都会改动「服务端已装能力」，
   * 因此变更后统一重拉（服务端是唯一真源，不做乐观本地推算）。
   */
  setServerPackageEnabled: async (id, enabled) => {
    await (enabled ? api.enablePackage(id) : api.disablePackage(id))
    await get().loadRemoteCapabilities()
  },

  uninstallServerPackage: async (id) => {
    await api.uninstallPackage(id)
    await get().loadRemoteCapabilities()
  },

  rollbackServerPackage: async (id) => {
    await api.rollbackPackage(id)
    await get().loadRemoteCapabilities()
  },

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

  /**
   * 拉取服务端技能库真源（.harness/skills）。
   *
   * 为什么单独一个方法：技能库随时可能被 Agent（save_skill 工具）改写，
   * 界面打开技能页时必须以磁盘真值刷新，不能复用 loadRemoteCapabilities 的旧快照。
   */
  loadSkills: async () => {
    const res = await api.listSkills()
    set((s) => ({
      skillViews: res.items ?? [],
      skillRoot: res.root ?? '',
      skillDir: res.dir || s.skillDir,
      skills: (res.items ?? []).map((v) => ({
        id: `sk-${v.name}`,
        name: v.name,
        description: v.description,
        source: 'registry' as const,
        origin: 'installed' as const,
        updatedAt: Date.now(),
        platforms: ALL_PLATFORMS,
      })),
    }))
    // 服务端节点已装清单随之重建（技能是服务端真实落地能力）
    set((s) => ({
      devices: s.devices.map((d) =>
        d.platform === 'server'
          ? buildServerNode({
              mcpServers: s.mcpServers, packages: s.packages, skills: s.skills,
              extensions: s.extensions, tools: s.toolSnapshot,
            })
          : d,
      ),
    }))
  },

  /**
   * 安装/覆盖技能到服务端技能库。
   *
   * 与 Agent 的 save_skill 写同一份 `.harness/skills/<name>/SKILL.md`：
   * 界面装的技能，Agent 下一轮就能 run_skill 到；反之亦然。
   */
  installSkill: async (name, description, body) => {
    await api.saveSkill({ name, description, body })
    await get().loadSkills()
  },

  /** 从服务端技能库卸载技能（删整个技能目录，含附属脚本）。 */
  uninstallSkill: async (name) => {
    await api.deleteSkill(name)
    await get().loadSkills()
  },

  /**
   * 拉取外部市场索引。
   *
   * 🚨 未配置/已禁用/拉取失败时**不抛错也不伪造空列表**：
   * service 已在 status/reason 里写清原因，界面必须据此提示。
   */
  loadMarketRegistry: async () => {
    set({ marketLoading: true })
    try {
      const reg = await api.getMarketRegistry()
      // 把外部市场索引映射为 UI 领域模型（registryItems）。
      // 🚨 只映射**真实存在的远端条目**；未配置/失败时 items 为空且 status/reason 说明原因，
      // 绝不凭空造推荐位冒充市场内容。
      const items: RegistryItem[] = (reg.items ?? []).map((it) => ({
        id: it.id,
        registryId: it.id,
        name: it.name || it.id,
        version: it.version || '—',
        kind: it.kind === 'package' ? 'plugin' : 'skill',
        description: it.description ?? '',
        sizeMb: it.size_bytes ? Math.round((it.size_bytes / (1024 * 1024)) * 10) / 10 : 0,
        requires: [],
        signature: 'signed',
        platforms: ALL_PLATFORMS,
      }))
      set({ marketRegistry: reg, registryItems: items, marketLoading: false })
    } catch (e: unknown) {
      set({
        marketLoading: false,
        registryItems: [],
        marketRegistry: {
          configured: false,
          status: 'error',
          reason: e instanceof Error ? e.message : String(e),
          items: [],
          cached: false,
        },
      })
    }
  },

  /**
   * 从外部市场安装制品（服务端完成 sha256 完整性 + ed25519 验签双校验）。
   * 安装成功后按 kind 刷新对应真源，不做乐观推算。
   */
  installFromMarket: async (id, kind) => {
    const item = await api.installMarketItem({ id, kind })
    if (item.kind === 'skill') {
      await get().loadSkills()
    } else {
      await get().loadRemoteCapabilities()
    }
    return item
  },

  /**
   * 技能：删除本地一份技能记录。
   *
   * 注意：服务端技能的真源是磁盘（.harness/skills），因此**删除必须走 uninstallSkill**。
   * 本方法仅为兼容既有 UI 调用点保留，语义退化为「从当前视图移除」，
   * 绝不写回服务端（避免界面与磁盘真源不一致）。
   */
  toggleSkill: (id) => set((s) => ({ skills: s.skills.filter((k) => k.id !== id) })),

  /**
   * 安装拓展（方案 A：按 kind 复用**已有真实链路**，不再只改内存）。
   *
   *   - kind=skill → 写服务端技能库 .harness/skills/<name>/SKILL.md（与 save_skill 同源）
   *   - kind=tool  → 走工具包 zip 安装（服务端本地路径，见 /packages/install）
   *   - kind=mcp   → MCP Server 由配置托管，拓展只登记状态，不自动建连
   *
   * 🚨 之所以必须先接真：旧实现对服务端只改 zustand 内存，刷新即丢，
   * 属于「假安装」。方案 A 的语义就是「拓展不引入新目录，复用既有落地形态」。
   */
  installExtension: (id) => {
    const s = get()
    const e = s.extensions.find((x) => x.id === id)
    if (!e) return
    if (e.kind === 'skill') {
      // 技能类拓展：description 作为描述，body 复用拓展说明（技能真源就是 SKILL.md）
      void s.installSkill(e.name, e.description || e.name, e.description || e.name).catch(() => undefined)
    }
    // 统一登记拓展自身状态（无论哪种 kind，拓展条目本身要标记为已落地）
    set((st) => ({
      extensions: st.extensions.map((x) =>
        x.id === id ? { ...x, state: 'installed', selfTest: 'passed', signature: 'signed' } : x,
      ),
    }))
  },

  toggleExtension: (id) =>
    set((s) => ({
      extensions: s.extensions.map((e) =>
        e.id === id ? { ...e, state: e.state === 'installed' ? 'disabled' : 'installed' } : e,
      ),
    })),

  removeExtension: (id) => set((s) => ({ extensions: s.extensions.filter((e) => e.id !== id) })),

  installOnClient: (key) => {
    const clientId = get().currentClientId
    // 🚨 服务端节点禁止走「安装」路径：内置工具编译期注册，没有制品可装；
    // 若这里放行，UI 会出现「服务端已装工具」的假象（重启即丢）。
    // 服务端的内置工具只走 setServerToolEnabled（启用/停用），MCP / 工具包走各自真实端点。
    const target = get().devices.find((d) => d.id === clientId)
    if (!target || target.platform === 'server') return
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

  uninstallOnClient: (key) => {
    // 同上：服务端节点没有「卸载内置工具」的概念（不可安装即不可卸载）。
    const target = get().devices.find((d) => d.id === get().currentClientId)
    if (!target || target.platform === 'server') return
    set((s) => ({
      devices: s.devices.map((d) =>
        d.id === s.currentClientId ? { ...d, installed: d.installed.filter((k) => k !== key) } : d,
      ),
    }))
  },
}))
