import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Check, Clock, Copy, Globe, KeyRound, Monitor, Server, Smartphone, Terminal, Trash2, Wifi, WifiOff } from 'lucide-react'
import { useDevices } from '../../stores/deviceStore'
import { useUI } from '../../stores/uiStore'
import { Badge, Button, Empty, cn, inputCls } from '../../components/ui'
import type { DeviceDTO, DeviceKind } from '../../lib/apiTypes'

const KIND_META: Record<DeviceKind, { label: string; icon: ReactNode }> = {
  desktop: { label: '桌面端', icon: <Monitor size={13} className="text-emerald-400 shrink-0" /> },
  tablet: { label: '平板', icon: <Smartphone size={13} className="text-purple-400 shrink-0" /> },
  mobile: { label: '移动端', icon: <Smartphone size={13} className="text-purple-400 shrink-0" /> },
  cli: { label: '命令行', icon: <Terminal size={13} className="text-sky-400 shrink-0" /> },
  browser: { label: '浏览器端', icon: <Globe size={13} className="text-accent shrink-0" /> },
}

function fmtAgo(t?: number) {
  if (!t) return '从未'
  const diff = Math.max(0, Date.now() - t)
  const m = Math.floor(diff / 60_000)
  if (m < 1) return '刚刚'
  if (m < 60) return `${m} 分钟前`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时前`
  return `${Math.floor(h / 24)} 天前`
}

function StatusBadge({ status }: { status: DeviceDTO['status'] }) {
  if (status === 'online') return <Badge tone="green">在线</Badge>
  if (status === 'revoked') return <Badge tone="red">已吊销</Badge>
  return <Badge tone="gray">离线</Badge>
}

/** 配对码倒计时（TTL 默认 120s；服务端一次性消费，过期即失效） */
function PairCodeCard() {
  const { pairCode, createPairCode, clearPairCode, pairTTLSec, error } = useDevices()
  const { toast } = useUI()
  const [hintName, setHintName] = useState('')
  const [hintKind, setHintKind] = useState<DeviceKind | ''>('')
  const [busy, setBusy] = useState(false)
  const [now, setNow] = useState(Date.now())

  useEffect(() => {
    if (!pairCode) return
    const t = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(t)
  }, [pairCode])

  const left = pairCode ? Math.max(0, Math.ceil((pairCode.expires_at - now) / 1000)) : 0
  const expired = Boolean(pairCode) && left <= 0

  const onIssue = async () => {
    setBusy(true)
    const code = await createPairCode({ name: hintName.trim() || undefined, kind: hintKind || undefined })
    setBusy(false)
    if (code) {
      setNow(Date.now())
      toast('success', `配对码已生成，${code.ttl_s}s 内有效`)
    } else {
      toast('error', '生成配对码失败')
    }
  }

  const copy = async () => {
    if (!pairCode) return
    try {
      await navigator.clipboard.writeText(pairCode.code)
      toast('success', '配对码已复制')
    } catch {
      toast('info', `配对码：${pairCode.code}`)
    }
  }

  return (
    <div className="rounded-xl border border-edge bg-panel2/40 p-3.5 space-y-3">
      <div className="flex items-start justify-between gap-3 flex-wrap">
        <div>
          <div className="text-[13px] font-medium flex items-center gap-1.5">
            <KeyRound size={13} className="text-accent shrink-0" />
            添加设备（一次性配对码）
          </div>
          <div className="text-[12px] text-inkdim mt-1 leading-relaxed max-w-[560px]">
            在端侧（桌面端 / 移动端 / CLI）输入 6 位配对码即完成接入。配对码
            <span className="text-ink"> {pairTTLSec}s </span>内有效、只能用一次；服务端只保存设备令牌的摘要，明文仅在配对响应中出现一次。
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <Button variant="primary" onClick={onIssue} disabled={busy}>
            {busy ? '生成中…' : pairCode ? '重新生成' : '生成配对码'}
          </Button>
        </div>
      </div>

      <div className="flex flex-col sm:flex-row gap-2">
        <input
          className={cn(inputCls, 'sm:max-w-[220px]')}
          placeholder="预置设备名（可选，端侧可覆盖）"
          value={hintName}
          onChange={(e) => setHintName(e.target.value)}
        />
        <select
          className={cn(inputCls, 'sm:max-w-[140px]')}
          value={hintKind}
          onChange={(e) => setHintKind(e.target.value as DeviceKind | '')}
        >
          <option value="">形态：不指定</option>
          {(Object.keys(KIND_META) as DeviceKind[]).map((k) => (
            <option key={k} value={k}>{KIND_META[k].label}</option>
          ))}
        </select>
      </div>

      {pairCode && (
        <div className="flex items-center gap-3 flex-wrap">
          <span className={cn(
            'font-mono text-[22px] tracking-[0.18em] font-semibold select-all',
            expired ? 'text-inkfaint line-through' : 'text-accent',
          )}>
            {pairCode.code}
          </span>
          <span className={cn('inline-flex items-center gap-1 text-[11.5px]', expired ? 'text-err' : 'text-inkdim')}>
            <Clock size={11} className="shrink-0" />
            {expired ? '已过期，请重新生成' : `${left}s 后失效`}
          </span>
          <Button variant="ghost" onClick={copy} disabled={expired}>
            <Copy size={12} className="mr-1" />复制
          </Button>
          <Button variant="ghost" onClick={clearPairCode}>收起</Button>
        </div>
      )}

      {error && <div className="text-[12px] text-err leading-relaxed">{error}</div>}
    </div>
  )
}

function DeviceCard({ d }: { d: DeviceDTO }) {
  const { revoke, heartbeatSec, offlineAfterSec } = useDevices()
  const { toast } = useUI()
  const [confirming, setConfirming] = useState(false)
  const [openCaps, setOpenCaps] = useState(false)
  const [busy, setBusy] = useState(false)
  const meta = KIND_META[d.kind] ?? { label: d.kind, icon: <Server size={13} className="text-inkdim shrink-0" /> }
  const revoked = d.status === 'revoked'
  const caps = d.capabilities ?? []

  const onRevoke = async () => {
    if (!confirming) {
      setConfirming(true)
      toast('info', `再次点击「确认吊销」以吊销「${d.name}」`)
      window.setTimeout(() => setConfirming(false), 8000)
      return
    }
    setBusy(true)
    const ok = await revoke(d.id)
    setBusy(false)
    setConfirming(false)
    toast(ok ? 'success' : 'error', ok ? `已吊销「${d.name}」，令牌立即失效并断开长连接` : '吊销失败')
  }

  return (
    <div className={cn(
      'rounded-xl border p-3 flex flex-col gap-2 transition-colors',
      revoked ? 'border-edge/50 opacity-70' : 'border-edge hover:border-edge2 bg-panel2/20',
    )}>
      <div className="flex items-start gap-2.5">
        <div className="w-7 h-7 rounded-lg bg-panel2 flex items-center justify-center shrink-0">{meta.icon}</div>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-1.5 flex-wrap">
            <span className="text-[13px] font-semibold text-ink truncate">{d.name || d.id}</span>
            <StatusBadge status={d.status} />
            <span className="px-1 py-[1px] rounded bg-panel3 text-[9.5px] font-mono text-inkdim">{meta.label}</span>
            {d.platform && <span className="px-1 py-[1px] rounded bg-panel3 text-[9.5px] font-mono text-inkdim">{d.platform}</span>}
          </div>
          <div className="flex items-center gap-2 mt-1 text-[11px] text-inkfaint flex-wrap">
            <span className="inline-flex items-center gap-1">
              {d.status === 'online' ? <Wifi size={10} className="text-ok shrink-0" /> : <WifiOff size={10} className="shrink-0" />}
              最后心跳 {fmtAgo(d.last_seen_at)}
            </span>
            <span>· 协议 v{d.protocol_version ?? '-'}</span>
            {d.app_version && <span>· 应用 {d.app_version}</span>}
            <span>· 能力 {caps.length} 项</span>
          </div>
        </div>
        <div className="flex items-center gap-1.5 shrink-0">
          {caps.length > 0 && (
            <Button variant="ghost" onClick={() => setOpenCaps((v) => !v)}>
              {openCaps ? '收起能力' : '能力'}
            </Button>
          )}
          <Button variant="danger" onClick={onRevoke} disabled={revoked || busy}>
            <Trash2 size={12} className="mr-1" />
            {revoked ? '已吊销' : confirming ? '确认吊销' : '吊销'}
          </Button>
        </div>
      </div>

      {openCaps && (
        <div className="flex flex-wrap gap-1.5 pl-10 pt-0.5">
          {caps.map((c) => (
            <span
              key={`${c.device_id ?? d.id}:${c.name}`}
              className="px-1.5 py-[1px] rounded border border-edge bg-panel2 text-[10.5px] font-mono text-inkdim"
            >
              {c.name}{c.version ? `@${c.version}` : ''}
            </span>
          ))}
        </div>
      )}

      <div className="text-[10.5px] text-inkfaint pl-10">
        配对 <span className="font-mono">{fmtAgo(d.paired_at)}</span>
        {' · '}
        心跳间隔 {heartbeatSec}s / 超 {offlineAfterSec}s 无帧判离线
      </div>
    </div>
  )
}

/**
 * 设备面板（阶段 C-8）：设备列表 / 配对码签发 / 吊销 / 能力查看。
 *
 * 与后端契约一一对应（api/openapi.yaml 设备段）：
 * `GET /devices`、`POST /devices/pair-codes`、`DELETE /devices/{did}`。
 * 设备长连接（/devices/ws）与租约（/sessions/{sid}/lease）不在此面板操作：
 * 前者由端侧 Runner 持有，后者随会话上下文展示。
 */
export function DevicePanel() {
  const { devices, loadDevices, loading, heartbeatSec, offlineAfterSec } = useDevices()

  useEffect(() => {
    void loadDevices()
    // 列表随面板打开刷新一次即可；在线状态由端侧心跳驱动，不做前端轮询
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const stats = useMemo(() => {
    const online = devices.filter((d) => d.status === 'online').length
    const offline = devices.filter((d) => d.status === 'offline').length
    const revoked = devices.filter((d) => d.status === 'revoked').length
    return { online, offline, revoked }
  }, [devices])

  return (
    <div className="space-y-3">
      <div className="rounded-xl border border-edge bg-panel2/50 p-3 text-[12px] text-inkfaint leading-relaxed space-y-1">
        <div>· 服务端是「大脑」，设备是「肢体」：工具执行可派发到在线设备，端侧缺席时自动降级为服务端本地执行，不报错。</div>
        <div>· 端侧结果一律标记为<span className="text-inkdim">不可信来源</span>（`source_trust=untrusted`），最终消息与事件仍以服务端落库为准。</div>
        <div>· 每个会话同时只允许<span className="text-inkdim">一个写者</span>：新设备接管会使 epoch +1，旧端收到 `lease.revoked`。</div>
      </div>

      <PairCodeCard />

      <div className="flex items-center justify-between gap-2 flex-wrap">
        <div className="flex items-center gap-2 text-[12px] text-inkdim">
          <Server size={13} className="text-inkdim shrink-0" />
          <span>已配对设备</span>
          <Badge tone="green">在线 {stats.online}</Badge>
          <Badge tone="gray">离线 {stats.offline}</Badge>
          {stats.revoked > 0 && <Badge tone="red">吊销 {stats.revoked}</Badge>}
        </div>
        <Button variant="ghost" onClick={() => void loadDevices()} disabled={loading}>
          {loading ? '刷新中…' : '刷新'}
        </Button>
      </div>

      {devices.length === 0 ? (
        <Empty
          icon={<Monitor size={22} className="text-inkdim" />}
          title="暂无已配对设备"
          hint="生成配对码后，在端侧输入即可接入；未接入时所有工具仍在服务端本地执行。"
        />
      ) : (
        <div className="space-y-2">
          {devices.map((d) => <DeviceCard key={d.id} d={d} />)}
        </div>
      )}

      <div className="flex items-center gap-1.5 text-[11px] text-inkfaint">
        <Check size={11} className="text-ok shrink-0" />
        心跳 {heartbeatSec}s / 离线判定 {offlineAfterSec}s（服务端下发，前端只做展示）
      </div>
    </div>
  )
}
