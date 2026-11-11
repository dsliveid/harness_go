import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Monitor, Smartphone, Globe, Server, Wifi, WifiOff, Trash2, Lock, Link2, Link2Off, AlertTriangle } from 'lucide-react'
import { useSessions, wsOfProject, wsOfSession } from '../../stores/sessionStore'
import { useDevices } from '../../stores/deviceStore'
import { useUI } from '../../stores/uiStore'
import { Button, Empty, Modal, cn } from '../../components/ui'
import type { Workspace, WorkspaceKind } from '../../types'

const KIND_META: Record<WorkspaceKind, { label: string; icon: ReactNode }> = {
  server: { label: '服务端', icon: <Server size={13} className="text-sky-400 shrink-0" /> },
  desktop: { label: '桌面端', icon: <Monitor size={13} className="text-emerald-400 shrink-0" /> },
  mobile: { label: '移动端', icon: <Smartphone size={13} className="text-purple-400 shrink-0" /> },
  browser: { label: '浏览器端', icon: <Globe size={13} className="text-accent shrink-0" /> },
}

/** 设备形态 → 工作空间类型的对应关系（与后端 workspaceKindForDevice 逐字一致）。 */
function kindForDeviceBody(kind: string): WorkspaceKind | null {
  switch (kind) {
    case 'desktop': return 'desktop'
    case 'mobile': return 'mobile'
    case 'browser': return 'browser'
    default: return null // tablet / cli：无对应 WorkspaceKind，不参与绑定
  }
}

function fmtAgo(t?: number) {
  if (!t) return '未知'
  const diff = Math.max(0, Date.now() - t)
  const m = Math.floor(diff / 60_000)
  if (m < 1) return '刚刚'
  if (m < 60) return `${m} 分钟前`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时前`
  return `${Math.floor(h / 24)} 天前`
}

/**
 * 工作空间管理面板（高级 → 工作空间）。
 *
 * 本面板的职责边界（2026-11 定案）：
 *   - **只做归属管理**（绑定 / 解绑 / 移除），不做「切换」——切换是「当前操作哪个空间」，
 *     已由主界面下拉承担，两处并存会让用户分不清「切换」到底改了归属还是改了视图。
 *   - 绑定呈严格一对一：一台设备只能被一个空间占用（服务端 409 兜底）。
 *   - 已绑定设备的空间**不可移除**；必须先解绑。
 *
 * 🚨 死锁逃生（最关键）：设备被吊销或物理删除后，空间仍指向该设备。
 * 此时若不显示「解绑」按钮，该空间既因已绑定而删不掉、又因设备不存在而解不开，
 * 变成永久僵尸。因此解绑按钮的显示条件是「deviceId 非空」，**与设备是否存活无关**。
 */
export function WorkspacePanel() {
  const {
    workspaces, projects, sessions, activeWorkspaceId, removeWorkspace,
    bindWorkspaceDevice, unbindWorkspaceDevice,
  } = useSessions()
  const { devices, loadDevices } = useDevices()
  const { toast } = useUI()
  // 单次点击即弹出确认对话框（与设备面板「解除绑定」同一交互范式）。
  const [confirmWs, setConfirmWs] = useState<{ id: string; name: string } | null>(null)
  const [unbindWs, setUnbindWs] = useState<Workspace | null>(null)
  const [bindWs, setBindWs] = useState<Workspace | null>(null)
  const [busy, setBusy] = useState(false)

  // 候选设备需要全量设备列表（配对面板可能还没加载过）。
  useEffect(() => { void loadDevices() }, [loadDevices])

  /** 已被任何工作空间占用的设备 ID 集合：一对一约束下的候选集排除依据。 */
  const occupiedDeviceIds = useMemo(
    () => new Set(workspaces.map((w) => w.deviceId).filter((id): id is string => Boolean(id))),
    [workspaces],
  )

  /**
   * 候选设备 = 未被占用 + 形态与空间 kind 匹配。
   *
   * 前端先筛是为了「不让用户点了才报错」；服务端仍然会二次校验（409/400 兜底），
   * 前端筛选**不是**安全边界。
   */
  const bindCandidates = useMemo(() => {
    if (!bindWs) return []
    return devices.filter((d) => {
      if (occupiedDeviceIds.has(d.id)) return false
      return kindForDeviceBody(String(d.kind)) === bindWs.kind
    })
  }, [bindWs, devices, occupiedDeviceIds])

  const confirmStats = confirmWs
    ? {
        pCount: projects.filter((p) => wsOfProject(p) === confirmWs.id).length,
        sCount: sessions.filter((s) => wsOfSession(s, projects) === confirmWs.id).length,
      }
    : { pCount: 0, sCount: 0 }

  const unbindStats = unbindWs
    ? sessions.filter((s) => wsOfSession(s, projects) === unbindWs.id).length
    : 0

  /** 确认移除：真调服务端 DELETE（migrate_refs=true）。失败原样提示，不做假成功。 */
  const onConfirmRemove = async () => {
    if (!confirmWs) return
    const { id, name } = confirmWs
    setBusy(true)
    try {
      const ok = await removeWorkspace(id)
      toast(ok ? 'success' : 'error', ok
        ? `已移除「${name}」`
        : '内置服务端空间不可移除')
      if (ok) setConfirmWs(null)
    } catch (e) {
      // 409（仍绑定设备 / 仍被引用）/ 404 / 网络错误：把服务端 message 原样抛给用户
      const msg = e instanceof Error && e.message ? e.message : '移除工作空间失败'
      toast('error', msg)
    } finally {
      setBusy(false)
    }
  }

  const onConfirmUnbind = async () => {
    if (!unbindWs) return
    setBusy(true)
    try {
      await unbindWorkspaceDevice(unbindWs.id)
      toast('success', `「${unbindWs.name}」已解除设备归属`)
      setUnbindWs(null)
    } catch (e) {
      toast('error', e instanceof Error && e.message ? e.message : '解绑失败')
    } finally {
      setBusy(false)
    }
  }

  const onPickDevice = async (deviceId: string) => {
    if (!bindWs) return
    setBusy(true)
    try {
      await bindWorkspaceDevice(bindWs.id, deviceId)
      toast('success', `「${bindWs.name}」已绑定设备`)
      setBindWs(null)
    } catch (e) {
      toast('error', e instanceof Error && e.message ? e.message : '绑定失败')
    } finally {
      setBusy(false)
    }
  }

  if (workspaces.length === 0) {
    return <Empty icon={<Server size={22} className="text-inkdim" />} title="暂无工作空间" hint="服务端内置空间应始终存在" />
  }

  return (
    <div className="space-y-3">
      <div className="rounded-xl border border-edge bg-panel2/50 p-3 text-[12px] text-inkfaint leading-relaxed space-y-1">
        <div>· 工作空间与设备<b className="text-inkdim">严格一对一</b>：一台设备只能被一个空间占用。</div>
        <div>· 配对设备时服务端会<b className="text-inkdim">自动创建</b>该设备的工作空间。</div>
        <div>· <span className="text-inkdim">服务端工作空间</span>为内置空间，执行面恒为服务端本身，不可绑定/解绑设备、不可移除。</div>
        <div>· 已绑定设备的空间<b className="text-inkdim">不可移除</b>，需先解绑归属。</div>
        <div>· 解绑后该空间仍可对话，但<b className="text-inkdim">失去工具执行能力</b>。</div>
      </div>

      <div className="space-y-2">
        {workspaces.map((w) => {
          const isActive = w.id === activeWorkspaceId
          const pCount = projects.filter((p) => wsOfProject(p) === w.id).length
          const sCount = sessions.filter((s) => wsOfSession(s, projects) === w.id).length
          const meta = KIND_META[w.kind]
          // deviceId 非空 = 有归属。注意这里**不看**设备是否存活（死锁逃生口）。
          const hasDevice = Boolean(w.deviceId)
          const isBuiltin = Boolean(w.builtin)
          // 已绑定 → 不可删除。这一条与后端 Delete 校验严格一致。
          const canRemove = !isBuiltin && !hasDevice
          return (
            <div
              key={w.id}
              className={cn(
                'rounded-xl border bg-panel p-3 shadow-subtle transition-all',
                isActive ? 'border-accent/50' : 'border-edge hover:border-edge2',
              )}
            >
              <div className="flex items-start gap-2.5">
                <div className="w-7 h-7 rounded-lg bg-panel2 flex items-center justify-center shrink-0 mt-0.5">
                  {meta.icon}
                </div>

                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-1.5 min-w-0">
                    <span className="text-[13px] font-semibold text-ink truncate">{w.name}</span>
                    {isBuiltin && (
                      <span className="shrink-0 inline-flex items-center gap-0.5 px-1 py-[1px] rounded bg-panel3 text-[9.5px] font-mono text-inkdim">
                        <Lock size={8} />内置
                      </span>
                    )}
                    {isActive && (
                      <span className="shrink-0 inline-flex items-center gap-0.5 px-1 py-[1px] rounded bg-accent-soft text-[9.5px] font-medium text-accent">
                        当前
                      </span>
                    )}
                  </div>

                  {/* 第一行：空间类型 · 在线状态 · 项目/对话计数 */}
                  <div className="flex items-center gap-1.5 mt-0.5 text-[11px] text-inkfaint flex-wrap">
                    <span className="font-mono">{meta.label}</span>
                    <span className="opacity-40">·</span>
                    {w.online ? (
                      <span className="inline-flex items-center gap-0.5 text-ok/90">
                        <Wifi size={9} />在线
                      </span>
                    ) : (
                      <span className="inline-flex items-center gap-0.5">
                        <WifiOff size={9} />离线 · {fmtAgo(w.lastSeenAt)}活跃
                      </span>
                    )}
                    <span className="opacity-40">·</span>
                    <span>{pCount} 项目 / {sCount} 对话</span>
                  </div>

                  {/* 第二行：设备绑定情况（本面板的核心信息） */}
                  <DeviceBindingLine w={w} isBuiltin={isBuiltin} />
                </div>

                <div className="flex items-center gap-1.5 shrink-0">
                  {/* 绑定 / 解绑：二者互斥呈现，内置空间两者都不显示 */}
                  {!isBuiltin && !hasDevice && (
                    <Button
                      size="sm"
                      variant="outline"
                      className="h-7 px-2 text-[11px] gap-1"
                      onClick={() => setBindWs(w)}
                    >
                      <Link2 size={11} />
                      绑定设备
                    </Button>
                  )}
                  {!isBuiltin && hasDevice && (
                    <Button
                      size="sm"
                      variant="outline"
                      className="h-7 px-2 text-[11px] gap-1"
                      onClick={() => setUnbindWs(w)}
                      title="解除该空间与设备的归属关系"
                    >
                      <Link2Off size={11} />
                      解绑归属
                    </Button>
                  )}
                  {isBuiltin ? (
                    <span className="text-[10.5px] text-inkfaint px-1" title="内置空间不可移除">不可移除</span>
                  ) : (
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-7 px-2 text-[11px] gap-1 text-inkdim hover:text-err disabled:opacity-40 disabled:hover:text-inkdim"
                      disabled={!canRemove}
                      onClick={() => setConfirmWs({ id: w.id, name: w.name })}
                      title={canRemove ? '移除该工作空间' : '已绑定设备，请先解绑归属'}
                    >
                      <Trash2 size={11} />
                      移除
                    </Button>
                  )}
                </div>
              </div>
            </div>
          )
        })}
      </div>

      {/* ---- 移除确认 ---- */}
      <Modal
        open={!!confirmWs}
        onClose={() => { if (!busy) setConfirmWs(null) }}
        title="移除工作空间"
        width="max-w-md"
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmWs(null)} disabled={busy}>取消</Button>
            <Button variant="danger" onClick={() => void onConfirmRemove()} disabled={busy}>
              <Trash2 size={12} className="mr-1" />
              {busy ? '移除中…' : '确认移除'}
            </Button>
          </>
        }
      >
        <div className="p-4 space-y-2 text-[12.5px] leading-relaxed text-inkdim">
          <div>
            确认移除「<span className="text-ink font-medium">{confirmWs?.name}</span>」？
          </div>
          <div className="text-inkfaint">
            · 该空间关联 {confirmStats.pCount} 个项目 / {confirmStats.sCount} 个对话，将<span className="text-ink">解除对该空间的归属</span>（对话与项目本身保留，但会失去该设备的执行能力）；
          </div>
          <div className="text-inkfaint">
            · 工作空间本身会从服务端<b className="text-ink">彻底删除</b>，刷新后不会再出现；
          </div>
          <div className="text-inkfaint">
            · 需重新配对该设备才能再获得其执行能力。
          </div>
        </div>
      </Modal>

      {/* ---- 解绑确认 ---- */}
      <Modal
        open={!!unbindWs}
        onClose={() => { if (!busy) setUnbindWs(null) }}
        title="解除设备归属"
        width="max-w-md"
        footer={
          <>
            <Button variant="ghost" onClick={() => setUnbindWs(null)} disabled={busy}>取消</Button>
            <Button variant="primary" onClick={() => void onConfirmUnbind()} disabled={busy}>
              <Link2Off size={12} className="mr-1" />
              {busy ? '解绑中…' : '确认解绑'}
            </Button>
          </>
        }
      >
        <div className="p-4 space-y-2 text-[12.5px] leading-relaxed text-inkdim">
          <div>
            确认把「<span className="text-ink font-medium">{unbindWs?.name}</span>」从
            <span className="text-ink font-medium"> {unbindWs?.deviceName || unbindWs?.deviceId || '该设备'}</span> 上解绑？
          </div>
          <div className="text-inkfaint">
            · 解绑后该空间<b className="text-ink">保留</b>，但不再拥有设备执行能力；
          </div>
          <div className="text-inkfaint">
            · 该空间下 <b className="text-ink">{unbindStats}</b> 个对话将<span className="text-ink">失去工具能力</span>（仍可正常对话与浏览历史）；
          </div>
          <div className="text-inkfaint">
            · 解绑后即可移除该空间，也可绑定到其他同类设备。
          </div>
          {unbindWs?.deviceRevoked || unbindWs?.deviceMissing ? (
            <div className="flex items-start gap-1.5 rounded-lg border border-warn/30 bg-warn/10 px-2.5 py-2 text-[11.5px] text-warn">
              <AlertTriangle size={12} className="shrink-0 mt-0.5" />
              <span>
                该设备已{unbindWs?.deviceMissing ? '被删除' : '解绑'}。解绑归属后即可移除本工作空间。
              </span>
            </div>
          ) : null}
        </div>
      </Modal>

      {/* ---- 绑定设备 ---- */}
      <Modal
        open={!!bindWs}
        onClose={() => { if (!busy) setBindWs(null) }}
        title="绑定设备"
        width="max-w-md"
        footer={<Button variant="ghost" onClick={() => setBindWs(null)} disabled={busy}>取消</Button>}
      >
        <div className="p-4 space-y-2 text-[12.5px] leading-relaxed text-inkdim">
          <div className="text-inkfaint">
            为「<span className="text-ink font-medium">{bindWs?.name}</span>」选择一台
            <span className="font-mono"> {KIND_META[bindWs?.kind ?? 'server'].label} </span>
            设备。仅列出<b className="text-ink">未被其他空间占用</b>且形态匹配的设备。
          </div>
          {bindCandidates.length === 0 ? (
            <div className="rounded-lg border border-edge bg-panel2/50 px-3 py-4 text-center text-[11.5px] text-inkfaint">
              没有可绑定的设备。请先在「设备」面板配对一台
              <span className="font-mono"> {KIND_META[bindWs?.kind ?? 'server'].label} </span>
              设备，或先解绑占用中的设备。
            </div>
          ) : (
            <div className="space-y-1.5 max-h-[260px] overflow-y-auto">
              {bindCandidates.map((d) => (
                <button
                  key={d.id}
                  disabled={busy}
                  onClick={() => void onPickDevice(d.id)}
                  className="w-full text-left rounded-lg border border-edge bg-panel px-2.5 py-2 hover:border-accent/50 hover:bg-panel2 transition-colors disabled:opacity-50"
                >
                  <div className="flex items-center gap-1.5">
                    <span className={cn(
                      'w-1.5 h-1.5 rounded-full shrink-0',
                      d.status === 'online' ? 'bg-ok' : 'bg-inkfaint/60',
                    )} />
                    <span className="text-[12.5px] font-medium text-ink truncate">{d.name || d.id}</span>
                    <span className="text-[10.5px] text-inkfaint font-mono">{d.kind}</span>
                  </div>
                  <div className="mt-0.5 pl-3 text-[10.5px] text-inkfaint font-mono truncate">{d.id}</div>
                </button>
              ))}
            </div>
          )}
        </div>
      </Modal>
    </div>
  )
}

/**
 * 设备绑定情况展示行：把「内置 / 已绑定 / 未绑定 / 设备已删除 / 设备已解绑」五种状态分开。
 *
 * 🚨 离线（可恢复）与已删除/已解绑（需重新配对）绝不能用同一句话，
 * 否则用户会一直等一台永远不会回来的设备。
 */
function DeviceBindingLine({ w, isBuiltin }: { w: Workspace; isBuiltin: boolean }) {
  const base = 'flex items-center gap-1 mt-1 text-[11px] flex-wrap'

  if (isBuiltin) {
    return (
      <div className={cn(base, 'text-inkfaint')}>
        <Link2 size={9} />
        <span>执行面：<span className="text-inkdim">服务端本身</span>（不可绑定设备）</span>
      </div>
    )
  }
  if (w.deviceMissing) {
    return (
      <div className={cn(base, 'text-danger/90')}>
        <AlertTriangle size={9} />
        <span>绑定的设备已被删除 — 需解绑归属后重新配对</span>
      </div>
    )
  }
  if (w.deviceRevoked) {
    return (
      <div className={cn(base, 'text-danger/90')}>
        <AlertTriangle size={9} />
        <span>绑定的设备已解绑 — 需解绑归属后重新配对</span>
      </div>
    )
  }
  if (!w.deviceId) {
    return (
      <div className={cn(base, 'text-inkfaint')}>
        <Link2Off size={9} />
        <span>未绑定设备 — 仅可对话，无工具能力</span>
      </div>
    )
  }
  return (
    <div className={cn(base, 'text-inkfaint')}>
      <Link2 size={9} />
      <span>绑定设备：</span>
      <span className="text-inkdim font-medium">{w.deviceName || w.deviceId}</span>
      {w.deviceKind && <span className="font-mono opacity-70">· {w.deviceKind}</span>}
      <span className={cn('inline-flex items-center gap-0.5', w.online ? 'text-ok/90' : 'opacity-70')}>
        · {w.online ? <><Wifi size={8} />在线</> : <><WifiOff size={8} />离线</>}
      </span>
      {w.toolPolicy === 'readonly' && <span className="text-warn/90">· 仅只读</span>}
    </div>
  )
}
