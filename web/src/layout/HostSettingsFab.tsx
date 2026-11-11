/**
 * 「宿主设置」浮标（契约三，2026-11 客户端通用流程）。
 *
 * ## 为什么浮标而不是菜单项
 *
 * 主界面三端同构（服务端下发同一份代码），客户端的「设置」入口不属于任何业务
 * 菜单 —— 它是壳的属性。浮动小圆钮对三端形态都成立，且不与 TopBar 的业务动作
 * 抢占空间。
 *
 * ## 渲染条件
 *
 * 客户端壳内（桌面 Tauri / 移动 Flutter 桥）才显示，纯浏览器零干扰。
 *
 * ## 位置与拖拽（2026-11 增强）
 *
 * - **默认右侧、上下居中**。历史版本固定在右下角，正压住聊天输入区的发送热区，
 *   用户明确反馈「挡住我操作」，故默认位改到右侧中部；
 * - **可拖到任意位置**，松手后横向自动吸附到最近的左/右边缘（垂直位置保留），
 *   与 SideRail（右侧浮动编排胶囊栏）同一套交互纪律；
 * - 位置写入 localStorage（归一化比例），刷新与重启后保持；
 * - 吸附到右侧且与 SideRail 竖向重叠时自动让位，绝不覆盖编排胶囊。
 *
 * ## 点击降级纪律（本次修复的真实缺陷）
 *
 * 旧代码 `if (!openHostSettings()) setDisabled(true)`：`openHostSettings` 是
 * **async** 函数，`!Promise` 恒为 false → `setDisabled(true)` 永不执行 →
 * 注释里承诺的「通道不可用就隐藏浮标」从未生效，桌面端一旦 invoke 失败
 * （ACL 拒绝 / 壳侧导航失败），用户看到的就是一个**点了毫无反应的死钮**。
 * 现改为 `await` 具名 handler，且壳侧 `open_baseline` 已改为失败返回 Err，
 * 语义闭环：跳不过去 → 浮标自隐 + console 带原因。
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { Settings } from 'lucide-react'
import { isMobileBridge, openHostSettings, shouldShowHostSettings } from '../lib/hostShell'
import {
  DRAG_THRESHOLD,
  EDGE_GAP,
  FAB_SIZE,
  avoidSideRail,
  defaultFabPos,
  fabPosFromPersisted,
  fabPosToPersisted,
  readPersistedFabPos,
  snapFabPos,
  writePersistedFabPos,
  type FabPos,
  type FabRect,
} from '../lib/fabDrag'

/** 视口尺寸（WebView 内即可视区，固定定位的参照系）。 */
function viewport(): { vw: number; vh: number } {
  if (typeof window === 'undefined') return { vw: 1024, vh: 768 }
  return { vw: window.innerWidth, vh: window.innerHeight }
}

/**
 * 边缘内距：移动端比桌面多留一些。
 *
 * 原因：iOS WebView 左缘约 20px 是系统「右滑返回」手势区，浮标若贴着 left:12
 * 起步拖拽会被返回手势抢走；留 20 可显著降低误触。
 */
function edgeGap(): number {
  return isMobileBridge() ? 20 : EDGE_GAP
}

/** 右侧浮动编排胶囊栏的包围盒（没有则不避让）。 */
function sideRailRect(): FabRect | null {
  if (typeof document === 'undefined') return null
  const el = document.querySelector('[data-harness-side-rail]')
  return el ? el.getBoundingClientRect() : null
}

/** 依据持久化记录（无记录则默认位）算出落点，含夹取与 SideRail 避让。 */
function resolveFabPos(): FabPos {
  const { vw, vh } = viewport()
  const gap = edgeGap()
  const saved = readPersistedFabPos()
  const base = saved ? fabPosFromPersisted(saved, vw, vh, FAB_SIZE, gap) : defaultFabPos(vw, vh, FAB_SIZE, gap)
  return avoidSideRail(base, sideRailRect(), vw, vh, FAB_SIZE, gap)
}

export function HostSettingsFab() {
  // ⚠️ 所有 hooks 必须在此「提前 return」之前声明完毕（React error #310 铁律）。
  const [disabled, setDisabled] = useState(false)
  const [busy, setBusy] = useState(false)
  const [pos, setPos] = useState<FabPos>(() => resolveFabPos())
  const [dragOffset, setDragOffset] = useState<{ x: number; y: number } | null>(null)
  const [isDragging, setIsDragging] = useState(false)

  const btnRef = useRef<HTMLButtonElement | null>(null)
  const dragStartRef = useRef<{ x: number; y: number; pos: FabPos } | null>(null)
  const hasMovedRef = useRef(false)
  const activePointerRef = useRef<number | null>(null)
  // 松手瞬间要读「最后一次位移」。state 在 pointerup 里可能尚未 flush（批处理），
  // 故再用一个 ref 兜底，保证落点与用户眼睛看到的最终位置一致。
  const dragOffsetRef = useRef<{ x: number; y: number } | null>(null)

  // 视口变化（窗口缩放 / 转屏 / 分屏拖拽）时按持久化比例重算，防止浮标留在屏幕外。
  useEffect(() => {
    const onViewportChange = () => {
      // 拖拽进行中不打断用户手势，松手时自会重新夹取。
      if (hasMovedRef.current) return
      setPos(resolveFabPos())
    }
    window.addEventListener('resize', onViewportChange)
    window.addEventListener('orientationchange', onViewportChange)
    return () => {
      window.removeEventListener('resize', onViewportChange)
      window.removeEventListener('orientationchange', onViewportChange)
    }
  }, [])

  // 挂载后补一次定位：`useState` 初始化时 DOM 尚未提交，`sideRailRect()` 取不到
  // 编排胶囊栏的真实包围盒（首帧只会用默认位）。等一帧后重算，才能正确避让。
  useEffect(() => {
    const id = window.requestAnimationFrame(() => {
      if (!hasMovedRef.current) setPos(resolveFabPos())
    })
    return () => window.cancelAnimationFrame(id)
  }, [])

  // 编排胶囊栏是**运行态**出现的（有工具调用才渲染），首帧挂载时它还不存在。
  // 故监听 DOM 结构变化并防抖重算：一旦胶囊栏出现且与浮标相交，浮标立即让位；
  // 胶囊栏消失时也回到原（用户选定的）位置。
  useEffect(() => {
    if (typeof MutationObserver === 'undefined') return
    let timer: number | undefined
    const schedule = () => {
      window.clearTimeout(timer)
      timer = window.setTimeout(() => {
        if (hasMovedRef.current) return
        setPos((cur) => {
          const next = resolveFabPos()
          return next.x === cur.x && next.y === cur.y ? cur : next
        })
      }, 250)
    }
    const mo = new MutationObserver(schedule)
    mo.observe(document.body, { childList: true, subtree: true })
    return () => {
      window.clearTimeout(timer)
      mo.disconnect()
    }
  }, [])

  const persist = useCallback((next: FabPos) => {
    const { vw, vh } = viewport()
    writePersistedFabPos(fabPosToPersisted(next, vw, vh))
  }, [])

  // 首帧就检测不到壳 → 不渲染（零开销）；通道已验证不可用 → 隐藏，不留死钮。
  if (!shouldShowHostSettings() || disabled) return null

  // ---------------- 拖拽与吸附 ----------------
  // 注意：此处起不得再声明任何 hook（已在上方统一声明）。

  const onPointerDown = (e: React.PointerEvent<HTMLButtonElement>) => {
    if (e.button !== 0) return
    // 按下即 setPointerCapture 会把 click 重定向到元素（SideRail 踩过的坑），
    // 故此处只记 pointerId，确认拖拽意图（位移 > 阈值）后才捕获。
    activePointerRef.current = e.pointerId
    dragStartRef.current = { x: e.clientX, y: e.clientY, pos }
    dragOffsetRef.current = null
    hasMovedRef.current = false
    setIsDragging(false)
  }

  const onPointerMove = (e: React.PointerEvent<HTMLButtonElement>) => {
    const start = dragStartRef.current
    if (!start || e.pointerId !== activePointerRef.current) return
    const dx = e.clientX - start.x
    const dy = e.clientY - start.y

    if (!hasMovedRef.current && Math.hypot(dx, dy) > DRAG_THRESHOLD) {
      hasMovedRef.current = true
      setIsDragging(true)
      try {
        btnRef.current?.setPointerCapture(e.pointerId)
      } catch {
        // 老 WebView 无 pointer capture：退化为普通拖拽，不影响功能。
      }
    }
    if (hasMovedRef.current) {
      dragOffsetRef.current = { x: dx, y: dy }
      setDragOffset({ x: dx, y: dy })
    }
  }

  const onPointerUp = (e: React.PointerEvent<HTMLButtonElement>) => {
    if (activePointerRef.current === null) return
    activePointerRef.current = null
    try {
      btnRef.current?.releasePointerCapture(e.pointerId)
    } catch {
      // 未捕获过就释放会抛，忽略即可。
    }

    const start = dragStartRef.current
    const offset = dragOffsetRef.current
    dragStartRef.current = null
    dragOffsetRef.current = null

    if (hasMovedRef.current && start && offset) {
      const { vw, vh } = viewport()
      const gap = edgeGap()
      const raw = { x: start.pos.x + offset.x, y: start.pos.y + offset.y }
      // 松手吸附最近的左右边 → 再夹回视口 → 最后避让右侧编排胶囊栏。
      const snapped = snapFabPos(raw, vw, vh, FAB_SIZE, gap)
      const final = avoidSideRail(snapped, sideRailRect(), vw, vh, FAB_SIZE, gap)
      setPos(final)
      persist(final)
    }

    setDragOffset(null)
    // 延迟复位，防止同一个 pointerup 冒泡触发的 click 被误认成轻击。
    window.setTimeout(() => {
      setIsDragging(false)
      hasMovedRef.current = false
    }, 80)
  }

  const onClick = async () => {
    if (isDragging) return
    setBusy(true)
    try {
      const result = await openHostSettings()
      if (!result.ok) {
        // 通道不可用 → 隐藏浮标（绝不留死钮），原因保留在 console 供诊断。
        console.warn('[hostSettings] 浮标通道不可用，浮标将隐藏：', result.reason)
        setDisabled(true)
      }
    } catch (err) {
      console.warn('[hostSettings] 浮标点击异常，浮标将隐藏：', err)
      setDisabled(true)
    } finally {
      setBusy(false)
    }
  }

  const translateX = dragOffset ? dragOffset.x : 0
  const translateY = dragOffset ? dragOffset.y : 0

  return (
    <button
      ref={btnRef}
      type="button"
      title="返回宿主设置（配对/登录/日志）｜可拖动，松手自动吸附左右边缘"
      aria-label="返回宿主设置"
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onPointerCancel={onPointerUp}
      onClick={() => {
        void onClick()
      }}
      style={{
        left: `${pos.x}px`,
        top: `${pos.y}px`,
        transform: `translate3d(${translateX}px, ${translateY}px, 0)`,
        transition: isDragging ? 'none' : 'transform 0.28s cubic-bezier(0.16, 1, 0.3, 1), left 0.22s ease-out, top 0.22s ease-out',
        touchAction: 'none',
      }}
      className={`fixed z-[35] w-9 h-9 rounded-full bg-panel border border-line/60 text-inkfaint hover:text-ink hover:border-line shadow-sm flex items-center justify-center transition-colors select-none ${
        isDragging ? 'cursor-grabbing opacity-90' : 'cursor-grab'
      } ${busy ? 'opacity-60' : ''}`}
    >
      <Settings size={15} />
    </button>
  )
}
