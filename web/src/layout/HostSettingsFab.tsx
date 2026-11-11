/**
 * 「宿主设置」浮标（契约三，2026-11 客户端通用流程）。
 *
 * 为什么浮标而不是菜单项：主界面三端同构（服务端下发同一份代码），
 * 客户端的「设置」入口不属于任何业务菜单 —— 它是壳的属性。固定右下角
 * 的小圆钮对三端形态都成立，且不与 TopBar 的业务动作抢占空间。
 *
 * 渲染条件：客户端壳内（桌面 Tauri / 移动 Flutter 桥）才显示，纯浏览器零干扰。
 *
 * 降级纪律（lib/hostShell.ts）：桌面通道失败（invoke 抛错 / 无函数）由
 * openHostSettings 返回 false 并 console.warn；本组件据返回值隐藏浮标，
 * 避免留下一个「点了没反应」的死钮。移动深链没有可观察的返回值，
 * 失败场景由原生侧 toast 提示（webview_page 的 _rejectDeepLink）。
 */
import { useState } from 'react'
import { Settings } from 'lucide-react'
import { openHostSettings, shouldShowHostSettings } from '../lib/hostShell'

export function HostSettingsFab() {
  const [disabled, setDisabled] = useState(false)
  // 首帧就检测不到壳 → 不渲染（零开销）；点击通道失败 → 隐藏。
  if (!shouldShowHostSettings() || disabled) return null

  return (
    <button
      type="button"
      title="返回宿主设置（配对/登录/日志）"
      aria-label="返回宿主设置"
      className="fixed right-3 bottom-3 z-40 w-9 h-9 rounded-full bg-panel border border-line/60 text-inkfaint hover:text-ink hover:border-line shadow-sm flex items-center justify-center transition-colors"
      onClick={() => {
        // 桌面：open_baseline 不可用（capability 未授权 / 壳太旧）→ 隐藏浮标。
        // 移动：深链已写出，导航请求交给原生拦截（成功则页面即将离开）。
        if (!openHostSettings()) setDisabled(true)
      }}
    >
      <Settings size={15} />
    </button>
  )
}
