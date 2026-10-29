import type { Approval, FileDiff, ToolCall } from '../types'
import { sampleDiff } from './data'

export type StreamEvent =
  | { type: 'delta'; text: string }
  | { type: 'reasoning'; text: string }
  | { type: 'tool_start'; call: ToolCall }
  | { type: 'tool_result'; callId: string; result: string; durationMs: number; diff?: FileDiff; skipped?: boolean }
  | { type: 'step'; tokens?: number; promptTokens?: number; cachedTokens?: number }
  | { type: 'approval'; approval: Approval }
  | { type: 'done'; tokensIn: number; tokensOut: number; cachedTokens?: number }
  | { type: 'error'; code: string; message: string }

export type Decision = 'allow_once' | 'allow_session' | 'deny'

export interface MockRunHandle {
  stop: () => void
  resume: (decision: Decision, reason?: string) => void
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms))

function pickScenario(input: string): 'write' | 'command' | 'plan' | 'browser' | 'read' {
  if (/改|修复|实现|写|编辑/.test(input)) return 'write'
  if (/测试|跑|命令|构建|执行/.test(input)) return 'command'
  if (/计划|拆解|待办|todo|plan/i.test(input)) return 'plan'
  if (/浏览器|截图|打开网页|browser/i.test(input)) return 'browser'
  return 'read'
}

const TEXTS: Record<string, string[]> = {
  write: [
    '我先确认目标文件与现有实现，再落改动。',
    '改动包含三处：空名校验、重复检测、以及 `defer Unlock` 释放锁。',
    '写入完成后会跑一次单测验证，不会跳过验证步骤。',
  ],
  command: [
    '执行 `go test ./internal/tools/...`，超时 120s。',
    '测试通过，覆盖率 78.3%。',
    '如需查看完整输出，可以展开工具卡片。',
  ],
  plan: [
    '已把改造拆成 4 步，并登记到任务清单。',
    '第 1 步「定义 Manifest 与状态机」已完成，当前推进第 2 步。',
    '计划文档位于 `.harness/plans/plugin-registry-hot-reload.md`。',
  ],
  browser: [
    '浏览器能力当前不可用：未检测到 Chromium。',
    '可以在「插件 / 制品」页安装 `@playwright/mcp`，或改用已有的 MCP Server。',
    '安装后工具将在下一轮对话生效。',
  ],
  read: [
    '我读取了相关文件，确认当前实现没有同名冲突检测。',
    '建议补充三处校验，避免插件热加载时静默覆盖已有工具。',
    '需要我直接改吗？',
  ],
}

export function startMockRun(opts: {
  sessionId: string
  runId: string
  input: string
  onEvent: (e: StreamEvent) => void
}): MockRunHandle {
  const { sessionId, runId, input, onEvent } = opts
  const scenario = pickScenario(input)
  let stopped = false
  let resumeResolve: ((d: { decision: Decision; reason?: string }) => void) | null = null

  const emit = (e: StreamEvent) => {
    if (!stopped) onEvent(e)
  }

  const streamText = async (text: string) => {
    // 按 2~4 字符切片，模拟逐 token；真实实现由 UI 侧 50ms 批量提交
    let i = 0
    while (i < text.length && !stopped) {
      const step = 2 + Math.floor(Math.random() * 3)
      emit({ type: 'delta', text: text.slice(i, i + step) })
      i += step
      await sleep(22)
    }
  }

  const waitDecision = (approval: Approval): Promise<{ decision: Decision; reason?: string }> =>
    new Promise((resolve) => {
      resumeResolve = resolve
      emit({ type: 'approval', approval })
    })

  const run = async () => {
    await sleep(180)
    emit({ type: 'reasoning', text: '分析意图：需要修改工作区文件并验证。' })

    if (scenario === 'browser') {
      emit({ type: 'error', code: 'TOOL_UNAVAILABLE', message: 'browser 插件不可用：未检测到 Chromium' })
      for (const t of TEXTS.browser) {
        await streamText(t)
        await sleep(160)
      }
      emit({ type: 'done', tokensIn: 820, tokensOut: 96 })
      return
    }

    if (scenario === 'write') {
      // 步骤 1：只读调研（read_file）
      await streamText(TEXTS.write[0])
      const readCall: ToolCall = {
        id: `tc-${Date.now()}`, name: 'read_file', risk: 'readonly', status: 'running',
        args: { path: 'internal/tools/registry.go', offset_line: 1, max_lines: 60 },
      }
      emit({ type: 'tool_start', call: readCall })
      await sleep(300)
      emit({
        type: 'tool_result', callId: readCall.id, durationMs: 296,
        result: '     1  package tools\n     2\n     3  type Registry struct {\n     4      mu    sync.RWMutex\n     5      tools map[string]Tool\n     6  }\n...',
      })
      emit({ type: 'step', tokens: 340, promptTokens: 260, cachedTokens: 210 })

      // 步骤 2：写入（需审批）
      const call: ToolCall = {
        id: `tc-${Date.now() + 1}`, name: 'write_file', risk: 'write', status: 'pending',
        args: { path: 'internal/tools/registry.go' },
      }
      const approval: Approval = {
        id: `ap-${Date.now()}`, runId, toolCallId: call.id,
        toolName: 'write_file', risk: 'write',
        args: { path: 'internal/tools/registry.go', size: '4.2 KB' },
        summary: '写入 internal/tools/registry.go（+12 −3）', createdAt: Date.now(),
      }
      const d = await waitDecision(approval)
      resumeResolve = null
      if (stopped) return

      if (d.decision === 'deny') {
        emit({ type: 'step', tokens: 90, promptTokens: 60, cachedTokens: 45 })
        await streamText('好的，这次修改已取消。你可以告诉我需要调整的地方，我再改。')
        emit({ type: 'done', tokensIn: 860, tokensOut: 64, cachedTokens: 610 })
        return
      }

      emit({ type: 'tool_start', call: { ...call, status: 'running' } })
      await sleep(420)
      emit({ type: 'tool_result', callId: call.id, result: '已写入 internal/tools/registry.go（+12 −3）', durationMs: 418, diff: sampleDiff })
      emit({ type: 'step', tokens: 410, promptTokens: 300, cachedTokens: 240 })

      // 步骤 3：最终交付答复（无工具）
      await streamText(TEXTS.write[1])
      await streamText(TEXTS.write[2])
      emit({ type: 'done', tokensIn: 1240, tokensOut: 210, cachedTokens: 900 })
      return
    }

    if (scenario === 'command') {
      // 步骤 1：执行命令（需审批）
      await streamText(TEXTS.command[0])
      const call: ToolCall = {
        id: `tc-${Date.now()}`, name: 'run_command', risk: 'command', status: 'pending',
        args: { command: 'go test ./internal/tools/...', timeout_ms: 120000 },
      }
      const approval: Approval = {
        id: `ap-${Date.now()}`, runId, toolCallId: call.id,
        toolName: 'run_command', risk: 'command',
        args: { command: 'go test ./internal/tools/...' },
        summary: '执行命令：go test ./internal/tools/...', createdAt: Date.now(),
      }
      const d = await waitDecision(approval)
      resumeResolve = null
      if (stopped) return
      if (d.decision === 'deny') {
        emit({ type: 'step', tokens: 60, promptTokens: 45, cachedTokens: 30 })
        await streamText('命令未获批准，已跳过验证。需要我用其他方式确认吗？')
        emit({ type: 'done', tokensIn: 640, tokensOut: 40, cachedTokens: 430 })
        return
      }
      emit({ type: 'tool_start', call: { ...call, status: 'running' } })
      const lines = ['go: downloading deps...', '=== RUN   TestRegistryConflict', '--- PASS: TestRegistryConflict (0.00s)', 'PASS', 'ok  harness_go/internal/tools  0.412s']
      for (const line of lines) {
        await sleep(260)
        emit({ type: 'tool_result', callId: call.id, result: line, durationMs: 0 })
      }
      emit({ type: 'step', tokens: 380, promptTokens: 280, cachedTokens: 220 })

      // 步骤 2：最终交付答复
      await streamText(TEXTS.command[1])
      await streamText(TEXTS.command[2])
      emit({ type: 'done', tokensIn: 980, tokensOut: 130, cachedTokens: 720 })
      return
    }

    if (scenario === 'plan') {
      // 步骤 1：建计划 + 更新任务清单
      await streamText(TEXTS.plan[0])
      const planCall: ToolCall = {
        id: `tc-${Date.now()}`, name: 'create_plan', risk: 'write', status: 'running',
        args: { title: '插件注册表热加载改造', steps: 4 },
      }
      emit({ type: 'tool_start', call: planCall })
      await sleep(360)
      emit({ type: 'tool_result', callId: planCall.id, result: '已创建计划（4 步）', durationMs: 355 })
      const todoCall: ToolCall = {
        id: `tc-${Date.now() + 1}`, name: 'todo', risk: 'readonly', status: 'running',
        args: { items: 4 },
      }
      emit({ type: 'tool_start', call: todoCall })
      await sleep(220)
      emit({ type: 'tool_result', callId: todoCall.id, result: '任务清单已更新（4 项）', durationMs: 216 })
      emit({ type: 'step', tokens: 300, promptTokens: 220, cachedTokens: 170 })

      // 步骤 2：最终交付答复
      await streamText(TEXTS.plan[1])
      await streamText(TEXTS.plan[2])
      emit({ type: 'done', tokensIn: 760, tokensOut: 150, cachedTokens: 560 })
      return
    }

    // read 场景：步骤 1 读文件
    await streamText(TEXTS.read[0])
    const call: ToolCall = {
      id: `tc-${Date.now()}`, name: 'read_file', risk: 'readonly', status: 'running',
      args: { path: 'internal/tools/registry.go', offset_line: 1, max_lines: 60 },
    }
    emit({ type: 'tool_start', call })
    await sleep(300)
    emit({
      type: 'tool_result', callId: call.id, durationMs: 296,
      result: '     1  package tools\n     2\n     3  type Registry struct {\n     4      mu    sync.RWMutex\n     5      tools map[string]Tool\n     6  }\n...',
    })
    emit({ type: 'step', tokens: 320, promptTokens: 240, cachedTokens: 180 })

    // 步骤 2：最终交付答复
    await streamText(TEXTS.read[1])
    await streamText(TEXTS.read[2])
    emit({ type: 'done', tokensIn: 900, tokensOut: 140, cachedTokens: 680 })
  }

  void run()

  return {
    stop: () => { stopped = true },
    resume: (decision, reason) => { resumeResolve?.({ decision, reason }) },
  }
}
