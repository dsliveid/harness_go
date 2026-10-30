import { create } from 'zustand'
import type {
  PlanDoc, Snapshot, Subagent, TodoItem,
  CommandRun,
} from '../types'
import {
  commands as mockCommands, plans as mockPlans,
  snapshots as mockSnapshots, subagents as mockSubagents,
  todos as mockTodos,
} from '../mock/data'

interface WorkspaceState {
  plans: PlanDoc[]
  todos: TodoItem[]
  snapshots: Snapshot[]
  subagents: Subagent[]
  /** 运行中的命令行 */
  commands: CommandRun[]
  /** 停止正在执行的命令（进程组终止） */
  stopCommand: (id: string) => void
  revertSnapshot: (id: string) => void
  reapplySnapshot: (id: string) => void
  stopSubagent: (id: string) => void
  resumeSubagent: (id: string) => void
  toggleTodo: (id: string) => void
}

export const useWorkspace = create<WorkspaceState>((set) => ({
  plans: mockPlans,
  todos: mockTodos,
  snapshots: mockSnapshots,
  subagents: mockSubagents,
  commands: mockCommands,

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
