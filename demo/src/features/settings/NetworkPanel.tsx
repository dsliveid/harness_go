import { useState } from 'react'
import { useUI } from '../../stores/uiStore'
import { useSettings } from '../../stores/settingsStore'
import { Switch, inputCls } from '../../components/ui'
import { cn } from '../../components/ui'

// 网络代理：统一正向代理开关 + 地址 + 连接测试（对齐桌面端「网络代理」）
export function NetworkPanel() {
  const { draft, patchDraft } = useSettings()
  const { toast } = useUI()
  const [testing, setTesting] = useState(false)
  const [result, setResult] = useState<{ ok: boolean; msg: string } | null>(null)

  if (!draft) return null
  const d = draft

  const handleTest = () => {
    if (!d.proxyEnabled || testing) return
    const target = d.proxyUrl.trim() || 'http://127.0.0.1:7890'
    setTesting(true)
    setResult(null)
    setTimeout(() => {
      setTesting(false)
      // 原型模拟：socks5 或空地址给出提示，其余成功
      const ok = !target.startsWith('socks5://') || Math.random() > 0.3
      if (ok) {
        const ms = 60 + Math.round(Math.random() * 240)
        setResult({ ok: true, msg: `连接正常（延迟 ${ms}ms）` })
        toast('success', `代理连接正常（${ms}ms）`)
      } else {
        setResult({ ok: false, msg: '连接失败：proxy refused（原型模拟）' })
        toast('error', '代理连接失败')
      }
    }, 600)
  }

  return (
    <div className="flex flex-col gap-4">
      <div>
        <div className="text-[13px] font-medium">网络代理设置</div>
        <div className="text-[12px] text-inkdim mt-1 leading-relaxed max-w-[560px]">
          统一配置客户端网络正向代理。开启后，网页抓取工具、终端命令环境与厂商模型请求将统一经由此代理访问，解决境外网站与模型 API 受阻的问题。
        </div>
      </div>

      {/* 启用开关 */}
      <div className="border border-edge rounded-xl p-3.5 bg-panel2/30 flex items-start justify-between gap-4">
        <div>
          <div className="text-[13px] font-medium">启用网络代理</div>
          <div className="text-[12px] text-inkdim mt-1 leading-relaxed">
            {d.proxyEnabled
              ? '代理已启用：模型请求、网页抓取与命令环境将经由下方代理地址转发。'
              : '当前为直连模式：所有网络请求不经代理，直接访问目标地址。'}
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0 pt-0.5">
          <span className={cn('text-[12px] select-none', d.proxyEnabled ? 'text-ok font-medium' : 'text-inkdim')}>
            {d.proxyEnabled ? '已启用' : '未启用'}
          </span>
          <Switch
            checked={d.proxyEnabled}
            title={d.proxyEnabled ? '停用网络代理' : '启用网络代理'}
            onChange={(v) => {
              patchDraft((cur) => ({ ...cur, proxyEnabled: v }))
              setResult(null)
            }}
          />
        </div>
      </div>

      {/* 代理地址 */}
      <div className={cn('border border-edge rounded-xl p-3.5 bg-panel2/30 flex flex-col gap-3 transition-opacity', !d.proxyEnabled && 'opacity-60')}>
        <div className="text-[13px] font-medium">代理服务器地址</div>
        <div className="flex items-center gap-2">
          <input
            type="text"
            className={`${inputCls} font-mono`}
            placeholder="http://127.0.0.1:7890"
            value={d.proxyUrl}
            disabled={!d.proxyEnabled}
            onChange={(e) => { patchDraft((cur) => ({ ...cur, proxyUrl: e.target.value })); setResult(null) }}
          />
          <button
            type="button"
            className="h-9 sm:h-8 px-3 rounded-lg border border-edge bg-panel2 hover:bg-edge/40 text-[12px] font-medium shrink-0 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
            disabled={!d.proxyEnabled || testing}
            onClick={handleTest}
          >
            {testing ? '测试中…' : '测试连接'}
          </button>
        </div>

        {result && (
          <div className={cn(
            'text-[12px] px-2.5 py-1.5 rounded-lg border',
            result.ok ? 'text-ok bg-ok/10 border-ok/25' : 'text-err bg-err/10 border-err/25',
          )}>
            {result.msg}
          </div>
        )}

        <div className="text-[12px] text-inkdim/80 leading-relaxed space-y-1 mt-1">
          <div>• 支持协议：HTTP / HTTPS / SOCKS5（例如 <code className="font-mono text-[11px] bg-panel2 px-1 py-0.5 rounded">http://127.0.0.1:7890</code> 或 <code className="font-mono text-[11px] bg-panel2 px-1 py-0.5 rounded">socks5://127.0.0.1:7890</code>）。</div>
          <div>• 本地回环直连：内部通信及本地模型服务（<code className="font-mono text-[11px] bg-panel2 px-1 py-0.5 rounded">localhost</code>、<code className="font-mono text-[11px] bg-panel2 px-1 py-0.5 rounded">127.0.0.1</code>）自动白名单直连，不受代理拦截。</div>
        </div>
      </div>
    </div>
  )
}
