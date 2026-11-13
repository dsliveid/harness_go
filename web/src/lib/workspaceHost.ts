import { ApiError } from './api'
import { API_PATHS } from './apiTypes'
import type { Workspace } from '../types'

/**
 * 工作区宿主（WorkspaceHost）抽象 —— 目录选择能力的**唯一入口**。
 *
 * 契约见 docs/工作区宿主与目录选择对接约定.md：
 *  - 目录能力不按「窗口形态（浏览器/桌面/移动）」划分，而归属「在启动中的宿主」；
 *  - host = "server"（服务端本地） | "device:<id>"（某个在线客户端）；
 *  - 前端目录选择器只依赖本抽象，因此**浏览器端与客户端走同一条流程**。
 *
 * 当前进度（L0）：仅 server 宿主可用；device 宿主由服务端统一回
 * `fs.host_unsupported`，UI 据此置灰并保留手填退路。
 */

export type HostID = 'server' | `device:${string}`

/** 一个目录项（只含目录名与宿主本地绝对路径，不含任何文件元数据） */
export interface DirEntry {
  name: string
  path: string
}

/** 一次目录列举结果 */
export interface DirListing {
  host: HostID
  /** 当前目录的绝对路径；空串 = 尚未定位（仅回 roots） */
  path: string
  /** 上级目录；null 表示已在白名单根层级（绝不返回 ".."） */
  parent: string | null
  /** 路径分隔符：Windows `\`，POSIX `/`（用于面包屑切分） */
  separator: string
  /** 可选根（仅在 path 为空时给出） */
  roots: DirEntry[]
  /** 子目录（只目录，不含文件） */
  dirs: DirEntry[]
  /** 目录项过多被截断（超大目录保护） */
  truncated?: boolean
}

/** 宿主是否可参与目录选择 */
export interface HostStatus {
  id: HostID
  online: boolean
  canList: boolean
  /** canList=false 时给用户看的原因 */
  reason?: string
}

/* ============ 宿主映射（纯函数，禁止运行时按在线情况自动挑宿主） ============ */

/**
 * 工作空间 → 宿主 ID。
 *
 * 铁律：这是**纯映射**。绝不能写成「在线就挑某台设备」，否则同一空间在不同时刻
 * 会列出不同机器的目录（对接约定 §1.2）。
 * 内置服务端空间（builtin / kind=server）→ 'server'；其余客户端空间 → 'device:<id>'。
 */
export function hostOfWorkspace(ws: Workspace | undefined | null): HostID {
  if (!ws) return 'server'
  if (ws.builtin || ws.kind === 'server') return 'server'
  return `device:${ws.clientId || ws.id}`
}

/**
 * 宿主能力状态（按**工作空间 kind** 精确区分，不按客户端形态）。
 *
 * 2026-11 产品决策：
 *  - `mobile` 工作空间：**不做目录浏览**。App 沙箱根没有浏览价值，且移动端
 *    WebView 里根本拿不到本机文件系统，因此移动端空间一律置灰。
 *  - `desktop` 工作空间：**已支持**。桌面端在 hello/ping 中申报了 `fs.list`，
 *    服务端 `device:<id>` 可真实派发（见 internal/device/pathquery.go）。
 *  - `server` 内置空间：服务端本地列举（L0）。
 *
 * ⚠️ 为什么不在此处探测设备是否真的申报了能力：
 * 前端无法在渲染期同步得知（要额外打请求且结果会随设备重连抖动）。这里按 kind
 * 给出**保守但可用**的判断，真正的能力闸门是服务端的 `fsDispatchDevice`
 * ——未申报能力的设备（例如还没升级的旧桌面端）会被它拦成 `fs.host_unsupported`，
 * 前端再把该错误如实展示即可。
 */
export function hostStatus(ws: Workspace | undefined | null): HostStatus {
  const id = hostOfWorkspace(ws)
  if (id === 'server') {
    return { id, online: true, canList: true }
  }
  const online = !!ws?.online
  // 移动端工作空间：产品决策明确不支持目录浏览。
  if (ws?.kind === 'mobile') {
    return {
      id,
      online,
      canList: false,
      reason: '移动端工作空间支持纯对话，不支持浏览或指定目录',
    }
  }
  // 桌面端工作空间：服务端已支持派发。
  if (ws?.kind === 'desktop') {
    return {
      id,
      online,
      canList: online,
      reason: online ? undefined : '该工作区离线，无法列举其目录',
    }
  }
  // 其余 kind（如 browser）：尚无列举实现，如实置灰。
  return {
    id,
    online,
    canList: false,
    reason: online
      ? '该客户端工作区暂不支持目录浏览，请手动填写路径'
      : '该工作区离线，无法列举其目录',
  }
}

/**
 * 能否在当前工作空间新建项目。
 *
 * 2026-11 产品决策：**移动端工作空间只支持纯对话，不支持添加项目**。
 *
 * 🚨 这是前端 UX 层面的一道闸，不是安全边界。服务端另有两道真强制：
 *  1. `createProject` 按令牌 kind 拒绝 webview（移动端 WebView 会话）；
 *  2. `resolveToolPolicy` 对 `kind=mobile` 的空间返回零工具。
 * 本函数只负责"别让用户点一个注定失败的按钮"。
 */
export function canCreateProjectIn(ws: Workspace | undefined | null): boolean {
  if (!ws) return true
  return ws.kind !== 'mobile'
}

/* ============ 调用 ============ */

const toHost = (h: HostID) => encodeURIComponent(h)

/** hello 分隔符兜底：服务端总会回，这里仅防御异常响应 */
const guessSeparator = (p: string) => (p.includes('\\') ? '\\' : '/')

/**
 * 列出 host 的根（盘符 / 白名单根）。
 */
export async function listHostRoots(host: HostID): Promise<DirListing> {
  const raw = await fetch(`${API_PATHS.fsRoots}?host=${toHost(host)}`, { credentials: 'include' })
  const body = await raw.json().catch(() => null)
  if (!raw.ok) {
    const err = body?.error
    throw new ApiError(err?.message || `列举根目录失败（HTTP ${raw.status}）`, err?.code || 'http_error', raw.status, err?.trace_id || '')
  }
  return {
    host,
    path: '',
    parent: null,
    separator: body?.separator ?? guessSeparator(''),
    roots: body?.roots ?? [],
    dirs: [],
  }
}

/**
 * 列出 host 上 path 的子目录。
 * path 为空 → 等同 listHostRoots（从根开始浏览）。
 */
export async function listHostDirs(host: HostID, path: string): Promise<DirListing> {
  if (!path) return listHostRoots(host)
  const q = `host=${toHost(host)}&path=${encodeURIComponent(path)}`
  const raw = await fetch(`${API_PATHS.fsDirs}?${q}`, { credentials: 'include' })
  const body = await raw.json().catch(() => null)
  if (!raw.ok) {
    const err = body?.error
    throw new ApiError(err?.message || `列举目录失败（HTTP ${raw.status}）`, err?.code || 'http_error', raw.status, err?.trace_id || '')
  }
  return {
    host,
    path: body?.path ?? path,
    parent: body?.parent ?? null,
    separator: body?.separator ?? guessSeparator(path),
    roots: body?.roots ?? [],
    dirs: body?.dirs ?? [],
    truncated: !!body?.truncated,
  }
}

/**
 * 按分隔符把绝对路径切成面包屑段（用于逐级回跳）。
 * 只做展示切分，**不参与安全判断**（安全判断全在服务端）。
 */
export function breadcrumbs(path: string, separator: string): Array<{ name: string; path: string }> {
  if (!path) return []
  const sep = separator || '/'
  const isWin = sep === '\\'
  const parts = path.split(sep).filter(Boolean)
  const out: Array<{ name: string; path: string }> = []
  let acc = ''
  for (const part of parts) {
    acc = acc ? `${acc}${sep}${part}` : part
    // Windows 盘符段（如 "D:"）需补分隔符才构成合法绝对路径
    const full = isWin && acc.length === 2 && acc.endsWith(':') ? `${acc}${sep}` : acc
    out.push({ name: part, path: full })
  }
  return out
}
