import { create } from 'zustand'
import type { ModelInfo, Provider } from '../types'
import { api } from '../lib/api'
import { DEFAULT_CONTEXT_LIMIT, registerProviderSource } from '../lib/context'
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

/** 服务端持久化的默认对话模型键（settings 白名单；值为 providerID/modelID 引用） */
const DEFAULT_MODEL_KEY = 'agent.default_model'

const initialSettings: AppSettings = {
  // 厂商来自主库 providers 表（loadProviders 拉取），初始为空态绝不预置演示数据
  providers: [],
  defaultProviderId: '',
  capabilityModels: { chat: '', vision: '', image: '', decision: '' },
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
      contextLimit: DEFAULT_CONTEXT_LIMIT,
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
 * 把「草稿里的厂商配置」同步到服务端。
 *
 * 差异提交：新增 → create、字段变化 → update、被删 → delete；
 * 未变化的厂商不发请求（避免无谓写入与密钥覆盖）。返回是否有变更。
 */
async function syncProvidersToServer(prev: Provider[], next: Provider[]): Promise<boolean> {
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

/** 持久化默认对话模型（chat 槽位 → settings.agent.default_model；内存态刷新即丢，必须落库） */
async function syncDefaultModelToServer(modelRef: string): Promise<void> {
  try {
    await api.patchSettings({ [DEFAULT_MODEL_KEY]: modelRef })
  } catch {
    useUI.getState().toast('error', '保存默认对话模型失败')
  }
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
   * 「应用」语义（对齐桌面端 ModalActions）：提交草稿使其生效，但**不结束编辑**。
   *
   * 与 endEdit(true) 的差异：提交后立刻用最新设置重开草稿，弹窗保持打开，
   * 用户可继续改下一项；会等服务端同步完成再重开草稿，
   * 保证界面回到「已落库」的真值（厂商 id / 密钥掩码都以服务端为准）。
   */
  applyDraft: () => Promise<void>
  /**
   * 直接生效的内置工具启停（「高级 → 工具 → 系统内置」无「保存」按钮，改动立即落库；
   * 若当前正在编辑草稿，则同步草稿避免出现「改了但没保存」的错觉）
   */
  setDisabledTools: (next: string[]) => void
  /** 从服务端拉取厂商列表（登录后调用） */
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
    if (!commit) return
    // 默认对话模型落库（内存态刷新即丢）+ 厂商差异提交后再拉回服务端真值（密钥只回掩码）
    void syncDefaultModelToServer(draft.capabilityModels?.chat ?? '')
    void syncProvidersToServer(settings.providers, draft.providers).then((changed) => {
      if (changed) void get().loadProviders()
    })
  },

  // 登录后拉取厂商列表：服务端为唯一真源（has_api_key 只回布尔，不回明文）
  loadProviders: async () => {
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
      // 恢复持久化的默认对话模型（agent.default_model）：须命中启用厂商，否则视为未设置
      try {
        const items = await api.getSettings()
        const ref = typeof items[DEFAULT_MODEL_KEY] === 'string' ? (items[DEFAULT_MODEL_KEY] as string) : ''
        if (ref) {
          const sep = ref.indexOf('/')
          const pid = sep > 0 ? ref.slice(0, sep) : ''
          const mid = sep > 0 ? ref.slice(sep + 1) : ''
          const p = list.find((x) => x.id === pid)
          if (p?.enabled && p.models.some((m) => m.id === mid)) {
            set((s) => ({
              settings: { ...s.settings, capabilityModels: { ...s.settings.capabilityModels, chat: ref } },
            }))
          }
        }
      } catch {
        /* 设置读取失败不阻塞厂商加载 */
      }
    } catch {
      /* 服务未起/网络异常：保留现有列表，避免把已配置厂商清空 */
    }
  },
  patchDraft: (fn) =>
    set((s) => (s.draft ? { draft: fn(s.draft) } : s)),

  // 「应用」：提交草稿 → 同步服务端 → 用最新真值重开草稿（弹窗不关）
  applyDraft: async () => {
    const { draft, settings } = get()
    if (!draft) return
    set({ settings: draft })
    // 默认对话模型落库 + 差异提交厂商，再拉回服务端真值后重开草稿
    void syncDefaultModelToServer(draft.capabilityModels?.chat ?? '')
    const changed = await syncProvidersToServer(settings.providers, draft.providers)
    if (changed) {
      await get().loadProviders()
    }
    set((s) => ({ draft: structuredClone(s.settings) }))
  },
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
  const m: ModelInfo = { id, contextLimit: DEFAULT_CONTEXT_LIMIT, capabilities: ['chat'], protocol: 'chat_completions' }
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

/**
 * 向 lib/context 注册厂商表读取器（消除 contextLimit 对演示数据的直引）。
 *
 * 放在模块末尾：此刻 `useSettings` 已求值完成，回调只读 `settings.providers` 快照，
 * 既不形成 import 循环，也不会在 loop 初始化期被访问到 TDZ。
 * 该表来自 `GET /providers`（ProviderDTO）。
 */
registerProviderSource(() => useSettings.getState().settings.providers)
