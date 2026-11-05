import { MessageList } from './MessageList'
import { Composer } from './Composer'
import { ApprovalBar } from './ApprovalBar'
import { SideRail } from './SideRail'
import { SessionLeaseBar } from './SessionLeaseBar'
import { useChat } from '../../stores/chatStore'
import { useUI } from '../../stores/uiStore'
import { Button } from '../../components/ui'

export function ChatView() {
  const { lastError, runStatus, retry, approval } = useChat()

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

      {/* 会话租约（C-6）：展示当前会话写者归属 / epoch / 心跳，无租约时为「服务端本地可写」 */}
      <SessionLeaseBar />

      <div className="relative flex-1 min-h-0 flex flex-col">
        <MessageList />
        <SideRail />
      </div>

      <ApprovalBar />
      <Composer />
    </div>
  )
}
