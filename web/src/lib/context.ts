/**
 * 上下文窗口解析（唯一入口，避免多处各算一份漂移）。
 *
 * 数据来源口径：
 * - 真实厂商表在 `stores/settingsStore.ts` 的 `settings.providers`（real 模式由 `loadProviders()`
 *   从服务端 `GET /providers` 的 ProviderDTO 归一化而来；demo 模式是本地样例）。
 * - ⚠️ 后端 `ProviderDTO.models` 只是 `[]string`（模型 id 列表），**不含 contextLimit 字段**；
 *   归一化时已用 `DEFAULT_CONTEXT_LIMIT` 兜底（见 settingsStore.ts `toUiProvider`）。
 *   因此本模块不编造任何模型专属窗口数字：命中模型时用该条目的 contextLimit（可能就是兜底值），
 *   未命中时同样退回 `DEFAULT_CONTEXT_LIMIT`。
 * - 将来后端若补上 per-model 窗口，只需让 ProviderDTO 带上该字段，本函数与全部调用方无需改动。
 *
 * 依赖方向：settingsStore 已 import 本模块的 DEFAULT_CONTEXT_LIMIT，若这里再 import 回去会形成
 * ESM 循环（初始化顺序稍有变化就触发 TDZ）。因此改为「settingsStore 反向注册数据源」：
 * 本模块不依赖任何 store，未被注册时安全退回兜底窗口。
 */

/** 智能压缩触发比例（与桌面端一致：达到 75% 需人工确认） */
export const COMPACTION_RATIO = 0.75
/** 未匹配到模型时的兜底上下文窗口（默认 1M）；也是后端未提供 per-model 窗口时的口径值 */
export const DEFAULT_CONTEXT_LIMIT = 1000000

/** 本模块需要的最小厂商结构（避免 lib 依赖 stores 的完整类型图） */
export interface ContextProviderLike {
  id: string
  name: string
  models?: Array<{ id: string; contextLimit?: number }>
}

/** 厂商表数据源（由 stores/settingsStore 在模块初始化时注册，见 registerProviderSource） */
let providerSource: (() => ContextProviderLike[]) | null = null

/** 由 settingsStore 注册真实厂商表读取器；重复注册后者覆盖前者 */
export function registerProviderSource(fn: () => ContextProviderLike[]): void {
  providerSource = fn
}

/** 统一解析会话模型的上下文窗口，避免多处各算一份漂移 */
export function resolveModelContext(modelRef?: string): {
  limit: number
  modelName: string
  providerName: string
} {
  let providers: ContextProviderLike[] = []
  try {
    providers = providerSource?.() ?? []
  } catch {
    // 数据源尚未就绪（settingsStore 初始化中）：按兜底口径返回，不抛给渲染层
    providers = []
  }
  for (const p of providers) {
    for (const m of p.models ?? []) {
      if (`${p.id}/${m.id}` === modelRef) {
        return {
          limit: m.contextLimit && m.contextLimit > 0 ? m.contextLimit : DEFAULT_CONTEXT_LIMIT,
          modelName: m.id,
          providerName: p.name,
        }
      }
    }
  }
  return { limit: DEFAULT_CONTEXT_LIMIT, modelName: 'default', providerName: '' }
}

/** 紧凑 token 展示：12345 → 12.3k */
export const formatTokens = (n: number) => (n >= 1000 ? `${(n / 1000).toFixed(1)}k` : String(n))
