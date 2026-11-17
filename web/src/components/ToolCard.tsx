import { useEffect, useRef, useState } from 'react'
import {
  Bot, Terminal, FileText, FileCode2, Search, Wrench,
  ChevronDown, ChevronRight, Clock, ArrowRight, Download, Copy, Check, FolderOpen
} from 'lucide-react'
import type { ToolCall } from '../types'
import { Badge, Button, StateBadge, cn } from './ui'
import { CodeBlock } from './CodeBlock'
import { DiffView } from './DiffView'
import { useUI } from '../stores/uiStore'
import { useWorkspace } from '../stores/workspaceStore'
import { buildCommandLine, commandDir, commandTimeout, isCommandTool } from '../lib/commandLine'

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
  // 折叠状态机（与 ProcessDrawer / ReasoningCard 统一）：
  // - 执行中（running/pending）自动展开，便于看实时输出
  // - 进入终态（ok/error/denied）后自动收起，避免历史卡片占满屏幕
  // - 用户手动点击后以用户为准（userOpen 非 null），直到状态再次跃迁才复位
  const live = call.status === 'running' || call.status === 'pending'
  const [userOpen, setUserOpen] = useState<boolean | null>(null)
  const open = userOpen ?? live
  const prevStatus = useRef(call.status)
  useEffect(() => {
    const was = prevStatus.current === 'running' || prevStatus.current === 'pending'
    prevStatus.current = call.status
    // 运行态 → 终态：复位用户干预，回到「跟随状态」→ 自动收起
    if (was && !live) setUserOpen(null)
  }, [call.status, live])
  // 实时输出折叠面板：默认展开，用户可收起
  const [liveOpen, setLiveOpen] = useState(true)
  const { toast } = useUI()
  const risk = RISK[call.risk] || RISK.readonly
  // 命令类工具（run_command / shell 等）：命令行由 {cmd, args[]} 拼装而成，
  // 后端不下发 cmdline 字段，故此处显式拼出完整命令行并单独呈现，
  // 不再只靠 args chip 里的 `cmd: xxx`（会被 max-w-280px 截断，看不到完整命令）。
  const isCmd = isCommandTool(call.name, call.risk)
  const cmdLine = isCmd ? buildCommandLine(call.args) : ''
  const cmdDir = isCmd ? commandDir(call.args) : ''
  const cmdTimeout = isCmd ? commandTimeout(call.args) : 0
  // 命令类参数已由「完整命令行 + 上下文」完整呈现，args chip 不再重复展示
  const CMD_ARG_KEYS = new Set(['cmd', 'command', 'args', 'dir', 'timeout_s'])
  const argEntries = Object.entries(call.args || {}).filter(
    ([k]) => !(isCmd && CMD_ARG_KEYS.has(k)),
  )
  const [cmdCopied, setCmdCopied] = useState(false)
  const copyCmd = async () => {
    try {
      await navigator.clipboard.writeText(cmdLine)
      setCmdCopied(true)
      setTimeout(() => setCmdCopied(false), 1400)
    } catch {
      setCmdCopied(false)
      toast('error', '复制失败：浏览器未授予剪贴板权限，请手动选中命令行复制')
    }
  }
  const hasBody = Boolean(call.result || call.liveOutput || call.diff || Object.keys(call.args || {}).length)
  // 实时输出容器：新 chunk 到达时自动滚底（用户手动上翻查看历史时不被强制拉回）
  const liveRef = useRef<HTMLPreElement>(null)
  useEffect(() => {
    const el = liveRef.current
    if (!el) return
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 48
    if (nearBottom) el.scrollTop = el.scrollHeight
  }, [call.liveOutput])

  const isSpawn = call.name === 'spawn_subagent' || call.name === 'spawn_subprocess'
  const { subagents } = useWorkspace()
  const { setSubagent, panel } = useUI()
  // real 模式：后端 spawn_subagent 的参数只有 {task, model_ref}，没有 subagentId，
  // 但派生层把子代理 id 定为该次工具调用的 tool_event_id（即 call.id），
  // 故回退用 call.id 匹配，否则真实子代理永远无法从工具卡片跳进详情。
  const spawnId = isSpawn
    ? (typeof call.args?.subagentId === 'string' ? call.args.subagentId : call.id)
    : ''
  const spawnTarget = spawnId ? subagents.find((a) => a.id === spawnId) : undefined

  // 多模态制品（generate_image / recognize_image）：后端 image 插件已真实注册这两个工具
  // （参数见 manifest：generate=prompt+path，recognize=path+question），此处提供制品下载入口。
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
        onClick={() => hasBody && setUserOpen(!open)}
      >
        <span className="text-inkfaint w-4 h-4 flex items-center justify-center shrink-0">
          {hasBody ? (open ? <ChevronDown size={12} /> : <ChevronRight size={12} />) : '·'}
        </span>

        {getToolIcon(call.name)}

        <span className="font-mono text-[12px] font-semibold text-ink shrink-0 tracking-tight">{call.name}</span>

        {/* 命令类工具：标题行直接露出命令行预览（单行省略，hover 出全量；展开后有完整块）。
            这样折叠状态下也能一眼看出「跑的是什么命令」，而不是只看到一个 run_command 工具名。 */}
        {cmdLine && (
          <span
            className="min-w-0 flex-1 truncate font-mono text-[11.5px] text-inkdim"
            title={cmdLine}
          >
            {cmdLine}
          </span>
        )}

        <Badge tone={risk.tone}>{risk.text}</Badge>
        <StateBadge state={call.status} />

        {/* 命令行预览已占用弹性空间时不再补占位，避免右侧信息被挤成一半宽度 */}
        {!cmdLine && <span className="flex-1" />}

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
          {/* 完整命令行：后端只下发 {cmd, args[]}，命令行是拼出来的。
              这里用 whitespace-pre-wrap + break-all 保证长命令自动折行、绝不省略号截断，
              并附一键复制（复制的是可直接粘进终端的命令串，含参数引号转义）。 */}
          {cmdLine && (
            <div className="rounded-lg border border-edge/70 bg-[#090d14] overflow-hidden">
              <div className="flex items-center justify-between gap-2 px-2.5 h-7 border-b border-edge/60 bg-panel2/60">
                <span className="inline-flex items-center gap-1.5 text-[10.5px] text-inkfaint font-mono select-none">
                  <Terminal size={10} className="text-amber-400" />
                  完整命令行
                </span>
                <button
                  type="button"
                  className="inline-flex items-center gap-1 text-[10.5px] text-inkfaint hover:text-ink transition-colors px-1 py-0.5 rounded hover:bg-panel3"
                  onClick={copyCmd}
                  title="复制完整命令行"
                >
                  {cmdCopied ? <Check size={11} className="text-ok" /> : <Copy size={11} />}
                  <span>{cmdCopied ? '已复制' : '复制'}</span>
                </button>
              </div>
              <code className="block px-2.5 py-2 font-mono text-[11.5px] leading-5 text-[#e6edf3] whitespace-pre-wrap break-all select-text">
                {cmdLine}
              </code>
              {(cmdDir || cmdTimeout > 0) && (
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-2.5 py-1 border-t border-edge/40 text-[10.5px] text-inkfaint font-mono">
                  {cmdDir && (
                    <span className="inline-flex items-center gap-1">
                      <FolderOpen size={10} />
                      cwd: {cmdDir}
                    </span>
                  )}
                  {cmdTimeout > 0 && <span>timeout: {cmdTimeout}s</span>}
                </div>
              )}
            </div>
          )}

          {argEntries.length > 0 && (
            <div className="flex flex-wrap gap-1.5 text-[11px]">
              {argEntries.map(([k, v]) => (
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
              // 命令类工具的返回值是 stdout/stderr（输出），不是命令本身；
              // 标签写 bash 会和上方「完整命令行」混淆，故明确标为「输出」。
              ? <CodeBlock code={call.result} lang={isCmd ? '输出' : 'text'} />
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

          {/* 实时增量输出（tool.progress，200ms 节流推送）：命令执行中的流式回显；
              终态后以 result 展示（completed 时 liveOutput 不再渲染）。
              payload 只有 tool_event_id + chunk，无任何百分比/ETA 概念，此处如实只显示累计文本。 */}
          {call.status === 'running' && call.liveOutput && (
            <div className="rounded-lg border border-edge/60 bg-panel overflow-hidden">
              <div className="flex items-center justify-between px-2.5 py-1 bg-panel2/60 border-b border-edge/40">
                <span className="inline-flex items-center gap-1 text-[10.5px] text-inkfaint font-mono">
                  <Terminal size={10} className="text-accent2" />
                  实时输出 · {call.liveOutput.length.toLocaleString('zh-CN')} 字符
                </span>
                <button
                  type="button"
                  className="text-[10.5px] text-inkfaint hover:text-ink transition-colors"
                  onClick={() => setLiveOpen((v) => !v)}
                  title={liveOpen ? '收起实时输出' : '展开实时输出'}
                >
                  {liveOpen ? '收起' : '展开'}
                </button>
              </div>
              {liveOpen && (
                <pre
                  ref={liveRef}
                  className="text-[11.5px] leading-5 font-mono text-inkdim bg-panel p-2.5 max-h-48 overflow-auto whitespace-pre-wrap break-words"
                >
                  {call.liveOutput}
                </pre>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
