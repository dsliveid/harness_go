import { create } from 'zustand'
import type { ModelInfo, Provider } from '../types'
import { defaultModels, providers as mockProviders } from '../mock/data'

// 能力槽位（demo 命名沿用 types.ts：chat / vision / image / decision）
export type CapSlot = 'chat' | 'vision' | 'image' | 'decision'

export interface AppSettings {
  providers: Provider[]
  /** 默认对话厂商（桌面端「默认对话」徽标） */
  defaultProviderId: string
  /** 能力槽位 → 'providerId/modelId' | ''（跟随全局；chat 槽禁止 NULL） */
  capabilityModels: Record<CapSlot, string>
  disabledTools: string[]
  disabledSops: string[]
  proxyEnabled: boolean
  proxyUrl: string
}

const initialSettings: AppSettings = {
  providers: mockProviders,
  defaultProviderId: mockProviders[0]?.id ?? '',
  capabilityModels: { ...defaultModels },
  disabledTools: [],
  disabledSops: [],
  proxyEnabled: false,
  proxyUrl: '',
}

interface SettingsState {
  /** 已提交生效的设置 */
  settings: AppSettings
  /** 编辑草稿：打开弹窗时从 settings 拷贝，保存时提交；null = 未在编辑 */
  draft: AppSettings | null
  beginEdit: () => void
  /** commit=true 保存，false=取消丢弃 */
  endEdit: (commit: boolean) => void
  /** 对草稿做函数式修改（面板统一入口） */
  patchDraft: (fn: (d: AppSettings) => AppSettings) => void
  /**
   * 直接生效的内置工具启停（「高级 → 工具 → 系统内置」无「保存」按钮，改动立即落库；
   * 若当前正在编辑草稿，则同步草稿避免出现「改了但没保存」的错觉）
   */
  setDisabledTools: (next: string[]) => void
}

export const useSettings = create<SettingsState>((set, get) => ({
  settings: initialSettings,
  draft: null,

  beginEdit: () => {
    if (get().draft) return
    set({ draft: structuredClone(get().settings) })
  },
  endEdit: (commit) => {
    const { draft, settings } = get()
    if (!draft) return
    set(commit ? { settings: draft, draft: null } : { draft: null })
    void settings
  },
  patchDraft: (fn) =>
    set((s) => (s.draft ? { draft: fn(s.draft) } : s)),
  setDisabledTools: (next) =>
    set((s) => ({
      settings: { ...s.settings, disabledTools: next },
      draft: s.draft ? { ...s.draft, disabledTools: next } : null,
    })),
}))

// ---------------- 草稿层面的派生操作（供各面板复用） ----------------

export function updateProvider(d: AppSettings, id: string, patch: Partial<Provider>): AppSettings {
  return { ...d, providers: d.providers.map((p) => (p.id === id ? { ...p, ...patch } : p)) }
}

export function addProvider(d: AppSettings): AppSettings {
  const id = `pv-${Date.now()}`
  const p: Provider = { id, name: '新厂商', baseUrl: 'https://api.example.com/v1', enabled: true, apiKeyMasked: '', models: [] }
  return { ...d, providers: [...d.providers, p] }
}

export function removeProvider(d: AppSettings, id: string): AppSettings {
  const providers = d.providers.filter((p) => p.id !== id)
  // 默认厂商被删除时自动顺延到第一个还有模型的厂商（与桌面端一致）
  let defaultProviderId = d.defaultProviderId
  if (defaultProviderId === id) {
    defaultProviderId = providers.find((p) => p.models.length > 0)?.id ?? providers[0]?.id ?? ''
  }
  // 清理指向被删厂商的能力槽位
  const capabilityModels = { ...d.capabilityModels }
  ;(Object.keys(capabilityModels) as CapSlot[]).forEach((cap) => {
    if (capabilityModels[cap].startsWith(`${id}/`)) capabilityModels[cap] = ''
  })
  return { ...d, providers, defaultProviderId, capabilityModels }
}

export function updateModel(d: AppSettings, providerId: string, index: number, patch: Partial<ModelInfo>): AppSettings {
  return updateProvider(d, providerId, {
    models: d.providers
      .find((p) => p.id === providerId)!
      .models.map((m, i) => (i === index ? { ...m, ...patch } : m)),
  })
}

export function addModel(d: AppSettings, providerId: string, modelId?: string): AppSettings {
  const id = (modelId ?? '').trim() || 'new-model'
  const p = d.providers.find((x) => x.id === providerId)!
  // 已存在同 ID 模型则不重复添加
  if (p.models.some((m) => m.id === id)) return d
  const m: ModelInfo = { id, contextLimit: 64000, capabilities: ['chat'], protocol: 'chat_completions' }
  return updateProvider(d, providerId, { models: [...p.models, m] })
}

export function removeModel(d: AppSettings, providerId: string, index: number): AppSettings {
  const p = d.providers.find((x) => x.id === providerId)!
  const m = p.models[index]
  const capabilityModels = { ...d.capabilityModels }
  ;(Object.keys(capabilityModels) as CapSlot[]).forEach((cap) => {
    if (capabilityModels[cap] === `${providerId}/${m.id}`) capabilityModels[cap] = ''
  })
  return updateProvider(d, providerId, { models: p.models.filter((_, i) => i !== index) })
}

export function toggleDisabled(list: string[], id: string): string[] {
  return list.includes(id) ? list.filter((x) => x !== id) : [...list, id]
}
