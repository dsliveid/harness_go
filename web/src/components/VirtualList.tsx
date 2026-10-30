import { useEffect, type RefObject } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'

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
