import { create } from 'zustand'
import type { ModelInfo, Provider } from '../types'
import { defaultModels, providers as mockProviders } from '../mock/data'
import { api, apiMode } from '../lib/api'
import type { ProviderDTO, ProviderInput } from '../lib/apiTypes'
import { useUI } from './uiStore'

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

/** 已配置密钥时的输入框掩码占位（提交时若仍等于该值，表示「不修改密钥」） */
export const PROVIDER_KEY_MASK = '••••••••'

const initialSettings: AppSettings = {
  // real 模式的厂商来自主库 providers 表（loadProviders 拉取）；mock 沿用 demo 样例
  providers: apiMode === 'mock' ? mockProviders : [],
  defaultProviderId: apiMode === 'mock' ? (mockProviders[0]?.id ?? '') : '',
  capabilityModels:
    apiMode === 'mock'
      ? { ...defaultModels }
      : { chat: '', vision: '', image: '', decision: '' },
  disabledTools: [],
  disabledSops: [],
  proxyEnabled: false,
  proxyUrl: '',
}

/** 服务端 ProviderDTO → UI Provider（密钥恒为掩码，明文永不出栈） */
function toUiProvider(d: ProviderDTO): Provider {
  return {
    id: d.id,
    name: d.name,
    baseUrl: d.base_url,
    enabled: d.enabled,
    apiKeyMasked: d.has_api_key ? PROVIDER_KEY_MASK : '',
    models: (d.models ?? []).map((id) => ({
      id,
      contextLimit: 64000,
      capabilities: ['chat'],
      protocol: 'chat_completions',
    })),
  }
}

/** UI Provider → 提交体：掩码占位不得当密钥回传（否则会把「••••」写成真密钥） */
function toProviderInput(p: Provider): ProviderInput {
  const input: ProviderInput = {
    name: p.name,
    base_url: p.baseUrl,
    models: p.models.map((m) => m.id),
    enabled: p.enabled,
  }
  if (p.apiKeyMasked && p.apiKeyMasked !== PROVIDER_KEY_MASK) input.api_key = p.apiKeyMasked
  return input
}

/**
 * 把「草稿里的厂商配置」同步到服务端（仅 real 模式）。
 *
 * 差异提交：新增 → create、字段变化 → update、被删 → delete；
 * 未变化的厂商不发请求（避免无谓写入与密钥覆盖）。返回是否有变更。
 */
async function syncProvidersToServer(prev: Provider[], next: Provider[]): Promise<boolean> {
  if (apiMode !== 'real') return false
  const prevMap = new Map(prev.map((p) => [p.id, p]))
  const nextIds = new Set(next.map((p) => p.id))
  let changed = false
  try {
    for (const p of next) {
      const before = prevMap.get(p.id)
      if (!before) {
        await api.createProvider(toProviderInput(p))
        changed = true
        continue
      }
      const same =
        before.name === p.name &&
        before.baseUrl === p.baseUrl &&
        before.enabled === p.enabled &&
        before.apiKeyMasked === p.apiKeyMasked &&
        before.models.map((m) => m.id).join(',') === p.models.map((m) => m.id).join(',')
      if (same) continue
      await api.updateProviderById(p.id, toProviderInput(p))
      changed = true
    }
    for (const p of prev) {
      if (nextIds.has(p.id)) continue
      await api.deleteProvider(p.id)
      changed = true
    }
  } catch {
    useUI.getState().toast('error', '同步厂商配置失败，请检查服务状态后重试')
    return false
  }
  return changed
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
  /** 从服务端拉取厂商列表（real 模式；登录后调用） */
  loadProviders: () => Promise<void>
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
    if (!commit || apiMode !== 'real') return
    // 厂商/模型改动立即落库：差异提交后再拉回服务端真值（密钥只回掩码）
    void syncProvidersToServer(settings.providers, draft.providers).then((changed) => {
      if (changed) void get().loadProviders()
    })
  },

  // 登录后拉取厂商列表：服务端为唯一真源（has_api_key 只回布尔，不回明文）
  loadProviders: async () => {
    if (apiMode !== 'real') return
    try {
      const list = (await api.listProviders()).map(toUiProvider)
      set((s) => ({
        settings: {
          ...s.settings,
          providers: list,
          defaultProviderId: list.some((p) => p.id === s.settings.defaultProviderId)
            ? s.settings.defaultProviderId
            : (list[0]?.id ?? ''),
        },
      }))
    } catch {
      /* 服务未起/网络异常：保留现有列表，避免把已配置厂商清空 */
    }
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
