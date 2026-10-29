import { useEffect, useRef, useState } from 'react'
import { Button, Field, Modal, inputCls } from './ui'

export interface PromptField {
  key: string
  label: string
  value: string
  placeholder?: string
  hint?: string
}

/** 通用文本输入弹窗：重命名会话/项目、新建项目等多字段轻量表单（替代 window.prompt） */
export function PromptModal({
  open, title, fields, confirmText = '确定', onCancel, onSubmit,
}: {
  open: boolean
  title: string
  fields: PromptField[]
  confirmText?: string
  onCancel: () => void
  onSubmit: (values: Record<string, string>) => void
}) {
  const [draft, setDraft] = useState<Record<string, string>>({})
  const refs = useRef<Record<string, HTMLInputElement | null>>({})

  // 打开（或切换目标）时装载初始值
  useEffect(() => {
    if (!open) return
    const init: Record<string, string> = {}
    for (const f of fields) init[f.key] = f.value
    setDraft(init)
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
        {fields.map((f) => (
          <Field key={f.key} label={f.label} hint={f.hint}>
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
          </Field>
        ))}
      </div>
    </Modal>
  )
}
