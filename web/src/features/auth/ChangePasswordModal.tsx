import { useEffect, useRef, useState } from 'react'
import { KeyRound, Eye, EyeOff, Check } from 'lucide-react'
import { Button, Modal, inputCls, cn } from '../../components/ui'
import { useAuth } from '../../stores/authStore'

// 修改密码弹窗：真实实现 PUT /api/auth/password（校验旧密码摘要、写新摘要、吊销其他会话）；
// 原型在 authStore 内本地模拟：改完立即生效，当前会话保持登录，下次登录用新密码。
export function ChangePasswordModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { changePassword } = useAuth()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [showPwd, setShowPwd] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  // DOM 值兜底：避免受控 state 未同步时丢输入（与项目设置表单同一先例）
  const currentRef = useRef<HTMLInputElement | null>(null)
  const nextRef = useRef<HTMLInputElement | null>(null)
  const confirmRef = useRef<HTMLInputElement | null>(null)

  // 打开时重置
  useEffect(() => {
    if (open) {
      setCurrent('')
      setNext('')
      setConfirm('')
      setError(null)
      setShowPwd(false)
    }
  }, [open])

  const submit = async () => {
    // 以 DOM 值为准做兜底
    const cur = currentRef.current?.value ?? current
    const nxt = nextRef.current?.value ?? next
    const conf = confirmRef.current?.value ?? confirm
    if (conf !== nxt) {
      setError('两次输入的新密码不一致')
      return
    }
    setLoading(true)
    const res = await changePassword(cur, nxt)
    setLoading(false)
    if (!res.ok) {
      setError(res.error ?? '修改失败')
      return
    }
    onClose()
  }

  const rulesOk = next.length >= 8 && next === confirm && current.length > 0

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={
        <span className="flex items-center gap-2">
          <KeyRound size={14} className="text-accent shrink-0" />
          <span>修改密码</span>
        </span>
      }
      width="max-w-md"
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={loading}>取消</Button>
          <Button variant="primary" disabled={!rulesOk || loading} onClick={() => { void submit() }}>
            {loading ? '提交中…' : '确认修改'}
          </Button>
        </>
      }
    >
      <div className="space-y-3.5 p-1">
        <p className="text-[12px] text-inkfaint leading-relaxed">
          修改后立即生效，当前会话保持登录；真实实现会同时吊销其他设备的会话，需用新密码重新登录。
        </p>

        <label className="block">
          <span className="text-[12px] text-inkdim">当前密码</span>
          <input
            ref={currentRef}
            className={cn(inputCls, 'mt-1')}
            type={showPwd ? 'text' : 'password'}
            value={current}
            onChange={(e) => { setCurrent(e.target.value); if (error) setError(null) }}
            placeholder="••••••••"
            autoComplete="current-password"
            disabled={loading}
          />
        </label>

        <label className="block">
          <span className="text-[12px] text-inkdim">新密码</span>
          <div className="relative mt-1">
            <input
              ref={nextRef}
              className={cn(inputCls, 'pr-9')}
              type={showPwd ? 'text' : 'password'}
              value={next}
              onChange={(e) => { setNext(e.target.value); if (error) setError(null) }}
              placeholder="至少 8 位"
              autoComplete="new-password"
              disabled={loading}
            />
            <button
              type="button"
              className="absolute right-1.5 top-1/2 -translate-y-1/2 w-7 h-7 rounded-md flex items-center justify-center text-inkfaint hover:text-ink hover:bg-panel2 transition-colors"
              title={showPwd ? '隐藏密码' : '显示密码'}
              onClick={() => setShowPwd((v) => !v)}
              tabIndex={-1}
            >
              {showPwd ? <EyeOff size={13} /> : <Eye size={13} />}
            </button>
          </div>
        </label>

        <label className="block">
          <span className="text-[12px] text-inkdim">确认新密码</span>
          <input
            ref={confirmRef}
            className={cn(inputCls, next.length > 0 && confirm.length > 0 ? (next === confirm ? 'border-ok/60' : 'border-err/60') : '')}
            type={showPwd ? 'text' : 'password'}
            value={confirm}
            onChange={(e) => { setConfirm(e.target.value); if (error) setError(null) }}
            placeholder="再输入一次新密码"
            autoComplete="new-password"
            disabled={loading}
          />
          {next.length > 0 && confirm.length > 0 && (
            <span className={cn('mt-1 inline-flex items-center gap-1 text-[11px]', next === confirm ? 'text-ok' : 'text-err')}>
              {next === confirm ? <><Check size={10} />两次输入一致</> : '两次输入不一致'}
            </span>
          )}
        </label>

        {error && (
          <div className="rounded-lg border border-err/40 bg-err/[0.08] px-2.5 py-2 text-[12px] text-err">{error}</div>
        )}

        <div className="rounded-lg border border-edge bg-panel2/40 px-2.5 py-2 text-[11px] text-inkfaint leading-relaxed">
          · 新密码至少 8 位，且不能与当前密码相同
          <br />· 忘记当前密码时，可在服务端用 <code className="font-mono text-accent">harness reset-password</code> 重置（原型不模拟）
        </div>
      </div>
    </Modal>
  )
}
