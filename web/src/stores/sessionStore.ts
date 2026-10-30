import { create } from 'zustand'
import type { AccessMode, ApprovalRule, DataScope, DataScopeGrant, DataScopeMode, PlanMode, Project, ProjectLink, Session, Workspace, WorkspaceKind } from '../types'
import {
  approvalRules as mockApprovalRules, dataGrants as mockDataGrants, projects as mockProjects,
  projectLinks as mockProjectLinks, sessions as mockSessions, workspaces as mockWorkspaces,
  DEFAULT_WORKSPACE_ID,
} from '../mock/data'
import { api, apiMode } from '../lib/api'
import { useUI } from './uiStore'

/** 项目所属工作空间（缺省视为内置服务端空间） */
export const wsOfProject = (p: Project) => p.workspaceId ?? DEFAULT_WORKSPACE_ID

/** 会话所属工作空间：优先自身 workspaceId，其次按项目推导，纯对话缺省内置空间 */
export const wsOfSession = (s: Session, projects: Project[]) =>
  s.workspaceId
  ?? (s.projectId ? projects.find((p) => p.id === s.projectId)?.workspaceId : undefined)
  ?? DEFAULT_WORKSPACE_ID

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

export const useSessions = create<SessionState>((set, get) => ({
  workspaces: mockWorkspaces,
  activeWorkspaceId: DEFAULT_WORKSPACE_ID,
  // real 模式会话列表来自主库（load() 拉取）；mock 模式沿用 demo 样例
  sessions: apiMode === 'mock' ? mockSessions : [],
  projects: mockProjects,
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
      set({ sessions: list, currentId: current })
      // 动态 import 规避 sessionStore ↔ chatStore 的静态循环依赖
      const { useChat } = await import('./chatStore')
      void useChat.getState().reloadMessages(current)
    } catch {
      useUI.getState().toast('error', '加载会话列表失败，请检查服务状态')
    }
  },

  select: (id) => {
    set({ currentId: id })
    if (apiMode === 'real' && id) {
      // 切换会话即拉取该会话消息快照（以 DB 为准）
      void import('./chatStore').then((m) => m.useChat.getState().reloadMessages(id))
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
      return
    }
    set({ activeWorkspaceId: id, currentId: inWs[0]?.id ?? '' })
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
    if (needCreate) get().create()
    return true
  },

  create: (projectId) => {
    const id = `s-${Date.now()}`
    const proj = projectId ? get().projects.find((p) => p.id === projectId) : undefined
    const s: Session = {
      id,
      title: '新对话',
      projectId,
      workspaceId: proj ? wsOfProject(proj) : get().activeWorkspaceId,
      workspacePath: proj?.path,
      accessMode: 'confirm',
      modelRef: 'deepseek/deepseek-chat',
      updatedAt: Date.now(),
    }
    set((st) => ({ sessions: [s, ...st.sessions], currentId: id }))
    if (apiMode === 'real') {
      // 先乐观插入（同步返回 id 供调用方 select），再以服务端返回的会话替换本地壳
      void api
        .createSession({ title: s.title })
        .then((created) => {
          set((st) => ({
            sessions: st.sessions.map((x) => (x.id === id ? created : x)),
            currentId: st.currentId === id ? created.id : st.currentId,
          }))
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
    if (apiMode === 'real') {
      void api.deleteSession(id).catch(() => useUI.getState().toast('error', '删除会话失败'))
    }
  },
  archive: (id) =>
    set((s) => ({
      sessions: s.sessions.map((x) => (x.id === id ? { ...x, archivedAt: Date.now() } : x)),
      currentId: s.currentId === id ? (s.sessions.find((x) => x.id !== id && !x.archivedAt)?.id ?? s.currentId) : s.currentId,
    })),
  unarchive: (id) =>
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, archivedAt: undefined } : x)) })),
  setAccessMode: (id, accessMode) =>
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, accessMode } : x)) })),
  setModel: (id, modelRef) =>
    set((s) => ({ sessions: s.sessions.map((x) => (x.id === id ? { ...x, modelRef } : x)) })),

  createProject: (name, path) => {
    const id = `p-${Date.now()}`
    const p: Project = {
      id,
      name: name.trim() || '未命名项目',
      path: path.trim(),
      workspaceId: get().activeWorkspaceId,
    }
    set((s) => ({ projects: [...s.projects, p] }))
    return id
  },
  renameProject: (id, name) =>
    set((s) => ({ projects: s.projects.map((p) => (p.id === id ? { ...p, name: name.trim() || p.name } : p)) })),
  // 移除项目：其下会话移回「纯对话」顶层，不删除会话（与桌面端一致）
  removeProject: (id) =>
    set((s) => ({
      projects: s.projects.filter((p) => p.id !== id),
      sessions: s.sessions.map((x) => (x.projectId === id ? { ...x, projectId: undefined } : x)),
    })),
  toggleProjectPin: (id) =>
    set((s) => ({ projects: s.projects.map((p) => (p.id === id ? { ...p, pinned: !p.pinned } : p)) })),
  setProjectConstraints: (id, constraints) =>
    set((s) => ({ projects: s.projects.map((p) => (p.id === id ? { ...p, constraints } : p)) })),
  setProjectPlanMode: (id, planMode) =>
    set((s) => ({ projects: s.projects.map((p) => (p.id === id ? { ...p, planMode } : p)) })),

  // ---- 关联项目（草稿由项目设置外壳统一提交） ----
  addProjectLink: (projectId, path, description) => {
    const id = `pl-${Date.now()}`
    const link: ProjectLink = { id, projectId, path: path.trim(), description, createdAt: '' }
    set((s) => ({ projectLinks: [...s.projectLinks, link] }))
  },
  updateProjectLink: (id, path, description) =>
    set((s) => ({ projectLinks: s.projectLinks.map((l) => (l.id === id ? { ...l, path: path.trim(), description } : l)) })),
  deleteProjectLink: (id) => set((s) => ({ projectLinks: s.projectLinks.filter((l) => l.id !== id) })),

  // ---- 审批规则（项目级） ----
  addApprovalRule: (projectId, toolName, pattern) => {
    const id = `ar-${Date.now()}`
    const rule: ApprovalRule = { id, projectId, toolName, pattern, createdAt: Date.now() }
    set((s) => ({ approvalRules: [...s.approvalRules, rule] }))
  },
  removeApprovalRule: (id) => set((s) => ({ approvalRules: s.approvalRules.filter((r) => r.id !== id) })),

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
