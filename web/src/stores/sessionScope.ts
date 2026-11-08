/**
 * 会话归属（工作空间 / 项目）恢复：以**已落库的 workspace_path** 为唯一真源反查项目。
 *
 * 背景（踩坑：会话刷新后掉进「纯对话」）：
 *   - 服务端 sessions 表只有 `workspace_path`，**没有** project_id / workspace_id
 *     （项目在前端目前是纯客户端概念，后端没有 projects 表与接口）。
 *   - `lib/api.ts` 的 toUiSession() 只能映射出 workspacePath，projectId / workspaceId 恒为 undefined。
 *   - 而 Sidebar 的分组判据是 `wsOfSession(s, projects)` 与 `!s.projectId`：
 *     两个字段都是 undefined 时，会话必然落到内置空间 + 「纯对话」分组。
 *   → 结果：刷新前明明在项目/工作区里的对话，刷新后一律显示为纯对话。
 *
 * 结论：归属恢复必须靠 `workspace_path` 反查项目目录（零迁移、零 API 契约变更）。
 * 反查不到时**不伪造归属**：保持 projectId 为空，但 workspacePath 仍在（顶栏照实显示真实目录）。
 */
import type { Project, Session } from '../types'

/**
 * 路径规范化：统一分隔符、去掉尾部斜杠、合并重复斜杠、去空白。
 * Windows 下用户可能写成 `D:\a\b\`，服务端落库的可能是 `D:/a/b`，必须能对上。
 */
export function normalizeFsPath(path: string | undefined | null): string {
  let p = (path ?? '').trim()
  if (!p) return ''
  p = p.replace(/\\/g, '/')
  // 合并重复斜杠（保留 UNC 前缀 //）
  p = p.replace(/([^:])\/{2,}/g, '$1/')
  // 去掉尾部斜杠（根路径 "/" 除外）
  if (p.length > 1) p = p.replace(/\/+$/, '')
  return p
}

/** 在项目列表中按目录路径反查项目；同一路径命中多个时取第一个（项目路径本应唯一）。 */
export function resolveProjectByPath(path: string | undefined | null, projects: Project[]): Project | undefined {
  const target = normalizeFsPath(path)
  if (!target) return undefined
  // 先做精确匹配（区分大小写的文件系统语义）
  const exact = projects.find((p) => normalizeFsPath(p.path) === target)
  if (exact) return exact
  // 再退化到大小写不敏感匹配（Windows 习惯，且避免用户手输路径大小写不一致导致归属丢失）
  const lower = target.toLowerCase()
  return projects.find((p) => normalizeFsPath(p.path).toLowerCase() === lower)
}

/**
 * 把服务端会话列表补全为带归属的 UI 会话：
 *   ① 已有 projectId 的（本地乐观插入、尚未被服务端返回覆盖的壳）原样保留；
 *   ② 否则用 workspacePath 反查项目，命中则补 projectId + workspaceId；
 *   ③ 仍未命中 → projectId 留空（诚实归为纯对话），但保留 workspacePath。
 */
export function adoptServerSessions(list: Session[], projects: Project[]): Session[] {
  const overrides = loadScopeOverrides()
  return list.map((s) => {
    if (s.projectId) {
      const proj = projects.find((p) => p.id === s.projectId)
      return proj && !s.workspaceId ? { ...s, workspaceId: proj.workspaceId } : s
    }
    // ② 上次解析出的归属（用户手工把会话放进某项目/空间时记下的，优先级高于路径反查）
    const ov = overrides[s.id]
    if (ov?.projectId && projects.some((p) => p.id === ov.projectId)) {
      const proj = projects.find((p) => p.id === ov.projectId)!
      return { ...s, projectId: proj.id, workspaceId: ov.workspaceId ?? s.workspaceId ?? proj.workspaceId }
    }
    // ③ 路径反查项目目录
    const proj = resolveProjectByPath(s.workspacePath, projects)
    if (!proj) {
      return ov?.workspaceId ? { ...s, workspaceId: ov.workspaceId } : s
    }
    return {
      ...s,
      projectId: proj.id,
      workspaceId: s.workspaceId ?? proj.workspaceId,
      // 服务端没带路径但项目有（如项目路径后补）：以项目路径为准，保证顶栏与归属一致
      workspacePath: s.workspacePath ?? proj.path,
    }
  })
}

/* ==================== 本地持久化（real 模式的项目/空间/归属） ==================== */

/**
 * 为什么需要它：real 模式的项目（projects）与工作空间（workspaces）目前都是**纯前端概念**
 * （后端无 projects 表与接口，createProject/connectWorkspace 只改内存），刷新即回到
 * mock 初始列表 —— 用户自建的项目连同其下会话的归属一起蒸发，会话就"掉进纯对话"。
 * 在项目真正落库之前，至少把这几张表持久化到本地，保证刷新前后一致。
 */
const LS_PROJECTS = 'harness.projects'
const LS_WORKSPACES = 'harness.workspaces'
const LS_ACTIVE_WS = 'harness.activeWorkspaceId'
const LS_SCOPE = 'harness.sessionScope'

interface ScopeOverride {
  projectId?: string
  workspaceId?: string
}

function readJSON<T>(key: string): T | null {
  try {
    const raw = localStorage.getItem(key)
    if (!raw) return null
    return JSON.parse(raw) as T
  } catch {
    return null // 隐私模式 / 脏数据：降级为无持久化，绝不阻断渲染
  }
}

function writeJSON(key: string, value: unknown) {
  try {
    localStorage.setItem(key, JSON.stringify(value))
  } catch {
    /* 存储不可用时仅内存有效 */
  }
}

export function loadPersistedProjects(): Project[] | null {
  const v = readJSON<Project[]>(LS_PROJECTS)
  return Array.isArray(v) ? v : null
}

export function savePersistedProjects(projects: Project[]) {
  writeJSON(LS_PROJECTS, projects)
}

export function loadPersistedWorkspaces(): unknown[] | null {
  const v = readJSON<unknown[]>(LS_WORKSPACES)
  return Array.isArray(v) ? v : null
}

export function savePersistedWorkspaces(workspaces: unknown[]) {
  writeJSON(LS_WORKSPACES, workspaces)
}

export function loadPersistedActiveWorkspace(): string | null {
  try {
    return localStorage.getItem(LS_ACTIVE_WS)
  } catch {
    return null
  }
}

export function savePersistedActiveWorkspace(id: string) {
  try {
    localStorage.setItem(LS_ACTIVE_WS, id)
  } catch {
    /* 同上 */
  }
}

export function loadScopeOverrides(): Record<string, ScopeOverride> {
  return readJSON<Record<string, ScopeOverride>>(LS_SCOPE) ?? {}
}

/** 记录某会话的归属（服务端无此字段，只能在本地记住；路径反查失败时是唯一依据）。 */
export function setScopeOverride(sessionId: string, ov: ScopeOverride) {
  if (!sessionId) return
  const all = loadScopeOverrides()
  all[sessionId] = { ...all[sessionId], ...ov }
  writeJSON(LS_SCOPE, all)
}

export function clearScopeOverride(sessionId: string) {
  const all = loadScopeOverrides()
  if (!(sessionId in all)) return
  delete all[sessionId]
  writeJSON(LS_SCOPE, all)
}

/** 项目 / 空间被移除时清理其残留归属记录，避免悬空 id 反复误导渲染。 */
export function pruneScopeOverrides(aliveProjectIds: string[], aliveWorkspaceIds: string[]) {
  const all = loadScopeOverrides()
  const aliveP = new Set(aliveProjectIds)
  const aliveW = new Set(aliveWorkspaceIds)
  let dirty = false
  for (const [sid, ov] of Object.entries(all)) {
    if ((ov.projectId && !aliveP.has(ov.projectId)) || (ov.workspaceId && !aliveW.has(ov.workspaceId))) {
      delete all[sid]
      dirty = true
    }
  }
  if (dirty) writeJSON(LS_SCOPE, all)
}
