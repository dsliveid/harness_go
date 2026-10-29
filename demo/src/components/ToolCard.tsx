import { useState } from 'react'
import {
  Bot, Terminal, FileText, FileCode2, Search, Wrench,
  ChevronDown, ChevronRight, Clock, ArrowRight, Download
} from 'lucide-react'
import type { ToolCall } from '../types'
import { Badge, Button, StateBadge, cn } from './ui'
import { CodeBlock } from './CodeBlock'
import { DiffView } from './DiffView'
import { useUI } from '../stores/uiStore'
import { useWorkspace } from '../stores/workspaceStore'

const RISK: Record<string, { tone: 'gray' | 'blue' | 'yellow' | 'red'; text: string }> = {
  readonly: { tone: 'gray', text: '只读' },
  write: { tone: 'blue', text: '写入' },
  command: { tone: 'yellow', text: '命令' },
  danger: { tone: 'red', text: '高危' },
}

function getToolIcon(name: string) {
  if (name.includes('command') || name.includes('bash') || name.includes('terminal')) {
    return <Terminal size={12} className="text-amber-400 shrink-0" />
  }
  if (name.includes('write') || name.includes('edit')) {
    return <FileText size={12} className="text-accent shrink-0" />
  }
  if (name.includes('read') || name.includes('cat')) {
    return <FileCode2 size={12} className="text-accent2 shrink-0" />
  }
  if (name.includes('search') || name.includes('grep') || name.includes('find')) {
    return <Search size={12} className="text-purple-400 shrink-0" />
  }
  if (name.includes('subagent') || name.includes('subprocess')) {
    return <Bot size={12} className="text-cyan-400 shrink-0" />
  }
  return <Wrench size={12} className="text-inkdim shrink-0" />
}

export function ToolCard({ call, onRevertHunk }: { call: ToolCall; onRevertHunk?: (hunkId: string) => void }) {
  const [open, setOpen] = useState(call.status === 'running' || call.status === 'pending')
  const risk = RISK[call.risk] || RISK.readonly
  const hasBody = Boolean(call.result || call.diff || Object.keys(call.args || {}).length)
  const { toast } = useUI()

  const isSpawn = call.name === 'spawn_subagent' || call.name === 'spawn_subprocess'
  const { subagents } = useWorkspace()
  const { setSubagent, panel } = useUI()
  const spawnId = isSpawn && typeof call.args?.subagentId === 'string' ? call.args.subagentId : ''
  const spawnTarget = spawnId ? subagents.find((a) => a.id === spawnId) : undefined

  // 多模态制品（generate_image / recognize_image）：提供下载入口（正式实现走制品 CAS 下载 API）
  const isImageTool = call.name === 'generate_image' || call.name === 'recognize_image'
  const artifactName =
    typeof call.args?.filename === 'string'
      ? call.args.filename
      : typeof call.args?.prompt === 'string'
        ? `generated-${call.name}-${call.id}.png`
        : `${call.name}-artifact.png`
  const downloadArtifact = () => {
    toast('success', `已开始下载制品 ${artifactName}（正式实现走 GET /api/artifacts/:id，走鉴权与审计）`)
  }

  return (
    <div className={cn(
      'rounded-xl border bg-panel overflow-hidden transition-all duration-150',
      call.status === 'denied' ? 'border-warn/40 bg-warn/[0.02]' : 'border-edge hover:border-edge2',
    )}>
      <button
        type="button"
        className="w-full flex items-center gap-2 px-3 h-9 text-left hover:bg-panel2/60 transition-colors select-none"
        onClick={() => hasBody && setOpen(!open)}
      >
        <span className="text-inkfaint w-4 h-4 flex items-center justify-center shrink-0">
          {hasBody ? (open ? <ChevronDown size={12} /> : <ChevronRight size={12} />) : '·'}
        </span>

        {getToolIcon(call.name)}

        <span className="font-mono text-[12px] font-semibold text-ink shrink-0 tracking-tight">{call.name}</span>

        <Badge tone={risk.tone}>{risk.text}</Badge>
        <StateBadge state={call.status} />

        <span className="flex-1" />

        {spawnTarget && (
          <span
            role="button"
            tabIndex={0}
            title={`查看子进程对话详情：${spawnTarget.name}`}
            onClick={(e) => { e.stopPropagation(); setSubagent(spawnTarget.id) }}
            className={cn(
              'inline-flex items-center gap-1.5 rounded-lg px-2 py-0.5 text-[11px] font-medium transition-all cursor-pointer',
              panel === 'subagent'
                ? 'bg-cyan-500/20 text-cyan-500 shadow-sm'
                : 'bg-cyan-500/10 text-cyan-600 hover:bg-cyan-500/20',
            )}
          >
            <Bot size={11} className="shrink-0" />
            <span>{spawnTarget.name}</span>
            <ArrowRight size={10} className="opacity-75" />
          </span>
        )}

        {call.durationMs !== undefined && call.durationMs > 0 && (
          <span className="text-[11px] text-inkfaint font-mono shrink-0 flex items-center gap-1">
            <Clock size={10} />
            <span>{call.durationMs}ms</span>
          </span>
        )}
      </button>

      {open && (
        <div className="px-3.5 pb-3.5 space-y-2.5 border-t border-edge pt-2.5 bg-panel2">
          {Object.keys(call.args || {}).length > 0 && (
            <div className="flex flex-wrap gap-1.5 text-[11px]">
              {Object.entries(call.args).map(([k, v]) => (
                <span key={k} className="inline-flex items-center gap-1 rounded-md bg-panel px-2 py-0.5">
                  <span className="text-inkfaint">{k}:</span>
                  <span className="text-ink font-mono font-medium max-w-[280px] truncate" title={typeof v === 'string' ? v : JSON.stringify(v)}>
                    {typeof v === 'string' ? v : JSON.stringify(v)}
                  </span>
                </span>
              ))}
            </div>
          )}

          {call.status === 'denied' && (
            <div className="text-[12px] text-warn bg-warn/10 rounded-lg p-2 font-medium">
              已拒绝：{call.deniedReason || '用户拒绝，原因已回传模型'}
            </div>
          )}

          {call.diff && <DiffView diff={call.diff} onRevertHunk={onRevertHunk} compact />}

          {call.result && !call.diff && (
            call.result.includes('\n') || call.result.length > 80
              ? <CodeBlock code={call.result} lang={call.name === 'run_command' ? 'bash' : 'text'} />
              : (
                <div className="text-[12px] text-ink font-mono bg-panel p-2.5 rounded-lg break-words">
                  {call.result}
                </div>
              )
          )}

          {isImageTool && call.status === 'ok' && (
            <div className="flex items-center gap-2 rounded-lg border border-edge bg-panel p-2.5">
              <span className="text-[11.5px] text-inkdim truncate flex-1" title={artifactName}>
                制品：<span className="font-mono text-ink">{artifactName}</span>
              </span>
              <Button size="sm" variant="outline" className="gap-1.5 shrink-0" onClick={downloadArtifact} title="下载该制品到本地">
                <Download size={12} />
                <span>下载</span>
              </Button>
            </div>
          )}

          {call.status === 'running' && (
            <div className="flex items-center gap-1.5 text-[12px] text-accent py-1">
              <span className="w-1.5 h-1.5 rounded-full bg-accent inline-block typing-dot" />
              <span className="w-1.5 h-1.5 rounded-full bg-accent inline-block typing-dot" />
              <span className="w-1.5 h-1.5 rounded-full bg-accent inline-block typing-dot" />
              <span className="ml-1 font-medium">执行中…</span>
            </div>
          )}
        </div>
      )}
    </div>
  )
}
