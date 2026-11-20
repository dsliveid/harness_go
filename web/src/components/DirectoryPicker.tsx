import { useCallback, useEffect, useRef, useState } from 'react'
import { AlertTriangle, ChevronRight, Folder, HardDrive, Loader2, RefreshCw } from 'lucide-react'
import { Button, Modal, cn } from './ui'
import {
  breadcrumbs, isRetryableFsError, listHostDirs, listHostRoots,
  type DirEntry, type DirListing, type HostID,
} from '../lib/workspaceHost'

/**
 * 可视化目录选择器（工作区宿主目录列举的 UI 侧）。
 *
 * 设计约束（docs/工作区宿主与目录选择对接约定.md）：
 *  1. **只显示目录**，不显示任何文件 —— 这是目录选择器，不是文件浏览器；
 *  2. **只读**：无新建 / 重命名 / 删除入口；
 *  3. 只依赖 host 参数，server 宿主与 device 宿主在 UI 侧完全同形
 *     （L1 起 device 宿主经 fs.list 帧真实派发，不再由调用方置灰）；
 *  4. 出错必须明确区分「越界 / 不存在 / 宿主不支持 / 超时 / 离线」，
 *     绝不静默展示空列表（那会让用户以为目录是空的）；
 *  5. 错误分档的判定权在服务端（`fs.host_timeout` 可重试 / `fs.host_unsupported`
 *     不可重试），本组件**只负责按 code 展示对应出路**，不数次数、不做能力探测。
 */
export function DirectoryPicker({
  open, host, initialPath, hostLabel, onClose, onPick,
}: {
  open: boolean
  host: HostID
  /** 打开时的起始路径（通常回填当前已填的路径，便于就地修改） */
  initialPath?: string
  /** 宿主显示名，用于标题提示「这是哪个工作区的目录」 */
  hostLabel?: string
  onClose: () => void
  onPick: (path: string) => void
}) {
  const [listing, setListing] = useState<DirListing | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<{ message: string; code: string } | null>(null)
  /**
   * 最近一次发起的请求目标 —— 供错误面板上的「重试一次」精确复现同一请求，
   * 而不是重新载入根列表（用户明明是在某个子目录里超时的）。
   */
  const target = useRef<{ kind: 'roots' } | { kind: 'path'; path: string }>({ kind: 'roots' })

  const loadRoots = useCallback(async () => {
    target.current = { kind: 'roots' }
    setLoading(true)
    setError(null)
    try {
      setListing(await listHostRoots(host))
    } catch (e) {
      setListing(null)
      setError(toErr(e))
    } finally {
      setLoading(false)
    }
  }, [host])

  const loadPath = useCallback(async (path: string) => {
    target.current = { kind: 'path', path }
    setLoading(true)
    setError(null)
    try {
      setListing(await listHostDirs(host, path))
    } catch (e) {
      setError(toErr(e))
    } finally {
      setLoading(false)
    }
  }, [host])

  /** 重试最近一次请求：错误面板上的「重试一次」只对可重试错误（超时）出现 */
  const retry = useCallback(() => {
    const t = target.current
    if (t.kind === 'path') void loadPath(t.path)
    else void loadRoots()
  }, [loadPath, loadRoots])

  // 打开时定位：有初始路径则直接进入（可能越界 → 落回 roots），否则从根开始
  useEffect(() => {
    if (!open) return
    const init = (initialPath ?? '').trim()
    if (init) void loadPath(init)
    else void loadRoots()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, host])

  const current = listing?.path ?? ''
  const crumbs = breadcrumbs(current, listing?.separator ?? '/')
  const entries: DirEntry[] = listing ? (current ? listing.dirs : listing.roots) : []
  /**
   * 已定位到某个根（盘符根 / 配置的受限根）时，服务端刻意返回 parent=null
   * （绝不回 ".."，见 internal/api/fs_handler.go 的 parent 计算）。
   * 但 UI 侧「上一级」不该因此置灰：根之上还有「根列表」这一层视图，
   * 只是它不是文件系统里的上级目录，而是未定位状态（loadRoots）。
   */
  const atRootLevel = !!(listing && current && !listing.parent)
  const canAscend = !!(listing && current)

  const footer = (
    <>
      <Button variant="ghost" onClick={onClose}>取消</Button>
      <Button
        variant="primary"
        disabled={!current || loading}
        onClick={() => { if (current) onPick(current) }}
      >
        {current ? '选择此目录' : '请先进入一个目录'}
      </Button>
    </>
  )

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={hostLabel ? `选择目录 · ${hostLabel}` : '选择目录'}
      width="max-w-2xl"
      height="max-h-[76vh]"
      footer={footer}
    >
      <div className="flex flex-col min-h-0 h-[52vh]">
        {/* 工具条：面包屑 + 刷新 */}
        <div className="flex items-center gap-1.5 px-3 py-2 border-b border-edge shrink-0 min-w-0">
          <button
            className="text-[12px] text-inkdim hover:text-accent shrink-0 px-1"
            title="回到根列表"
            onClick={() => void loadRoots()}
          >
            <HardDrive size={13} />
          </button>
          <div className="flex items-center gap-0.5 overflow-x-auto min-w-0 flex-1">
            {crumbs.length === 0 ? (
              <span className="text-[12px] text-inkfaint shrink-0">请选择根目录</span>
            ) : (
              crumbs.map((c, i) => (
                <span key={`${c.path}-${i}`} className="flex items-center shrink-0">
                  <button
                    className={cn(
                      'text-[12px] px-1 rounded hover:bg-panel2 max-w-[140px] truncate',
                      i === crumbs.length - 1 ? 'text-ink font-medium' : 'text-inkdim',
                    )}
                    title={c.path}
                    onClick={() => void loadPath(c.path)}
                  >
                    {c.name}
                  </button>
                  {i < crumbs.length - 1 && <ChevronRight size={11} className="text-inkfaint shrink-0" />}
                </span>
              ))
            )}
          </div>
          <button
            className="text-inkfaint hover:text-accent shrink-0 px-1"
            title="刷新"
            onClick={() => void (current ? loadPath(current) : loadRoots())}
          >
            <RefreshCw size={12} />
          </button>
        </div>

        {/* 列表区 */}
        <div className="flex-1 min-h-0 overflow-y-auto p-1.5">
          {loading && (
            <div className="flex items-center gap-2 px-3 py-6 text-[12px] text-inkdim">
              <Loader2 size={14} className="animate-spin" /> 正在读取目录…
            </div>
          )}

          {!loading && error && (
            <div className="m-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2.5 text-[12px] text-warn leading-relaxed flex gap-2">
              <AlertTriangle size={14} className="shrink-0 mt-0.5" />
              <div className="min-w-0">
                <div>{error.message}</div>
                <FsErrorHint code={error.code} />
                {isRetryableFsError(error.code) && (
                  <div className="mt-2">
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => retry()}
                    >
                      <RefreshCw size={11} className="mr-1" /> 重试一次
                    </Button>
                  </div>
                )}
              </div>
            </div>
          )}

          {!loading && !error && listing && entries.length === 0 && (
            <div className="px-3 py-6 text-[12px] text-inkfaint leading-relaxed">
              {current
                ? '该目录下没有子目录'
                : host.startsWith('device:')
                  // 成功响应但 roots 与 dirs 都为空：不是故障，是端侧没配置根目录。
                  // 只有 listing 存在（成功响应）+ 无错误 + 两个列表都空时才会走到这里。
                  ? '该设备尚未设置工作区根目录，可在桌面端设置后重试。也可以直接手动填写路径。'
                  : '服务端尚未配置可浏览的根目录（workspace.fs_roots），可直接手动填写路径。'}
            </div>
          )}

          {!loading && !error && entries.map((d) => (
            <button
              key={d.path}
              className="w-full flex items-center gap-2 px-2.5 py-1.5 rounded-lg hover:bg-panel2 text-left min-w-0 group"
              title={d.path}
              onDoubleClick={() => onPick(d.path)}
              onClick={() => void loadPath(d.path)}
            >
              <Folder size={14} className="text-accent shrink-0" />
              <span className="text-[13px] truncate flex-1">{d.name}</span>
              <ChevronRight size={12} className="text-inkfaint shrink-0 opacity-0 group-hover:opacity-100" />
            </button>
          ))}

          {!loading && !error && listing?.truncated && (
            <div className="px-3 py-2 text-[11px] text-inkfaint">
              目录过多，仅显示前 {entries.length} 项
            </div>
          )}
        </div>

        {/* 底部：当前路径 + 返回上级 */}
        <div className="border-t border-edge px-3 py-2 shrink-0 flex items-center gap-2 min-w-0">
          <span className="text-[11px] text-inkdim shrink-0">当前</span>
          <code className="text-[11px] font-mono text-inkfaint truncate flex-1" title={current}>
            {current || '（未选择）'}
          </code>
          <Button
            size="sm"
            variant="ghost"
            disabled={!canAscend || loading}
            title={atRootLevel ? '回到根列表（盘符/根目录列表）' : '返回上级目录'}
            onClick={() => {
              if (listing?.parent) { void loadPath(listing.parent); return }
              // 已在根层级：上跳不是文件系统目录，而是「根列表」视图
              void loadRoots()
            }}
          >
            {atRootLevel ? '回到根列表' : '上一级'}
          </Button>
        </div>
      </div>
    </Modal>
  )
}

/** 统一错误归一：拿到 ApiError 的 code 便于分场景提示 */
function toErr(e: unknown): { message: string; code: string } {
  const anyErr = e as { message?: string; code?: string }
  return {
    message: anyErr?.message || '目录读取失败',
    code: anyErr?.code || 'http_error',
  }
}

/**
 * 按服务端 `apperr` code 给出**有指导意义**的补充说明。
 *
 * 文案分工（2026-11 错误语义重构，判定权在服务端，本组件不数次数、不探测）：
 *  - `fs.host_timeout`    端侧 3 秒无应答 —— **可重试一次**；持续超时则实为客户端
 *                         未实现目录列举（服务端会在连续 ≥2 次超时后自己升级成
 *                         `fs.host_unsupported`），故文案前置两条出路：重试 / 升级或手填；
 *  - `fs.host_unsupported` 设备未申报能力或客户端未实现 —— **不可重试**；
 *  - `fs.host_offline`    设备不在线 —— 等上线再重试；
 *  - `fs.path_denied`     越界或无权限；
 *  - `fs.path_not_found`  路径不存在或不是目录。
 * 未识别的 code 不出补充说明（仅展示服务端 message），避免臆造。
 */
function FsErrorHint({ code }: { code: string }) {
  if (code === 'fs.host_timeout') {
    return (
      <div className="text-inkdim mt-1">
        设备响应超时。首次超时可能是设备繁忙，请重试一次；若持续超时，说明该客户端版本不支持目录列举，请升级桌面端或直接手动填写路径。
      </div>
    )
  }
  if (code === 'fs.host_unsupported') {
    return (
      <div className="text-inkdim mt-1">
        该工作区暂不支持目录列举，请在输入框手动填写路径。
        <br />
        若这是桌面端工作区，通常是客户端版本过旧未实现目录列举能力，升级桌面端后重试即可（此错误重试无效，不必连点）。
      </div>
    )
  }
  if (code === 'fs.host_offline') {
    return (
      <div className="text-inkdim mt-1">
        该工作区绑定的设备当前不在线，无法列举其目录。请先启动并连接桌面端，待其上线后再刷新；也可以直接手动填写路径。
      </div>
    )
  }
  if (code === 'fs.path_denied') {
    return (
      <div className="text-inkdim mt-1">
        该目录不在服务端可浏览的范围内（仅当服务端配置了
        <code className="mx-1 px-1 rounded bg-panel3 font-mono">workspace.fs_roots</code>
        限制时才会出现）。可改用左侧其它目录，或直接手动填写该路径。
      </div>
    )
  }
  if (code === 'fs.path_not_found') {
    return (
      <div className="text-inkdim mt-1">
        该路径不存在，或者它不是一个目录（比如指向了文件）。可在上方面包屑回退到上一级，或直接手动填写正确的路径。
      </div>
    )
  }
  return null
}

