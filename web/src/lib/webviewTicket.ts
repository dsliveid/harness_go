/**
 * WebView 免登 ticket 的 Web 端消费逻辑（协议 §6.2 方案 C 的「Web 一半」）。
 *
 * 背景（为何必须有这个模块）：移动端 `prepareAuth` 会调 `POST /api/v1/auth/webview-ticket`
 * 签出一次性 ticket，并用 `appendTicketFragment` 把它拼进入口 URL 的 **fragment**：
 *   - 无 hash 路由：`https://host/#ticket=xxx`
 *   - 有 hash 路由：`https://host/#/route&ticket=xxx`（在原 fragment 后以 `&` 追加）
 * fragment 按规范**不会发到服务端**（不进 Referer、不进 access log），所以服务端无法代替
 * 浏览器完成兑换 —— 必须由 Web 前端读取 fragment 后主动 `POST /api/v1/auth/webview-exchange`，
 * 服务端才通过 `Set-Cookie: hg_sess=...` 落一个降级会话。
 *
 * 历史事故：本模块缺失时，ticket 被签出后无人消费 → `/auth/me` 无 Cookie → 首屏渲染
 * 登录表单，表现为移动端「两轮登录」。故这里的纯函数就是那条断链的接线点。
 *
 * 本文件只做**纯字符串/地址栏处理**，不发任何网络请求，便于静态守门与单测。
 */

/** fragment 中承载 ticket 的键名（与移动端 `appendTicketFragment` 严格一致） */
const TICKET_KEY = 'ticket'

/** 去掉 fragment 可能的井号前缀，避免调用方传 `#ticket=x` 或 `ticket=x` 时行为不一致 */
function bareFragment(hash: string): string {
  const text = hash.startsWith('#') ? hash.slice(1) : hash
  return text
}

/**
 * 从 URL fragment 中读取一次性 ticket。
 *
 * 为什么这么解析：移动端 `appendTicketFragment` 的产物有两种形态——
 *   `#ticket=xxx`（服务端 `webEntryUri` 只给 `/`）与 `#/route&ticket=xxx`（保留原 hash 路由）。
 * 因此不能假定 fragment 只有 ticket 一项，必须按 `&` 切分键值对逐项匹配；
 * 值由移动端 `Uri.encodeComponent` 编码，这里对应 `decodeURIComponent` 还原。
 *
 * @returns ticket 明文；不存在、空值或解码失败（畸形 percent-encoding）一律返回 null。
 */
export function readTicketFromHash(hash: string): string | null {
  for (const pair of bareFragment(hash).split('&')) {
    if (!pair) continue
    const eq = pair.indexOf('=')
    // 没有等号的片段是纯路径（如 `/route`），不是键值对，跳过
    if (eq < 0) continue
    if (pair.slice(0, eq) !== TICKET_KEY) continue
    const raw = pair.slice(eq + 1)
    if (!raw) return null // 空 ticket 视为无效（服务端同样按无效处理）
    try {
      return decodeURIComponent(raw)
    } catch {
      return null // 畸形编码不得抛异常打断启动流程
    }
  }
  return null
}

/**
 * 从 fragment 中抹掉 ticket 键，保留其它键与原有顺序。
 *
 * 为什么要抹：ticket 是一次性凭据，若继续留在地址栏，会被写入浏览器历史
 * 或经 Referer/截图/history 转发泄露，造成「已用票据被重放」的风险面。
 *
 * @returns 处理后的 fragment（带 `#` 前缀）；若抹完什么都不剩则返回**空串**
 *          —— 绝不能返回孤立的 `#` 或残留的 `&`（会污染 hash 路由解析）。
 */
export function stripTicketFromFragment(hash: string): string {
  const kept = bareFragment(hash)
    .split('&')
    // 空片段（如 `a&&b` 里的空串）与 ticket 键一并丢弃
    .filter((pair) => pair && pair.split('=')[0] !== TICKET_KEY)
  return kept.length === 0 ? '' : `#${kept.join('&')}`
}

/**
 * 组合动作：从真实地址栏取 ticket → **立刻**抹干净地址栏 → 返回 ticket 供调用方兑换。
 *
 * ⚠️ 顺序铁律：必须先 `replace` 再返回。调用方随后才会发兑换请求；
 * 若先返回后抹，ticket 会在网络往返期间继续留在地址栏（历史/Referer 可被重放）。
 * 同时用 `pathname + search + 清理后的 hash` 重建 URL，只动 fragment，不改路由与查询参数。
 *
 * @param loc     形如 `window.location` 的最小结构（便于纯函数测试）
 * @param replace 地址栏替换回调（生产传 `window.history.replaceState` 包装，
 *                不产生新的历史条目，用户按返回键不会回到带 ticket 的地址）
 * @returns ticket 明文；读不到则返回 null 且**不调用** replace（不制造无谓的历史操作）。
 */
export function takeTicketFromLocation(
  loc: { hash: string; pathname: string; search: string },
  replace: (url: string) => void,
): string | null {
  const ticket = readTicketFromHash(loc.hash)
  if (!ticket) return null
  // 先抹地址栏，再交还 ticket —— 顺序不可交换（见上方「顺序铁律」）
  replace(`${loc.pathname}${loc.search}${stripTicketFromFragment(loc.hash)}`)
  return ticket
}
