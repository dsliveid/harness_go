import { useEffect, useState } from 'react'
import { useUI } from '../../stores/uiStore'
import { useChat } from '../../stores/chatStore'
import { Button, Switch } from '../../components/ui'
import { promptInstall, usePwa } from '../../lib/pwa'
import { api } from '../../lib/api'
import { DataBackupPanel } from './DataBackupPanel'

/**
 * GET /maintenance 真实返回（已核实 internal/service/maintenance_service.go）：
 *   { protocol_version, sessions, open_session_dbs, running_runs, pending_approvals,
 *     main_db: { path, page_size, page_count, freelist_count, file_size_bytes, ... } }
 *
 * 接真能力边界（勿越界造假）：
 * - 数据库体积：✅ main_db.file_size_bytes
 * - 会话数：✅ sessions（消息总数后端不提供 → 一律展示「—」）
 * - 快照 blob 占用：❌ 后端无任何快照占用统计 → 只能「—」并注明未采集
 * - 库页数等：可选展示 page_count（真实值）
 */
type MaintenanceView = {
  protocolVersion?: string
  sessions?: number
  openSessionDbs?: number
  runningRuns?: number
  pendingApprovals?: number
  dbFilePath?: string
  dbSizeBytes?: number
  dbPageCount?: number
  dbFreelist?: number
}

const asNum = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined
const asStr = (v: unknown): string | undefined =>
  typeof v === 'string' && v.length > 0 ? v : undefined

function pickMaintenance(raw: Record<string, unknown>): MaintenanceView {
  const db = (raw.main_db ?? {}) as Record<string, unknown>
  return {
    protocolVersion: asStr(raw.protocol_version),
    sessions: asNum(raw.sessions),
    openSessionDbs: asNum(raw.open_session_dbs),
    runningRuns: asNum(raw.running_runs),
    pendingApprovals: asNum(raw.pending_approvals),
    dbFilePath: asStr(db.path),
    dbSizeBytes: asNum(db.file_size_bytes),
    dbPageCount: asNum(db.page_count),
    dbFreelist: asNum(db.freelist_count),
  }
}

/** 字节 → 人类可读；未取到就返回「—」，绝不凭空给一个数 */
const fmtBytes = (n?: number): string => {
  if (n === undefined) return '—'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`
  return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`
}
const fmtCount = (n?: number): string => (n === undefined ? '—' : String(n))

// 数据与存储面板：原设置弹窗「数据与存储」标签页迁入「高级」弹窗
export function DataPanel() {
  const { toast, setAdvanced } = useUI()
  const { pushDataApproval } = useChat()
  const pwa = usePwa()

  // 容量/计数全部来自 GET /maintenance 真实返回
  const [maint, setMaint] = useState<MaintenanceView | null>(null)
  const [maintErr, setMaintErr] = useState<string | null>(null)
  const [maintLoading, setMaintLoading] = useState(true)

  useEffect(() => {
    let alive = true
    api
      .getMaintenance()
      .then((raw) => {
        if (!alive) return
        setMaint(pickMaintenance(raw))
        setMaintErr(null)
      })
      .catch((e: unknown) => {
        // 如实展示失败，不兜底成看似正常的假容量
        if (alive) setMaintErr(e instanceof Error ? e.message : String(e))
      })
      .finally(() => {
        if (alive) setMaintLoading(false)
      })
    return () => {
      alive = false
    }
  }, [])

  // 后端不提供的能力（快照占用 / 消息总数）统一显示「—」并注明未接入，绝不展示"看起来真实"的写死值
  const notWired = '—'
  const dbValue = maintLoading
    ? '读取中…'
    : maintErr
      ? '读取失败'
      : fmtBytes(maint?.dbSizeBytes)
  const dbHint = maintErr
    ? 'GET /maintenance 失败'
    : maint?.dbPageCount !== undefined
      ? `${fmtCount(maint.dbPageCount)} 页 · freelist ${fmtCount(maint.dbFreelist)} · SQLite WAL`
      : 'SQLite WAL'
  // 快照占用：后端完全无统计能力 → 恒为「—」，不保留任何假数字
  const snapValue = notWired
  const snapHint = '后端未提供快照占用统计'
  // 消息总数后端不提供 → 恒为「—」；仅 sessions 是真实值
  const sessionValue = maintLoading
    ? '读取中…'
    : maintErr
      ? '读取失败'
      : `${fmtCount(maint?.sessions)} / —`
  const sessionHint = maint?.openSessionDbs !== undefined
    ? `已打开 ${maint.openSessionDbs} 个会话库 · 消息总数后端未提供`
    : '会话数真实 · 消息总数后端未提供'

  return (
    <div className="p-3 space-y-4">
      <div className="grid grid-cols-2 gap-3">
        <Stat label="数据目录" value={maint?.dbFilePath ? maint.dbFilePath : 'data/'} hint="可整体拷贝迁移" />
        <Stat label="数据库" value={dbValue} hint={dbHint} />
        <Stat label="快照 blob" value={snapValue} hint={snapHint} />
        <Stat label="会话 / 消息" value={sessionValue} hint={sessionHint} />
        <button
          type="button"
          onClick={() =>
            toast(
              'info',
              `主屏安装（PWA）：${pwa.installAvailable ? '可安装，见下方「安装到主屏」' : pwa.registered ? '已安装 / 暂无安装提示' : pwa.supported ? '未注册' : '不支持'}；离线不缓存（服务端执行）`,
            )
          }
          className="rounded-lg border border-edge p-2.5 text-left hover:bg-panel2"
        >
          <div className="text-[11px] text-inkfaint">主屏安装（PWA）</div>
          <div className="text-[14px] font-medium mt-0.5">
            {pwa.installAvailable ? '可安装' : pwa.registered ? '已就绪' : pwa.supported ? '未注册' : '不支持'}
          </div>
          <div className="text-[11px] text-inkfaint mt-0.5">
            仅更新与安装 · 离线不缓存
          </div>
        </button>
      </div>
      {/* B-2：制品下载 / 会话导出 / 全量备份恢复（真实端点接线，mock 下如实禁用） */}
      <DataBackupPanel />
      <div className="flex flex-wrap gap-2">
        <Button
          variant="outline"
          onClick={() =>
            // 后端未提供 VACUUM 触发接口，因此不做「已回收 x MB」这种编造结果
            toast('info', '服务端未提供 VACUUM 触发接口，本按钮暂不执行任何压缩操作')
          }
        >压缩数据库（VACUUM）</Button>
        <Button
          variant="outline"
          title="导出会话数据受「项目设置 → 数据权限」控制，越界会触发审批"
          onClick={() => {
            setAdvanced(false)
            const sessions = maint?.sessions
            pushDataApproval({
              scope: 'session_export',
              toolName: 'export_session',
              // 会话数用真实值（未取到就不编数）；制品数后端无统计，标注为未知
              summary: `导出${sessions !== undefined ? ` ${sessions} 个` : ''}会话的全部消息与工具结果，打包为 data/export/session-bundle.zip（制品数未采集）`,
              args: {
                scope: 'session_export',
                target: 'data/export/session-bundle.zip',
                ...(sessions !== undefined ? { sessions } : {}),
              },
            })
            toast('info', '已发起数据权限审批，请在对话区处置')
          }}
        >
          导出会话包
        </Button>
        <Button variant="outline" onClick={() => toast('info', '快照保留最近 50 轮，超出自动淘汰')}>查看快照策略</Button>
        {pwa.installAvailable && (
          <Button
            variant="outline"
            onClick={async () => {
              const ok = await promptInstall()
              toast(ok ? 'success' : 'info', ok ? '已开始安装到主屏' : '已取消安装')
            }}
          >
            安装到主屏
          </Button>
        )}
      </div>
      <div className="rounded-lg border border-edge p-3 text-[12px] text-inkfaint space-y-1">
        <div>· 快照：单文件 &gt; 5MB 跳过并标记「不可回滚」</div>
        <div>· 保留：最近 50 轮（可配），孤儿 blob 24h 后 GC</div>
        <div>· run_command 产生的文件变更不纳入快照（需自行用 git）</div>
      </div>
    </div>
  )
}

// 外观面板：原设置弹窗「外观」标签页迁入「高级」弹窗
const TOKEN_COLORS: Record<string, string> = {
  bg: 'var(--bg)',
  panel: 'var(--panel)',
  panel2: 'var(--panel2)',
  edge: 'var(--edge)',
  accent: 'var(--accent)',
  accent2: 'var(--accent2)',
  ok: 'var(--ok)',
  warn: 'var(--warn)',
  err: 'var(--err)',
}

export function ThemePanel() {
  const { theme, toggleTheme } = useUI()

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between rounded-xl border border-edge bg-panel p-3.5 shadow-subtle">
        <div>
          <div className="text-[14px] font-medium text-ink">深色主题</div>
          <div className="text-[12px] text-inkfaint mt-0.5">默认；也可跟随系统（原型用开关模拟）</div>
        </div>
        <Switch checked={theme === 'dark'} onChange={toggleTheme} />
      </div>
      <div className="rounded-xl border border-edge bg-panel p-3.5 shadow-subtle">
        <div className="text-[13px] font-medium text-ink mb-2.5">设计令牌预览</div>
        <div className="flex gap-2.5 flex-wrap">
          {Object.entries(TOKEN_COLORS).map(([name, cssVar]) => (
            <div key={name} className="text-center">
              <div
                className="w-10 h-10 rounded-xl border border-edge/80 shadow-subtle transition-transform hover:scale-105"
                style={{ background: cssVar }}
              />
              <div className="text-[10px] font-mono text-inkfaint mt-1">{name}</div>
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}

function Stat({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <div className="rounded-lg border border-edge p-2.5">
      <div className="text-[11px] text-inkfaint">{label}</div>
      <div className="text-[14px] font-medium mt-0.5">{value}</div>
      <div className="text-[11px] text-inkfaint mt-0.5">{hint}</div>
    </div>
  )
}
