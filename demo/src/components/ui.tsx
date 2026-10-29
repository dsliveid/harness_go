import React from 'react'
import { X, CheckCircle2, AlertTriangle, AlertCircle, Info, Inbox } from 'lucide-react'
import { useUI } from '../stores/uiStore'

export const cn = (...xs: Array<string | false | null | undefined>) => xs.filter(Boolean).join(' ')

// ---------------- Button ----------------
type BtnVariant = 'primary' | 'outline' | 'ghost' | 'danger' | 'soft'
export function Button({
  children, onClick, variant = 'outline', size = 'md', disabled, className, title, type = 'button',
}: {
  children: React.ReactNode
  onClick?: () => void
  variant?: BtnVariant
  size?: 'sm' | 'md' | 'lg'
  disabled?: boolean
  className?: string
  title?: string
  type?: 'button' | 'submit'
}) {
  const base = 'inline-flex items-center justify-center gap-1.5 rounded-lg font-medium transition-all duration-150 select-none active:scale-[0.98] disabled:opacity-40 disabled:cursor-not-allowed disabled:active:scale-100'
  const sizes = {
    sm: 'h-8 px-2.5 text-[12px] sm:h-7 sm:px-2.5',
    md: 'h-8 px-3 text-[13px] sm:h-8 sm:px-3',
    lg: 'h-10 px-4 text-[14px]',
  }
  const variants: Record<BtnVariant, string> = {
    primary: 'bg-accent text-white hover:bg-accent/90 border border-accent/20',
    outline: 'border border-edge hover:border-edge2 bg-panel hover:bg-panel2 text-ink',
    ghost: 'text-inkdim hover:text-ink hover:bg-panel2',
    danger: 'bg-err text-white hover:bg-err/90 border border-err/20',
    soft: 'bg-panel2 hover:bg-panel3 text-ink',
  }
  return (
    <button type={type} title={title} disabled={disabled} onClick={onClick} className={cn(base, sizes[size], variants[variant], className)}>
      {children}
    </button>
  )
}

// ---------------- IconButton（常驻图标按钮：顶栏/工具行的统一形态） ----------------
export function IconButton({
  children, onClick, active, tone, className, title, ariaLabel,
}: {
  children: React.ReactNode
  onClick?: () => void
  active?: boolean
  tone?: 'warn' | 'accent' | 'ink'
  className?: string
  title?: string
  ariaLabel?: string
}) {
  return (
    <button
      type="button"
      title={title}
      aria-label={ariaLabel ?? title}
      onClick={onClick}
      className={cn(
        'h-8 w-8 inline-flex items-center justify-center rounded-lg text-[14px] transition-all duration-150 shrink-0 select-none active:scale-95',
        active
          ? 'bg-accent/15 text-accent border border-accent/30'
          : tone === 'warn'
            ? 'bg-warn/15 text-warn hover:bg-warn/25'
            : 'text-inkdim hover:text-ink hover:bg-panel2 border border-transparent hover:border-edge',
        className,
      )}
    >
      {children}
    </button>
  )
}

// ---------------- Badge ----------------
type Tone = 'gray' | 'blue' | 'green' | 'yellow' | 'red' | 'purple' | 'cyan'
export function Badge({ children, tone = 'gray', className }: { children: React.ReactNode; tone?: Tone; className?: string }) {
  const tones: Record<Tone, string> = {
    gray: 'bg-panel2 text-inkdim',
    blue: 'bg-accent/12 text-accent',
    green: 'bg-ok/12 text-ok',
    yellow: 'bg-warn/15 text-warn',
    red: 'bg-err/12 text-err',
    purple: 'bg-accent2/12 text-accent2',
    cyan: 'bg-cyan-500/12 text-cyan-400',
  }
  return <span className={cn('inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-medium leading-none tracking-tight', tones[tone], className)}>{children}</span>
}

// ---------------- Modal ----------------
export function Modal({
  open, onClose, title, children, footer, width = 'max-w-3xl', height,
}: { open: boolean; onClose: () => void; title: React.ReactNode; children: React.ReactNode; footer?: React.ReactNode; width?: string; height?: string }) {
  if (!open) return null
  return (
    <div className="fixed inset-0 z-50 flex items-end sm:items-center justify-center bg-black/50 p-0 sm:p-6 fade-in" onClick={onClose}>
      <div
        className={cn('w-full bg-panel border border-edge rounded-t-2xl sm:rounded-xl2 flex flex-col slide-up overflow-hidden', height || 'max-h-[92dvh] sm:max-h-[86dvh]', width)}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-4 h-12 border-b border-edge shrink-0 bg-panel2/40">
          <div className="font-semibold text-[14px] tracking-tight text-ink">{title}</div>
          <button
            className="w-7 h-7 rounded-lg inline-flex items-center justify-center text-inkfaint hover:text-ink hover:bg-panel2 transition-colors"
            onClick={onClose}
            aria-label="关闭"
          >
            <X size={15} />
          </button>
        </div>
        <div className="flex-1 overflow-y-auto min-h-0">{children}</div>
        {footer && <div className="px-4 py-3 border-t border-edge flex items-center justify-end gap-2 shrink-0 bg-panel2/20">{footer}</div>}
      </div>
    </div>
  )
}

// ---------------- Drawer ----------------
export function Drawer({ open, onClose, children, side = 'left', width = 'w-[86%] max-w-[340px]' }: {
  open: boolean; onClose: () => void; children: React.ReactNode; side?: 'left' | 'right'; width?: string
}) {
  if (!open) return null
  return (
    <div className="fixed inset-0 z-40 bg-black/40 fade-in" onClick={onClose}>
      <div
        className={cn('absolute top-0 bottom-0 bg-panel border-edge flex flex-col', width, side === 'left' ? 'left-0 border-r' : 'right-0 border-l')}
        onClick={(e) => e.stopPropagation()}
      >
        {children}
      </div>
    </div>
  )
}

// ---------------- ScrollFade ----------------
export function ScrollFade({ className, children }: { className?: string; children: React.ReactNode }) {
  const ref = React.useRef<HTMLDivElement>(null)
  const [shade, setShade] = React.useState({ left: false, right: false })

  const update = React.useCallback(() => {
    const el = ref.current
    if (!el) return
    setShade({
      left: el.scrollLeft > 4,
      right: el.scrollLeft + el.clientWidth < el.scrollWidth - 4,
    })
  }, [])

  React.useEffect(() => {
    update()
    const el = ref.current
    if (!el) return
    el.addEventListener('scroll', update, { passive: true })
    const ro = new ResizeObserver(update)
    ro.observe(el)
    return () => {
      el.removeEventListener('scroll', update)
      ro.disconnect()
    }
  }, [update])

  return (
    <div className="relative min-w-0">
      {shade.left && (
        <div className="absolute inset-y-0 left-0 w-6 bg-gradient-to-r from-panel to-transparent pointer-events-none z-10" />
      )}
      {shade.right && (
        <div className="absolute inset-y-0 right-0 w-6 bg-gradient-to-l from-panel to-transparent pointer-events-none z-10" />
      )}
      <div ref={ref} onScroll={update} className={cn('overflow-x-auto no-scrollbar', className)}>
        {children}
      </div>
    </div>
  )
}

// ---------------- Tabs ----------------
export function Tabs<T extends string>({ items, value, onChange, className }: {
  items: Array<{ key: T; label: React.ReactNode }>; value: T; onChange: (v: T) => void; className?: string
}) {
  return (
    <ScrollFade className={cn('flex items-center gap-1 p-0.5', className)}>
      {items.map((it) => (
        <button
          key={it.key}
          onClick={() => onChange(it.key)}
          className={cn(
            'h-8 px-3 rounded-lg text-[13px] whitespace-nowrap transition-all duration-150 select-none',
            value === it.key
              ? 'bg-accent/12 text-accent font-medium'
              : 'text-inkdim hover:text-ink hover:bg-panel2',
          )}
        >
          {it.label}
        </button>
      ))}
    </ScrollFade>
  )
}

// ---------------- Progress ----------------
export function Progress({ value, tone = 'accent', className }: { value: number; tone?: 'accent' | 'ok' | 'warn' | 'err'; className?: string }) {
  const color = { accent: 'bg-accent', ok: 'bg-ok', warn: 'bg-warn', err: 'bg-err' }[tone]
  return (
    <div className={cn('h-1.5 rounded-full bg-panel2 overflow-hidden', className)}>
      <div className={cn('h-full transition-all duration-300 rounded-full', color)} style={{ width: `${Math.max(0, Math.min(100, value))}%` }} />
    </div>
  )
}

// ---------------- Empty ----------------
export function Empty({ icon, title, hint, action }: { icon?: React.ReactNode; title: string; hint?: string; action?: React.ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center py-12 px-6 text-center select-none">
      <div className="w-12 h-12 rounded-2xl bg-panel2 flex items-center justify-center text-inkdim mb-3">
        {icon ?? <Inbox size={22} className="text-inkfaint" />}
      </div>
      <div className="text-[14px] font-medium text-ink">{title}</div>
      {hint && <div className="text-[12px] text-inkfaint mt-1 max-w-[300px] leading-relaxed">{hint}</div>}
      {action && <div className="mt-3.5">{action}</div>}
    </div>
  )
}

// ---------------- Switch ----------------
export function Switch({ checked, onChange, disabled, title }: { checked: boolean; onChange: (v: boolean) => void; disabled?: boolean; title?: string }) {
  return (
    <button
      type="button"
      disabled={disabled}
      title={title}
      aria-label={title}
      onClick={() => onChange(!checked)}
      className={cn(
        'w-9 h-5 rounded-full transition-colors relative shrink-0 disabled:opacity-40 focus:outline-none focus:ring-2 focus:ring-accent/20',
        checked ? 'bg-accent' : 'bg-panel3',
      )}
      aria-pressed={checked}
    >
      <span
        className={cn(
          'absolute top-0.5 w-3.5 h-3.5 rounded-full bg-white transition-all duration-200 shadow-sm',
          checked ? 'left-[19px]' : 'left-0.5',
        )}
      />
    </button>
  )
}

// ---------------- 表单 ----------------
export function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <label className="block">
      <div className="text-[12px] font-medium text-inkdim mb-1.5">{label}</div>
      {children}
      {hint && <div className="text-[11px] text-inkfaint mt-1.5 leading-normal">{hint}</div>}
    </label>
  )
}

export const inputCls = 'w-full h-8 px-3 rounded-lg bg-panel2 border border-edge text-[13px] text-ink placeholder:text-inkfaint outline-none transition-all duration-150 focus:border-accent focus:ring-2 focus:ring-accent/20'

// ---------------- Toast ----------------
export function ToastHost() {
  const { toasts, dismissToast } = useUI()
  return (
    <div className="fixed top-3 left-1/2 -translate-x-1/2 z-[60] flex flex-col gap-2 w-[92%] max-w-sm pointer-events-none">
      {toasts.map((t) => (
        <div
          key={t.id}
          className={cn(
            'toast-in pointer-events-auto rounded-xl border px-3.5 py-2.5 text-[13px] bg-panel flex items-start gap-2.5 cursor-pointer select-none transition-all',
            t.type === 'success' && 'border-ok/40 text-ink',
            t.type === 'warn' && 'border-warn/40 text-ink',
            t.type === 'error' && 'border-err/40 text-ink',
            t.type === 'info' && 'border-edge text-ink',
          )}
          onClick={() => dismissToast(t.id)}
        >
          <span className="shrink-0 mt-0.5">
            {t.type === 'success' ? <CheckCircle2 size={15} className="text-ok" /> :
             t.type === 'warn' ? <AlertTriangle size={15} className="text-warn" /> :
             t.type === 'error' ? <AlertCircle size={15} className="text-err" /> :
             <Info size={15} className="text-accent" />}
          </span>
          <span className="text-ink text-[13px] leading-snug flex-1">{t.text}</span>
        </div>
      ))}
    </div>
  )
}

// ---------------- 状态徽标（工具/插件/MCP 复用） ----------------
export function StateBadge({ state }: { state: string }) {
  const map: Record<string, { tone: Tone; text: string }> = {
    ok: { tone: 'green', text: '成功' },
    running: { tone: 'blue', text: '运行中' },
    pending: { tone: 'gray', text: '等待' },
    error: { tone: 'red', text: '失败' },
    denied: { tone: 'yellow', text: '已拒绝' },
    enabled: { tone: 'green', text: '已启用' },
    disabled: { tone: 'gray', text: '已禁用' },
    available: { tone: 'blue', text: '就绪' },
    unavailable: { tone: 'yellow', text: '不可用' },
    degraded: { tone: 'red', text: '降级' },
    ready: { tone: 'green', text: '正常' },
    starting: { tone: 'blue', text: '启动中' },
    failed: { tone: 'red', text: '失败' },
    completed: { tone: 'green', text: '完成' },
    cancelled: { tone: 'gray', text: '已取消' },
    interrupted: { tone: 'yellow', text: '已中断' },
    paused: { tone: 'yellow', text: '已暂停' },
    idle: { tone: 'gray', text: '空闲' },
    busy: { tone: 'blue', text: '工作中' },
    done: { tone: 'green', text: '完成' },
  }
  const m = map[state] || { tone: 'gray' as Tone, text: state }
  return <Badge tone={m.tone}>{m.text}</Badge>
}
