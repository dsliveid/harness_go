/**
 * 工具结果 → 文件 diff 提取（B3：消除 sampleDiff 冒充）。
 *
 * 唯一真源（后端已确认的链路）：
 *   internal/tools/invoke.go  → tool.result 事件的 `result` 字段 / tool_events.result_json
 *   = `{"content": "<主文本>", "meta": {"data": {...}}, "cost_tokens": <n>}`
 *   internal/pluginkit/kit.go DataResult(content, data) 把 data 放进 `Meta["data"]`
 *   internal/plugins/fs/fs.go 写文件/编辑文件 → DataResult(content, {"diff": FileDiff, "snapshots": [...]})
 *
 * 因此逐行 diff 只存在于 `result` 字符串里的 `meta.data.diff`，
 * 不在 tool.result 事件 payload 的顶层（payload 只有 tool_event_id/status/result/error/...）。
 *
 * 容错策略（绝不抛错、绝不伪造）：
 *   - 非字符串 / 非 JSON / 解析异常            → undefined
 *   - 无 meta.data.diff（如端侧执行、普通只读工具、纯文本结果） → undefined
 *   - path 非字符串或 hunks 非数组             → undefined
 *   - hunks 逐块校验通过者保留，全部不合法      → undefined
 *   - 合法但 hunks 为空（无实际变更）           → undefined（避免渲染空白 diff 卡片）
 */

import type { DiffHunk, DiffLine, FileDiff } from '../types'

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === 'object' && v !== null

/** 把后端的 `{type, num1?, num2?, text}` 规整为前端 DiffLine `{type, no?, text}`。 */
function toDiffLine(raw: unknown): DiffLine | null {
  if (!isRecord(raw)) return null
  const type = raw.type
  if (type !== 'add' && type !== 'del' && type !== 'ctx') return null
  // 后端用 num1/num2 标记旧/新行号；统一视图按单一 no 渲染：删除取旧行号，其余取新行号
  const num1 = typeof raw.num1 === 'number' ? raw.num1 : undefined
  const num2 = typeof raw.num2 === 'number' ? raw.num2 : undefined
  const no = typeof raw.no === 'number' ? raw.no : type === 'del' ? num1 ?? num2 : num2 ?? num1
  return {
    type,
    text: typeof raw.text === 'string' ? raw.text : String(raw.text ?? ''),
    ...(no !== undefined ? { no } : {}),
  }
}

function toDiffLines(raw: unknown): DiffLine[] | null {
  if (!Array.isArray(raw)) return null
  const out: DiffLine[] = []
  for (const item of raw) {
    const line = toDiffLine(item)
    if (!line) return null // 结构不符即整块作废，不做半真渲染
    out.push(line)
  }
  return out
}

function toDiffHunk(raw: unknown, index: number): DiffHunk | null {
  if (!isRecord(raw)) return null
  const before = toDiffLines(raw.before)
  const after = toDiffLines(raw.after)
  if (!before || !after) return null
  const id = typeof raw.id === 'string' && raw.id ? raw.id : `hunk-${index}`
  return {
    id,
    header: typeof raw.header === 'string' ? raw.header : '',
    before,
    after,
  }
}

/** 把后端 FileDiff 形状（path/hunks/additions/deletions）校验并转成前端 FileDiff。 */
function toFileDiff(raw: unknown): FileDiff | undefined {
  if (!isRecord(raw)) return undefined
  if (typeof raw.path !== 'string' || !raw.path) return undefined
  if (!Array.isArray(raw.hunks)) return undefined

  const hunks: DiffHunk[] = []
  raw.hunks.forEach((h, i) => {
    const hunk = toDiffHunk(h, i)
    if (hunk) hunks.push(hunk)
  })
  if (hunks.length === 0) return undefined // 空 diff 不渲染（无变更时后端不会产出 hunks）

  const count = (pick: 'add' | 'del') =>
    hunks.reduce((acc, h) => acc + (pick === 'add'
      ? h.after.filter((l) => l.type === 'add').length
      : h.before.filter((l) => l.type === 'del').length), 0)
  const additions = typeof raw.additions === 'number' ? raw.additions : count('add')
  const deletions = typeof raw.deletions === 'number' ? raw.deletions : count('del')

  return { path: raw.path, hunks, additions, deletions }
}

/**
 * 从工具结果中提取文件 diff。
 *
 * @param result 工具结果：SSE `tool.result` 的 `result`（字符串化 ResultJSON），
 *               或历史回放中已解析的对象（ToolEvent.result / tool 消息 content）
 * @returns 校验通过的 FileDiff；任一环节不符合契约均返回 undefined
 */
export function extractFileDiff(result: unknown): FileDiff | undefined {
  let root: unknown = result

  if (typeof result === 'string') {
    const text = result.trim()
    // 绝大多数工具结果是纯文本（命令输出、文件内容），不是 JSON → 直接放弃解析
    if (!text.startsWith('{')) return undefined
    try {
      root = JSON.parse(text) as unknown
    } catch {
      return undefined
    }
  }
  if (!isRecord(root)) return undefined

  const meta = root.meta
  const data = isRecord(meta) ? meta.data : undefined
  if (!isRecord(data)) return undefined

  return toFileDiff(data.diff)
}

/**
 * 从工具结果中取出用于展示的主文本。
 *
 * real 模式下 `tool.result` 的 `result` 是整包 ResultJSON
 * （`{"content":..., "meta":..., "cost_tokens":...}`），直接展示会把 meta 噪声铺到卡片上。
 * 因此只在**形状确认为 ResultJSON**（`{content: string}`）时解出 content，
 * 其余情况（纯文本输出 / 端侧返回任意结构）原样返回，保证不丢信息。
 */
export function extractResultContent(result: unknown): string {
  if (typeof result !== 'string') return JSON.stringify(result ?? '')
  const text = result.trim()
  if (!text.startsWith('{')) return result
  try {
    const parsed = JSON.parse(text) as unknown
    if (isRecord(parsed) && typeof parsed.content === 'string') return parsed.content
  } catch {
    /* 非 JSON：按原文展示 */
  }
  return result
}
