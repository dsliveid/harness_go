import { useEffect, useRef, useState } from 'react'
import { FolderSearch } from 'lucide-react'
import { Button, Field, Modal, inputCls } from './ui'
import { DirectoryPicker } from './DirectoryPicker'
import type { HostID } from '../lib/workspaceHost'

export interface PromptField {
  key: string
  label: string
  value: string
  placeholder?: string
  hint?: string
}

/**
 * 通用文本输入弹窗：重命名会话/项目、新建项目等多字段轻量表单（替代 window.prompt）。
 *
 * 目录浏览（可选）：传 `browseFieldKey` + `browseHost` 后，该字段右侧出现「浏览」按钮，
 * 打开 DirectoryPicker 回填路径。不传则退化为纯手填（保持向后兼容）。
 */
export function PromptModal({
  open, title, fields, confirmText = '确定', onCancel, onSubmit,
  browseFieldKey, browseHost, browseHostLabel, browseDisabledReason,
}: {
  open: boolean
  title: string
  fields: PromptField[]
  confirmText?: string
  onCancel: () => void
  onSubmit: (values: Record<string, string>) => void
  /** 需要「浏览」按钮的字段 key（通常为目录路径字段） */
  browseFieldKey?: string
  /** 目录列举的宿主（server / device:<id>） */
  browseHost?: HostID
  /** 宿主显示名，用于选择器标题 */
  browseHostLabel?: string
  /** 非空表示该宿主不可列举：按钮置灰并用它做 tooltip 说明原因 */
  browseDisabledReason?: string
}) {
  const [draft, setDraft] = useState<Record<string, string>>({})
  const [browseOpen, setBrowseOpen] = useState(false)
  const refs = useRef<Record<string, HTMLInputElement | null>>({})

  // 打开（或切换目标）时装载初始值
  useEffect(() => {
    if (!open) return
    const init: Record<string, string> = {}
    for (const f of fields) init[f.key] = f.value
    setDraft(init)
    setBrowseOpen(false)
  }, [open, title, fields])

  // 以 DOM 值为准做兜底，避免受控 state 未同步时丢输入
  const collect = (): Record<string, string> => {
    const out: Record<string, string> = {}
    for (const f of fields) out[f.key] = refs.current[f.key]?.value ?? draft[f.key] ?? ''
    return out
  }
  const commit = () => {
    const v = collect()
    const firstKey = fields[0]?.key
    if (firstKey && !(v[firstKey] ?? '').trim()) return
    onSubmit(v)
  }

  return (
    <>
      <Modal
        open={open}
        onClose={onCancel}
        title={title}
        width="max-w-md"
        footer={
          <>
            <Button variant="ghost" onClick={onCancel}>取消</Button>
            <Button variant="primary" onClick={commit}>{confirmText}</Button>
          </>
        }
      >
        <div className="p-4 space-y-3">
          {fields.map((f) => {
            const withBrowse = !!browseFieldKey && f.key === browseFieldKey
            return (
              <Field key={f.key} label={f.label} hint={f.hint}>
                <div className="flex items-center gap-1.5">
                  <input
                    ref={(el) => { refs.current[f.key] = el }}
                    className={inputCls}
                    value={draft[f.key] ?? ''}
                    placeholder={f.placeholder}
                    onChange={(e) => setDraft((d) => ({ ...d, [f.key]: e.target.value }))}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') commit()
                    }}
                  />
                  {withBrowse && (
                    <Button
                      size="sm"
                      variant="soft"
                      className="shrink-0"
                      disabled={!!browseDisabledReason || !browseHost}
                      title={browseDisabledReason || '在服务端工作区中选择目录'}
                      onClick={() => setBrowseOpen(true)}
                    >
                      <FolderSearch size={13} />
                      浏览
                    </Button>
                  )}
                </div>
              </Field>
            )
          })}
        </div>
      </Modal>

      {/* 目录选择器：与 PromptModal 分层渲染，避免嵌套 Modal 的层级/关闭冲突 */}
      {browseFieldKey && browseHost && (
        <DirectoryPicker
          open={browseOpen}
          host={browseHost}
          hostLabel={browseHostLabel}
          initialPath={draft[browseFieldKey] ?? ''}
          onClose={() => setBrowseOpen(false)}
          onPick={(p) => {
            setDraft((d) => ({ ...d, [browseFieldKey]: p }))
            setBrowseOpen(false)
          }}
        />
      )}
    </>
  )
}
