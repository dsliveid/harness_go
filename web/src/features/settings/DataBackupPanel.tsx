import { useRef, useState } from 'react'
import { Archive, Download, FileUp, PackageCheck } from 'lucide-react'
import { Button } from '../../components/ui'
import { RealModeNotice } from '../../components/RealModeNotice'
import { useUI } from '../../stores/uiStore'
import { useSessions } from '../../stores/sessionStore'
import { api, apiMode } from '../../lib/api'
import type { RestoreResult } from '../../lib/api'

/**
 * 数据备份 / 导出面板（B-2：制品下载 + 会话导出 + 全量备份恢复接线）。
 *
 * 数据来源（后端契约已定，前端不得自创端点）：
 *   - GET  /api/v1/artifacts/{id}            → 二进制流（Content-Disposition: attachment）
 *   - GET  /api/v1/sessions/{sid}/export?format=zip → application/zip 流
 *   - GET  /api/v1/data/backup               → application/zip 流（owner 专用）
 *   - POST /api/v1/data/restore              → 请求体为备份包**原始 zip 字节流**（Content-Type: application/zip）
 *
 * 铁律：
 *   1. 错误**原样透传**（含后端 message / code / trace_id），绝不静默吞掉；
 *   2. mock 演示态如实拒绝（见 api.ts mockApi），本面板在 mock 下禁用并明示，不展示「假成功」；
 *   3. 后端恢复只做「校验 + 解压到 staging 目录」，**不覆盖运行中的 data/**，须停机替换才生效；
 *      因此必须二次确认 + 必须显式选择 .zip 文件，且结果文案必须给出 staging_path，不得谎称「已完成恢复」。
 */

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e))

const fmtBytes = (n: number): string => {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}

type Busy = null | 'artifact' | 'export' | 'backup' | 'restore'

export function DataBackupPanel() {
  const toast = useUI((s) => s.toast)
  const sessions = useSessions((s) => s.sessions)
  const currentId = useSessions((s) => s.currentId)
  const real = apiMode === 'real'

  const [busy, setBusy] = useState<Busy>(null)
  /** 本次面板最近一次失败的原始错误文案（不加工、不截断） */
  const [error, setError] = useState<string | null>(null)
  /** 最近一次成功的结果文案（含文件名 / 体积 / 恢复条数） */
  const [okText, setOkText] = useState<string | null>(null)

  // 制品下载：手动输入制品 ID（后端只提供按 id 取流的端点，没有列制品清单接口，
  // 因此这里不臆造文件列表，只允许精确输入 id）。
  const [artifactId, setArtifactId] = useState('')

  // 恢复：必须是用户显式选择的 zip
  const fileRef = useRef<HTMLInputElement | null>(null)
  const [restoreFile, setRestoreFile] = useState<File | null>(null)
  const [confirmRestore, setConfirmRestore] = useState(false)

  const targetSessionId = currentId || sessions[0]?.id || ''

  const run = async (kind: Exclude<Busy, null>, fn: () => Promise<string>) => {
    setBusy(kind)
    setError(null)
    setOkText(null)
    try {
      const text = await fn()
      setOkText(text)
      toast('success', text)
    } catch (e) {
      // 原样透传：后端 ApiError.message / trace_id 均在 message 或附加信息中，不做任何改写
      const msg = errText(e)
      setError(msg)
      toast('error', msg)
    } finally {
      setBusy(null)
    }
  }

  const onDownloadArtifact = () =>
    run('artifact', async () => {
      const id = artifactId.trim()
      if (!id) throw new Error('请先填写制品 ID（后端仅提供按 id 取流的端点，不提供制品清单）')
      const res = await api.downloadArtifact(id)
      return `制品已下载：${res.filename}（${fmtBytes(res.blob.size)}）`
    })

  const onExportSession = () =>
    run('export', async () => {
      if (!targetSessionId) throw new Error('当前没有可导出的会话：请先在左侧选择或新建一个会话')
      const res = await api.exportSession(targetSessionId)
      return `会话已导出：${res.filename}（${fmtBytes(res.blob.size)}）`
    })

  const onBackup = () =>
    run('backup', async () => {
      const res = await api.backupData()
      return `全量备份已下载：${res.filename}（${fmtBytes(res.blob.size)}）`
    })

  const onRestore = () =>
    run('restore', async () => {
      if (!restoreFile) throw new Error('请先选择备份 zip 文件')
      if (!confirmRestore) throw new Error('恢复需停机替换数据目录，请先勾选确认')
      const res: RestoreResult = await api.restoreData(restoreFile)
      const count = res.skipped?.length ? ` · 跳过 ${res.skipped.length} 项` : ''
      // 后端只做校验+暂存，绝不自动生效：必须把 staging_path 明确交给用户，
      // 否则用户会误以为「点一下就恢复好了」。
      return `已校验并解压 ${res.entries} 项（${fmtBytes(res.bytes)}）${count} → 暂存于 ${res.staging_path}，需停机替换后生效`
    })

  return (
    <div className="rounded-xl border border-edge bg-panel p-3.5 space-y-3">
      <div className="flex items-start gap-2">
        <Archive size={15} className="mt-0.5 shrink-0 text-inkdim" />
        <div className="min-w-0">
          <div className="text-[14px] font-medium text-ink">导出与备份</div>
          <div className="text-[12px] text-inkfaint mt-0.5">
            会话导出 / 全量备份为服务端打包的 zip 流；恢复仅校验并解压到暂存目录，需停机替换后才生效。
          </div>
        </div>
      </div>

      <RealModeNotice
        text={
          real
            ? '本区块四个入口均直连真实端点（/artifacts/{id} · /sessions/{sid}/export · /data/backup · /data/restore）。失败时后端错误文案原样展示，不做任何静默兜底；/data/backup 与 /data/restore 为 owner 专用，非 owner 会收到 403。'
            : '当前为 DEMO 模式：制品下载 / 会话导出 / 全量备份 / 恢复均**不可用**（不伪造 zip 内容，避免产生 0 字节假文件）。切换到 REAL 模式后本区块才真正打服务端。'
        }
      />

      {/* 1) 制品下载 */}
      <div className="rounded-lg border border-edge bg-panel2/40 p-2.5 space-y-2">
        <div className="flex items-center gap-1.5 text-[12px] font-medium text-ink">
          <Download size={12} className="text-inkdim" /> 制品下载
        </div>
        <div className="flex items-center gap-2">
          <input
            value={artifactId}
            onChange={(e) => setArtifactId(e.target.value)}
            placeholder="制品 ID（GET /artifacts/{id}）"
            className="h-8 flex-1 min-w-0 px-2.5 rounded-lg bg-panel2 border border-edge text-[12px] outline-none focus:border-accent placeholder:text-inkfaint font-mono"
          />
          <Button variant="outline" disabled={!real || busy !== null} onClick={onDownloadArtifact}>
            {busy === 'artifact' ? '下载中…' : '下载'}
          </Button>
        </div>
        <div className="text-[11px] text-inkfaint">
          后端未提供制品清单接口，故此处只能按 ID 精确下载，不展示臆造的文件列表。
        </div>
      </div>

      {/* 2) 会话导出 + 3) 全量备份 */}
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
        <div className="rounded-lg border border-edge bg-panel2/40 p-2.5 space-y-2">
          <div className="flex items-center gap-1.5 text-[12px] font-medium text-ink">
            <PackageCheck size={12} className="text-inkdim" /> 导出会话
          </div>
          <div className="text-[11px] text-inkfaint break-all">
            目标会话：<span className="font-mono text-inkdim">{targetSessionId || '（无可用会话）'}</span>
          </div>
          <Button variant="outline" disabled={!real || busy !== null || !targetSessionId} onClick={onExportSession}>
            {busy === 'export' ? '导出中…' : '导出当前会话 zip'}
          </Button>
        </div>

        <div className="rounded-lg border border-edge bg-panel2/40 p-2.5 space-y-2">
          <div className="flex items-center gap-1.5 text-[12px] font-medium text-ink">
            <Archive size={12} className="text-inkdim" /> 全量备份
          </div>
          <div className="text-[11px] text-inkfaint">owner 专用；包含全局库与会话库。</div>
          <Button variant="outline" disabled={!real || busy !== null} onClick={onBackup}>
            {busy === 'backup' ? '打包中…' : '下载全量备份 zip'}
          </Button>
        </div>
      </div>

      {/* 4) 从备份恢复 */}
      <div className="rounded-lg border border-edge bg-panel2/40 p-2.5 space-y-2">
        <div className="flex items-center gap-1.5 text-[12px] font-medium text-ink">
          <FileUp size={12} className="text-inkdim" /> 从备份恢复
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <input
            ref={fileRef}
            type="file"
            accept=".zip,application/zip"
            className="hidden"
            onChange={(e) => {
              setRestoreFile(e.target.files?.[0] ?? null)
              setConfirmRestore(false)
            }}
          />
          <Button variant="outline" disabled={!real || busy !== null} onClick={() => fileRef.current?.click()}>
            选择备份 zip
          </Button>
          <span className="text-[11.5px] text-inkdim font-mono truncate max-w-[240px]">
            {restoreFile ? `${restoreFile.name}（${fmtBytes(restoreFile.size)}）` : '未选择文件'}
          </span>
        </div>
        <label className="flex items-start gap-1.5 text-[11.5px] text-warn cursor-pointer select-none">
          <input
            type="checkbox"
            checked={confirmRestore}
            disabled={!real || busy !== null || !restoreFile}
            onChange={(e) => setConfirmRestore(e.target.checked)}
            className="mt-0.5"
          />
          <span>我已知晓：恢复只解压到服务端暂存目录、不会自动生效，仍需停机替换数据目录。</span>
        </label>
        <Button variant="outline" disabled={!real || busy !== null || !restoreFile || !confirmRestore} onClick={onRestore}>
          {busy === 'restore' ? '恢复中…' : '上传并恢复'}
        </Button>
      </div>

      {error && (
        <div className="rounded-lg border border-err/40 bg-err/[0.06] px-2.5 py-1.5 text-[11.5px] text-err leading-relaxed break-all select-text">
          错误（后端原样返回）：{error}
        </div>
      )}
      {okText && (
        <div className="rounded-lg border border-ok/40 bg-ok/[0.06] px-2.5 py-1.5 text-[11.5px] text-ok leading-relaxed break-all select-text">
          {okText}
        </div>
      )}
    </div>
  )
}
