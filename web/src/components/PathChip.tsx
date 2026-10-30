import { FileCode, FileText } from 'lucide-react'
import { useUI } from '../stores/uiStore'

/** 是否为潜在文件路径（桌面端 PathLink 同源的保守判定） */
export function isPotentialPath(t: string): boolean {
  if (!t || t.length > 160 || /\s/.test(t)) return false
  if (/^(https?:|mailto:|file:|x-path:)/i.test(t)) return false
  if (/^[\w.-]+@/.test(t)) return false
  const hasSlash = /[\\/]/.test(t)
  const hasExt = /\.(go|ts|tsx|js|jsx|mjs|cjs|rs|py|md|json|yaml|yml|toml|sh|c|cpp|h|hpp|css|scss|html|vue|sql|txt|mod|sum)$/i.test(t)
  return (hasSlash || /^\./.test(t)) && hasExt
}

/** 是否为计划文档路径（.harness/plans/**.md 或 plans/**.md） */
export function isPlanPath(p: string): boolean {
  return /(^|\/)\.harness\/plans\//i.test(p) || /(^|\/)plans?\/[^\\/]+\.md$/i.test(p)
}

const PLAIN_PATH_RE = /(?:[A-Za-z0-9_.\-]+[\\/])+[A-Za-z0-9_.\-]+\.(?:go|ts|tsx|js|jsx|mjs|cjs|rs|py|md|json|yaml|yml|toml|sh|c|cpp|h|hpp|css|scss|html|vue|sql|txt|mod|sum)\b/g

/**
 * 文件路径胶囊（对齐桌面端 PathLink 高亮）：
 * - 实际文件：teal 青绿胶囊，点击在右侧浮窗打开文件详情
 * - 计划文档：emerald 胶囊，点击打开任务清单/方案浮窗
 */
export function PathChip({ path, isPlan }: { path: string; isPlan?: boolean }) {
  const { setPanel, openFileDetail } = useUI()
  const plan = isPlan ?? isPlanPath(path)

  const click = (e: React.MouseEvent) => {
    e.preventDefault()
    e.stopPropagation()
    if (plan) setPanel('todo')
    else openFileDetail(path)
  }

  return (
    <a
      href={`#path:${path}`}
      onClick={click}
      title={plan ? `打开方案: ${path}` : `查看文件详情: ${path}`}
      className={
        plan
          ? 'inline-flex items-center gap-1.5 font-mono text-[12px] font-medium cursor-pointer select-none transition-all rounded-md px-1.5 py-0.5 border shadow-subtle my-0.5 bg-emerald-500/10 text-emerald-300 border-emerald-500/30 hover:bg-emerald-500/20 hover:border-emerald-500/50 hover:text-emerald-200'
          : 'inline-flex items-center gap-1.5 font-mono text-[12px] font-medium cursor-pointer select-none transition-all rounded-md px-1.5 py-0.5 border shadow-subtle my-0.5 bg-teal-500/10 text-teal-300 border-teal-500/30 hover:bg-teal-500/20 hover:border-teal-500/50 hover:text-teal-200'
      }
    >
      {plan ? <FileText size={12} className="text-emerald-400 shrink-0" /> : <FileCode size={12} className="text-teal-400 shrink-0" />}
      <span className="truncate max-w-[280px]">{path}</span>
    </a>
  )
}

/** 把段落文本中出现的普通路径替换为 PathChip（桌面端 autolinkPlainPaths 同源思路） */
export function linkifyPaths(text: string): React.ReactNode[] {
  const out: React.ReactNode[] = []
  PLAIN_PATH_RE.lastIndex = 0
  let last = 0
  let m: RegExpExecArray | null
  let k = 0
  while ((m = PLAIN_PATH_RE.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index))
    out.push(<PathChip key={`p-${k++}`} path={m[0]} />)
    last = m.index + m[0].length
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}
