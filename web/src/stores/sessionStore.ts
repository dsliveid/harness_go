import { create } from 'zustand'
import type { AccessMode, ApprovalRule, DataScope, DataScopeGrant, DataScopeMode, PlanMode, Project, ProjectLink, Session, Workspace, WorkspaceKind } from '../types'
import { api } from '../lib/api'
import type { CreateWorkspaceInput } from '../lib/api'
import { useUI } from './uiStore'
import { useSettings } from './settingsStore'
import {
  adoptServerSessions, clearScopeOverride, loadPersistedActiveWorkspace, loadPersistedProjects,
  loadPersistedWorkspaces, pruneScopeOverrides, resolveProjectByPath, savePersistedActiveWorkspace,
  savePersistedProjects, savePersistedWorkspaces, setScopeOverride,
} from './sessionScope'

/**
 * 内置服务端空间 id。
 *
 * 🚨 必须与服务端 `store.BuiltinWorkspaceServerID = "ws-server"` **逐字一致**。
 * 历史缺陷（2026-11）：前端曾用 'ws-srv'，与服务端差一个字符，
 * 导致归属判定、在线状态、内置空间识别全部对不上（会话总被归到「纯对话」）。
 */
const DEFAULT_WORKSPACE_ID = 'ws-server'

/** 旧前端常量（'ws-srv'）：仅用于 localStorage 脏数据迁移，不得再作为 id 使用 */
const LEGACY_WORKSPACE_ID = 'ws-srv'

/** 内置服务端空间：服务端 0009 迁移已落库，这里只是后端未装配时的兜底占位 */
const BUILTIN_WORKSPACE: Workspace = {
  id: DEFAULT_WORKSPACE_ID,
  name: '服务端工作空间',
  kind: 'server',
  online: true,
  limbCount: 1,
  builtin: true,
  // 服务端内置空间自身就是执行宿主：恒在线、全工具能力。
  bound: true,
  toolPolicy: 'full',
}

/** 项目所属工作空间（缺省视为内置服务端空间） */
export const wsOfProject = (p: Project) => p.workspaceId ?? DEFAULT_WORKSPACE_ID

/**
 * 新会话的初始模型（不存在「默认模型」这个模糊选项，落到具体模型或空）：
 *  ① 同一作用域内最近的会话：项目对话看同项目最新一条，纯对话看上一条纯对话；
 *  ② 设置 → 能力模型槽位 chat（全局默认对话模型，需仍在启用厂商列表内）；
 *  ③ 兜底为空串 = 未选择（发送时会提示先选模型）。
 */
function inheritModelRef(sessions: Session[], projectId?: string): string {
  const scope = sessions.filter((s) => (projectId ? s.projectId === projectId : !s.projectId))
  const inherited = scope.find((s) => !s.archivedAt && s.modelRef)?.modelRef
  if (inherited) return inherited
  const { providers, capabilityModels } = useSettings.getState().settings
  const chatSlot = capabilityModels?.chat ?? ''
  if (chatSlot) {
    const pid = chatSlot.slice(0, chatSlot.indexOf('/'))
    const mid = chatSlot.slice(chatSlot.indexOf('/') + 1)
    const p = providers.find((x) => x.id === pid)
    if (p?.enabled && p.models.some((m) => m.id === mid)) return chatSlot
  }
  return ''
}

/** 会话所属工作空间：优先自身 workspaceId，其次按项目推导，纯对话缺省内置空间 */
export const wsOfSession = (s: Session, projects: Project[]) =>
  s.workspaceId
  ?? (s.projectId ? projects.find((p) => p.id === s.projectId)?.workspaceId : undefined)
  ?? DEFAULT_WORKSPACE_ID

/**
 * 把输入框下方的 token 用量 / 上下文水位切到目标会话自己的账本。
 *
 * 动态 import 规避 sessionStore ↔ chatStore 的静态循环依赖（同 reloadMessages 的既有写法）。
 * 不调用它就会出现「新开对话后 token 用量与上下文占用纹丝不动」——数值还是上一个会话的累计量。
 */
function syncUsage(sessionId: string) {
  void import('./chatStore').then((m) => m.useChat.getState().activateSession(sessionId))
}

/**
 * 把「项目 / 工作空间 / 当前空间」写入本地持久化。
 * 三者都是纯前端实体，不在服务端落库，只能靠本地存储保证刷新前后一致。
 */
function persistScope(st: { projects: Project[]; workspaces: Workspace[]; activeWorkspaceId: string }) {
  savePersistedProjects(st.projects)
  savePersistedWorkspaces(st.workspaces)
  savePersistedActiveWorkspace(st.activeWorkspaceId)
}

/**
 * 把项目设置补丁写到服务端，**成功后**才更新本地 state。
 *
 * 为什么不用乐观更新：这些配置（约束 / 规划模式 / 数据权限）会被后端真正消费
 * （约束注入 prompt、数据权限决定弹不弹审批）。若先改本地再写服务端，
 * 一旦请求失败，界面显示「已改」而引擎仍按旧值运行 —— 属安全语义漂移，
 * 与 setProjectDefaultAccessMode 的既有判断完全同源。
 */
function patchProject(
  id: string,
  patch: { constraints?: string; planMode?: PlanMode },
  failMsg: string,
): Promise<void> {
  return api
    .updateProject(id, patch)
    .then(() => {
      useSessions.setState((s) => ({
        projects: s.projects.map((p) => (p.id === id ? { ...p, ...patch } : p)),
      }))
    })
    .catch((err) => {
      useUI.getState().toast('error', failMsg)
      throw err
    })
}

/**
 * 是否为「本地乐观壳」的临时 id。
 *
 * create() 用 `s-<Date.now()>` 先行插入本地列表，等服务端返回真实 id 后替换。
 * 实时快照/合并必须能识别出这类条目并保留它们，否则会出现
 * 「刚点新对话，列表里却没有它」（服务端还不知道它存在）。
 *
 * 判定刻意收窄为「s- 前缀 + 纯数字时间戳」：服务端真实 id 由服务端生成，
 * 不会撞上这一形态；同时绝不能把所有非快照条目都当乐观壳（那会把别端删除的
 * 幽灵会话永久保留下来）。
 */
function isOptimisticId(id: string): boolean {
  return /^s-\d{10,}$/.test(id)
}

/**
 * 按 updatedAt 降序稳定排序。
 *
 * 「稳定」是指：updatedAt 相同时保持**原有相对顺序**（用下标兜底），
 * 否则每来一帧就会出现等值条目互换位置，用户看到列表莫名跳动。
 * 乐观壳的 updatedAt 是 Date.now()，天然排在前面（刚新建的会话就该在最上）。
 */
function sortStable(list: Session[]): Session[] {
  return list
    .map((s, i) => ({ s, i }))
    .sort((a, b) => (b.s.updatedAt ?? 0) - (a.s.updatedAt ?? 0) || a.i - b.i)
    .map((x) => x.s)
}

/**
 * 在给定列表里挑出「指定工作空间内、未归档、最新一条」会话。
 *
 * 统一口径的用途（2026-11 用户要求「刷新后默认选中最新一条对话」）：
 *   - `load()` 首屏默认选中；
 *   - `applyServerSnapshot` 的快照回落；
 *   - 删除/归档当前会话后的回落。
 * 三处若各写一套判定，必然漂移成「左边选中 A、右边像没内容」的错位体感。
 *
 * 🚨 不依赖调用方传入的列表是否已排序，直接按 updatedAt 取最大：
 * `applyServerRemoval` 的候选集是 `sessions.filter(...)`，其数组顺序就是 store
 * 里的物理顺序（可能是乐观壳插到最前、也可能是历史遗留顺序），若这里用 `find`
 * 取「首个命中」，得到的其实是「数组里最靠前的那条」而不是「最新的那条」。
 */
function newestVisibleIn(
  list: Session[],
  wsId: string,
  projects: Project[],
): Session | undefined {
  let best: Session | undefined
  for (const s of list) {
    if (s.archivedAt) continue
    if (wsOfSession(s, projects) !== wsId) continue
    if (!best || (s.updatedAt ?? 0) > (best.updatedAt ?? 0)) best = s
  }
  return best
}

interface SessionState {
  workspaces: Workspace[]
  activeWorkspaceId: string
  sessions: Session[]
  projects: Project[]
  // 项目级数据：关联项目 + 审批规则（在项目设置中管理）
  projectLinks: ProjectLink[]
  approvalRules: ApprovalRule[]
  /** 项目级数据权限策略（数据范围 × 拒绝/询问/允许） */
  dataGrants: DataScopeGrant[]
  currentId: string
  /** 从服务端拉取会话列表（real 模式；登录后/进入工作台时调用） */
  load: () => Promise<void>
  select: (id: string) => void
  create: (projectId?: string) => string
  rename: (id: string, title: string) => void
  /**
   * 拉取单个会话并以服务端 title 回填本地列表。
   *
   * 存在意义（2026-11 缺陷：发任何消息对话名都还叫「新对话」）：
   * 自动标题由**服务端**在首条消息时生成并落库（run_service.deriveSessionTitle），
   * 而前端 POST /runs 的 202 回包只带 run_id/status，从不回传 title ——
   * 本地 sessions 的 title 就一直停在乐观壳的「新对话」，直到用户刷新页面
   * （重新 listSessions）才会突然变成正文前 24 字。中间这段时间用户看到的就是
   * 「发了消息，标题还是新对话」。
   *
   * 因此发送成功后主动 GET 一次（只同步 title，不整段替换会话：避免覆盖本地
   * 独有的 workspaceId 分组），失败静默不打扰用户（标题只是体验项）。
   */
  refreshTitle: (id: string) => Promise<void>
  remove: (id: string) => void
  archive: (id: string) => void
  unarchive: (id: string) => void
  setAccessMode: (id: string, mode: AccessMode) => void
  setModel: (id: string, modelRef: string) => void
  // ---- 工作空间（服务端为准：执行面实体 + 设备联动） ----
  /** 从服务端拉取全部工作空间（唯一数据源；失败时保留上次已知状态） */
  loadWorkspaces: () => Promise<void>
  setWorkspace: (id: string) => void
  /** 在服务端新建工作空间（设备绑定/kind 一致性/唯一性全部由服务端校验） */
  createWorkspace: (input: CreateWorkspaceInput) => Promise<Workspace>
  /**
   * 移除工作空间（级联删除）。
   *
   * 🚨 必须真的调服务端 DELETE（`migrate_refs=true`）：旧实现只改本地 zustand +
   * localStorage，服务端仍有该空间，刷新时 `loadWorkspaces()` 又拉回来 ——
   * 这正是用户报告的「移除后刷新界面又出现了」。
   *
   * 服务端语义：单事务内把该空间的会话/项目一并软删、清规则、物理删空间行。
   * 成功后本地同步移除这些会话与项目，最后重拉列表以服务端为准校正。
   *
   * 失败（409/网络）抛错给调用方 toast，界面绝不做「假成功」的乐观移除。
   */
  removeWorkspace: (id: string) => Promise<boolean>
  /**
   * 绑定设备到未绑定的工作空间（一对一：设备必须未被任何空间占用）。
   *
   * 成功后重拉服务端列表：deviceName / online / toolPolicy 都是服务端派生字段，
   * 前端拼不出来（拼出来就是第二套判定，必然漂移）。
   */
  bindWorkspaceDevice: (wsId: string, deviceId: string) => Promise<void>
  /**
   * 解绑工作空间的设备归属（`device_id` 置空）。
   *
   * 🚨 服务端对解绑**不校验设备存在性**：设备被吊销/物理删除后仍必须能解绑，
   * 否则该空间既删不掉（已绑定）又解不开（设备不存在）→ 永久僵尸空间。
   * 因此前端对「设备已解绑/已删除」的空间也必须显示解绑按钮。
   */
  unbindWorkspaceDevice: (wsId: string) => Promise<void>
  // ---- 项目操作（对齐桌面端） ----
  /**
   * 新建项目（落服务端 projects 表）。
   *
   * 🚨 返回 Promise：**调用方必须 await 成功后再提示成功**。
   * 旧签名 `=> string` 只回本地临时 id，落库在后台异步进行，调用方拿到就提示
   * 「已创建」→ 服务端拒绝时会出现「成功 + 失败」双 toast（2026-11 实测缺陷）。
   *
   * @returns 服务端最终生成的真实项目 id（失败时 reject，并已由本方法提示错误）
   */
  createProject: (name: string, path: string) => Promise<string>
  renameProject: (id: string, name: string) => void
  removeProject: (id: string) => void
  toggleProjectPin: (id: string) => void
  setProjectConstraints: (id: string, constraints: string) => void
  setProjectPlanMode: (id: string, planMode: PlanMode) => void
  /**
   * 从服务端拉取项目列表（real 模式；服务端 projects 表为权威源）。
   * 合并服务端字段与本地独有的 workspaceId/constraints/planMode，并回写本地持久化。
   */
  loadProjects: () => Promise<void>
  /**
   * 拉取单个项目的设置子集合（关联项目 + 数据权限）。
   *
   * 这两个集合是项目级数据，服务端只提供按项目的列表端点（与审批规则同口径），
   * 因此由「打开项目设置」时按需触发，不进入首屏批量加载。
   */
  loadProjectSettings: (projectId: string) => Promise<void>
  /**
   * 设置项目默认控制权限（**只影响此后新建的对话**）。
   * real 模式落服务端 projects.default_access_mode，是权限继承的唯一权威源。
   */
  setProjectDefaultAccessMode: (id: string, mode: 'confirm' | 'full') => Promise<void>
  // ---- 关联项目 / 审批规则（项目设置中管理） ----
  addProjectLink: (projectId: string, path: string, description: string) => void
  updateProjectLink: (id: string, path: string, description: string) => void
  deleteProjectLink: (id: string) => void
  addApprovalRule: (projectId: string, toolName: string, pattern: string) => void
  removeApprovalRule: (id: string) => void
  // ---- 数据权限策略 ----
  setDataScope: (projectId: string, scope: DataScope, mode: DataScopeMode) => void
  dataScopeOf: (projectId: string, scope: DataScope) => DataScopeMode
  // ---- 实时同步（会话列表级 SSE：/api/v1/sessions/events） ----
  /**
   * 应用服务端**全量快照**（SSE 首帧 / 重连后首帧）。
   *
   * 🚨 不能直接 `set({ sessions: list })`：必须走 adoptServerSessions 补
   * projectId/workspaceId 归属，否则刷新后项目会话会掉进「纯对话」（历史缺陷）。
   * 同时保留本地乐观插入的壳（服务端尚未落库的「新对话」不能被快照抹掉）。
   */
  applyServerSnapshot: (list: Session[]) => void
  /**
   * 应用单个会话变更（按 id upsert）。
   *
   * 幂等：同一 id 多次到达只表现为重复覆盖，故 SSE 的乱序/重复帧天然安全。
   */
  applyServerPatch: (session: Session) => void
  /**
   * 应用单个会话删除。
   *
   * 若删除的正是当前会话，回落到列表首条（或空），避免右侧停留在已不存在的会话上。
   */
  applyServerRemoval: (id: string) => void
  /**
   * 上报当前会话已读（清掉侧栏红点）。
   *
   * 同时做**本地乐观清除**（立刻 `unread=false`）再异步上报：
   * 若等网络往返才清红点，用户点开会话后会看到红点"迟滞一拍"才消失，
   * 观感上像没生效。服务端校验通过后会由 SSE 推回权威值，两者最终一致。
   */
  markRead: (id: string) => void
  /**
   * 本地把某会话的「最新时间」推到现在并重排列表（发消息即置顶）。
   *
   * 为什么必须本地做：服务端 updated_at 要靠 `TouchMessage` 回写，而那是
   * **与会话库写入不同事务、最终一致** 的异步摘要回写 —— 用户发完消息到
   * SSE `session.updated` 帧到达之间存在真空期，列表纹丝不动，
   * 体感就是「刚聊过的对话不往上走」。这里按本地事实先置顶，
   * 随后被服务端权威帧覆盖（同口径，不会来回跳）。
   */
  bumpSession: (id: string, at?: number) => void
}

// 项目：**服务端 projects 表是权威源**（由 loadProjects() 拉取）；
// 本地持久化只用于首屏秒开与水合前占位，且**只保留前端独有字段**（workspaceId/constraints/planMode）。
//
// 🚨 踩坑记录（2026-11）：这里曾用演示项目兜底，导致首屏注入 p1/p2/p3 三条假项目。
// 它们服务端根本不存在，用户对其切换权限 → PATCH 404 → 前端误报「保存失败」
// 且刷新后回退旧值（实为服务端从未存过）。必须从空列表起步，由服务端填充。
const initialProjects: Project[] = loadPersistedProjects() ?? []

/** 工作空间：首屏用本地持久化占位，随后由 loadWorkspaces() 以服务端为准覆盖 */
// loadPersistedWorkspaces() 已做脏数据自愈（逐项校验形状），此处拿到的必然是合法 Workspace[]，
// 不会再出现 undefined 元素撑爆首屏渲染（2026-11 登录后黑屏事故）。
//
// 🚨 in-flight 迁移：把历史 localStorage 里的 'ws-srv' 全部改写为 'ws-server'。
// 若不迁移，用户本地残留的旧空间记录会与当前会话归属对不上，表现为
// 「刚聊过的对话刷新后掉进纯对话」（因为 ws-srv 在服务端根本不存在）。
const persistedWorkspaces: Workspace[] = migrateLegacyWorkspaces(loadPersistedWorkspaces())
const initialWorkspaces: Workspace[] = persistedWorkspaces.some((w) => w.id === DEFAULT_WORKSPACE_ID)
  ? persistedWorkspaces
  : [BUILTIN_WORKSPACE, ...persistedWorkspaces.filter((w) => w.id !== DEFAULT_WORKSPACE_ID)]
const persistedActiveWs = loadPersistedActiveWorkspace()
const initialActiveWs = persistedActiveWs === LEGACY_WORKSPACE_ID
  ? DEFAULT_WORKSPACE_ID
  : (persistedActiveWs ?? DEFAULT_WORKSPACE_ID)

/**
 * 把本地持久化里的旧内置空间 id 迁移到服务端口径。
 *
 * 只改 id 与 builtin/toolPolicy 这类「确定已知」的字段，其余字段原样保留：
 * 该空间最终会被 loadWorkspaces() 用服务端真实数据整体替换，这里只为过渡期不崩。
 */
function migrateLegacyWorkspaces(list: Workspace[]): Workspace[] {
  return list.map((w) =>
    w.id === LEGACY_WORKSPACE_ID
      ? { ...w, id: DEFAULT_WORKSPACE_ID, name: '服务端工作空间', kind: 'server', builtin: true, bound: true, online: true, toolPolicy: 'full' }
      : w,
  )
}

export const useSessions = create<SessionState>((set, get) => ({
  workspaces: initialWorkspaces,
  activeWorkspaceId: initialActiveWs,
  // 会话列表来自主库（load() 拉取），初始为空态
  sessions: [],
  projects: initialProjects,
  // 项目级数据（关联项目 / 审批规则 / 数据权限）同样从空态起步：
  // 服务端无「列全部规则」端点，规则由 loadProjects 之后按需回填，绝不预置演示行。
  projectLinks: [],
  approvalRules: [],
  dataGrants: [],
  currentId: '',

  // 登录后从主库拉取会话列表：空列表自动建一条，避免右侧空屏
  load: async () => {
    // 先拉工作空间：会话归属判定（wsOfSession）依赖它，晚拉会让首屏闪一次「纯对话」。
    await get().loadWorkspaces()
    try {
      const list = await api.listSessions({ limit: 100 })
      if (list.length === 0) {
        get().create()
        return
      }
      // 归属恢复（踩坑：刷新后项目会话掉进「纯对话」）：
      // 服务端 sessions 只有 project_dir，没有 project_id / workspace_id，
      // toUiSession 映射不出归属 → 必须按已落库的目录反查项目，把 projectId/workspaceId 补回来。
      const withScope = adoptServerSessions(list, get().projects)
      // 🚨 排序不信任服务端返回的物理顺序（2026-11 用户要求「参照老版本按最新时间倒序」）：
      // 服务端 SQL 虽已 ORDER BY updated_at DESC，但分页合并 / 本地乐观壳混入后
      // 顺序仍可能错乱 —— 统一在落状态前过一遍 sortStable，保证恒定倒序。
      const ordered = sortStable(withScope)
      const keep = get().currentId
      const activeWs = get().activeWorkspaceId
      // 刷新默认选中：**当前工作空间内、未归档的最新一条会话**。
      //
      // 旧实现用 `list[0]`（全局最新，可能落在别的空间或是一条已归档/空会话），
      // 一旦它与侧栏可见分组的首条不一致，用户看到的就是「左边选中的是 A，
      // 右边却像没内容」这种错位体感。
      const newestInWs = ordered.find(
        (s) => !s.archivedAt && wsOfSession(s, get().projects) === activeWs,
      )
      // 保持续用：上次选中的会话若仍存在且未归档，优先继续选中它（刷新不该打断用户手选）。
      const keepSess = keep ? ordered.find((s) => s.id === keep && !s.archivedAt) : undefined
      const current = keepSess?.id ?? newestInWs?.id ?? ordered.find((s) => !s.archivedAt)?.id ?? ordered[0].id
      // 当前空间若在新列表里已无任何可见会话（如它只是上次的临时空间），
      // 回落到当前会话自身所属空间，避免刷新后停在一个空空间、看不到刚聊过的对话。
      const curSess = ordered.find((s) => s.id === current)
      const hasInActive = Boolean(newestInWs)
      const nextActiveWs = hasInActive || !curSess ? activeWs : wsOfSession(curSess, get().projects)
      set({ sessions: ordered, currentId: current, activeWorkspaceId: nextActiveWs })
      // 动态 import 规避 sessionStore ↔ chatStore 的静态循环依赖。
      // 🚨 首屏这里必须 await（不能 void）：用户反馈「刷新后选中第一条但右侧空白」，
      // 根因之一就是 void 出去后任何一次静默失败都没有第二次机会。await 让首屏消息
      // 加载在 load() 返回前完成；即便失败，MessageList 的「选中即有内容」不变量
      // effect 还会兜一次（见 chatStore.ensureMessages）。
      const { useChat } = await import('./chatStore')
      const ok = await useChat.getState().ensureMessages(current)
      // 刷新后接回服务端仍在跑的 Run（否则界面看不出对话还在进行中）
      if (ok) void useChat.getState().resumeRun(current)
      // token 水位与活跃会话对齐（否则沿用上一个会话的用量）
      useChat.getState().activateSession(current)
    } catch {
      useUI.getState().toast('error', '加载会话列表失败，请检查服务状态')
    }
  },

  // ---- 实时同步（会话列表级 SSE） ----
  // 设计要点：三个 action 都**不做网络请求**，只把服务端推来的权威视图合并进本地状态。
  // 「查库」这件事由服务端在推送前完成（见 session_events_handler.go），前端只管落位。
  applyServerSnapshot: (list) => {
    const st = get()
    if (list.length === 0) return
    // 🚨 必须补归属：服务端 sessions 表只有 project_dir，没有 workspaceId 的前端分组语义。
    // 不调 adoptServerSessions 就会让项目会话刷新后掉进「纯对话」（历史缺陷）。
    const withScope = adoptServerSessions(list, st.projects)
    // 保留本地乐观壳：用户刚点「新对话」时服务端还没返回真实 id，
    // 快照若直接覆盖会把这行抹掉，界面表现为「点了新对话但列表里没有」。
    // 判定标准：不在快照里、且仍是本地临时 id（s-<时间戳>）的条目。
    const serverIds = new Set(withScope.map((s) => s.id))
    const optimistic = st.sessions.filter((s) => !serverIds.has(s.id) && isOptimisticId(s.id))
    const merged = sortStable([...withScope, ...optimistic])
    // currentId 若已不在列表中（被别端删除），回落到「当前空间内最新未归档会话」
    // （与 load() 同口径），避免右侧停在幽灵会话或空落落的错位会话上。
    const current = merged.some((s) => s.id === st.currentId)
      ? st.currentId
      : (newestVisibleIn(merged, st.activeWorkspaceId, st.projects)?.id ?? merged[0]?.id ?? '')
    set({ sessions: merged, currentId: current })
  },

  applyServerPatch: (session) => {
    const st = get()
    // 补归属（同上）：单条变更同样需要 projectId/workspaceId 才能正确分组。
    const [patched] = adoptServerSessions([session], st.projects)
    if (!patched) return
    const exists = st.sessions.some((s) => s.id === patched.id)
    // upsert：已存在则替换（服务端是权威视图，整体覆盖），否则插入。
    //
    // 注意：替换时**保留本地壳的 projectId/workspaceId 兜底**？不保留 ——
    // adoptServerSessions 已按 project_dir 反查补齐，本地 override 也存在其中，
    // 无条件相信服务端视图才不会产生「两套归属判定」漂移。
    const next = exists
      ? st.sessions.map((s) => (s.id === patched.id ? patched : s))
      : [patched, ...st.sessions]
    set({ sessions: sortStable(next) })
  },

  applyServerRemoval: (id) => {
    const st = get()
    if (!st.sessions.some((s) => s.id === id)) return
    const next = st.sessions.filter((s) => s.id !== id)
    // 删除的正是当前会话 → 回落到「当前空间内最新未归档会话」（与 load() 同口径），
    // 而不是机械取 next[0]（那可能是别的空间/已归档会话 → 右侧看起来空落落）。
    const fallback = st.currentId === id
      ? (newestVisibleIn(next, st.activeWorkspaceId, st.projects)?.id ?? next[0]?.id ?? '')
      : st.currentId
    set({ sessions: next, currentId: fallback })
    if (st.currentId === id) syncUsage(fallback)
  },

  bumpSession: (id, at) => {
    const st = get()
    const hit = st.sessions.find((s) => s.id === id)
    if (!hit) return
    const now = at ?? Date.now()
    set({
      sessions: sortStable(
        st.sessions.map((s) => (s.id === id ? { ...s, updatedAt: now } : s)),
      ),
    })
  },

  select: (id) => {
    if (id !== get().currentId) syncUsage(id)
    set({ currentId: id })
    if (id) {
      // 选中即已读：立刻清本地红点并异步上报服务端。
      // 放在 select 里而不是 Sidebar 的点击回调里 —— 任何入口（搜索结果、
      // 快捷键、深层链接）切换会话都必须同样清红点，避免漏点。
      get().markRead(id)
      // 切换会话即拉取该会话消息快照（以 DB 为准）。
      // 这里用 reloadMessages（而非 ensureMessages）：切回旧会话可能要拿到别端新写入的消息，
      // 必须强制重拉；首屏兜底由 MessageList 的「选中即有内容」不变量负责。
      void import('./chatStore').then((m) => {
        // 先清掉上一会话残留的进行中态，再按目标会话的真实情况重建
        void m.useChat.getState().reloadMessages(id)
          .then((ok) => { if (ok) void m.useChat.getState().resumeRun(id) })
      })
    }
  },

  markRead: (id) => {
    if (!id) return
    const st = get()
    const target = st.sessions.find((s) => s.id === id)
    // 已经是已读就完全不发请求：select 会被频繁触发（每次切会话都调），
    // 无脑上报会让「切来切去」产生一串无效 POST。
    if (!target?.unread) return
    // 本地乐观清除：立刻让红点消失，不等网络往返。
    set({ sessions: st.sessions.map((s) => (s.id === id ? { ...s, unread: false } : s)) })
    // 失败静默：红点只是提示项，不能因为它弹错误打扰对话。
    // 服务端成功后会由列表级 SSE 推回权威值（unread=false），最终一致。
    void api.markSessionRead(id).catch(() => {})
  },

  // ---- 工作空间 ----
  /**
   * 从服务端拉取全部工作空间（**唯一数据源**）。
   *
   * 为什么必须服务端为准：工作空间 = 执行面，其在线状态、绑定设备、工具能力策略
   * 都是运行时事实，前端不可能靠 localStorage 假装出来。
   * 历史缺陷（2026-11）：这里曾只读 localStorage + 一个硬编码内置空间，
   * 于是下拉列表永远是「静态数据」，且与设备完全脱节。
   */
  loadWorkspaces: async () => {
    try {
      const list = await api.listWorkspaces()
      const st = get()
      // 内置服务端空间必须存在（服务端 0009 迁移已插入 ws-server，理论上必在；
      // 若后端未装配工作空间域返回空列表，则保留本地内置项兜底，绝不让下拉列表空掉）。
      const next = list.length > 0 ? list : st.workspaces
      const activeId = next.some((w) => w.id === st.activeWorkspaceId)
        ? st.activeWorkspaceId
        : (next[0]?.id ?? DEFAULT_WORKSPACE_ID)
      set({ workspaces: next, activeWorkspaceId: activeId })
      persistScope(get())
    } catch {
      // 拉取失败不改列表：保留上次已知的可用状态，避免下拉列表瞬间清空
      // （不清空比清空安全：清空会让当前会话失去归属，界面闪「纯对话」）。
    }
  },
  setWorkspace: (id) => {
    const st = get()
    if (id === st.activeWorkspaceId) return
    if (!st.workspaces.some((w) => w.id === id)) return
    const inWs = st.sessions.filter((s) => !s.archivedAt && wsOfSession(s, st.projects) === id)
    if (inWs.some((s) => s.id === st.currentId)) {
      set({ activeWorkspaceId: id })
      persistScope(get())
      return
    }
    set({ activeWorkspaceId: id, currentId: inWs[0]?.id ?? '' })
    persistScope(get())
    // 切空间即切会话：token 水位同步切到目标会话账本
    syncUsage(inWs[0]?.id ?? '')
    // 该空间还没有任何会话时自动建一条，避免右侧空屏
    if (inWs.length === 0) get().create()
  },
  /**
   * 在服务端新建工作空间并切换到它。
   *
   * 🚨 与旧的 `connectWorkspace` 的根本区别：这里**真的调服务端**，
   * 且设备绑定必须在服务端完成（一对一 + kind 一致性 + 设备存在性全部由服务端校验）。
   * 旧实现在前端凭空造一条 `online: true` 记录、无设备参与，导致「接入设备」纯属幻觉。
   *
   * 失败时原样抛出，由调用方 toast 服务端返回的 message（不吞错、不伪造成功）。
   */
  createWorkspace: async (input) => {
    const created = await api.createWorkspace(input)
    set((st) => ({
      workspaces: st.workspaces.some((w) => w.id === created.id)
        ? st.workspaces.map((w) => (w.id === created.id ? created : w))
        : [...st.workspaces, created],
    }))
    persistScope(get())
    get().setWorkspace(created.id)
    return created
  },
  /**
   * 绑定设备到未绑定的工作空间。
   *
   * 服务端已保证：设备存在性、kind 一致性、一对一唯一性。前端不做二次判定，
   * 只在失败时把服务端 message 原样抛给调用方（绝不吞错）。
   */
  bindWorkspaceDevice: async (wsId, deviceId) => {
    await api.updateWorkspace(wsId, { deviceId })
    // 重拉：deviceName/online/toolPolicy 是服务端派生字段，必须由服务端算。
    await get().loadWorkspaces()
  },
  /**
   * 解绑工作空间的设备归属。
   *
   * 传 `deviceId: ''` 即解绑：服务端 Update 在置空时**跳过**设备存在性/在线/吊销校验，
   * 这是「设备已吊销后仍可解绑」的死锁逃生通道（见 workspace_service.go 铁律三）。
   */
  unbindWorkspaceDevice: async (wsId) => {
    await api.updateWorkspace(wsId, { deviceId: '' })
    await get().loadWorkspaces()
  },
  removeWorkspace: async (id) => {
    const st = get()
    const ws = st.workspaces.find((w) => w.id === id)
    // 内置服务端空间不可移除
    if (!ws || ws.builtin) return false
    // ① 先落服务端（真删 + 级联）：失败就原样抛出，界面不产生「假成功」。
    //    不先改本地状态的原因：旧实现只改本地 → 刷新被 loadWorkspaces 拉回来（用户报告的 bug）。
    await api.deleteWorkspace(id, { migrateRefs: true })
    // ② 服务端已级联删除成功，本地同步移除该空间及其会话/项目。
    //    语义必须与服务端一致：服务端把会话与项目一并删了，本地就不能留着
    //    （否则界面上还挂着一批"幽灵"会话，点进去必 404）。
    const cur = get()
    const goneProjectIds = cur.projects.filter((p) => wsOfProject(p) === id).map((p) => p.id)
    const keptProjects = cur.projects.filter((p) => wsOfProject(p) !== id)
    const keptSessions = cur.sessions.filter((s) => wsOfSession(s, cur.projects) !== id)
    const fallbackWs = cur.activeWorkspaceId === id ? DEFAULT_WORKSPACE_ID : cur.activeWorkspaceId
    const fallbackSessions = keptSessions.filter(
      (s) => !s.archivedAt && wsOfSession(s, keptProjects) === fallbackWs,
    )
    const needCreate = cur.activeWorkspaceId === id && fallbackSessions.length === 0
    const aliveWsIds = cur.workspaces.filter((w) => w.id !== id).map((w) => w.id)
    set({
      // 空间、其会话、其项目全部消失（与服务端级联删除一一对应）。
      workspaces: cur.workspaces.filter((w) => w.id !== id),
      projects: keptProjects,
      sessions: keptSessions,
      // 附属配置也要一并清掉，否则会残留指向已删项目/会话的悬空记录。
      projectLinks: cur.projectLinks.filter((l) => !goneProjectIds.includes(l.projectId)),
      approvalRules: cur.approvalRules.filter((r) => !goneProjectIds.includes(r.projectId)),
      dataGrants: cur.dataGrants.filter((g) => !goneProjectIds.includes(g.projectId)),
      activeWorkspaceId: fallbackWs,
      currentId: fallbackSessions.some((s) => s.id === cur.currentId)
        ? cur.currentId
        : (fallbackSessions[0]?.id ?? cur.currentId),
    })
    // 清理悬空归属：指向已删空间/项目的 override 必须清掉，否则分组会指向幽灵实体。
    pruneScopeOverrides(keptProjects.map((p) => p.id), aliveWsIds)
    persistScope(get())
    // ③ 以服务端为准校正一次列表：确保 UI 与 DB 同源（刷新前后一致）。
    //    失败时 loadWorkspaces 内部保留当前状态，不会把列表清空。
    await get().loadWorkspaces()
    if (needCreate) get().create()
    return true
  },

  create: (projectId) => {
    const id = `s-${Date.now()}`
    const proj = projectId ? get().projects.find((p) => p.id === projectId) : undefined
    // 控制权限由**服务端按项目默认值快照裁决**（单一权威源）；本地乐观壳仅做占位展示，
    // 服务端回包会覆盖它。前端不再复刻「项目默认 > confirm」这套逻辑，避免两处漂移。
    const s: Session = {
      id,
      title: '新对话',
      projectId,
      workspaceId: proj ? wsOfProject(proj) : get().activeWorkspaceId,
      projectDir: proj?.path,
      accessMode: proj?.defaultAccessMode ?? 'confirm',
      // 新会话模型：沿用同作用域最近会话的选择，其次全局 chat 槽位；为空 = 未选择
      modelRef: inheritModelRef(get().sessions, projectId),
      updatedAt: Date.now(),
    }
    set((st) => ({ sessions: [s, ...st.sessions], currentId: id }))
    // 新对话的 token 用量与上下文占用必须归零（否则沿用上个会话的累计量）
    syncUsage(id)
    // 归属同步落服务端（sessions.project_id / sessions.workspace_id）。
    // 本地 override 仍保留：用于承接前端独有的 workspaceId 分组。
    if (projectId || s.workspaceId) setScopeOverride(id, { projectId, workspaceId: s.workspaceId })
    // 先乐观插入（同步返回 id 供调用方 select），再以服务端返回的会话替换本地壳。
    // **只传 projectId，不传 accessMode**：默认控制权限由服务端按项目设置快照继承。
    void api
      .createSession({
        title: s.title,
        modelRef: s.modelRef,
        projectDir: s.projectDir,
        projectId,
        // 🚨 workspaceId 必须上送（2026-11 修复）：服务端缺失时兜底为 ws-server（恒在线），
        // 于是「归属离线设备」的会话会被记成「绑服务端空间的纯对话」，R2 离线拦截
        // （resolveToolPolicy → policyReasonOffline）结构上永不可能触发。
        // 只写本地 override 是不够的：归属是服务端权威字段，必须落库。
        workspaceId: s.workspaceId,
      })
      .then((created) => {
        set((st) => ({
          sessions: st.sessions.map((x) =>
            // 服务端已回传 workspace_id（R1 起落库）；回包缺字段时才用本地值兜底。
            x.id === id
              ? { ...created, projectId: created.projectId ?? x.projectId, workspaceId: created.workspaceId ?? x.workspaceId }
              : x,
          ),
          currentId: st.currentId === id ? created.id : st.currentId,
        }))
        // 本地壳 id → 服务端真实 id：账本与归属记录都要迁徙，否则切回来/刷新后丢失
        if (projectId || s.workspaceId) {
          setScopeOverride(created.id, { projectId, workspaceId: s.workspaceId })
          clearScopeOverride(id)
        }
        void import('./chatStore').then((m) => m.useChat.getState().migrateUsage(id, created.id))
      })
      .catch(() => {
        set((st) => {
          const rest = st.sessions.filter((x) => x.id !== id)
          return {
            sessions: rest,
            currentId: st.currentId === id ? (rest[0]?.id ?? '') : st.currentId,
          }
        })
        useUI.getState().toast('error', '创建会话失败，请检查服务状态')
      })
    return id
  },
  rename: (id, title) => {
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, title } : x)) }))
    void api.renameSession(id, title).catch(() => useUI.getState().toast('error', '重命名失败'))
  },
  refreshTitle: async (id) => {
    if (!id) return
    try {
      const remote = await api.getSession(id)
      const title = (remote.title ?? '').trim()
      if (!title) return
      // 只回填 title，不整段 replace：服务端不认前端的 workspaceId 分组，
      // 整段替换会让会话掉进「纯对话」（历史缺陷）。
      set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, title } : x)) }))
    } catch {
      /* 标题是体验项：拉取失败保持本地值，等下次 load() 对齐 */
    }
  },
  remove: (id) => {
    set((s) => {
      const rest = s.sessions.filter((x) => x.id !== id)
      return {
        sessions: rest,
        currentId: s.currentId === id ? (rest.find((x) => !x.archivedAt)?.id ?? s.currentId) : s.currentId,
      }
    })
    // 删的是当前会话 → token 水位跟着切到新的当前会话
    if (get().currentId !== id) syncUsage(get().currentId)
    void api.deleteSession(id).catch(() => useUI.getState().toast('error', '删除会话失败'))
  },
  archive: (id) => {
    set((s) => ({
      sessions: s.sessions.map((x) => (x.id === id ? { ...x, archivedAt: Date.now() } : x)),
      currentId: s.currentId === id ? (s.sessions.find((x) => x.id !== id && !x.archivedAt)?.id ?? s.currentId) : s.currentId,
    }))
    if (get().currentId !== id) syncUsage(get().currentId)
    // 归档落库：刷新/重启后归档中心仍完整（失败仅提示，本地状态已更新）
    void api.archiveSession(id).catch(() => useUI.getState().toast('error', '归档会话失败'))
  },
  unarchive: (id) => {
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, archivedAt: undefined } : x)) }))
    void api.unarchiveSession(id).catch(() => useUI.getState().toast('error', '取消归档失败'))
  },
  setAccessMode: (id, accessMode) => {
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, accessMode } : x)) }))
    // 访问模式落库：引擎每次审批实时重读，运行中切换立即生效（对齐桌面端语义）
    void api.setSessionAccessMode(id, accessMode).catch(() => useUI.getState().toast('error', '保存访问模式失败'))
  },
  setModel: (id, modelRef) => {
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, modelRef } : x)) }))
    // 会话级模型落库：重载 / 换端后仍生效（失败不影响本地选择）
    void api.setSessionModel(id, modelRef).catch(() => useUI.getState().toast('error', '保存模型选择失败'))
  },

  // 新建项目。
  //
  // 🚨 返回值是 Promise，**必须 await 后再提示成功**（2026-11 实测缺陷）：
  // 旧实现同步返回本地临时 id，而调用方（Sidebar.newProject）拿到就立刻 toast('success')，
  // 落库请求却在后台异步跑 —— 一旦服务端拒绝（如桌面端曾被误判成移动端 403），
  // 用户会同时看到「项目已创建」与「项目创建失败」两条互斥提示，且项目最终不存在。
  // 现在：成功由调用方提示，失败只由本函数提示一次，绝不再出现双 toast。
  createProject: async (name, path) => {
    const id = `p-${Date.now()}`
    const p: Project = {
      id,
      name: name.trim() || '未命名项目',
      path: path.trim(),
      workspaceId: get().activeWorkspaceId,
      // 新建项目默认「每次变更前确认」：最保守的初始值，用户可在项目设置中改为完全访问。
      defaultAccessMode: 'confirm',
      // 0014 起约束与规划模式也落服务端；新建时为空约束 + 标准模式（与服务端列 DEFAULT 一致）。
      constraints: '',
      planMode: 'standard',
    }
    // 乐观插入：列表即时可见（保留原有体验），失败再回滚。
    set((s) => ({ projects: [...s.projects, p] }))
    persistScope(get())
    // 项目是服务端一等实体：创建必须落库，否则后续 PATCH 权限/规则会 404。
    // 以服务端生成的 id 替换本地临时 id，避免本地 id 与服务端不一致导致后续调用全失败。
    try {
      const created = await api.createProject({
        name: p.name,
        path: p.path,
        defaultAccessMode: p.defaultAccessMode,
      })
      set((s) => ({
        projects: s.projects.map((x) => (x.id === id ? { ...created, workspaceId: x.workspaceId } : x)),
        sessions: s.sessions.map((x) => (x.projectId === id ? { ...x, projectId: created.id } : x)),
      }))
      persistScope(get())
      return created.id
    } catch (err) {
      // 回滚乐观插入，并**只**在这里提示失败（调用方不再提示成功）。
      set((s) => ({ projects: s.projects.filter((x) => x.id !== id) }))
      persistScope(get())
      // 透传服务端文案（如「移动端不支持创建项目」）：泛化的「请检查服务状态」
      // 会让用户以为只是网络抖动，从而反复重试一个必然失败的请求。
      const msg = (err as { message?: string })?.message?.trim()
      useUI.getState().toast('error', msg ? `项目创建失败：${msg}` : '项目创建失败，请检查服务状态')
      throw err
    }
  },
  renameProject: (id, name) => {
    set((s) => ({ projects: s.projects.map((p) => (p.id === id ? { ...p, name: name.trim() || p.name } : p)) }))
    persistScope(get())
  },
  // 移除项目：其下会话移回「纯对话」顶层，不删除会话（与桌面端一致）
  removeProject: (id) => {
    set((s) => ({
      projects: s.projects.filter((p) => p.id !== id),
      sessions: s.sessions.map((x) => (x.projectId === id ? { ...x, projectId: undefined } : x)),
    }))
    // 同步清掉这些会话在本地记住的项目归属，否则下次 load 又会按旧 override 挂回去
    for (const s of get().sessions) {
      if (!s.projectId) setScopeOverride(s.id, { projectId: undefined })
    }
    persistScope(get())
  },
  toggleProjectPin: (id) => {
    set((s) => ({ projects: s.projects.map((p) => (p.id === id ? { ...p, pinned: !p.pinned } : p)) }))
    persistScope(get())
  },
  // 项目约束：0014 起落服务端（此前只写 localStorage，换端即丢）。
  // 先写服务端、成功后再更新本地（与 setProjectDefaultAccessMode 同口径）：
  // 这是会被引擎注入 prompt 的配置，界面显示已改而服务端没存属于语义漂移。
  setProjectConstraints: (id, constraints) =>
    patchProject(id, { constraints }, '项目约束保存失败'),
  setProjectPlanMode: (id, planMode) =>
    patchProject(id, { planMode }, '规划与执行策略保存失败'),
  // 从服务端拉项目列表（real 模式的权威源）。
  //
  // 0014 起 constraints / planMode 也由服务端持久化，因此不再需要与本地
  // localStorage 按 id 合并（旧实现那样做是为了保住「服务端不认识」的前端字段）。
  // 现在服务端回包即完整真相：整段替换即可，避免本地残留值反向污染服务端。
  // workspaceId 仍是纯前端概念（项目归属哪台设备的执行面），由本地按 id 保留。
  loadProjects: async () => {
    try {
      const list = await api.listProjects()
      set((st) => {
        const prevById = new Map(st.projects.map((p) => [p.id, p]))
        const merged = list.map((p) => ({
          ...p,
          workspaceId: p.workspaceId ?? prevById.get(p.id)?.workspaceId,
        }))
        return { projects: merged }
      })
      persistScope(get())
    } catch {
      // 失败不打断工作台加载：保留本地缓存项目，权限裁决仍在服务端
      useUI.getState().toast('error', '加载项目列表失败，已使用本地缓存')
    }
  },
  // 拉取单个项目的「设置子集合」（关联项目 + 数据权限）。
  //
  // 为什么不放进 loadProjects：这两个集合是**项目级**数据，服务端没有
  // 「一次返回全部项目设置」的端点（沿用审批规则的既有口径），
  // 只能在打开该项目设置时按项目拉取。这样也避免首屏为 N 个项目发 2N 个请求。
  loadProjectSettings: async (projectId) => {
    try {
      const [links, grants] = await Promise.all([
        api.listProjectLinks(projectId),
        api.listProjectDataGrants(projectId),
      ])
      set((s) => ({
        // 整体替换该项目的两份子集合，避免残留已删除项
        projectLinks: [...s.projectLinks.filter((l) => l.projectId !== projectId), ...links],
        dataGrants: [...s.dataGrants.filter((g) => g.projectId !== projectId), ...grants],
      }))
    } catch {
      useUI.getState().toast('error', '加载项目设置失败')
    }
  },
  // 切换项目默认控制权限。
  // 🚨 语义选择：**先落服务端，成功后再提交本地 state**（非乐观更新）。
  // 原因：这是权限继承源，前后端不一致会导致「界面显示 full、实际新会话仍是 confirm」，
  // 属于安全语义漂移。加上原先的乐观更新 + catch 回滚会同时弹出「已设置为…」和
  // 「保存失败」两条互相矛盾的提示（成功但 state 未刷新的观感即源于此）。
  setProjectDefaultAccessMode: (id, mode) => {
    const apply = () => {
      set((s) => ({ projects: s.projects.map((p) => (p.id === id ? { ...p, defaultAccessMode: mode } : p)) }))
      persistScope(get())
    }
    return api
      .updateProject(id, { defaultAccessMode: mode })
      .then(apply)
      .catch((err) => {
        useUI.getState().toast('error', '默认控制权限保存失败')
        throw err
      })
  },

  // ---- 关联项目（0014 起落服务端；此前只在内存 + localStorage，换端即丢） ----
  //
  // 三个动作都改为「先写服务端，成功后更新本地」：
  // 关联项目是项目设置的一部分，界面显示已加而服务端没有，属于典型假成功。
  addProjectLink: (projectId, path, description) => {
    void api
      .addProjectLink(projectId, { path: path.trim(), description })
      .then((saved) => set((s) => ({ projectLinks: [...s.projectLinks, saved] })))
      .catch(() => useUI.getState().toast('error', '关联项目添加失败'))
  },
  updateProjectLink: (id, path, description) => {
    const victim = get().projectLinks.find((l) => l.id === id)
    if (!victim) return
    void api
      .updateProjectLink(victim.projectId, id, { path: path.trim(), description })
      .then(() =>
        set((s) => ({
          projectLinks: s.projectLinks.map((l) => (l.id === id ? { ...l, path: path.trim(), description } : l)),
        })),
      )
      .catch(() => useUI.getState().toast('error', '关联项目更新失败'))
  },
  deleteProjectLink: (id) => {
    const victim = get().projectLinks.find((l) => l.id === id)
    if (!victim) return
    void api
      .removeProjectLink(victim.projectId, id)
      .then(() => set((s) => ({ projectLinks: s.projectLinks.filter((l) => l.id !== id) })))
      .catch(() => useUI.getState().toast('error', '关联项目删除失败'))
  },

  // ---- 审批规则（项目级；服务端为权威源） ----
  addApprovalRule: (projectId, toolName, pattern) => {
    const id = `ar-${Date.now()}`
    const rule: ApprovalRule = { id, projectId, toolName, pattern, createdAt: Date.now() }
    set((s) => ({ approvalRules: [...s.approvalRules, rule] }))
    // 乐观插入后以服务端回包纠偏（幂等：重复添加服务端返回既有规则）。
    void api
      .addProjectRule(projectId, { toolName, pattern })
      .then((saved) =>
        set((s) => ({
          approvalRules: s.approvalRules.map((r) => (r.id === id ? saved : r)),
        })),
      )
      .catch(() => {
        // 失败即回滚乐观项，避免前端显示「规则已生效」而引擎实际不认
        set((s) => ({ approvalRules: s.approvalRules.filter((r) => r.id !== id) }))
        useUI.getState().toast('error', '审批规则保存失败')
      })
  },
  removeApprovalRule: (id) => {
    const prev = get().approvalRules
    const victim = prev.find((r) => r.id === id)
    set((s) => ({ approvalRules: s.approvalRules.filter((r) => r.id !== id) }))
    if (victim) {
      void api.removeProjectRule(victim.projectId, id).catch(() => {
        // 回滚：删除失败时恢复原条目，避免前端与引擎规则集不一致
        set(() => ({ approvalRules: prev }))
        useUI.getState().toast('error', '审批规则删除失败')
      })
    }
  },

  // ---- 数据权限策略（0014 起落服务端；此前仅内存，刷新即丢） ----
  //
  // 先写服务端、成功后更新本地。这里的语义漂移风险最高：
  // 数据权限决定「越界数据是弹审批还是自动放行」，界面显示 allow 而服务端仍是 ask
  // 会让用户以为已授权、实际每次都被拦（或反之更危险）。故不做乐观更新。
  setDataScope: (projectId, scope, mode) => {
    void api
      .setProjectDataGrant(projectId, scope, mode)
      .then(() =>
        set((s) => {
          const hit = s.dataGrants.find((g) => g.projectId === projectId && g.scope === scope)
          if (hit) {
            return {
              dataGrants: s.dataGrants.map((g) =>
                g.id === hit.id ? { ...g, mode, updatedAt: Date.now() } : g,
              ),
            }
          }
          const next: DataScopeGrant = {
            id: `dg-${Date.now()}-${scope}`,
            projectId,
            scope,
            mode,
            updatedAt: Date.now(),
          }
          return { dataGrants: [...s.dataGrants, next] }
        }),
      )
      .catch(() => useUI.getState().toast('error', '数据权限保存失败'))
  },
  dataScopeOf: (projectId, scope) =>
    get().dataGrants.find((g) => g.projectId === projectId && g.scope === scope)?.mode ?? 'ask',
}))
