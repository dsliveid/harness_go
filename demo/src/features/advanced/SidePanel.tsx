import { useState, type ReactNode } from 'react'
import { useUI, type SidePanel } from '../../stores/uiStore'
import { useWorkspace } from '../../stores/workspaceStore'
import { useSessions } from '../../stores/sessionStore'
import { useChat } from '../../stores/chatStore'
import { Badge, Button, cn, Drawer, StateBadge } from '../../components/ui'
import { DiffView } from '../../components/DiffView'
import { CodeBlock } from '../../components/CodeBlock'
import { sampleDiff } from '../../mock/data'
import { MessageThread } from '../chat/MessageList'
import { Check, Square, RotateCcw, X, ArrowLeft, Bot, FileText, CheckSquare, ListTodo, TerminalSquare, Layers, ShieldAlert, ChevronRight, ChevronDown, Download } from 'lucide-react'
import type { CommandRun, FileDiff, Message, PlanDoc, Subagent } from '../../types'

const TITLES: Record<Exclude<SidePanel, 'none'>, string> = {
  todo: '任务清单',
  plan: '方案',
  commands: '命令行',
  runs: '子进程',
  subagent: '子进程详情',
  snapshot: '快照与回滚',
  file: '文件详情',
}

const PANEL_ICONS: Record<Exclude<SidePanel, 'none'>, ReactNode> = {
  todo: <CheckSquare size={14} className="text-purple-400" />,
  plan: <ListTodo size={14} className="text-emerald-400" />,
  commands: <TerminalSquare size={14} className="text-amber-400" />,
  runs: <Layers size={14} className="text-cyan-400" />,
  subagent: <Bot size={14} className="text-cyan-400" />,
  snapshot: <RotateCcw size={14} className="text-accent" />,
  file: <FileText size={14} className="text-accent2" />,
}

export function SidePanel() {
  const { panel, setPanel, breakpoint, subagentId } = useUI()
  const { subagents } = useWorkspace()
  if (panel === 'none') return null

  const agent = subagents.find((a) => a.id === subagentId)
  const agentTitle = agent ? `${agent.name} · ${agent.role}` : '子进程详情'
  const isThread = panel === 'subagent' || (panel === 'runs' && Boolean(subagentId))
  const title = isThread ? agentTitle : TITLES[panel]

  const content = (
    <>
      <div className="h-12 px-3.5 flex items-center gap-2 border-b border-edge shrink-0 bg-panel2/40 select-none">
        <span className="shrink-0">{isThread ? <Bot size={14} className="text-cyan-400" /> : PANEL_ICONS[panel]}</span>
        <span className="text-[13px] font-semibold flex-1 truncate text-ink tracking-tight" title={title}>{title}</span>
        {/* 返回按钮：位于关闭按钮之前（子进程列表 → 详情时显示） */}
        {panel === 'runs' && subagentId && (
          <Button size="sm" variant="outline" onClick={() => setPanel('runs')} title="返回子进程列表" className="gap-1 h-7 px-2 text-[12px]">
            <ArrowLeft size={12} />
            <span>返回</span>
          </Button>
        )}
        <button
          className="w-7 h-7 rounded-lg inline-flex items-center justify-center text-inkfaint hover:text-ink hover:bg-panel2 transition-colors"
          onClick={() => setPanel('none')}
          aria-label="关闭面板"
        >
          <X size={15} />
        </button>
      </div>
      {isThread && agent ? (
        /* 线程面板：完整复用主对话 MessageThread 渲染（虚拟列表 + 贴底），仅外层容器为固定高 flex 列 */
        <div className="flex-1 min-h-0 flex flex-col">
          <SubagentDetailHeader agent={agent} />
          <MessageThread
            messages={toThreadMsgs(agent)}
            plain
            showCopy={false}
            emptyHint="该子进程暂无对话记录（可先恢复运行）"
            scrollCls="flex-1 min-h-0 overflow-y-auto px-3 py-3"
            actions={{}}
          />
        </div>
      ) : (
        <div className="flex-1 overflow-y-auto p-3.5 space-y-3">
          {panel === 'todo' && <TaskListPanel mode="todo" />}
          {panel === 'plan' && <TaskListPanel mode="plan" />}
          {panel === 'commands' && <CommandsPanel />}
          {panel === 'runs' && <SubagentsListPanel />}
          {panel === 'snapshot' && <SnapshotPanel />}
          {panel === 'file' && <FileDetailPanel />}
        </div>
      )}
    </>
  )
  if (breakpoint === 'desktop') {
    return <aside className="w-[410px] shrink-0 border-l border-edge bg-panel flex flex-col">{content}</aside>
  }
  return <Drawer open onClose={() => setPanel('none')} side="right" width="w-[92%] max-w-[420px]">{content}</Drawer>
}

// ---------------- 任务清单 / 方案（两个独立面板，各只显示对应列表） ----------------
function TaskListPanel({ mode }: { mode: 'todo' | 'plan' }) {
  const { todos, toggleTodo, plans } = useWorkspace()
  const activePlan = plans.find((p) => p.active)

  if (mode === 'todo') {
    if (todos.length === 0) {
      return <div className="py-6 text-center text-[12px] text-inkfaint">本轮对话暂无任务清单</div>
    }
    return (
      <div>
        <div className="text-[12px] text-inkdim mb-1.5 flex items-center gap-2">
          <span>任务清单</span>
          <span className="font-mono text-[11px] text-ok">
            {todos.filter((t) => t.status === 'done').length}/{todos.length}
          </span>
        </div>
        <div className="space-y-1.5">
          {todos.map((t) => (
            <button
              key={t.id}
              className="w-full flex items-start gap-2 rounded-lg border border-edge px-2.5 py-2 text-left hover:bg-panel2 transition-colors"
              onClick={() => toggleTodo(t.id)}
              title="点击切换完成状态"
            >
              {t.status === 'done'
                ? <Check size={13} className="mt-0.5 text-ok shrink-0" />
                : t.status === 'in_progress'
                  ? <Square size={13} className="mt-0.5 text-accent shrink-0" />
                  : <Square size={13} className="mt-0.5 text-inkfaint shrink-0" />}
              <span className={cn('text-[13px] flex-1', t.status === 'done' && 'text-inkfaint line-through')}>
                {t.content}
              </span>
              {t.status === 'in_progress' && <Badge tone="blue">进行中</Badge>}
            </button>
          ))}
        </div>
      </div>
    )
  }

  // plan 模式：方案详细信息 + 执行进度
  if (!activePlan) {
    return <div className="py-6 text-center text-[12px] text-inkfaint">当前没有进行中的方案</div>
  }

  return (
    <div>
      <div className="text-[12px] text-inkdim mb-1.5">
        方案 · {activePlan.title}
        <span className="ml-1.5 font-mono text-[11px] text-inkfaint">v{activePlan.version}</span>
        <span className="ml-2 font-mono text-[11px] text-ok">
          {activePlan.steps.filter((s) => s.status === 'done').length}/{activePlan.steps.length}
        </span>
      </div>
      <PlanDetailInfo plan={activePlan} />
      <div className="mt-3 mb-1.5 text-[12px] text-inkdim">执行进度</div>
      <div className="space-y-1.5">
        {activePlan.steps.map((s) => (
          <div key={s.id} className="flex items-start gap-2 rounded-lg border border-edge px-2.5 py-2">
            {s.status === 'done'
              ? <Check size={13} className="mt-0.5 text-ok shrink-0" />
              : s.status === 'in_progress'
                ? <span className="mt-1.5 w-1.5 h-1.5 rounded-full bg-accent animate-pulse shrink-0" />
                : <span className="mt-1.5 w-1.5 h-1.5 rounded-full bg-edge shrink-0" />}
            <span className={cn('text-[13px] flex-1', s.status === 'done' && 'text-inkfaint line-through')}>
              {s.content}
            </span>
            {s.status === 'in_progress' && <Badge tone="blue">进行中</Badge>}
          </div>
        ))}
      </div>
    </div>
  )
}

// ---------------- 方案详细信息（目标 / 架构 / 涉及文件 / 验证方式） ----------------
function PlanDetailInfo({ plan }: { plan: PlanDoc }) {
  const [open, setOpen] = useState(true)
  const hasDetail = Boolean(plan.goals || plan.architecture || plan.files?.length || plan.verification)
  if (!hasDetail) return null

  const sections: Array<{ label: string; body: ReactNode }> = []
  if (plan.goals) sections.push({ label: '目标', body: <p className="text-[12px] leading-relaxed text-inkdim">{plan.goals}</p> })
  if (plan.architecture) sections.push({ label: '架构方案', body: <p className="text-[12px] leading-relaxed text-inkdim">{plan.architecture}</p> })
  if (plan.files?.length) {
    sections.push({
      label: `涉及文件（${plan.files.length}）`,
      body: (
        <div className="space-y-0.5">
          {plan.files.map((f, i) => (
            <div key={i} className="font-mono text-[11px] text-inkdim break-all">{f}</div>
          ))}
        </div>
      ),
    })
  }
  if (plan.verification) sections.push({ label: '验证方式', body: <p className="text-[12px] leading-relaxed text-inkdim">{plan.verification}</p> })

  return (
    <div className="rounded-lg border border-edge bg-panel2/40">
      <button
        className="w-full flex items-center gap-1.5 px-2.5 py-2 text-left"
        onClick={() => setOpen((v) => !v)}
        title={open ? '收起方案详情' : '展开方案详情'}
      >
        <span className="text-[12px] font-medium text-inkdim flex-1">方案详情</span>
        {plan.status && (
          <Badge tone={plan.status === 'completed' ? 'green' : plan.status === 'suspended' ? 'gray' : 'blue'}>
            {plan.status === 'completed' ? '已完成' : plan.status === 'suspended' ? '已挂起' : '推进中'}
          </Badge>
        )}
        <ChevronRight size={12} className={cn('text-inkfaint transition-transform duration-200', open && 'rotate-90')} />
      </button>
      {open && (
        <div className="px-2.5 pb-2.5 space-y-2.5">
          {plan.path && (
            <div className="flex items-center gap-1.5 font-mono text-[11px] text-inkfaint break-all" title={plan.path}>
              <FileText size={12} className="shrink-0" />
              <span>{plan.path}</span>
            </div>
          )}
          {sections.map((sec) => (
            <div key={sec.label}>
              <div className="text-[11px] font-medium text-inkfaint uppercase tracking-wide mb-1">{sec.label}</div>
              {sec.body}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

// ---------------- 正在执行的命令行（可停止） ----------------
function CommandsPanel() {
  const { commands, stopCommand } = useWorkspace()
  const toast = useUI((s) => s.toast)
  const running = commands.filter((c) => c.status === 'running')

  if (running.length === 0) {
    return <div className="py-6 text-center text-[12px] text-inkfaint">当前没有正在执行的命令行</div>
  }

  return (
    <div className="space-y-3">
      <div className="text-[12px] text-inkdim">正在执行（{running.length}）· 点击停止终止进程组</div>
      <div className="space-y-1.5">
        {running.map((c) => <CommandRow key={c.id} cmd={c} onStop={() => { stopCommand(c.id); toast('info', `已停止命令行（PID ${c.pid}，终止进程组）`) }} />)}
      </div>
    </div>
  )
}

function CommandRow({ cmd, onStop }: { cmd: CommandRun; onStop: () => void }) {
  const elapsed = formatElapsed(Date.now() - cmd.startedAt)
  return (
    <div className="rounded-lg border border-edge p-2.5">
      <div className="flex items-center gap-2">
        <span className="w-2 h-2 rounded-full bg-ok animate-pulse shrink-0" title="执行中" />
        <code className="font-mono text-[12px] text-ink flex-1 min-w-0 truncate break-all" title={cmd.cmd}>{cmd.cmd}</code>
        <StateBadge state="running" />
      </div>
      <div className="mt-1.5 flex items-center gap-2 text-[11px] text-inkfaint font-mono">
        <span>PID {cmd.pid}</span>
        <span>已运行 {elapsed}</span>
        <span className="flex-1" />
        <Button size="sm" variant="outline" onClick={onStop}>停止</Button>
      </div>
    </div>
  )
}

const formatElapsed = (ms: number) => {
  const s = Math.max(0, Math.floor(ms / 1000))
  if (s < 60) return `${s}秒`
  const m = Math.floor(s / 60)
  return m < 60 ? `${m}分${s % 60}秒` : `${Math.floor(m / 60)}小时${m % 60}分`
}

// ---------------- 子进程列表（浮窗按钮入口；点击进入列表域内详情，可返回） ----------------
function SubagentsListPanel() {
  const { subagents } = useWorkspace()
  const { openRunsDetail } = useUI()

  if (subagents.length === 0) {
    return <div className="py-6 text-center text-[12px] text-inkfaint">本轮对话暂无子进程</div>
  }

  return (
    <div className="space-y-3">
      <div className="text-[12px] text-inkdim">子进程（{subagents.length}）· 点击查看对话详情</div>
      <div className="space-y-1.5">
        {subagents.map((a) => (
          <button
            key={a.id}
            className="w-full rounded-lg border border-edge p-2.5 text-left hover:bg-panel2 transition-colors"
            onClick={() => openRunsDetail(a.id)}
          >
            <div className="flex items-center gap-2">
              <span className="text-[13px] font-medium">{a.name}</span>
              <Badge>{a.role}</Badge>
              <StateBadge state={a.status} />
            </div>
            <div className="text-[11px] text-inkfaint mt-1 truncate">{a.summary}</div>
            <div className="text-[11px] text-inkfaint mt-0.5 font-mono">{a.tokens} tok</div>
          </button>
        ))}
      </div>
      <div className="text-[11px] text-inkfaint">
        子任务状态使用显式终态（completed/failed/interrupted/cancelled），不以「没在跑」推断成功。
      </div>
    </div>
  )
}

// ---------------- 子进程列表（仅子进程，无协作者 / 长任务） ----------------
function SubagentsPanel() {
  const { subagents } = useWorkspace()
  const { setSubagent } = useUI()

  if (subagents.length === 0) {
    return <div className="py-6 text-center text-[12px] text-inkfaint">本轮对话暂无子进程</div>
  }

  return (
    <div className="space-y-3">
      <div className="text-[12px] text-inkdim">子进程（{subagents.length}）· 点击查看对话详情</div>
      <div className="space-y-1.5">
        {subagents.map((a) => (
          <button
            key={a.id}
            className="w-full rounded-lg border border-edge p-2.5 text-left hover:bg-panel2 transition-colors"
            onClick={() => setSubagent(a.id)}
          >
            <div className="flex items-center gap-2">
              <span className="text-[13px] font-medium">{a.name}</span>
              <Badge>{a.role}</Badge>
              <StateBadge state={a.status} />
            </div>
            <div className="text-[11px] text-inkfaint mt-1 truncate">{a.summary}</div>
            <div className="text-[11px] text-inkfaint mt-0.5 font-mono">{a.tokens} tok</div>
          </button>
        ))}
      </div>
      <div className="text-[11px] text-inkfaint">
        子任务状态使用显式终态（completed/failed/interrupted/cancelled），不以「没在跑」推断成功。
      </div>
    </div>
  )
}

// ---------------- 子进程详情：完整复用主对话 MessageThread（无输入/复制/分支/撤回） ----------------
/** 把子进程消息映射为消息形状，并按提问自动推导轮次 */
function toThreadMsgs(agent: Subagent): Message[] {
  let seq = 0
  let turnSeq = 0
  return agent.messages.map((m) => ({
    id: m.id,
    sessionId: agent.id,
    seq: ++seq,
    turnSeq: m.role === 'user' ? ++turnSeq : turnSeq,
    role: m.role,
    content: m.content,
    createdAt: agent.startedAt + seq,
    streaming: m.streaming,
    tokens: m.tokens,
    promptTokens: m.promptTokens,
    cachedTokens: m.cachedTokens,
    durationMs: m.durationMs,
    toolCalls: m.toolCalls,
  }))
}

/** 详情头部：状态 / 停止 / 恢复 / 概要（不随对话滚动） */
function SubagentDetailHeader({ agent }: { agent: Subagent }) {
  const { stopSubagent, resumeSubagent } = useWorkspace()
  const toast = useUI((s) => s.toast)

  return (
    <div className="px-3 pt-3 pb-2 space-y-2 border-b border-edge shrink-0">
      <div className="flex items-center gap-2">
        <span className="text-[13px] font-medium truncate">{agent.name}</span>
        <Badge>{agent.role}</Badge>
        <StateBadge state={agent.status} />
        <span className="flex-1" />
        <span className="font-mono text-[11px] text-inkfaint shrink-0">{agent.tokens} tok</span>
        {agent.status === 'running'
          ? <Button size="sm" variant="danger" onClick={() => { stopSubagent(agent.id); toast('info', `已停止子进程「${agent.name}」`) }}>停止</Button>
          : (agent.status === 'interrupted' || agent.status === 'cancelled' || agent.status === 'failed') && (
            <Button size="sm" variant="primary" onClick={() => { resumeSubagent(agent.id); toast('success', `已恢复子进程「${agent.name}」（新建 Run）`) }}>恢复</Button>
          )}
      </div>
      <div className="text-[11px] text-inkfaint border-l-2 border-edge pl-2.5">{agent.summary}</div>
    </div>
  )
}

// ---------------- 快照 ----------------
function SnapshotPanel() {
  const { snapshots, revertSnapshot, reapplySnapshot } = useWorkspace()
  const toast = useUI((s) => s.toast)
  const [openId, setOpenId] = useState<string | null>(null)

  return (
    <div className="space-y-3">
      <div className="rounded-lg border border-edge bg-panel2/50 p-2.5 text-[11px] text-inkfaint space-y-0.5">
        <div>· 保留最近 50 轮；单文件 &gt; 5MB 跳过并标记</div>
        <div>· 仅覆盖经内核写入的路径；run_command 变更不可回滚</div>
      </div>

      {snapshots.map((s) => (
        <div key={s.id} className="rounded-lg border border-edge overflow-hidden">
          <button className="w-full flex items-center gap-2 px-2.5 py-2 text-left hover:bg-panel2" onClick={() => setOpenId(openId === s.id ? null : s.id)}>
            <span className="w-3.5 h-3.5 flex items-center justify-center shrink-0">
              {openId === s.id ? <ChevronDown size={12} className="text-inkfaint" /> : <ChevronRight size={12} className="text-inkfaint" />}
            </span>
            <span className="font-mono text-[12px] truncate flex-1">{s.filePath}</span>
            {s.isNewFile && <Badge tone="green">新建</Badge>}
            {s.skipped ? (
              <Badge tone="yellow">跳过</Badge>
            ) : s.revertedAt ? (
              <Badge tone="gray">已回滚</Badge>
            ) : (
              <Badge tone="blue">可回滚</Badge>
            )}
          </button>
          <div className="px-2.5 pb-1 text-[11px] text-inkfaint">
            第 {s.turnSeq} 轮 · {fmtSize(s.sizeBytes)} · {s.beforeHash ? s.beforeHash : '（无旧版本）'} → {s.afterHash}
          </div>
          {openId === s.id && (
            <div className="p-2 border-t border-edge space-y-2">
              {s.skipped ? (
                <div className="text-[12px] text-warn">文件超过 5MB，已跳过快照，本次修改不可回滚。</div>
              ) : (
                <DiffView diff={{ ...sampleDiff, path: s.filePath }} onRevertHunk={(h) => { toast('success', `已回滚 hunk ${h}（重新定位当前文件）`) }} />
              )}
              <div className="flex gap-2">
                {s.revertedAt ? (
                  <Button size="sm" variant="outline" onClick={() => { reapplySnapshot(s.id); toast('success', '已重新应用（正序写回）') }}>重新应用</Button>
                ) : (
                  <Button size="sm" variant="primary" disabled={s.skipped} onClick={() => { revertSnapshot(s.id); toast('success', '已回滚到该快照（校验当前 hash 后写回）') }}>回滚到此快照</Button>
                )}
              </div>
            </div>
          )}
        </div>
      ))}
    </div>
  )
}

// ---------------- 文件详情（仅该文件） ----------------
interface FileSource {
  content?: string
  diff?: FileDiff
  toolName?: string
}

function findFileInSession(filePath: string): FileSource | null {
  const { messagesOf } = useChat.getState()
  const currentId = useSessions.getState().currentId
  const base = filePath.replace(/\\/g, '/').split('/').pop() || filePath
  let best: FileSource | null = null

  for (const m of messagesOf(currentId)) {
    for (const c of m.toolCalls ?? []) {
      const argsPath = typeof c.args?.path === 'string' ? c.args.path : ''
      if (!argsPath) continue
      const match = argsPath.replace(/\\/g, '/') === filePath
        || argsPath.replace(/\\/g, '/').endsWith(`/${filePath}`)
        || filePath.endsWith(`/${argsPath.replace(/\\/g, '/')}`)
        || base === argsPath.replace(/\\/g, '/').split('/').pop()
      if (!match) continue
      if ((c.name === 'write_file' || c.name === 'edit_file') && c.status === 'ok') {
        // 写入/编辑优先级更高：读取结果会被后续修改覆盖
        best = { diff: c.diff, content: c.diff ? undefined : c.result, toolName: c.name }
      } else if (c.name === 'read_file' && c.result && !best) {
        best = { content: c.result, toolName: c.name }
      } else if (!best) {
        best = { content: c.result, toolName: c.name }
      }
    }
  }
  return best
}

function FileDetailPanel() {
  const { filePath, toast } = useUI()
  if (!filePath) {
    return <div className="py-6 text-center text-[12px] text-inkfaint">未指定文件</div>
  }
  const found = findFileInSession(filePath)
  const fileName = filePath.replace(/\\/g, '/').split('/').pop() || filePath

  // 制品下载：有内容时用 Blob 真实下载（正式实现走 GET /api/files/download，走鉴权与审计）
  const doDownload = () => {
    if (!found?.content) return
    const blob = new Blob([found.content], { type: 'text/plain;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = fileName
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(url)
    toast('success', `已开始下载 ${fileName}`)
  }

  return (
    <div className="space-y-3">
      <div className="rounded-lg border border-edge bg-panel2/50 px-2.5 py-2">
        <div className="flex items-center gap-2">
          <div className="min-w-0 flex-1">
            <div className="text-[13px] font-medium truncate" title={filePath}>{fileName}</div>
            <div className="text-[11px] text-inkfaint font-mono mt-0.5 break-all">{filePath}</div>
            {found?.toolName && (
              <div className="text-[11px] text-inkfaint mt-1">来源工具：{found.toolName}</div>
            )}
          </div>
          <Button
            size="sm"
            variant="outline"
            className="gap-1.5 shrink-0"
            disabled={!found?.content}
            title={found?.content ? '下载该文件到本地' : '本会话暂无该文件内容（实际实现走 read_file 后可下载）'}
            onClick={doDownload}
          >
            <Download size={12} />
            <span>下载</span>
          </Button>
        </div>
      </div>

      {found?.diff ? (
        <DiffView diff={found.diff} />
      ) : found?.content ? (
        <CodeBlock code={found.content} lang="text" />
      ) : (
        <div className="rounded-lg border border-dashed border-edge px-3 py-4 text-[12px] text-inkfaint">
          本会话中尚未读取或写入该文件的完整内容（仅出现过路径）。实际实现中此处将调用 read_file 展示全文详情。
        </div>
      )}
    </div>
  )
}

const fmtSize = (n: number) => (n > 1024 * 1024 ? `${(n / 1024 / 1024).toFixed(1)} MB` : `${Math.round(n / 1024)} KB`)
