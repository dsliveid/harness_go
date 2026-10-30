import { create } from 'zustand'
import type { DeviceNode, ExtensionItem, McpServer, MemoryItem, Plugin, RegistryItem, Skill } from '../types'
import {
  devices as mockDevices, extensions as mockExtensions,
  mcpServers as mockMcp, memories as mockMemories,
  plugins as mockPlugins, registryItems as mockRegistry, skills as mockSkills,
} from '../mock/data'

/**
 * 能力安装模型：客户端自行安装。
 * - 服务端不再「下发」，安装动作只作用于当前客户端（currentClientId）自身；
 * - 每个端节点各自维护 installed / installing 清单（key：工具名 / mcp:<server> / skill:<name> / ext:<name> / reg:<id>）。
 */
interface PluginState {
  plugins: Plugin[]
  mcpServers: McpServer[]
  registryItems: RegistryItem[]
  skills: Skill[]
  memories: MemoryItem[]
  /** 拓展：程序在开发过程中为自身扩展的能力（自演化引擎产出） */
  extensions: ExtensionItem[]
  /** 客户端节点清单（客户端自行安装的落地载体） */
  devices: DeviceNode[]
  /** 当前操作所在客户端（安装/卸载只作用于本端） */
  currentClientId: string
  setCurrentClient: (id: string) => void
  togglePlugin: (id: string) => void
  reloadMcp: (id: string) => void
  toggleMcp: (id: string) => void
  testMcp: (id: string) => { ok: boolean; message: string }
  installRegistry: (id: string) => void
  toggleSkill: (id: string) => void
  /** 安装拓展项：自检通过 → 已安装（原型模拟审批放行） */
  installExtension: (id: string) => void
  toggleExtension: (id: string) => void
  removeExtension: (id: string) => void
  /** 把能力安装到当前客户端（客户端自行安装，非服务端下发） */
  installOnClient: (key: string) => void
  /** 从当前客户端卸载能力 */
  uninstallOnClient: (key: string) => void
}

export const usePlugins = create<PluginState>((set, get) => ({
  plugins: mockPlugins,
  mcpServers: mockMcp,
  registryItems: mockRegistry,
  skills: mockSkills,
  memories: mockMemories,
  extensions: mockExtensions,
  devices: mockDevices,
  currentClientId: mockDevices.find((d) => d.self)?.id ?? 'client-win',

  setCurrentClient: (currentClientId) => set({ currentClientId }),

  togglePlugin: (id) =>
    set((s) => ({
      plugins: s.plugins.map((p) => {
        if (p.id !== id) return p
        if (p.state === 'unavailable') return p
        return { ...p, state: p.state === 'enabled' ? 'disabled' : 'enabled' }
      }),
    })),

  reloadMcp: (id) => {
    set((s) => ({
      mcpServers: s.mcpServers.map((m) => (m.id === id ? { ...m, state: 'starting', lastError: undefined } : m)),
    }))
    setTimeout(() => {
      set((s) => ({
        mcpServers: s.mcpServers.map((m) => {
          if (m.id !== id) return m
          const ok = m.name !== 'postgres'
          return {
            ...m,
            state: ok ? 'ready' : 'failed',
            lastError: ok ? undefined : '连接失败：connection refused（127.0.0.1:7788）',
          }
        }),
      }))
    }, 900)
  },

  toggleMcp: (id) =>
    set((s) => ({
      mcpServers: s.mcpServers.map((m) =>
        m.id === id ? { ...m, enabled: !m.enabled, state: !m.enabled ? 'starting' : 'disabled' } : m,
      ),
    })),

  testMcp: (id) => {
    const m = get().mcpServers.find((x) => x.id === id)
    if (!m) return { ok: false, message: '未找到该 Server' }
    if (m.name === 'postgres') return { ok: false, message: '握手超时（10s）：connection refused' }
    return { ok: true, message: `握手成功，发现 ${m.tools.length} 个工具（耗时 218ms）` }
  },

  installRegistry: (id) => {
    set((s) => ({
      registryItems: s.registryItems.map((r) => (r.id === id ? { ...r, installing: true, progress: 0 } : r)),
    }))
    let p = 0
    const timer = setInterval(() => {
      p += 18 + Math.floor(Math.random() * 14)
      if (p >= 100) {
        clearInterval(timer)
        set((s) => {
          const item = s.registryItems.find((r) => r.id === id)
          const skills = item && item.kind === 'skill'
            ? s.skills.some((k) => k.name === item.name)
              ? s.skills
              : [{ id: `sk-${item.id}`, name: item.name, description: item.description, source: 'registry' as const, origin: 'installed' as const, updatedAt: Date.now() }, ...s.skills]
            : s.skills
          return {
            skills,
            registryItems: s.registryItems.map((r) =>
              r.id === id ? { ...r, installing: false, progress: 100, installed: true } : r,
            ),
          }
        })
        // 制品落地同时装入当前客户端
        get().installOnClient(`reg:${id}`)
      } else {
        set((s) => ({
          registryItems: s.registryItems.map((r) => (r.id === id ? { ...r, progress: p } : r)),
        }))
      }
    }, 220)
  },

  toggleSkill: (id) => set((s) => ({ skills: s.skills.filter((k) => k.id !== id) })),

  installExtension: (id) =>
    set((s) => {
      const e = s.extensions.find((x) => x.id === id)
      const key = e ? `ext:${e.name}` : null
      return {
        extensions: s.extensions.map((x) =>
          x.id === id ? { ...x, state: 'installed', selfTest: 'passed', signature: 'signed' } : x,
        ),
        devices: key
          ? s.devices.map((d) =>
              d.id === s.currentClientId && !d.installed.includes(key)
                ? { ...d, installed: [...d.installed, key], lastSyncAt: Date.now() }
                : d,
            )
          : s.devices,
      }
    }),

  toggleExtension: (id) =>
    set((s) => ({
      extensions: s.extensions.map((e) =>
        e.id === id ? { ...e, state: e.state === 'installed' ? 'disabled' : 'installed' } : e,
      ),
    })),

  removeExtension: (id) => set((s) => ({ extensions: s.extensions.filter((e) => e.id !== id) })),

  installOnClient: (key) => {
    const clientId = get().currentClientId
    set((s) => ({
      devices: s.devices.map((d) =>
        d.id === clientId && !d.installed.includes(key) && !d.installing.includes(key)
          ? { ...d, installing: [...d.installing, key] }
          : d,
      ),
    }))
    setTimeout(() => {
      set((s) => ({
        devices: s.devices.map((d) =>
          d.id === clientId
            ? {
                ...d,
                installing: d.installing.filter((k) => k !== key),
                installed: d.installed.includes(key) ? d.installed : [...d.installed, key],
                lastSyncAt: Date.now(),
              }
            : d,
        ),
      }))
    }, 700)
  },

  uninstallOnClient: (key) =>
    set((s) => ({
      devices: s.devices.map((d) =>
        d.id === s.currentClientId ? { ...d, installed: d.installed.filter((k) => k !== key) } : d,
      ),
    })),
}))
