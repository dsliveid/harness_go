import { useCallback, useEffect, useState } from 'react'
import { RefreshCw, ShieldAlert, ShieldCheck, Smartphone } from 'lucide-react'
import { api } from '../../lib/api'
import { useSessions } from '../../stores/sessionStore'
import { useUI } from '../../stores/uiStore'
import { Badge, cn } from '../../components/ui'
import type { SessionLeaseDTO } from '../../lib/apiTypes'

/** 租约带 TTL 与心跳，20s 静默轮询即可反映「谁在写」，无需 WS 推送。 */
const POLL_MS = 20_000

/** 毫秒时间戳 → 本地 HH:MM:SS；0 / 缺省返回占位符（0 = 无租约，不是错误）。 */
function fmtClock(ms?: number): string {
  if (!ms) return '—'
  const d = new Date(ms)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

/**
 * 会话租约状态条（C-6）：展示「当前谁持有该会话的写权」。
 *
 * 契约依据 `internal/api/device_handler.go:126` + `internal/service/lease_adapter.go:63`：
 * 字段为 session_id / holder / holder_kind / epoch / active / expires_at / heartbeat_at。
 *
 * 语义铁律（后端注释明写）：**无租约不是 404**——`active=true / epoch=0 / holder=''`
 * 表示「无跨端写者，服务端本地可写」。故此处不把空 holder 当异常，也不伪造写者。
 * 读取失败一律原样透传（行内红字 + toast），绝不兜底成假租约。
 *
 * 展示为单行紧凑布局以适配移动端：仅保留「写者身份 + 状态」；
 * epoch / 心跳 / 到期等排障细节在 sm 及以上（≥640px）才显示，窄屏不换行不溢出。
 */
export function SessionLeaseBar() {
  const currentId = useSessions((s) => s.currentId)
  const toast = useUI((s) => s.toast)
  const [lease, setLease] = useState<SessionLeaseDTO | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  const load = useCallback(
    async (silent = false) => {
      if (!currentId) {
        setLease(null)
        setError(null)
        return
      }
      if (!silent) setLoading(true)
      try {
        const view = await api.getSessionLease(currentId)
        setLease(view)
        setError(null)
      } catch (e: unknown) {
        // 不求兜底成假值：失败就如实展示错误
        const msg = e instanceof Error ? e.message : String(e)
        setLease(null)
        setError(msg)
        if (!silent) toast('error', `读取会话租约失败：${msg}`)
      } finally {
        if (!silent) setLoading(false)
      }
    },
    [currentId, toast],
  )

  useEffect(() => {
    void load()
  }, [load])

  useEffect(() => {
    if (!currentId) return
    const t = window.setInterval(() => void load(true), POLL_MS)
    return () => window.clearInterval(t)
  }, [currentId, load])

  if (!currentId) return null

  const isDevice = lease?.holder_kind === 'device' && Boolean(lease?.holder)
  const active = Boolean(lease?.active)

  return (
    <div className="mx-3 mt-2 rounded-lg border border-edge bg-panel2/30 px-2.5 py-1.5 text-[11px] flex items-center gap-2 min-w-0">
      {isDevice ? (
        <Smartphone size={12} className="text-accent shrink-0" />
      ) : active ? (
        <ShieldCheck size={12} className="text-ok shrink-0" />
      ) : (
        <ShieldAlert size={12} className="text-warn shrink-0" />
      )}

      {error ? (
        // 错误原样透传，不做美化或吞掉（窄屏省略号截断，完整信息见 toast）
        <span className="text-err font-mono truncate min-w-0 flex-1" title={error}>
          {error}
        </span>
      ) : !lease ? (
        <span className="text-inkfaint truncate min-w-0 flex-1">
          {loading ? '读取中…' : '暂无租约数据'}
        </span>
      ) : (
        <>
          <span
            className={cn('font-medium truncate min-w-0', active ? 'text-ink' : 'text-warn')}
            title={isDevice ? `设备 ${lease.holder}` : '服务端本地执行面'}
          >
            {isDevice ? `设备 ${lease.holder}` : '服务端本地'}
          </span>

          <Badge tone={active ? (isDevice ? 'blue' : 'green') : 'yellow'} className="shrink-0">
            {active ? (isDevice ? 'device' : 'server') : '空闲'}
          </Badge>

          {/* 排障细节：窄屏隐藏，避免换行撑高顶栏 */}
          {active && (lease.epoch > 0 || lease.heartbeat_at) && (
            <span className="hidden sm:inline text-inkfaint truncate">
              epoch <span className="font-mono text-inkdim">{lease.epoch}</span>
              {lease.heartbeat_at ? (
                <>
                  {' · '}心跳 <span className="font-mono text-inkdim">{fmtClock(lease.heartbeat_at)}</span>
                </>
              ) : null}
            </span>
          )}
        </>
      )}

      <button
        type="button"
        onClick={() => void load()}
        disabled={loading}
        title={loading ? '正在刷新会话写者' : '刷新会话写者'}
        aria-label="刷新会话写者"
        className="ml-auto shrink-0 w-6 h-6 rounded-md inline-flex items-center justify-center text-inkfaint hover:text-accent hover:bg-panel2 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
      >
        <RefreshCw size={11} className={cn(loading && 'animate-spin')} />
      </button>
    </div>
  )
}
