import { Info } from 'lucide-react'
import { apiMode } from '../lib/api'

/**
 * 「本面板仍是本地演示数据」提示条（阶段 B-5 明示降级）。
 *
 * 背景：插件市场 / 定时任务 / 项目空间等能力的真实后端属于阶段 E/F/G，
 * B 阶段只做「接线 + 明示」，不造后端。real 模式下若不告知，用户会误以为
 * 面板里看到的是服务端真实状态（例如"已安装 3 个插件"实际只是内存 mock）。
 *
 * mock 模式下不渲染（整个应用都是演示数据，无需逐处提示）。
 */
export function RealModeNotice({ text }: { text: string }) {
  if (apiMode !== 'real') return null
  return (
    <div className="shrink-0 mx-3 mt-2 rounded-lg border border-warn/40 bg-warn/[0.06] px-2.5 py-1.5 text-[11px] text-warn leading-relaxed flex items-start gap-1.5 select-none">
      <Info size={11} className="mt-0.5 shrink-0" />
      <span>{text}</span>
    </div>
  )
}
