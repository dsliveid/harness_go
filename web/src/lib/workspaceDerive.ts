import type {
  CommandRun, Message, PlanDoc, PlanStep, Subagent, TodoItem, ToolCall,
} from '../types'

/**
 * 运行态派生层：把「真实工具调用序列」折算成右侧浮动编排栏要展示的四类数据。
 *
 * 为什么是派生而不是拉接口：后端 REST 层**没有** todo / plan / subagent / command
 * 的专用端点（`internal/api/router.go` 全部路由已核对）。这四类数据的唯一真源
 * 是 agent 工具调用的产物：
 *   - `todo_write`（internal/plugins/todo）   → 任务清单（整体覆写语义）
 *   - `plan_set`  （internal/plugins/plan）   → 执行计划（标题 + 步骤）
 *   - `spawn_subagent`（internal/plugins/subagent）→ 子代理（阻塞等待子 Run 终态）
 *   - `run_command`（internal/plugins/command）→ 命令行（增量输出走 tool.progress）
 * 这些调用及其 args/status 已经由 SSE 实时落进 `chatStore.messages[].toolCalls`，
 * 历史会话也会经 `GET /sessions/{sid}/messages` 回灌成同样的形状，因此派生层
 * 天然同时具备「实时」与「刷新后回灌」两种能力，无需额外接口。
 *
 * 铁律：本模块只做**纯字段映射与归并**，绝不补造后端未提供的字段。
 * 后端没有的东西一律如实留空 / 置中性值，由 UI 显式呈现「未提供」。
 */

/** 后端工具名常量（与各插件 manifest.json 里的 tools[].name 逐字对齐） */
const TOOL = {
  todoWrite: 'todo_write',
  planSet: 'plan_set',
  spawnSubagent: 'spawn_subagent',
  runCommand: 'run_command',
} as const

export interface WorkspaceDerived {
  todos: TodoItem[]
  plans: PlanDoc[]
  subagents: Subagent[]
  commands: CommandRun[]
}

/** 归并后的单条工具调用：同一 tool_event_id 的 started/completed 只保留最新态 */
interface CallEntry {
  id: string
  call: ToolCall
  /** 该调用所属消息的落库时刻（后端未下发工具级时间戳，仅能用消息时间作近似基准） */
  at: number
}

/**
 * 把消息序列摊平成「按首次出现排序、按 id 去重取最新」的调用列表。
 *
 * 必须归并的原因：SSE 会先发 `tool.started`（status=running）再发 `tool.completed`
 * （status=ok/error），两者共用同一个 tool_event_id。若不去重，同一次命令执行会
 * 在列表里出现两条（一条还在「运行中」）。
 */
function flattenCalls(messages: Message[]): CallEntry[] {
  const order: string[] = []
  const latest = new Map<string, ToolCall>()
  const firstAt = new Map<string, number>()

  for (const m of messages) {
    const calls = m.toolCalls ?? []
    calls.forEach((c, idx) => {
      // 兜底 id：极少数历史记录 tool_event_id 缺失，用消息 id + 序号保证唯一
      const id = c.id || `${m.id}#${idx}`
      if (!latest.has(id)) {
        order.push(id)
        firstAt.set(id, m.createdAt)
      }
      latest.set(id, c)
    })
  }

  return order.map((id) => ({ id, call: latest.get(id) as ToolCall, at: firstAt.get(id) ?? 0 }))
}

/** 调用是否处于「不可采信」的终态：出错 / 被拒时不应用它改写运行态 */
function isRejected(call: ToolCall): boolean {
  return call.status === 'error' || call.status === 'denied'
}

/**
 * 任务清单：取**最后一次**未被拒/未出错的 `todo_write`。
 *
 * 依据后端语义：`todo_write` 是整体覆写（`internal/plugins/todo/todo.go`），
 * 所以只有最后一次有意义，逐次累加会得到一份错误的合并清单。
 * 运行中（status=running）也采信——args 在 tool.started 时已完整下发，
 * 这样清单能随 agent 写入即刻刷新，而不是等整轮结束才出现。
 */
function deriveTodos(entries: CallEntry[]): TodoItem[] {
  for (let i = entries.length - 1; i >= 0; i--) {
    const { call } = entries[i]
    if (call.name !== TOOL.todoWrite || isRejected(call)) continue

    const raw = call.args?.items
    if (!Array.isArray(raw)) continue

    const out: TodoItem[] = []
    for (const item of raw) {
      const it = item as { content?: unknown; status?: unknown }
      const content = typeof it.content === 'string' ? it.content.trim() : ''
      if (!content) continue
      // 后端枚举固定为 pending | in_progress | done，非法值归一到 pending（不臆造第四态）
      const status: TodoItem['status'] =
        it.status === 'done' ? 'done' : it.status === 'in_progress' ? 'in_progress' : 'pending'
      out.push({ id: `todo-${out.length}`, content, status })
    }
    if (out.length === 0) continue
    return out
  }
  return []
}

/**
 * 执行计划：取最后一次未被拒的 `plan_set`。
 *
 * 同样按「整体覆写」处理（plan.go 是写文件覆盖）。步骤的完成态来自 args.steps[].done，
 * 后端只有布尔 done，UI 的三态中 `in_progress` 后端不提供 → 一律不臆造，只映射 done/pending。
 */
function derivePlans(entries: CallEntry[]): PlanDoc[] {
  for (let i = entries.length - 1; i >= 0; i--) {
    const { id, call, at } = entries[i]
    if (call.name !== TOOL.planSet || isRejected(call)) continue

    const title = typeof call.args?.title === 'string' ? call.args.title.trim() : ''
    const rawSteps = call.args?.steps
    if (!title || !Array.isArray(rawSteps) || rawSteps.length === 0) continue

    const steps: PlanStep[] = []
    rawSteps.forEach((item, idx) => {
      const s = item as { title?: unknown; done?: unknown }
      const content = typeof s.title === 'string' ? s.title.trim() : ''
      if (!content) return
      steps.push({ id: `ps-${idx}`, content, status: s.done === true ? 'done' : 'pending' })
    })
    if (steps.length === 0) continue

    return [{
      id,
      title,
      // 后端无版本号下发 → 固定 1，不假冒 v2/v3 的迭代事实
      version: 1,
      // 当前会话内最新的计划即视为活跃计划（与后端「唯一计划文件」语义一致）
      active: true,
      steps,
      updatedAt: at,
    }]
  }
  return []
}

/** 工具调用终态 → 子代理状态（显式终态，绝不以「没在跑」推断成功） */
function subagentStatus(call: ToolCall): Subagent['status'] {
  switch (call.status) {
    case 'running':
    case 'pending':
      return 'running'
    case 'ok':
      return 'completed'
    case 'denied':
      return 'cancelled'
    default:
      return 'failed'
  }
}

/**
 * 子代理：`spawn_subagent` 每次调用即一个子 Run。
 *
 * 后端语义是阻塞等待子 Run 终态（subagent.go 里 child.Wait），因此同一 tool_event_id
 * 只有 running → 终态 两个阶段，归并后即一条记录。
 * 后端未下发：子代理命名 / 细分角色 / 子 Run 的 token 与对话转录 → 如实留空（tokens=0、
 * messages=[]），由 UI 显示空态，绝不填充样例转录冒充真实过程。
 */
function deriveSubagents(entries: CallEntry[]): Subagent[] {
  const out: Subagent[] = []
  for (const { id, call, at } of entries) {
    if (call.name !== TOOL.spawnSubagent) continue
    const task = typeof call.args?.task === 'string' ? call.args.task.trim() : ''
    if (!task) continue
    out.push({
      id,
      name: `子代理 ${out.length + 1}`,
      role: '子代理',
      status: subagentStatus(call),
      summary: task,
      tokens: 0,
      startedAt: at,
      messages: [],
    })
  }
  return out
}

/** 拼出可读命令行（后端 args 形如 {cmd, args[], dir, timeout_s}） */
function buildCmdLine(args: Record<string, unknown> | undefined): string {
  const cmd = typeof args?.cmd === 'string' ? args.cmd.trim() : ''
  if (!cmd) return ''
  const rest = Array.isArray(args?.args)
    ? args.args.filter((a): a is string => typeof a === 'string')
    : []
  return [cmd, ...rest].join(' ')
}

/**
 * 命令行：`run_command`（risk=command）的每次调用。
 *
 * 后端未下发真实 PID 与进程启动时刻 → `pid` 固定 0（UI 在 pid===0 时不渲染 PID 行，
 * 不编造进程号），`startedAt` 用该轮消息落库时刻近似。
 */
function deriveCommands(entries: CallEntry[]): CommandRun[] {
  const out: CommandRun[] = []
  for (const { id, call, at } of entries) {
    // 兼容按 risk 识别：任何命令级风险工具都归入命令行面板
    if (call.name !== TOOL.runCommand && call.risk !== 'command') continue
    const cmd = buildCmdLine(call.args)
    if (!cmd) continue

    const status: CommandRun['status'] =
      call.status === 'running' || call.status === 'pending'
        ? 'running'
        : call.status === 'denied'
          ? 'cancelled'
          : 'exited'

    out.push({
      id,
      cmd,
      pid: 0,
      startedAt: at,
      status,
      // 增量输出优先（tool.progress 实时累积），终态后由 result 覆盖
      output: call.liveOutput ?? call.result,
    })
  }
  return out
}

/** 从消息序列一次性派生四类运行态数据（纯函数，无副作用） */
export function deriveWorkspace(messages: Message[]): WorkspaceDerived {
  const entries = flattenCalls(messages)
  return {
    todos: deriveTodos(entries),
    plans: derivePlans(entries),
    subagents: deriveSubagents(entries),
    commands: deriveCommands(entries),
  }
}
