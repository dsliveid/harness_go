/**
 * 宿主壳环境检测 + 「返回宿主设置」浮标（契约三，2026-11 客户端通用流程）。
 *
 * ## 为什么这个检测要做在服务端 SPA 里（而不是各客户端自带壳）
 *
 * 主界面是**服务端下发**的（三端共享同一份前端代码）。桌面端配对后会导航进来，
 * 此时它的基线页（设置/登录/配对入口）已被替换，用户在 Web UI 里没有任何通道
 * 回去 —— 这是「单向门」。把检测做成通用件后：
 * - 桌面壳：浮标调 `invoke('open_baseline')`（壳侧导航回基线页）；
 * - 移动壳：浮标写 `harness://settings` 深链（原生拦截后 push /setup）；
 * - 纯浏览器：两者都检测不到，不渲染浮标（零干扰）。
 *
 * 新客户端只要暴露这两个特征之一，浮标即自动可用 —— 这就是「通用流程」的接缝。
 *
 * ## 降级纪律
 *
 * 检测到壳 ≠ IPC 一定通（桌面 capability 可能未授权 remote domain）。
 * invoke 失败时浮标**自动隐藏**并 console.warn 一次，绝不弹错误打断用户工作。
 */

/** 桌面壳（Tauri 2）检测：__TAURI_INTERNALS__ 由 WebView 初始化脚本无条件注入。 */
export function isDesktopShell(): boolean {
  return typeof window !== 'undefined' && '__TAURI_INTERNALS__' in window
}

/** 移动壳（Flutter inappwebview 桥）检测：js_bridge 注入 window.harnessMobile。 */
export function isMobileBridge(): boolean {
  if (typeof window === 'undefined') return false
  const bridge = (window as unknown as { harnessMobile?: { installed?: unknown } }).harnessMobile
  return bridge?.installed === true
}

/** 是否运行在任一客户端壳内（纯浏览器为 false）。 */
export function isHostedShell(): boolean {
  return isDesktopShell() || isMobileBridge()
}

/** 移动壳深链契约（对齐移动端 js_bridge 的 kDeepLinkScheme） */
const HARNESS_DEEP_LINK_SCHEME = 'harness'

/**
 * 浮标点击结果。
 *
 * 🔴 为什么不是裸 boolean：调用方要区分「跳过去了」与「通道不通」才能决定是否
 * 隐藏浮标；同时 `reason` 让降级原因（无 invoke / ACL 拒绝 / 壳侧导航失败）
 * 能被日志带出来 —— 早期版本只返回 boolean 且被 `if (!promise)` 误判，
 * 结果是一个点了毫无反应、也不会自我隐藏的死钮（2026-11 修复）。
 */
export interface OpenHostResult {
  ok: boolean
  /** 失败原因（仅用于 console 诊断，不面向用户展示）。 */
  reason?: string
}

/** 浮标点击动作：按壳环境分派（桌面 invoke / 移动深链）。 */
async function openHostSettingsImpl(): Promise<OpenHostResult> {
  if (isDesktopShell()) {
    try {
      const internals = (
        window as unknown as { __TAURI_INTERNALS__?: { invoke?: (cmd: string) => Promise<unknown> } }
      ).__TAURI_INTERNALS__
      const invoke = internals?.invoke
      if (typeof invoke !== 'function') {
        return { ok: false, reason: '__TAURI_INTERNALS__.invoke 缺失' }
      }
      await invoke('open_baseline')
      return { ok: true }
    } catch (err) {
      // capability 未授权 / 壳太旧没有该命令 / 壳侧导航失败：降级隐藏，不打断工作。
      const reason = err instanceof Error ? err.message : String(err)
      console.warn('[hostSettings] open_baseline 不可用（浮标将隐藏）：', err)
      return { ok: false, reason }
    }
  }
  if (isMobileBridge()) {
    // 单向深链：页面把 location 指到 harness://settings，原生拦截导航请求。
    // 用 try 包住：部分 WebView 对自定义 scheme 的赋值可能抛错。
    try {
      window.location.href = `${HARNESS_DEEP_LINK_SCHEME}://settings`
      return { ok: true }
    } catch (err) {
      const reason = err instanceof Error ? err.message : String(err)
      console.warn('[hostSettings] 深链跳转失败（浮标将隐藏）：', err)
      return { ok: false, reason }
    }
  }
  return { ok: false, reason: '非客户端壳环境（无 IPC 通道）' }
}

/** 浮标是否渲染：只在客户端壳内显示（纯浏览器无「宿主设置」概念）。 */
export function shouldShowHostSettings(): boolean {
  return isHostedShell()
}

/**
 * 浮标点击动作（供 HostSettingsFab 绑定）：按壳环境分派。
 *
 * 🔴 调用方**必须 await 后再读 `ok`**。此函数返回 Promise，
 * `if (!openHostSettings())` 这类写法永远是 false（Promise 恒真值），
 * 会退化成「点了没反应且不隐藏」的死钮 —— 正是本次修复的缺陷根源。
 */
export async function openHostSettings(): Promise<OpenHostResult> {
  return openHostSettingsImpl()
}
