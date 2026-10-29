import { useEffect } from 'react'
import { MessageList } from './MessageList'
import { Composer } from './Composer'
import { ApprovalBar } from './ApprovalBar'
import { CompactionBar, CompactionNotice } from './CompactionBar'
import { SideRail } from './SideRail'
import { useChat } from '../../stores/chatStore'
import { useSessions } from '../../stores/sessionStore'
import { COMPACTION_RATIO, resolveModelContext } from '../../lib/context'
import { useUI } from '../../stores/uiStore'
import { Button } from '../../components/ui'

export function ChatView() {
  const { lastError, runStatus, retry, approval, tokensIn, tokensOut, compactionFreed, compactionPending, requestCompaction } = useChat()
  const sessions = useSessions((s) => s.sessions)
  const currentId = useSessions((s) => s.currentId)

  const limit = resolveModelContext(sessions.find((x) => x.id === currentId)?.modelRef).limit
  const activeTokens = Math.max(0, tokensIn + tokensOut - compactionFreed)

  // 达到阈值自动弹压缩确认（用户选「稍后再说」后需再涨 5% 才会重新提示）
  useEffect(() => {
    if (compactionPending) return
    if (limit > 0 && activeTokens / limit >= COMPACTION_RATIO) requestCompaction('auto')
  }, [activeTokens, limit, compactionPending, requestCompaction])

  return (
    <div className="flex flex-col h-full min-w-0">
      {runStatus === 'interrupted' && (
        <div className="mx-3 mt-2 rounded-lg border border-warn/40 bg-warn/[0.07] px-3 py-2 text-[12px] text-warn flex items-center gap-2">
          <span>该次运行被中断（进程重启）。</span>
          <Button size="sm" variant="outline" onClick={retry}>重试本轮</Button>
        </div>
      )}

      {lastError && (
        <div className="mx-3 mt-2 rounded-lg border border-err/40 bg-err/[0.07] px-3 py-2 text-[12px] text-err font-mono">
          {lastError}
        </div>
      )}

      {/* 压缩摘要：仅在发生过压缩后出现，可展开查看摘要全文 */}
      <CompactionNotice />

      <div className="relative flex-1 min-h-0 flex flex-col">
        <MessageList />
        <SideRail />
      </div>

      {/* 审批优先：有待审批项时先处理审批，压缩确认顺延 */}
      {!approval && <CompactionBar />}
      <ApprovalBar />
      <Composer />
    </div>
  )
}
