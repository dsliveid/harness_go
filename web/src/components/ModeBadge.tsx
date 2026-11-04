import { useState } from 'react'
import { FlaskConical, Server, Info } from 'lucide-react'
import { API_MODE_STORAGE_KEY, apiMode } from '../lib/api'
import { cn } from './ui'

/**
 * 全局数据模式角标（🟢 REAL / 🟡 DEMO）。
 *
 * 背景：前端存在两类假数据——「走开关的假」（api 在 real/mock 间切换）与「绕过开关的假」
 * （组件直引 mock）。用户在页面上无法分辨当前看到的到底是后端真实数据还是演示数据。
 * 本角标把 `apiMode` 变成**任何页面都能一眼看到**的事实，并提供一键切换。
 *
 * 切换方式：写 localStorage['harness.apiMode'] 后整页 reload。
 * （`apiMode` 是模块加载期求值的常量，运行期无法热改，必须重载才生效。）
 */
export function ModeBadge() {
  const [asking, setAsking] = useState(false)
  const isReal = apiMode === 'real'

  const switchTo = (mode: 'real' | 'mock') => {
    try {
      localStorage.setItem(API_MODE_STORAGE_KEY, mode)
    } catch {
      /* 无 localStorage（隐私模式）时降级：仅提示刷新无效，不抛错 */
    }
    location.reload()
  }

  return (
    <div className="relative shrink-0">
      <button
        type="button"
        onClick={() => setAsking((v) => !v)}
        title={
          isReal
            ? '当前为 REAL 模式：所有已接线面板走真实后端接口（未接线面板会有黄色提示条）'
            : '当前为 DEMO 模式：全部数据为前端演示样例，与后端无关'
        }
        className={cn(
          'inline-flex items-center gap-1 px-1.5 py-0.5 rounded-md text-[10px] font-semibold border transition-colors select-none',
          isReal
            ? 'text-ok border-ok/35 bg-ok/10 hover:bg-ok/20'
            : 'text-warn border-warn/35 bg-warn/10 hover:bg-warn/20',
        )}
      >
        <span className={cn('h-1.5 w-1.5 rounded-full shrink-0', isReal ? 'bg-ok' : 'bg-warn')} />
        <span className="hidden sm:inline">{isReal ? 'REAL' : 'DEMO'}</span>
      </button>

      {asking && (
        <>
          <div className="fixed inset-0 z-40" onClick={() => setAsking(false)} />
          <div className="absolute right-0 top-full mt-1.5 z-50 w-[286px] rounded-xl border border-edge bg-panel shadow-modal p-3 text-[11.5px]">
            <div className="flex items-center gap-1.5 text-ink font-semibold text-[12px]">
              {isReal ? <Server size={13} className="text-ok" /> : <FlaskConical size={13} className="text-warn" />}
              <span>数据模式：{isReal ? 'REAL（真实后端）' : 'DEMO（前端演示）'}</span>
            </div>
            <div className="mt-1.5 text-[10.5px] text-inkfaint leading-relaxed flex items-start gap-1">
              <Info size={11} className="mt-0.5 shrink-0" />
              <span>
                {isReal
                  ? '已接线的面板读取服务端真实数据；仍有部分面板是本地演示数据，会显示黄色提示条明示。'
                  : '所有数据都是前端内置样例，不发起任何后端请求（登录页的演示入口仅在此模式出现）。'}
              </span>
            </div>
            <div className="mt-2.5 pt-2 border-t border-edge/60 flex items-center gap-1.5">
              <button
                type="button"
                disabled={isReal}
                onClick={() => switchTo('real')}
                className="flex-1 h-7 rounded-lg text-[11px] font-medium border border-ok/35 text-ok hover:bg-ok/10 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
              >
                切换到 REAL
              </button>
              <button
                type="button"
                disabled={!isReal}
                onClick={() => switchTo('mock')}
                className="flex-1 h-7 rounded-lg text-[11px] font-medium border border-warn/35 text-warn hover:bg-warn/10 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
              >
                切换到 DEMO
              </button>
            </div>
            <div className="mt-1.5 text-[9.5px] text-inkfaint leading-relaxed">
              切换后整页重载生效（写入 localStorage['{API_MODE_STORAGE_KEY}']）。
            </div>
          </div>
        </>
      )}
    </div>
  )
}
