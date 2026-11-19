import React from 'react'
import { CodeBlock } from './CodeBlock'
import { PathChip, isPotentialPath, linkifyPaths } from './PathChip'

/**
 * 原型级 Markdown 渲染：支持 ```代码块```、行内 code、**粗体**、#/##/###/#### 标题、
 * 无序列表（- *）、有序列表（1. 2.）、> 引用、表格之外的常见段落。
 * 正式实现替换为 react-markdown（保持同一组件签名）。
 *
 * ⚠️ 排版契约（与 web/src/index.css 的 .md 段配套，改这里前务必先读那段注释）：
 * - **间距、字号、行高一律由 .md 段的 CSS 决定**，本文件不写 my-* / text-[13px] 之类的
 *   工具类。原因：.md p { margin } 是简写（特异性 0,1,1），会整体覆盖 Tailwind 的
 *   my-*（0,1,0），两套并存时 tsx 里的类完全不生效，历史上正是「改了没效果 + 排版杂乱」的根源。
 * - 标题必须产出真实 h1~h4 语义标签，CSS 的 .md h2/.md h3 规则才会生效
 *   （历史 bug：全部渲染成 <div>，导致标题层级坍塌、字号无差异）。
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
      out.push(<CodeBlock key={key++} code={buf.join('\n')} lang={lang} />)
      continue
    }

    // 标题：产出真实语义标签，让 .md h1~h4 的层级样式生效
    const heading = /^(#{1,4})\s+(.*)$/.exec(line)
    if (heading) {
      const level = heading[1].length
      const content = inline(heading[2].trim())
      const Tag = (level === 1 ? 'h1' : level === 2 ? 'h2' : level === 3 ? 'h3' : 'h4') as 'h1'
      out.push(<Tag key={key++}>{content}</Tag>)
      i++
      continue
    }

    // 无序列表
    if (/^\s*[-*]\s+/.test(line)) {
      const items: string[] = []
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*[-*]\s+/, ''))
        i++
      }
      out.push(<ul key={key++}>{items.map((t, n) => <li key={n}>{inline(t)}</li>)}</ul>)
      continue
    }

    // 有序列表（历史完全缺此分支，"1. 2. 3." 的编号清单会被 join 成一大段文字）
    if (/^\s*\d+[.)]\s+/.test(line)) {
      const items: string[] = []
      let startNum = 1
      while (i < lines.length && /^\s*\d+[.)]\s+/.test(lines[i])) {
        if (!items.length) startNum = parseInt(lines[i].trimStart().match(/^(\d+)/)?.[1] ?? '1', 10) || 1
        items.push(lines[i].replace(/^\s*\d+[.)]\s+/, ''))
        i++
      }
      out.push(
        <ol key={key++} start={startNum !== 1 ? startNum : undefined}>
          {items.map((t, n) => <li key={n}>{inline(t)}</li>)}
        </ol>
      )
      continue
    }

    // 引用
    if (line.startsWith('> ')) {
      const buf: string[] = []
      while (i < lines.length && lines[i].startsWith('> ')) {
        buf.push(lines[i].slice(2))
        i++
      }
      out.push(<blockquote key={key++}>{inline(buf.join(' '))}</blockquote>)
      continue
    }

    if (line.trim() === '') {
      i++
      continue
    }

    // 普通段落（连续的标题/列表/引用/代码围栏都不算段落内）
    const buf: string[] = []
    while (
      i < lines.length &&
      lines[i].trim() !== '' &&
      !lines[i].startsWith('```') &&
      !/^\s*[-*]\s+/.test(lines[i]) &&
      !/^\s*\d+[.)]\s+/.test(lines[i]) &&
      !/^#{1,4}\s+/.test(lines[i]) &&
      !lines[i].startsWith('> ')
    ) {
      buf.push(lines[i])
      i++
    }
    out.push(<p key={key++}>{inline(buf.join(' '))}</p>)
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
        // 样式由 index.css 的 .md code 统一接管（中性墨色，不再用晃眼的蓝色）
        parts.push(<code key={k++}>{inner}</code>)
      }
    }
    last = m.index + tok.length
  }
  if (last < text.length) parts.push(...linkifyPaths(text.slice(last)))
  if (!parts.length) return text
  return parts.map((p, i) => (typeof p === 'string' ? p : <React.Fragment key={i}>{p}</React.Fragment>))
}
