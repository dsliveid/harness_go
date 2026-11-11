/**
 * 宿主设置浮标（HostSettingsFab）的定位数学 —— **纯函数、零 DOM 依赖**。
 *
 * ## 为什么单独抽一层
 *
 * 拖拽定位的正确性全在「夹取 / 吸附 / 避让 / 归一化」这四件纯计算上，
 * 把它们从组件里剥出来，才能在改动时用肉眼+构建即可复核（本项目前端暂无
 * 单测运行器，见 web/package.json 只有 build 脚本），也避免组件被几何细节淹没。
 *
 * ## 坐标约定（务必统一，否则吸附方向会反）
 *
 * - `FabPos.x / y` 是浮标**左上角**在视口中的 CSS 像素坐标，非中心点；
 * - 视口尺寸一律用 `window.innerWidth / innerHeight`（WebView 内即可视区）；
 * - 持久化**不存像素**，存 `{ side, ry }`：`side` 是吸附到的左右边，
 *   `ry` 是浮标中心点的垂直归一化比例 ∈ [0,1]。这样换个分辨率/窗口大小
 *   打开，浮标仍在「同一相对高度、同一边」，而不是飞到屏幕外。
 *
 * ## 与 SideRail 的关系
 *
 * 右侧浮动编排胶囊栏（features/chat/SideRail.tsx）同样贴右边缘。浮标吸附到
 * 右侧时可能与之竖向重叠 —— 由 `avoidSideRail` 负责竖向让位，绝不覆盖。
 */

/** 浮标直径（px，与组件 w-9 h-9 保持一致）。 */
export const FAB_SIZE = 36

/** 距视口边缘的安全内距（px）。移动端刘海/圆角与 iOS 左缘返回手势区都靠它兜住。 */
export const EDGE_GAP = 12

/** 轻击 / 拖拽的判定阈值（px）。与 SideRail 保持同一手感。 */
export const DRAG_THRESHOLD = 5

/** 持久化 key（带版本号，未来几何语义变更时改名即可自然失效旧值）。 */
export const STORAGE_KEY = 'harness.hostSettingsFabPos.v1'

export type FabSide = 'left' | 'right'

export interface FabPos {
  x: number
  y: number
}

export interface FabPersisted {
  side: FabSide
  ry: number
}

/** 只取本模块需要的包围盒字段（DOMRect 可直接传进来）。 */
export interface FabRect {
  top: number
  right: number
  bottom: number
  left: number
}

function clampNum(v: number, lo: number, hi: number): number {
  // hi 可能小于 lo（视口比浮标还小时），此时优先保证 lo，避免出现负值区间。
  return Math.min(Math.max(v, lo), Math.max(lo, hi))
}

/** 把浮标夹取回视口内（四周留 EDGE_GAP）。 */
export function clampFabPos(pos: FabPos, vw: number, vh: number, size = FAB_SIZE, gap = EDGE_GAP): FabPos {
  return {
    x: clampNum(pos.x, gap, vw - size - gap),
    y: clampNum(pos.y, gap, vh - size - gap),
  }
}

/**
 * 默认位置：**右侧、上下居中**。
 *
 * 为什么默认不在左下角：右下角是聊天输入区的发送/滚动热区，浮标压上去会
 * 直接挡住用户操作（这正是本次反馈的原始痛点）；右上则与 TopBar 动作区抢位。
 */
export function defaultFabPos(vw: number, vh: number, size = FAB_SIZE, gap = EDGE_GAP): FabPos {
  return clampFabPos({ x: vw - size - gap, y: (vh - size) / 2 }, vw, vh, size, gap)
}

/** 松手吸附：按中心点与视口中线的比较，贴到最近的左 / 右边，垂直位置保留。 */
export function snapFabPos(pos: FabPos, vw: number, vh: number, size = FAB_SIZE, gap = EDGE_GAP): FabPos {
  const centerX = pos.x + size / 2
  const side: FabSide = centerX <= vw / 2 ? 'left' : 'right'
  const x = side === 'left' ? gap : vw - size - gap
  return clampFabPos({ x, y: pos.y }, vw, vh, size, gap)
}

/**
 * 避开右侧浮动编排胶囊栏（SideRail）：竖向相交时优先下移，下方放不下再上移。
 *
 * 只在两个包围盒（含 spacing 缓冲）横竖都相交时才动，否则原样返回 ——
 * 避免「用户在左侧拖好了，浮标却被这层无谓挪位」。
 */
export function avoidSideRail(
  pos: FabPos,
  rail: FabRect | null,
  vw: number,
  vh: number,
  size = FAB_SIZE,
  gap = EDGE_GAP,
  spacing = 10,
): FabPos {
  const base = clampFabPos(pos, vw, vh, size, gap)
  if (!rail) return base

  const vOverlap = base.y < rail.bottom + spacing && base.y + size > rail.top - spacing
  const hOverlap = base.x < rail.right + spacing && base.x + size > rail.left - spacing
  if (!vOverlap || !hOverlap) return base

  const below = rail.bottom + spacing
  const above = rail.top - spacing - size
  if (below <= vh - size - gap) return clampFabPos({ x: base.x, y: below }, vw, vh, size, gap)
  if (above >= gap) return clampFabPos({ x: base.x, y: above }, vw, vh, size, gap)
  return base // 上下都塞不下（极端矮视口）：保持原位，由用户自行拖开。
}

/** 像素位置 → 持久化结构（归一化，跨分辨率自适应）。 */
export function fabPosToPersisted(pos: FabPos, vw: number, vh: number, size = FAB_SIZE): FabPersisted {
  const side: FabSide = pos.x + size / 2 <= vw / 2 ? 'left' : 'right'
  const ry = vh > 0 ? (pos.y + size / 2) / vh : 0.5
  return { side, ry: clampNum(ry, 0, 1) }
}

/** 持久化结构 → 像素位置（边距与夹取在此一并生效）。 */
export function fabPosFromPersisted(
  p: FabPersisted,
  vw: number,
  vh: number,
  size = FAB_SIZE,
  gap = EDGE_GAP,
): FabPos {
  const x = p.side === 'left' ? gap : vw - size - gap
  return clampFabPos({ x, y: p.ry * vh - size / 2 }, vw, vh, size, gap)
}

/** 严格解析 localStorage 原文：任何字段不合规一律返回 null（回落默认位）。 */
export function parsePersistedFabPos(raw: string | null): FabPersisted | null {
  if (!raw) return null
  try {
    const v: unknown = JSON.parse(raw)
    if (!v || typeof v !== 'object') return null
    const { side, ry } = v as { side?: unknown; ry?: unknown }
    if (side !== 'left' && side !== 'right') return null
    if (typeof ry !== 'number' || !Number.isFinite(ry) || ry < 0 || ry > 1) return null
    return { side, ry }
  } catch {
    return null
  }
}

function safeStorage(): Storage | null {
  // WebView 隐私模式 / 存储被禁时 `localStorage` 取用本身就可能抛错。
  try {
    return typeof window !== 'undefined' ? window.localStorage : null
  } catch {
    return null
  }
}

export function readPersistedFabPos(storage: Storage | null = safeStorage()): FabPersisted | null {
  if (!storage) return null
  try {
    return parsePersistedFabPos(storage.getItem(STORAGE_KEY))
  } catch {
    return null
  }
}

export function writePersistedFabPos(p: FabPersisted, storage: Storage | null = safeStorage()): void {
  if (!storage) return
  try {
    storage.setItem(STORAGE_KEY, JSON.stringify(p))
  } catch {
    // 配额满 / 被禁：位置记忆是锦上添花，写不进去绝不影响拖拽本身。
  }
}
