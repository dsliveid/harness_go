import { useState, useRef, useEffect } from 'react'
import { ListTodo, CheckSquare, TerminalSquare, Layers, GripHorizontal } from 'lucide-react'
import { useWorkspace } from '../../stores/workspaceStore'
import { useUI } from '../../stores/uiStore'
import { cn } from '../../components/ui'

// 会话期间记忆拖动停留的 Y 轴高度
let savedRailTop = 16

/**
 * 浮动编排胶囊栏（移动端/桌面端通用）：
 * - 背景透明，取消模糊显示（绝无遮挡下层主内容的毛玻璃脏色块）
 * - 支持全向触摸/鼠标拖拽调整位置
 * - 拖动松开后，自动平滑吸附浮动显示于该高度的【最右侧】
 * - 区分轻击与拖拽：拖拽位移不误触展开抽屉，轻触即开对应面板
 */
export function SideRail() {
  const { todos, plans, subagents, commands } = useWorkspace()
  const { panel, setPanel } = useUI()
  const activePlan = plans.find((p) => p.active)

  const doneCnt = todos.filter((t) => t.status === 'done').length
  const planDone = activePlan?.steps.filter((s) => s.status === 'done').length ?? 0
  const runningCmds = commands.filter((c) => c.status === 'running').length
  const runningAgents = subagents.filter((a) => a.status === 'running').length

  const hasItems = todos.length > 0 || Boolean(activePlan) || runningCmds > 0 || subagents.length > 0

  const [top, setTop] = useState(savedRailTop)
  const [dragOffset, setDragOffset] = useState<{ x: number; y: number } | null>(null)
  const [isDragging, setIsDragging] = useState(false)

  const railRef = useRef<HTMLDivElement>(null)
  const dragStartRef = useRef<{ startX: number; startY: number; initialTop: number } | null>(null)
  const hasMovedRef = useRef(false)

  // 视口尺寸变化时校正溢出边界
  useEffect(() => {
    const el = railRef.current
    if (!el) return
    const clampPos = () => {
      const parentH = el.parentElement?.clientHeight || window.innerHeight
      const railH = el.offsetHeight || 120
      const maxTop = Math.max(8, parentH - railH - 60)
      setTop((cur) => {
        const next = Math.max(8, Math.min(maxTop, cur))
        savedRailTop = next
        return next
      })
    }
    window.addEventListener('resize', clampPos)
    return () => window.removeEventListener('resize', clampPos)
  }, [])

  if (!hasItems) return null

  // ---------------- 拖拽与吸附逻辑 ----------------
  const activePointerRef = useRef<number | null>(null)

  const onPointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
    // 仅响应鼠标左键或单点触摸
    if (e.button !== 0) return

    // 【修复】按下即 setPointerCapture 会把 click 事件重定向到容器，
    // 导致胶囊按钮的 onClick 不触发（表现为点击无反应）。
    // 改为：按下只记录 pointerId；确认拖拽意图（位移 > 5px）后才捕获。
    activePointerRef.current = e.pointerId
    dragStartRef.current = {
      startX: e.clientX,
      startY: e.clientY,
      initialTop: top,
    }
    hasMovedRef.current = false
    setIsDragging(false)
  }

  const onPointerMove = (e: React.PointerEvent<HTMLDivElement>) => {
    if (!dragStartRef.current || e.pointerId !== activePointerRef.current) return
    const dx = e.clientX - dragStartRef.current.startX
    const dy = e.clientY - dragStartRef.current.startY

    // 位移超过 5px 判定为拖拽意图，避免正常点击误判；此刻才捕获指针，
    // 保证拖出元素边界后仍能持续跟踪。
    if (!hasMovedRef.current && Math.hypot(dx, dy) > 5) {
      hasMovedRef.current = true
      setIsDragging(true)
      try {
        railRef.current?.setPointerCapture(e.pointerId)
      } catch {}
    }

    if (hasMovedRef.current) {
      // 允许向左侧屏幕中央/任意方向拖动跟随
      setDragOffset({ x: dx, y: dy })
    }
  }

  const onPointerUp = (e: React.PointerEvent<HTMLDivElement>) => {
    if (activePointerRef.current === null) return
    activePointerRef.current = null

    try {
      railRef.current?.releasePointerCapture(e.pointerId)
    } catch {}

    if (!dragStartRef.current) return

    if (hasMovedRef.current && dragOffset) {
      const parent = railRef.current?.parentElement
      const parentH = parent?.clientHeight || window.innerHeight
      const railH = railRef.current?.offsetHeight || 120
      const minTop = 8
      const maxTop = Math.max(minTop, parentH - railH - 60) // 预留底部输入框避让空间

      const finalTop = Math.max(minTop, Math.min(maxTop, dragStartRef.current.initialTop + dragOffset.y))
      setTop(finalTop)
      savedRailTop = finalTop
    }

    dragStartRef.current = null
    // 清除横向偏移：通过 CSS 平滑回弹至最右侧 (right: 0)
    setDragOffset(null)

    // 延迟重置 hasMoved 状态，防止同一个 pointerup 冒泡触发 button onClick
    setTimeout(() => {
      setIsDragging(false)
      hasMovedRef.current = false
    }, 80)
  }

  const handlePillClick = (p: typeof panel) => {
    // 【修复】守卫仅看 isDragging：hasMoved 若因异常（如 pointercancel 丢失）卡在 true，
    // 不应永久吞掉所有胶囊点击；isDragging 有 80ms 兜底复位，且拖拽后误触也由它拦截。
    if (isDragging) return
    setPanel(panel === p ? 'none' : (p as never))
  }

  // 胶囊样式：背景透明，取消模糊，清晰线框，触摸友好
  const pillBase
    = 'group flex items-center gap-2 pl-3 pr-2.5 py-1.5 rounded-l-2xl bg-transparent hover:bg-panel2/70 border-y border-l border-edge hover:border-edge2 text-[11px] text-inkdim hover:text-ink transition-colors cursor-pointer active:scale-95 touch-none'

  const badgeCls = (tone: string) => `text-[10px] px-1.5 py-0.5 rounded-md font-medium inline-flex items-center gap-1 ${tone}`

  // 拖拽中的位置：水平自由跟随；松开后 snap 回最右侧 (x=0)
  const currentTranslateX = dragOffset ? dragOffset.x : 0
  const currentTranslateY = dragOffset ? dragOffset.y : 0

  return (
    <div
      ref={railRef}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onPointerCancel={onPointerUp}
      style={{
        top: `${top}px`,
        transform: `translate3d(${currentTranslateX}px, ${currentTranslateY}px, 0)`,
        transition: isDragging
          ? 'none'
          : 'transform 0.28s cubic-bezier(0.16, 1, 0.3, 1), top 0.12s ease-out',
        touchAction: 'none',
      }}
      className={cn(
        'absolute right-0 z-30 flex flex-col items-end gap-1.5 select-none',
        isDragging ? 'cursor-grabbing opacity-90' : 'cursor-grab',
      )}
      title="可上下左右拖动，松开后自动靠右吸附"
    >
      {/* 顶部微型手柄条：提示可拖动交互 */}
      <div className="w-10 h-3 rounded-l-full bg-transparent flex items-center justify-center text-inkfaint/70 hover:text-ink cursor-grab pr-1">
        <GripHorizontal size={13} />
      </div>

      {todos.length > 0 && (
        <button
          type="button"
          className={cn(pillBase, panel === 'todo' && 'border-accent bg-accent/15 text-ink')}
          onClick={() => handlePillClick('todo')}
          title="展开任务清单"
        >
          <span className={badgeCls('bg-purple-500/15 text-purple-400')}>
            <CheckSquare size={11} className="shrink-0" />
            <span>清单</span>
          </span>
          <span className="flex items-center gap-1 font-mono font-medium text-ink">
            <span className="text-purple-400 font-bold">{doneCnt}</span>
            <span className="text-inkfaint">/{todos.length}</span>
          </span>
        </button>
      )}

      {activePlan && (
        <button
          type="button"
          className={cn(pillBase, panel === 'plan' && 'border-accent bg-accent/15 text-ink')}
          onClick={() => handlePillClick('plan')}
          title="展开方案"
        >
          <span
            className={badgeCls(
              activePlan.steps.every((s) => s.status === 'done')
                ? 'bg-ok/15 text-ok'
                : 'bg-emerald-500/15 text-emerald-400',
            )}
          >
            <ListTodo size={11} className="shrink-0" />
            <span>v{activePlan.version} 方案</span>
          </span>
          <span className="flex items-center gap-1 font-mono font-medium text-ink">
            <span className="text-emerald-400 font-bold">{planDone}</span>
            <span className="text-inkfaint">/{activePlan.steps.length}</span>
          </span>
        </button>
      )}

      {runningCmds > 0 && (
        <button
          type="button"
          className={cn(pillBase, panel === 'commands' && 'border-accent bg-accent/15 text-ink')}
          onClick={() => handlePillClick('commands')}
          title={`展开正在执行的命令行（${runningCmds} 个）`}
        >
          <span className={badgeCls('bg-amber-500/15 text-amber-400')}>
            <TerminalSquare size={11} className="shrink-0" />
            <span>命令行</span>
          </span>
          <span className="relative flex h-2 w-2 shrink-0">
            <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-ok opacity-75" />
            <span className="relative inline-flex rounded-full h-2 w-2 bg-ok" />
          </span>
          <span className="font-mono text-[11px] font-bold text-amber-400">{runningCmds}</span>
        </button>
      )}

      {subagents.length > 0 && (
        <button
          type="button"
          className={cn(pillBase, (panel === 'runs' || panel === 'subagent') && 'border-accent bg-accent/15 text-ink')}
          onClick={() => handlePillClick('runs')}
          title={`展开子进程列表（${subagents.length} 个）`}
        >
          <span className={badgeCls('bg-cyan-500/15 text-cyan-400')}>
            <Layers size={11} className="shrink-0" />
            <span>子进程</span>
          </span>
          {runningAgents > 0 && (
            <span className="relative flex h-2 w-2 shrink-0">
              <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-ok opacity-75" />
              <span className="relative inline-flex rounded-full h-2 w-2 bg-ok" />
            </span>
          )}
          <span className="font-mono text-[11px] font-bold text-cyan-400">{subagents.length}</span>
        </button>
      )}
    </div>
  )
}
