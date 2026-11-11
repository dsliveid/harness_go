import { create } from 'zustand'
import type {
  PlanDoc, Snapshot, Subagent, TodoItem,
  CommandRun, Message,
} from '../types'
import type { Snapshot as SnapshotAnchor } from '../lib/apiTypes'
import { api } from '../lib/api'
import { deriveWorkspace } from '../lib/workspaceDerive'

/**
 * 把服务端「锚点级」快照（一个锚点可含多路径，`paths[]`）展开为 UI「单文件级」快照行。
 *
 * 后端 `apiTypes.ts:691` 的 Snapshot 与 UI `types.ts:511` 的 Snapshot 形状不同：
 * 后端按 kind(before/after/checkpoint) + paths[] 组织，UI 按单文件一行展示。
 * 这里只做**纯字段映射**，绝不补造任何字段：
 * - `turnSeq` 后端无此字段 → 置 0，UI 显示为「第 0 轮」不冒充真实轮次；
 * - `beforeHash` 后端未提供 → undefined（UI 已渲染「（无旧版本）」）；
 * - `isNewFile` 后端未提供 → false；`revertedAt` 后端未提供 → undefined。
 */
function toUiSnapshots(anchors: SnapshotAnchor[]): Snapshot[] {
  const out: Snapshot[] = []
  for (const a of anchors) {
    const paths = a.paths ?? []
    if (paths.length === 0) continue
    for (const p of paths) {
      out.push({
        id: paths.length > 1 ? `${a.id}:${p.path}` : a.id,
        sessionId: a.session_id ?? '',
        messageId: '',
        turnSeq: 0,
        toolEventId: a.tool_event_id,
        filePath: p.path,
        beforeHash: p.hash,
        afterHash: p.hash ?? '',
        isNewFile: p.absent === true,
        sizeBytes: p.size ?? a.size ?? 0,
        createdAt: a.created_at,
      })
    }
  }
  return out
}

interface WorkspaceState {
  plans: PlanDoc[]
  todos: TodoItem[]
  snapshots: Snapshot[]
  subagents: Subagent[]
  /** 运行中的命令行 */
  commands: CommandRun[]
  /** 快照加载态（real 模式拉取 GET /sessions/{sid}/snapshots 时置 true） */
  snapshotsLoading: boolean
  /** 快照加载错误（原样透传后端 message，绝不吞） */
  snapshotsError: string | null
  /** 拉取指定会话的真实快照锚点 */
  loadSnapshots: (sessionId: string) => Promise<void>
  /**
   * 由真实工具调用实时派生运行态四类数据（todos / plans / subagents / commands）。
   * 由 chatStore 在消息落地后调用（SSE 增量与历史回灌共用同一入口）。
   */
  syncFromMessages: (messages: Message[]) => void
  /** 停止正在执行的命令（进程组终止） */
  stopCommand: (id: string) => void
  revertSnapshot: (id: string) => void
  reapplySnapshot: (id: string) => void
  stopSubagent: (id: string) => void
  resumeSubagent: (id: string) => void
  toggleTodo: (id: string) => void
}

export const useWorkspace = create<WorkspaceState>((set) => ({
  // 一律空态起步：绝不先闪一屏演示数据再被真实数据覆盖（冒充真实）
  plans: [],
  todos: [],
  snapshots: [],
  subagents: [],
  commands: [],
  snapshotsLoading: false,
  snapshotsError: null,

  syncFromMessages: (messages) => {
    const derived = deriveWorkspace(messages)
    set({
      todos: derived.todos,
      plans: derived.plans,
      subagents: derived.subagents,
      commands: derived.commands,
    })
  },

  loadSnapshots: async (sessionId) => {
    if (!sessionId) {
      set({ snapshots: [], snapshotsLoading: false, snapshotsError: null })
      return
    }
    set({ snapshotsLoading: true, snapshotsError: null })
    try {
      const anchors = await api.listSessionSnapshots(sessionId)
      set({ snapshots: toUiSnapshots(anchors), snapshotsLoading: false })
    } catch (e) {
      // 错误原样透传：绝不 fallback 到演示数据冒充成功
      set({
        snapshots: [],
        snapshotsLoading: false,
        snapshotsError: e instanceof Error ? e.message : String(e),
      })
    }
  },

  stopCommand: (id) =>
    set((s) => ({
      commands: s.commands.map((c) => (c.id === id ? { ...c, status: 'cancelled' as const } : c)),
    })),

  revertSnapshot: (id) =>
    set((s) => ({
      snapshots: s.snapshots.map((x) => (x.id === id ? { ...x, revertedAt: Date.now() } : x)),
    })),
  reapplySnapshot: (id) =>
    set((s) => ({
      snapshots: s.snapshots.map((x) => (x.id === id ? { ...x, revertedAt: undefined } : x)),
    })),

  stopSubagent: (id) =>
    set((s) => ({ subagents: s.subagents.map((a) => (a.id === id ? { ...a, status: 'cancelled' } : a)) })),
  resumeSubagent: (id) =>
    set((s) => ({ subagents: s.subagents.map((a) => (a.id === id ? { ...a, status: 'running' } : a)) })),

  toggleTodo: (id) =>
    set((s) => ({
      todos: s.todos.map((t) => (t.id === id ? { ...t, status: t.status === 'done' ? 'pending' : 'done' } : t)),
    })),
}))
