import { useCallback, useEffect, useState } from 'react'
import { AlertCircle, ChevronDown, ChevronRight, RotateCcw } from 'lucide-react'
import { api } from '../../lib/api'
import type { Rollback, Snapshot } from '../../lib/apiTypes'
import { useSessions } from '../../stores/sessionStore'
import { useUI } from '../../stores/uiStore'
import { Badge, Button, cn } from '../../components/ui'

/**
 * 回滚历史与快照锚点（P1 真接面板）。
 *
 * 数据来源（均为后端真实端点，无任何假数据）：
 *   - GET /api/v1/sessions/{sid}/rollbacks   → { items: Rollback[] }（时间倒序）
 *   - GET /api/v1/sessions/{sid}/snapshots   → { items: Snapshot[] }（时间倒序，上限 200）
 *
 * 语义（docs/基础服务设计.md S10）：
 *   - revert 是物理写回：每个路径恢复为受影响工具事件中「最早」的 before 快照；
 *   - reapply 恢复「最晚」的 after 快照，并恢复被置灰的消息；不可重复执行（后端 409）；
 *   - 回滚点之后的消息「置灰但保留」（hidden_from_seq）。
 */

const MODE_LABEL: Record<Rollback['mode'], string> = {
  tool: '工具级',
  turn: '轮次级',
  message: '消息级',
}

const SNAP_KIND_LABEL: Record<Snapshot['kind'], string> = {
  before: '写入前',
  after: '写入后',
  checkpoint: '检查点',
}

const MODE_MINOR: Record<Rollback['mode'], string> = {
  tool: '工具级回滚',
  turn: '轮次级回滚',
  message: '消息级回滚',
}

function fmtTime(ms?: number) {
  if (!ms) return '—'
  const d = new Date(ms)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

function fmtSize(n?: number) {
  if (!n || n <= 0) return '0 B'
  if (n > 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  if (n > 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${n} B`
}

function errText(e: unknown) {
  if (e instanceof Error && e.message) return e.message
  return String(e)
}

export function RollbackHistoryPanel() {
  const currentId = useSessions((s) => s.currentId)
  const toast = useUI((s) => s.toast)

  const [rollbacks, setRollbacks] = useState<Rollback[]>([])
  const [snapshots, setSnapshots] = useState<Snapshot[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [openId, setOpenId] = useState<string | null>(null)
  const [reapplyingId, setReapplyingId] = useState<string | null>(null)

  const load = useCallback(
    async (sessionId: string) => {
      setLoading(true)
      setError(null)
      try {
        const [rbs, snaps] = await Promise.all([
          api.listRollbacks(sessionId),
          api.listSessionSnapshots(sessionId),
        ])
        setRollbacks(rbs)
        setSnapshots(snaps)
      } catch (e) {
        // 失败保留真实错误文案，绝不回填假数据
        setRollbacks([])
        setSnapshots([])
        setError(errText(e))
      } finally {
        setLoading(false)
      }
    },
    [],
  )

  useEffect(() => {
    if (!currentId) {
      setRollbacks([])
      setSnapshots([])
      setError(null)
      setOpenId(null)
      return
    }
    setOpenId(null)
    void load(currentId)
  }, [currentId, load])

  // 未选会话：空态，不渲染任何列表行
  if (!currentId) {
    return (
      <div className="space-y-3">
        <div className="rounded-lg border border-edge bg-panel2/50 p-3 text-[12px] text-inkfaint leading-relaxed">
          请先在左侧选择一个会话，此处将展示该会话的回滚历史与快照锚点。
        </div>
      </div>
    )
  }

  const doReapply = async (rid: string) => {
    setReapplyingId(rid)
    try {
      // 复用既有方法（严禁在此重复实现请求）
      await api.reapplyRollback(currentId, rid)
      toast('success', '已重新应用（恢复 after 快照 + 恢复置灰消息）')
      await load(currentId)
    } catch (e) {
      toast('warn', `重新应用失败：${errText(e)}`)
    } finally {
      setReapplyingId(null)
    }
  }

  return (
    <div className="space-y-3">
      <div className="rounded-lg border border-edge bg-panel2/50 p-2.5 text-[11px] text-inkfaint space-y-0.5">
        <div>· 回滚为物理写回：每个路径恢复受影响工具事件中「最早」的 before 快照</div>
        <div>· 重新应用恢复「最晚」的 after 快照，并恢复被置灰消息；每次回滚仅可重放一次</div>
        <div>· 回滚点之后的消息置灰但保留；run_command 变更不在快照覆盖范围内</div>
      </div>

      {error && (
        <div className="rounded-lg border border-err/40 bg-err/[0.07] px-3 py-2 text-[12px] text-err font-mono flex items-start gap-2">
          <AlertCircle size={13} className="mt-0.5 shrink-0" />
          <span className="break-all">加载失败：{error}</span>
        </div>
      )}

      {loading && !error && (
        <div className="text-[12px] text-inkfaint px-1">加载中…</div>
      )}

      {/* ---------------- 回滚历史 ---------------- */}
      <section className="space-y-2">
        <div className="flex items-center gap-2 px-1">
          <span className="text-[12px] font-semibold text-inkdim tracking-tight">回滚历史</span>
          <Badge tone="gray">{rollbacks.length}</Badge>
        </div>

        {!loading && !error && rollbacks.length === 0 && (
          <div className="rounded-lg border border-edge bg-panel2/40 p-3 text-[12px] text-inkfaint">
            该会话暂无可展示的回滚记录。
          </div>
        )}

        {rollbacks.map((rb) => {
          const open = openId === rb.id
          const affected = rb.affected ?? []
          const restored = rb.restored ?? []
          const reapplied = Boolean(rb.reapplied_at)
          return (
            <div key={rb.id} className="rounded-lg border border-edge overflow-hidden">
              <button
                className="w-full flex items-center gap-2 px-2.5 py-2 text-left hover:bg-panel2"
                onClick={() => setOpenId(open ? null : rb.id)}
              >
                <span className="w-3.5 h-3.5 flex items-center justify-center shrink-0">
                  {open ? (
                    <ChevronDown size={12} className="text-inkfaint" />
                  ) : (
                    <ChevronRight size={12} className="text-inkfaint" />
                  )}
                </span>
                <span className="font-mono text-[12px] truncate shrink-0">{rb.id}</span>
                <Badge tone={rb.mode === 'message' ? 'purple' : rb.mode === 'turn' ? 'cyan' : 'blue'}>
                  {MODE_LABEL[rb.mode] ?? rb.mode}
                </Badge>
                <span className="flex-1 truncate text-[11px] text-inkfaint text-right">
                  {fmtTime(rb.created_at)}
                </span>
                {reapplied ? <Badge tone="gray">已重放</Badge> : <Badge tone="green">可重放</Badge>}
              </button>

              <div className="px-2.5 pb-1 text-[11px] text-inkfaint truncate" title={rb.target}>
                {rb.mode === 'message' ? (
                  <>
                    目标消息序号 <span className="font-mono text-inkdim">{rb.target}</span>
                    {rb.hidden_from_seq ? (
                      <>
                        {' · '}自第 <span className="font-mono text-inkdim">{rb.hidden_from_seq}</span> 条起置灰
                      </>
                    ) : null}
                  </>
                ) : (
                  <>
                    目标 <span className="font-mono text-inkdim">{rb.target || '—'}</span>
                  </>
                )}
              </div>

              {open && (
                <div className="p-2 border-t border-edge space-y-2">
                  {/* 影响面：被变更项 */}
                  <div className="space-y-1">
                    <div className="text-[11px] font-semibold text-inkdim">受影响项（affected）· {affected.length}</div>
                    {affected.length === 0 ? (
                      <div className="text-[11px] text-inkfaint">无受影响工具事件（未涉及文件变更）。</div>
                    ) : (
                      affected.map((a, i) => (
                        <div key={`${a.tool_event_id}-${i}`} className="rounded-md border border-edge bg-panel2/40 px-2 py-1.5 space-y-0.5">
                          <div className="flex items-center gap-1.5 text-[11px]">
                            <span className="text-inkfaint shrink-0">工具事件</span>
                            <span className="font-mono text-inkdim truncate">{a.tool_event_id}</span>
                          </div>
                          {a.run_id && (
                            <div className="flex items-center gap-1.5 text-[11px]">
                              <span className="text-inkfaint shrink-0">Run</span>
                              <span className="font-mono text-inkfaint truncate">{a.run_id}</span>
                            </div>
                          )}
                          <div className="flex flex-wrap items-center gap-1.5 text-[11px]">
                            <span className="text-inkfaint">快照</span>
                            <span className="font-mono text-inkfaint">{a.snapshot_before || '（无 before）'}</span>
                            <span className="text-inkfaint">→</span>
                            <span className="font-mono text-inkfaint">{a.snapshot_after || '（无 after）'}</span>
                          </div>
                          {(a.paths ?? []).length > 0 && (
                            <div className="flex flex-wrap gap-1 pt-0.5">
                              {(a.paths ?? []).map((p) => (
                                <span key={p} className="font-mono text-[10px] px-1.5 py-0.5 rounded bg-panel2 text-inkfaint">
                                  {p}
                                </span>
                              ))}
                            </div>
                          )}
                        </div>
                      ))
                    )}
                  </div>

                  {/* 影响面：恢复项 */}
                  <div className="space-y-1">
                    <div className="text-[11px] font-semibold text-inkdim">已写回（restored）· {restored.length}</div>
                    {restored.length === 0 ? (
                      <div className="text-[11px] text-inkfaint">本次回滚未写回任何路径（可能仅置灰消息）。</div>
                    ) : (
                      restored.map((r, i) => (
                        <div
                          key={`${r.path}-${i}`}
                          className="flex items-center gap-2 rounded-md border border-edge bg-panel2/40 px-2 py-1 text-[11px]"
                        >
                          <Badge tone={r.action === 'deleted' ? 'red' : 'green'}>
                            {r.action === 'deleted' ? '已删除' : '已还原'}
                          </Badge>
                          <span className="font-mono text-inkdim truncate flex-1" title={r.path}>{r.path}</span>
                          <span className="text-inkfaint shrink-0">{fmtSize(r.bytes)}</span>
                        </div>
                      ))
                    )}
                  </div>

                  <div className="flex items-center gap-2 pt-0.5">
                    <Button
                      size="sm"
                      variant={reapplied ? 'outline' : 'primary'}
                      disabled={reapplied || reapplyingId === rb.id}
                      onClick={() => void doReapply(rb.id)}
                      className="gap-1"
                    >
                      <RotateCcw size={12} />
                      <span>{reapplyingId === rb.id ? '应用中…' : '重新应用'}</span>
                    </Button>
                    {reapplied && (
                      <span className="text-[11px] text-inkfaint">已于 {fmtTime(rb.reapplied_at ?? undefined)} 重放</span>
                    )}
                  </div>
                </div>
              )}
            </div>
          )
        })}
      </section>

      {/* ---------------- 快照锚点 ---------------- */}
      <section className="space-y-2">
        <div className="flex items-center gap-2 px-1">
          <span className="text-[12px] font-semibold text-inkdim tracking-tight">快照锚点</span>
          <Badge tone="gray">{snapshots.length}</Badge>
        </div>

        {!loading && !error && snapshots.length === 0 && (
          <div className="rounded-lg border border-edge bg-panel2/40 p-3 text-[12px] text-inkfaint">
            该会话暂无快照锚点（尚未发生文件写入，或写入路径不在快照覆盖范围）。
          </div>
        )}

        {snapshots.map((snap) => (
          <SnapshotRow key={snap.id} snap={snap} />
        ))}
      </section>
    </div>
  )
}

function SnapshotRow({ snap }: { snap: Snapshot }) {
  const [open, setOpen] = useState(false)
  const paths = snap.paths ?? []
  return (
    <div className="rounded-lg border border-edge overflow-hidden">
      <button
        className="w-full flex items-center gap-2 px-2.5 py-2 text-left hover:bg-panel2"
        onClick={() => setOpen(!open)}
      >
        <span className="w-3.5 h-3.5 flex items-center justify-center shrink-0">
          {open ? <ChevronDown size={12} className="text-inkfaint" /> : <ChevronRight size={12} className="text-inkfaint" />}
        </span>
        <Badge tone={snap.kind === 'before' ? 'blue' : snap.kind === 'after' ? 'green' : 'yellow'}>
          {SNAP_KIND_LABEL[snap.kind] ?? snap.kind}
        </Badge>
        <span className="font-mono text-[12px] truncate flex-1">{snap.id}</span>
        <span className="text-[11px] text-inkfaint shrink-0">{paths.length} 路径</span>
      </button>
      <div className="px-2.5 pb-1 text-[11px] text-inkfaint truncate">
        {fmtTime(snap.created_at)} · {fmtSize(snap.size)}
        {snap.run_id ? ` · run ${snap.run_id}` : ''}
        {snap.tool_event_id ? ` · 事件 ${snap.tool_event_id}` : ''}
      </div>
      {open && (
        <div className={cn('p-2 border-t border-edge space-y-1')}>
          {paths.length === 0 ? (
            <div className="text-[11px] text-inkfaint">该快照未绑定路径。</div>
          ) : (
            paths.map((b, i) => (
              <div key={`${b.path}-${i}`} className="flex items-center gap-2 text-[11px]">
                <span className="font-mono text-inkdim truncate flex-1" title={b.path}>{b.path}</span>
                {b.absent ? (
                  <Badge tone="gray">当时不存在</Badge>
                ) : (
                  <>
                    <span className="font-mono text-[10px] text-inkfaint shrink-0">{b.hash || '—'}</span>
                    <span className="text-inkfaint shrink-0">{fmtSize(b.size)}</span>
                  </>
                )}
              </div>
            ))
          )}
        </div>
      )}
    </div>
  )
}

/** 供侧栏徽标等处复用：把回滚模式转成中文短语（避免界面散落硬编码）。 */
export const rollbackModeLabel = (mode: Rollback['mode']) => MODE_MINOR[mode] ?? mode
