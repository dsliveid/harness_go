import { create } from 'zustand'
import type { AccessMode, ApprovalRule, DataScope, DataScopeGrant, DataScopeMode, PlanMode, Project, ProjectLink, Session, Workspace, WorkspaceKind } from '../types'
import {
  approvalRules as mockApprovalRules, dataGrants as mockDataGrants, projects as mockProjects,
  projectLinks as mockProjectLinks, sessions as mockSessions, workspaces as mockWorkspaces,
  DEFAULT_WORKSPACE_ID,
} from '../mock/data'
import { api, apiMode } from '../lib/api'
import { useUI } from './uiStore'
import { useSettings } from './settingsStore'
import {
  adoptServerSessions, clearScopeOverride, loadPersistedActiveWorkspace, loadPersistedProjects,
  loadPersistedWorkspaces, pruneScopeOverrides, resolveProjectByPath, savePersistedActiveWorkspace,
  savePersistedProjects, savePersistedWorkspaces, setScopeOverride,
} from './sessionScope'

/** 项目所属工作空间（缺省视为内置服务端空间） */
export const wsOfProject = (p: Project) => p.workspaceId ?? DEFAULT_WORKSPACE_ID

/**
 * 新会话的初始模型（不存在「默认模型」这个模糊选项，落到具体模型或空）：
 *  ① 同一作用域内最近的会话：项目对话看同项目最新一条，纯对话看上一条纯对话；
 *  ② 设置 → 能力模型槽位 chat（全局默认对话模型，需仍在启用厂商列表内）；
 *  ③ 兜底：mock 用演示模型；real 为空串 = 未选择（发送时会提示先选模型）。
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
  return apiMode === 'mock' ? 'deepseek/deepseek-chat' : ''
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
 * 把「项目 / 工作空间 / 当前空间」写入本地持久化（real 模式；mock 模式是演示数据不必落盘）。
 * 三者都是纯前端实体，不在服务端落库，只能靠本地存储保证刷新前后一致。
 */
function persistScope(st: { projects: Project[]; workspaces: Workspace[]; activeWorkspaceId: string }) {
  if (apiMode !== 'real') return
  savePersistedProjects(st.projects)
  savePersistedWorkspaces(st.workspaces)
  savePersistedActiveWorkspace(st.activeWorkspaceId)
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
  remove: (id: string) => void
  archive: (id: string) => void
  unarchive: (id: string) => void
  setAccessMode: (id: string, mode: AccessMode) => void
  setModel: (id: string, modelRef: string) => void
  // ---- 工作空间（服务端内置空间 + 客户端接入空间） ----
  setWorkspace: (id: string) => void
  connectWorkspace: (name: string, kind?: WorkspaceKind) => string
  removeWorkspace: (id: string) => boolean
  // ---- 项目操作（对齐桌面端） ----
  createProject: (name: string, path: string) => string
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
}

// 项目：real 模式下**服务端 projects 表是权威源**（由 loadProjects() 拉取）；
// 本地持久化只用于首屏秒开与水合前占位，且**只保留前端独有字段**（workspaceId/constraints/planMode）。
//
// 🚨 踩坑记录（2026-11）：这里曾用 `?? mockProjects` 兜底，导致 real 模式首屏注入 p1/p2/p3
// 这三条演示项目。它们服务端根本不存在，用户对其切换权限 → PATCH 404 → 前端误报「保存失败」
// 且刷新后回退旧值（实为服务端从未存过）。real 模式必须从空列表起步，由服务端填充。
const initialProjects = apiMode === 'mock' ? mockProjects : (loadPersistedProjects() ?? [])
const initialWorkspaces = apiMode === 'mock'
  ? mockWorkspaces
  : ((loadPersistedWorkspaces() as Workspace[] | null) ?? mockWorkspaces)
const initialActiveWs = apiMode === 'mock' ? DEFAULT_WORKSPACE_ID : (loadPersistedActiveWorkspace() ?? DEFAULT_WORKSPACE_ID)

export const useSessions = create<SessionState>((set, get) => ({
  workspaces: initialWorkspaces,
  activeWorkspaceId: initialActiveWs,
  // real 模式会话列表来自主库（load() 拉取）；mock 模式沿用 demo 样例
  sessions: apiMode === 'mock' ? mockSessions : [],
  projects: initialProjects,
  projectLinks: mockProjectLinks,
  approvalRules: mockApprovalRules,
  dataGrants: mockDataGrants,
  currentId: apiMode === 'mock' ? 's1' : '',

  // 登录后从主库拉取会话列表：空列表自动建一条，避免右侧空屏
  load: async () => {
    if (apiMode === 'mock') return
    try {
      const list = await api.listSessions({ limit: 100 })
      if (list.length === 0) {
        get().create()
        return
      }
      const keep = get().currentId
      const current = list.some((s) => s.id === keep) ? keep : list[0].id
      // 归属恢复（踩坑：刷新后项目会话掉进「纯对话」）：
      // 服务端 sessions 只有 workspace_path，没有 project_id / workspace_id，
      // toUiSession 映射不出归属 → 必须按已落库的目录反查项目，把 projectId/workspaceId 补回来。
      const withScope = adoptServerSessions(list, get().projects)
      // 当前空间若在新列表里已无任何可见会话（如它只是上次的临时空间），
      // 回落到当前会话自身所属空间，避免刷新后停在一个空空间、看不到刚聊过的对话。
      const curSess = withScope.find((s) => s.id === current)
      const activeWs = get().activeWorkspaceId
      const hasInActive = withScope.some((s) => !s.archivedAt && wsOfSession(s, get().projects) === activeWs)
      const nextActiveWs = hasInActive || !curSess ? activeWs : wsOfSession(curSess, get().projects)
      set({ sessions: withScope, currentId: current, activeWorkspaceId: nextActiveWs })
      // 动态 import 规避 sessionStore ↔ chatStore 的静态循环依赖
      const { useChat } = await import('./chatStore')
      void useChat.getState().reloadMessages(current)
      // 刷新后接回服务端仍在跑的 Run（否则界面看不出对话还在进行中）
      void useChat.getState().resumeRun(current)
      // token 水位与活跃会话对齐（否则沿用上一个会话的用量）
      useChat.getState().activateSession(current)
    } catch {
      useUI.getState().toast('error', '加载会话列表失败，请检查服务状态')
    }
  },

  select: (id) => {
    if (id !== get().currentId) syncUsage(id)
    set({ currentId: id })
    if (apiMode === 'real' && id) {
      // 切换会话即拉取该会话消息快照（以 DB 为准）
      void import('./chatStore').then((m) => {
        // 先清掉上一会话残留的进行中态，再按目标会话的真实情况重建
        m.useChat.getState().reloadMessages(id)
          .then(() => m.useChat.getState().resumeRun(id))
      })
    }
  },

  // ---- 工作空间 ----
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
  connectWorkspace: (name, kind = 'desktop') => {
    const seq = get().workspaces.filter((w) => !w.builtin).length + 1
    const id = `ws-${Date.now()}`
    const ws: Workspace = {
      id,
      name: name.trim() || `新接入设备 ${seq}`,
      kind,
      online: true,
      limbCount: 1,
      lastSeenAt: Date.now(),
      protocol: 'v1',
      clientId: `${kind}-${String(seq).padStart(2, '0')}`,
    }
    set((st) => ({ workspaces: [...st.workspaces, ws] }))
    persistScope(get())
    get().setWorkspace(id)
    return id
  },
  removeWorkspace: (id) => {
    const st = get()
    const ws = st.workspaces.find((w) => w.id === id)
    // 内置服务端空间不可移除
    if (!ws || ws.builtin) return false
    const goneProjectIds = st.projects.filter((p) => wsOfProject(p) === id).map((p) => p.id)
    const keptProjects = st.projects.filter((p) => wsOfProject(p) !== id)
    const keptSessions = st.sessions.filter((s) => wsOfSession(s, st.projects) !== id)
    const fallbackWs = st.activeWorkspaceId === id ? DEFAULT_WORKSPACE_ID : st.activeWorkspaceId
    const fallbackSessions = keptSessions.filter(
      (s) => !s.archivedAt && wsOfSession(s, keptProjects) === fallbackWs,
    )
    const needCreate = st.activeWorkspaceId === id && fallbackSessions.length === 0
    set({
      workspaces: st.workspaces.filter((w) => w.id !== id),
      projects: keptProjects,
      sessions: keptSessions,
      projectLinks: st.projectLinks.filter((l) => !goneProjectIds.includes(l.projectId)),
      approvalRules: st.approvalRules.filter((r) => !goneProjectIds.includes(r.projectId)),
      dataGrants: st.dataGrants.filter((g) => !goneProjectIds.includes(g.projectId)),
      activeWorkspaceId: fallbackWs,
      currentId: fallbackSessions[0]?.id ?? st.currentId,
    })
    // 空间移除后清理悬空的归属记录（否则残留 projectId/workspaceId 会反复误导分组）
    pruneScopeOverrides(keptProjects.map((p) => p.id), st.workspaces.filter((w) => w.id !== id).map((w) => w.id))
    persistScope(get())
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
      workspacePath: proj?.path,
      accessMode: proj?.defaultAccessMode ?? 'confirm',
      // 新会话模型：沿用同作用域最近会话的选择，其次全局 chat 槽位；为空 = 未选择
      modelRef: inheritModelRef(get().sessions, projectId),
      updatedAt: Date.now(),
    }
    set((st) => ({ sessions: [s, ...st.sessions], currentId: id }))
    // 新对话的 token 用量与上下文占用必须归零（否则沿用上个会话的累计量）
    syncUsage(id)
    // 归属同步落服务端（sessions.project_id，阶段七起服务端已有一等项目实体），
    // 本地 override 仍保留：用于承接前端独有的 workspaceId 分组。
    if (projectId || s.workspaceId) setScopeOverride(id, { projectId, workspaceId: s.workspaceId })
    if (apiMode === 'real') {
      // 先乐观插入（同步返回 id 供调用方 select），再以服务端返回的会话替换本地壳。
      // **只传 projectId，不传 accessMode**：默认控制权限由服务端按项目设置快照继承。
      void api
        .createSession({
          title: s.title,
          modelRef: s.modelRef,
          workspacePath: s.workspacePath,
          projectId,
        })
        .then((created) => {
          set((st) => ({
            sessions: st.sessions.map((x) =>
              // 服务端已回传 projectId；workspaceId 仍是前端概念，保留本地值兜底。
              x.id === id ? { ...created, projectId: created.projectId ?? x.projectId, workspaceId: x.workspaceId } : x,
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
    }
    return id
  },
  rename: (id, title) => {
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, title } : x)) }))
    if (apiMode === 'real') {
      void api.renameSession(id, title).catch(() => useUI.getState().toast('error', '重命名失败'))
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
    if (apiMode === 'real') {
      void api.deleteSession(id).catch(() => useUI.getState().toast('error', '删除会话失败'))
    }
  },
  archive: (id) => {
    set((s) => ({
      sessions: s.sessions.map((x) => (x.id === id ? { ...x, archivedAt: Date.now() } : x)),
      currentId: s.currentId === id ? (s.sessions.find((x) => x.id !== id && !x.archivedAt)?.id ?? s.currentId) : s.currentId,
    }))
    if (get().currentId !== id) syncUsage(get().currentId)
    if (apiMode === 'real') {
      // 归档落库：刷新/重启后归档中心仍完整（失败仅提示，本地状态已更新）
      void api.archiveSession(id).catch(() => useUI.getState().toast('error', '归档会话失败'))
    }
  },
  unarchive: (id) => {
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, archivedAt: undefined } : x)) }))
    if (apiMode === 'real') {
      void api.unarchiveSession(id).catch(() => useUI.getState().toast('error', '取消归档失败'))
    }
  },
  setAccessMode: (id, accessMode) => {
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, accessMode } : x)) }))
    if (apiMode === 'real') {
      // 访问模式落库：引擎每次审批实时重读，运行中切换立即生效（对齐桌面端语义）
      void api.setSessionAccessMode(id, accessMode).catch(() => useUI.getState().toast('error', '保存访问模式失败'))
    }
  },
  setModel: (id, modelRef) => {
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, modelRef } : x)) }))
    if (apiMode === 'real') {
      // 会话级模型落库：重载 / 换端后仍生效（失败不影响本地选择）
      void api.setSessionModel(id, modelRef).catch(() => useUI.getState().toast('error', '保存模型选择失败'))
    }
  },

  createProject: (name, path) => {
    const id = `p-${Date.now()}`
    const p: Project = {
      id,
      name: name.trim() || '未命名项目',
      path: path.trim(),
      workspaceId: get().activeWorkspaceId,
      // 新建项目默认「每次变更前确认」：最保守的初始值，用户可在项目设置中改为完全访问。
      defaultAccessMode: 'confirm',
    }
    set((s) => ({ projects: [...s.projects, p] }))
    persistScope(get())
    if (apiMode === 'real') {
      // 项目是服务端一等实体：创建必须落库，否则后续 PATCH 权限/规则会 404。
      // 以服务端生成的 id 替换本地临时 id，避免本地 id 与服务端不一致导致后续调用全失败。
      void api
        .createProject({ name: p.name, path: p.path, defaultAccessMode: p.defaultAccessMode })
        .then((created) =>
          set((s) => ({
            projects: s.projects.map((x) => (x.id === id ? { ...created, workspaceId: x.workspaceId } : x)),
            sessions: s.sessions.map((x) => (x.projectId === id ? { ...x, projectId: created.id } : x)),
          })),
        )
        .then(() => persistScope(get()))
        .catch(() => {
          set((s) => ({ projects: s.projects.filter((x) => x.id !== id) }))
          useUI.getState().toast('error', '项目创建失败，请检查服务状态')
        })
    }
    return id
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
  setProjectConstraints: (id, constraints) => {
    set((s) => ({ projects: s.projects.map((p) => (p.id === id ? { ...p, constraints } : p)) }))
    persistScope(get())
  },
  setProjectPlanMode: (id, planMode) => {
    set((s) => ({ projects: s.projects.map((p) => (p.id === id ? { ...p, planMode } : p)) }))
    persistScope(get())
  },
  // 从服务端拉项目列表（real 模式的权威源）。
  // 服务端不认识前端的 workspaceId/constraints/planMode，必须与本地既有值按 id 合并保留，
  // 否则一次刷新就会把用户的空间分组与项目约束全部抹掉。
  loadProjects: async () => {
    if (apiMode === 'mock') return
    try {
      const list = await api.listProjects()
      set((st) => {
        const prevById = new Map(st.projects.map((p) => [p.id, p]))
        const merged = list.map((p) => {
          const prev = prevById.get(p.id)
          return {
            ...p,
            workspaceId: p.workspaceId ?? prev?.workspaceId,
            constraints: prev?.constraints,
            planMode: prev?.planMode,
          }
        })
        return { projects: merged }
      })
      persistScope(get())
    } catch {
      // 失败不打断工作台加载：保留本地缓存项目，权限裁决仍在服务端
      useUI.getState().toast('error', '加载项目列表失败，已使用本地缓存')
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
    if (apiMode !== 'real') {
      apply()
      return Promise.resolve()
    }
    return api
      .updateProject(id, { defaultAccessMode: mode })
      .then(apply)
      .catch((err) => {
        useUI.getState().toast('error', '默认控制权限保存失败')
        throw err
      })
  },

  // ---- 关联项目（草稿由项目设置外壳统一提交） ----
  addProjectLink: (projectId, path, description) => {
    const id = `pl-${Date.now()}`
    const link: ProjectLink = { id, projectId, path: path.trim(), description, createdAt: '' }
    set((s) => ({ projectLinks: [...s.projectLinks, link] }))
  },
  updateProjectLink: (id, path, description) =>
    set((s) => ({ projectLinks: s.projectLinks.map((l) => (l.id === id ? { ...l, path: path.trim(), description } : l)) })),
  deleteProjectLink: (id) => set((s) => ({ projectLinks: s.projectLinks.filter((l) => l.id !== id) })),

  // ---- 审批规则（项目级；real 模式以服务端为权威源） ----
  addApprovalRule: (projectId, toolName, pattern) => {
    const id = `ar-${Date.now()}`
    const rule: ApprovalRule = { id, projectId, toolName, pattern, createdAt: Date.now() }
    set((s) => ({ approvalRules: [...s.approvalRules, rule] }))
    if (apiMode === 'real') {
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
    }
  },
  removeApprovalRule: (id) => {
    const prev = get().approvalRules
    const victim = prev.find((r) => r.id === id)
    set((s) => ({ approvalRules: s.approvalRules.filter((r) => r.id !== id) }))
    if (apiMode === 'real' && victim) {
      void api.removeProjectRule(victim.projectId, id).catch(() => {
        // 回滚：删除失败时恢复原条目，避免前端与引擎规则集不一致
        set(() => ({ approvalRules: prev }))
        useUI.getState().toast('error', '审批规则删除失败')
      })
    }
  },

  // ---- 数据权限策略（项目级；无记录时按「询问」兜底） ----
  setDataScope: (projectId, scope, mode) =>
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
  dataScopeOf: (projectId, scope) =>
    get().dataGrants.find((g) => g.projectId === projectId && g.scope === scope)?.mode ?? 'ask',
}))
