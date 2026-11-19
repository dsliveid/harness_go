import { useEffect, useRef, type RefObject } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'

export interface ScrollTarget {
  index: number
  nonce: number
  align: 'start' | 'center' | 'end'
}

export interface VirtualListProps<T> {
  items: T[]
  /** 滚动容器（必须是已挂载的 overflow-y-auto 元素） */
  scrollerRef: RefObject<HTMLElement>
  /** 初始高度估计（实测后由 measureElement 覆盖） */
  estimateSize: (index: number) => number
  renderItem: (item: T, index: number) => React.ReactNode
  getKey: (item: T, index: number) => string
  /** 是否贴底（仅当用户位于底部时传 true） */
  stickToBottom?: boolean
  /** 贴底触发信号：流式内容增长时变化，可在长度不变时也触发跟随 */
  followKey?: string | number
  overscan?: number
  /**
   * 受控定位请求（「定位出处」用）：nonce 变化即滚动到 index 项。
   *
   * 为什么不复用 scrollToIndex 直调：virtualizer 实例在 VirtualList 内部，
   * 外部（MessageList）拿不到；且定位需要等 items 更新、容器测量完成后再执行，
   * 只能由本组件内部的 effect 在正确时机触发。
   */
  scrollTarget?: ScrollTarget | null
  /** 定位请求执行后回调（父层据此清除请求与置位高亮） */
  onScrollTargetDone?: (nonce: number) => void
}

/**
 * 动态高度虚拟列表：
 * - 以 item 为单位（本项目为 turn），绝对定位渲染可视区 + overscan
 * - 通过 measureElement + ResizeObserver 实测高度（消息/工具卡片/diff 高度不固定）
 * - 仅在 stickToBottom 为 true 时自动贴底，用户上滚阅读时不会被强制拉回
 */
export function VirtualList<T>({
  items,
  scrollerRef,
  estimateSize,
  renderItem,
  getKey,
  stickToBottom,
  followKey,
  overscan = 6,
  scrollTarget,
  onScrollTargetDone,
}: VirtualListProps<T>) {
  const virtualizer = useVirtualizer({
    count: items.length,
    getScrollElement: () => scrollerRef.current,
    estimateSize,
    overscan,
    getItemKey: (index) => getKey(items[index], index),
  })

  useEffect(() => {
    if (!stickToBottom || items.length === 0) return
    // rAF 等容器完成首帧测量后再贴底（首屏与切换会话）
    const id = requestAnimationFrame(() => {
      virtualizer.scrollToIndex(items.length - 1, { align: 'end' })
    })
    return () => cancelAnimationFrame(id)
  }, [items.length, followKey, stickToBottom, virtualizer])

  // 受控定位：**独立 effect 且置于贴底 effect 之后**，保证同一帧内定位优先于贴底。
  // 两层 rAF 的原因：首帧 items/高度尚未测量完，立刻 scrollToIndex 会滚到估算位置后
  // 又被 measureElement 的实际高度修正顶偏；等一帧测量落地再滚，落点才准。
  const doneRef = useRef<number | null>(null)
  useEffect(() => {
    if (!scrollTarget || items.length === 0) return
    if (doneRef.current === scrollTarget.nonce) return
    const index = Math.max(0, Math.min(scrollTarget.index, items.length - 1))
    let raf2 = 0
    const raf1 = requestAnimationFrame(() => {
      raf2 = requestAnimationFrame(() => {
        virtualizer.scrollToIndex(index, { align: scrollTarget.align })
        doneRef.current = scrollTarget.nonce
        onScrollTargetDone?.(scrollTarget.nonce)
      })
    })
    return () => {
      cancelAnimationFrame(raf1)
      cancelAnimationFrame(raf2)
    }
    // onScrollTargetDone 故意不入依赖：父层每次都传新闭包，入依赖会反复重滚
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scrollTarget?.nonce, items.length, virtualizer])

  return (
    <div style={{ height: virtualizer.getTotalSize(), position: 'relative', width: '100%' }}>
      {virtualizer.getVirtualItems().map((vi) => (
        <div
          key={vi.key}
          data-index={vi.index}
          ref={virtualizer.measureElement}
          style={{
            position: 'absolute',
            top: 0,
            left: 0,
            width: '100%',
            transform: `translateY(${vi.start}px)`,
          }}
        >
          {renderItem(items[vi.index], vi.index)}
        </div>
      ))}
    </div>
  )
}
