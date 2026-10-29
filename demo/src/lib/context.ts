import { providers } from '../mock/data'

/** 智能压缩触发比例（与桌面端一致：达到 75% 需人工确认） */
export const COMPACTION_RATIO = 0.75
/** 未匹配到模型时的兜底上下文窗口 */
export const DEFAULT_CONTEXT_LIMIT = 64000

/** 统一解析会话模型的上下文窗口，避免多处各算一份漂移 */
export function resolveModelContext(modelRef?: string): {
  limit: number
  modelName: string
  providerName: string
} {
  for (const p of providers) {
    for (const m of p.models) {
      if (`${p.id}/${m.id}` === modelRef) {
        return { limit: m.contextLimit, modelName: m.id, providerName: p.name }
      }
    }
  }
  return { limit: DEFAULT_CONTEXT_LIMIT, modelName: 'default', providerName: '' }
}

/** 紧凑 token 展示：12345 → 12.3k */
export const formatTokens = (n: number) => (n >= 1000 ? `${(n / 1000).toFixed(1)}k` : String(n))
