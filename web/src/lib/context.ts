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

/**
 * 启发式估算文本 Token 数（与老版本 harness_mini `estimateTokens` 口径一致）。
 *
 * 用途：只用「最后一条真实 usage 之后新增的内容」这一小段做增量估算，
 * 让上下文水位在用户刚发消息、模型还没回来时也能动起来。
 * 非 ASCII（中日韩）按 0.7、ASCII 按 0.3 —— 与后端 estimate 口径对齐，
 * 只做趋势展示，不作为计费依据。
 */
export function estimateTokens(s: string): number {
  if (!s) return 0
  let cjk = 0
  let other = 0
  for (const ch of s) {
    if (ch.charCodeAt(0) <= 0x7f) other++
    else cjk++
  }
  return Math.ceil(cjk * 0.7 + other * 0.3)
}

/** 上下文水位计算所需的最小消息结构（避免 lib 依赖 stores/types 全图） */
export interface ContextMessageLike {
  role: string
  content?: string
  reasoning?: string
  promptTokens?: number
  tokens?: number
  reverted?: boolean
  queued?: boolean
}

export interface ActiveContextUsage {
  /** 当前活跃上下文 token 水位（≈ 下一次请求会携带的输入规模） */
  activeTokens: number
  /** 该水位是否含估算成分（无真实 usage / 尾部有新增内容） */
  isEstimated: boolean
}

/**
 * 计算**当前会话的活跃上下文水位**（唯一口径，ContextGauge 专用）。
 *
 * 🚨 铁律：上下文占用 ≠ 会话累计 token 用量。
 * 累计用量（tokensIn + tokensOut 逐轮累加）是「这个会话一共烧了多少 token」，
 * 会随轮数单调增长、永远逼近上限；而真正决定「还能不能继续对话」的是
 * **最后一次请求携带的输入 + 该轮产出**——它在新一轮开始后可能回落（前面的
 * 长工具结果被裁掉时），这才是上下文占用应有的语义。
 *
 * 算法（对齐老版本 harness_mini computeContextUsage）：
 *   ① 倒序找最近一条带真实 `promptTokens` 的 assistant 消息，水位 = prompt + completion；
 *      —— 用 `limit * 3` 过滤被历史累计账单污染的异常值，避免直接顶到 100%。
 *   ② 累加该消息之后的消息（用户新输入 / 尚未产生 usage 的步骤）的估算增量；
 *   ③ 一条真实 usage 都没有（新对话）时，估算全量内容 + 系统提示词基线。
 *
 * 之所以允许估算：模型还没回复时若坚持「无数据即 0」，用户会看到水位从 0 开始
 * 一跳一大截，误以为上下文被清空；估算值以 isEstimated 如实标注，绝不冒充真实值。
 */
export function computeActiveContext(
  messages: ContextMessageLike[],
  limit: number,
): ActiveContextUsage {
  // 已被服务端回滚（hidden）/ 排队未发送的消息不进入上下文，不参与水位
  const live = messages.filter((m) => !m.reverted && !m.queued)

  // ① 定位最近一条有真实用量的 assistant 消息
  let lastIdx = -1
  const maxSane = limit > 0 ? limit * 3 : Number.POSITIVE_INFINITY
  for (let i = live.length - 1; i >= 0; i--) {
    const m = live[i]
    if (m.role !== 'assistant') continue
    const pt = m.promptTokens ?? 0
    if (pt > 0 && pt <= maxSane) {
      lastIdx = i
      break
    }
  }

  if (lastIdx >= 0) {
    const last = live[lastIdx]
    const prompt = last.promptTokens ?? 0
    // tokens 为 0/缺失时视为「服务端未给总用量」，直接以输入为水位（勿把输出算成负数）
    const total = last.tokens && last.tokens > 0 ? last.tokens : prompt
    // 输出量 = 该步总 token − 输入（不信任单独字段，避免历史数据口径漂移）
    let activeTokens = prompt + Math.max(0, total - prompt)
    let isEstimated = false
    // ② 尾部增量：该步之后的内容（用户刚发的话 / 尚未结算的步骤）按估算累加
    for (let i = lastIdx + 1; i < live.length; i++) {
      const m = live[i]
      const est = estimateTokens(`${m.content ?? ''}${m.reasoning ?? ''}`)
      if (est > 0) {
        activeTokens += est
        isEstimated = true
      }
    }
    return { activeTokens, isEstimated }
  }

  // ③ 尚无任何真实 usage：全量估算（系统提示词基线 1500，与老版本一致）
  let est = live.length > 0 ? 1500 : 0
  for (const m of live) {
    est += estimateTokens(`${m.content ?? ''}${m.reasoning ?? ''}`)
  }
  return { activeTokens: est, isEstimated: live.length > 0 }
}
