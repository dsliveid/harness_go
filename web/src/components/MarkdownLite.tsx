import React from 'react'
import { CodeBlock } from './CodeBlock'
import { PathChip, isPotentialPath, linkifyPaths } from './PathChip'

/**
 * 原型级 Markdown 渲染：支持 ```代码块```、行内 code、**粗体**、### 标题、- 列表、> 引用。
 * 正式实现替换为 react-markdown（保持同一组件签名）。
 */
export function MarkdownLite({ text }: { text: string }) {
  const lines = text.split('\n')
  const out: React.ReactNode[] = []
  let i = 0
  let key = 0

  while (i < lines.length) {
    const line = lines[i]

    if (line.startsWith('```')) {
      const lang = line.slice(3).trim()
      const buf: string[] = []
      i++
      while (i < lines.length && !lines[i].startsWith('```')) {
        buf.push(lines[i])
        i++
      }
      i++
      out.push(<CodeBlock key={key++} code={buf.join('\n')} lang={lang} className="my-2" />)
      continue
    }

    if (line.startsWith('### ') || line.startsWith('## ')) {
      out.push(<div key={key++} className="text-[14px] font-semibold mt-3 mb-1">{inline(line.replace(/^#+\s*/, ''))}</div>)
      i++
      continue
    }

    if (/^\s*[-*]\s+/.test(line)) {
      const items: string[] = []
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*[-*]\s+/, ''))
        i++
      }
      out.push(<ul key={key++} className="list-disc pl-5 my-1.5 space-y-0.5">{items.map((t, n) => <li key={n}>{inline(t)}</li>)}</ul>)
      continue
    }

    if (line.startsWith('> ')) {
      const buf: string[] = []
      while (i < lines.length && lines[i].startsWith('> ')) {
        buf.push(lines[i].slice(2))
        i++
      }
      out.push(<blockquote key={key++} className="border-l-2 border-accent bg-panel2 px-2.5 py-1.5 my-2 rounded-r-md text-inkdim text-[13px]">{buf.join(' ')}</blockquote>)
      continue
    }

    if (line.trim() === '') {
      i++
      continue
    }

    const buf: string[] = []
    while (i < lines.length && lines[i].trim() !== '' && !lines[i].startsWith('```') && !/^\s*[-*]\s+/.test(lines[i]) && !lines[i].startsWith('> ')) {
      buf.push(lines[i])
      i++
    }
    out.push(<p key={key++} className="my-1.5 leading-[1.7] text-[13px]">{inline(buf.join(' '))}</p>)
  }

  return <div className="md">{out}</div>
}

function inline(text: string): React.ReactNode {
  const parts: React.ReactNode[] = []
  const regex = /(\*\*[^*]+\*\*|`[^`]+`)/g
  let last = 0
  let m: RegExpExecArray | null
  let k = 0
  while ((m = regex.exec(text))) {
    if (m.index > last) parts.push(...linkifyPaths(text.slice(last, m.index)))
    const tok = m[0]
    if (tok.startsWith('**')) {
      parts.push(<strong key={k++}>{tok.slice(2, -2)}</strong>)
    } else {
      const inner = tok.slice(1, -1)
      // 行内代码为真实文件路径时，渲染为高亮路径胶囊（对齐桌面端 PathLink）
      if (isPotentialPath(inner)) {
        parts.push(<PathChip key={k++} path={inner} />)
      } else {
        parts.push(<code key={k++} className="font-mono text-[12px] bg-panel2 px-1 py-0.5 rounded">{inner}</code>)
      }
    }
    last = m.index + tok.length
  }
  if (last < text.length) parts.push(...linkifyPaths(text.slice(last)))
  if (!parts.length) return text
  return parts.map((p, i) => (typeof p === 'string' ? p : <React.Fragment key={i}>{p}</React.Fragment>))
}
