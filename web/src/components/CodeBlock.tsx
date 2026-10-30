import { useState } from 'react'
import { Copy, Check, Terminal } from 'lucide-react'
import { cn } from './ui'

export function CodeBlock({ code, lang, className }: { code: string; lang?: string; className?: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code)
      setCopied(true)
      setTimeout(() => setCopied(false), 1400)
    } catch {
      setCopied(false)
    }
  }
  return (
    <div className={cn('relative group rounded-xl border border-edge bg-[#090d14] overflow-hidden my-2', className)}>
      <div className="flex items-center justify-between px-3 h-8 border-b border-edge/80 bg-panel2/60 text-[11px] text-inkdim select-none">
        <div className="flex items-center gap-1.5 font-mono text-[11px] font-medium text-inkdim">
          <Terminal size={12} className="text-accent opacity-80" />
          <span>{lang || 'text'}</span>
        </div>
        <button
          type="button"
          className="inline-flex items-center gap-1 text-[11px] hover:text-ink transition-colors px-1.5 py-0.5 rounded hover:bg-panel3 active:scale-95"
          onClick={copy}
        >
          {copied ? <Check size={12} className="text-ok" /> : <Copy size={12} />}
          <span>{copied ? '已复制' : '复制'}</span>
        </button>
      </div>
      <pre className="p-3 overflow-x-auto text-[12px] leading-[1.65] text-[#e6edf3] font-mono scrollbar-thin">
        <code>{code}</code>
      </pre>
    </div>
  )
}
