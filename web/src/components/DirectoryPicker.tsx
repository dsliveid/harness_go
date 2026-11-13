import { useCallback, useEffect, useState } from 'react'
import { AlertTriangle, ChevronRight, Folder, HardDrive, Loader2, RefreshCw } from 'lucide-react'
import { Button, Modal, cn } from './ui'
import {
  breadcrumbs, listHostDirs, listHostRoots,
  type DirEntry, type DirListing, type HostID,
} from '../lib/workspaceHost'

/**
 * 可视化目录选择器（工作区宿主目录列举的 UI 侧）。
 *
 * 设计约束（docs/工作区宿主与目录选择对接约定.md）：
 *  1. **只显示目录**，不显示任何文件 —— 这是目录选择器，不是文件浏览器；
 *  2. **只读**：无新建 / 重命名 / 删除入口；
 *  3. 只依赖 host 参数，server 宿主与 device 宿主在 UI 侧完全同形
 *     （L0 阶段 device 宿主由调用方置灰，不会打开本组件）；
 *  4. 出错必须明确区分「越界 / 不存在 / 宿主不支持 / 离线」，
 *     绝不静默展示空列表（那会让用户以为目录是空的）。
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

  const loadRoots = useCallback(async () => {
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
  const canAscend = !!(listing && current && listing.parent)

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
                {error.code === 'fs.path_denied' && (
                  <div className="text-inkdim mt-1">
                    该目录不在服务端已开放的范围内。可在服务端配置文件的
                    <code className="mx-1 px-1 rounded bg-panel3 font-mono">workspace.fs_roots</code>
                    中开放更多目录，或直接手动填写路径。
                  </div>
                )}
                {error.code === 'fs.host_unsupported' && (
                  <div className="text-inkdim mt-1">该工作区暂不支持目录列举，请在输入框手动填写路径。</div>
                )}
              </div>
            </div>
          )}

          {!loading && !error && entries.length === 0 && (
            <div className="px-3 py-6 text-[12px] text-inkfaint">
              {current ? '该目录下没有子目录' : '没有可浏览的根目录'}
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
            disabled={!canAscend}
            onClick={() => { if (listing?.parent) void loadPath(listing.parent) }}
          >
            上一级
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
