import { create } from 'zustand'
import type { ScheduledTask } from '../types'
import { scheduledTasks as mockTasks } from '../mock/data'
import { usePlugins } from './pluginStore'

/**
 * 定时任务状态：任务由「客户端本地调度」——
 * 每条任务绑定一个客户端（clientId），到点由该客户端自行拉起一次会话执行提示词。
 */
interface ScheduleState {
  tasks: ScheduledTask[]
  toggle: (id: string) => void
  runNow: (id: string) => void
  remove: (id: string) => void
  create: (input: { name: string; cron: string; prompt: string }) => void
}

const HOUR = 3_600_000

export const useSchedule = create<ScheduleState>((set, get) => ({
  tasks: mockTasks,

  toggle: (id) =>
    set((s) => ({
      tasks: s.tasks.map((t) =>
        t.id === id
          ? { ...t, enabled: !t.enabled, nextRunAt: !t.enabled ? Date.now() + HOUR : undefined }
          : t,
      ),
    })),

  runNow: (id) => {
    set((s) => ({
      tasks: s.tasks.map((t) => (t.id === id ? { ...t, lastStatus: 'running' } : t)),
    }))
    setTimeout(() => {
      set((s) => ({
        tasks: s.tasks.map((t) =>
          t.id === id
            ? {
                ...t,
                lastStatus: 'success',
                lastRunAt: Date.now(),
                nextRunAt: t.enabled ? Date.now() + HOUR : undefined,
                lastSummary: '手动触发：已在绑定的客户端上启动一次会话',
              }
            : t,
        ),
      }))
    }, 1200)
  },

  remove: (id) => set((s) => ({ tasks: s.tasks.filter((t) => t.id !== id) })),

  create: ({ name, cron, prompt }) => {
    const clientId = usePlugins.getState().currentClientId
    const task: ScheduledTask = {
      id: `sch-${Date.now()}`,
      name,
      prompt,
      cron,
      scheduleText: cron || '手动触发',
      enabled: true,
      nextRunAt: Date.now() + HOUR,
      lastStatus: 'never',
      clientId,
      accessMode: 'confirm',
    }
    set((s) => ({ tasks: [task, ...s.tasks] }))
    return task.id
  },
}))

/** 运行时便捷查询：某客户端上绑定的任务数 */
export const tasksOfClient = (tasks: ScheduledTask[], clientId: string) =>
  tasks.filter((t) => t.clientId === clientId).length
