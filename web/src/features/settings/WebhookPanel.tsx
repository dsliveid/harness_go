import { useCallback, useEffect, useState } from 'react'
import { Plug, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { Badge, Button, Switch, cn } from '../../components/ui'
import { useUI } from '../../stores/uiStore'
import { api } from '../../lib/api'
import type { Webhook } from '../../lib/apiTypes'

/**
 * Webhook 事件订阅管理（P1 / 需求 §4.4）。
 *
 * 数据来源：`api.listWebhooks()` 等 6 个方法直连 internal/api/router.go:57-62。
 * 铁律：
 *   - real 模式下列表/操作失败**原样展示后端错误文案**（尤其 `unavailable`：后端 h.d.Webhooks == nil），
 *     绝不静默回退演示数据；
 *   - secret 只写不读：后端只回 `has_secret`，表单提交后立即清空输入，永不在列表/表单回填明文。
 */

/** 一期支持的事件名（来源 internal/service/webhook_service.go:38-52 WebhookEvents 白名单）。 */
const EVENT_OPTIONS: Array<{ name: string; label: string }> = [
  { name: 'run.completed', label: 'Run 完成' },
  { name: 'run.failed', label: 'Run 失败' },
  { name: 'run.interrupted', label: 'Run 中断' },
  { name: 'approval.requested', label: '待审批' },
  { name: 'tool.failed', label: '工具失败' },
]

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e))

const fmtTime = (t: number) => {
  if (!t) return '—'
  const d = new Date(t)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

const inputCls =
  'h-8 px-2.5 rounded-lg bg-panel2 border border-edge text-[12px] outline-none focus:border-accent placeholder:text-inkfaint'

export function WebhookPanel() {
  const toast = useUI((s) => s.toast)
  const [items, setItems] = useState<Webhook[]>([])
  const [loading, setLoading] = useState(true)
  /** 列表级错误（含后端 unavailable），有值时整表替换为错误态，不展示任何假行 */
  const [error, setError] = useState<string | null>(null)
  /** 正在执行的操作标识：'create' 或订阅 id */
  const [busy, setBusy] = useState<string | null>(null)
  /** 删除二次确认目标 id */
  const [confirmId, setConfirmId] = useState<string | null>(null)
  /** 每行联通测试结果（本地展示，不持久化） */
  const [tested, setTested] = useState<Record<string, { ok: boolean; text: string }>>({})

  // 新增表单
  const [url, setUrl] = useState('')
  const [secret, setSecret] = useState('')
  const [events, setEvents] = useState<string[]>([])
  const [formErr, setFormErr] = useState<string | null>(null)

  const load = useCallback(() => {
    setLoading(true)
    api
      .listWebhooks()
      .then((res) => {
        setItems(res.items ?? [])
        setError(null)
      })
      .catch((e: unknown) => {
        // 不兜底成假数据：失败即展示后端原文（unavailable / http_error 等）
        setError(errText(e))
        setItems([])
      })
      .finally(() => setLoading(false))
  }, [])

  useEffect(load, [load])

  const toggleEvent = (name: string) =>
    setEvents((cur) => (cur.includes(name) ? cur.filter((x) => x !== name) : [...cur, name]))

  const submit = () => {
    const target = url.trim()
    if (!target) {
      setFormErr('URL 为必填项')
      return
    }
    setBusy('create')
    setFormErr(null)
    api
      .createWebhook({ url: target, secret: secret.trim() ? secret.trim() : undefined, events })
      .then(() => {
        // secret 只写不读：提交成功即清空，永不回填
        setUrl('')
        setSecret('')
        setEvents([])
        toast('success', '已新增 Webhook 订阅')
        load()
      })
      .catch((e: unknown) => setFormErr(errText(e)))
      .finally(() => setBusy(null))
  }

  const setEnabled = (w: Webhook, next: boolean) => {
    setBusy(w.id)
    const p = next ? api.enableWebhook(w.id) : api.disableWebhook(w.id)
    p.then(() => {
      toast('success', `已${next ? '启用' : '停用'}订阅`)
      load()
    })
      .catch((e: unknown) => toast('error', `${next ? '启用' : '停用'}失败：${errText(e)}`))
      .finally(() => setBusy(null))
  }

  const fire = (w: Webhook) => {
    setBusy(w.id)
    api
      .testWebhook(w.id)
      .then((r) => {
        setTested((cur) => ({
          ...cur,
          [w.id]: {
            ok: r.ok,
            text: r.ok ? `投递成功（HTTP ${r.status}）` : `投递失败（HTTP ${r.status}）· ${r.error || '无错误详情'}`,
          },
        }))
      })
      .catch((e: unknown) => {
        setTested((cur) => ({ ...cur, [w.id]: { ok: false, text: `测试失败：${errText(e)}` } }))
      })
      .finally(() => setBusy(null))
  }

  const remove = (w: Webhook) => {
    setBusy(w.id)
    api
      .deleteWebhook(w.id)
      .then(() => {
        setConfirmId(null)
        toast('success', '订阅已删除')
        load()
      })
      .catch((e: unknown) => toast('error', `删除失败：${errText(e)}`))
      .finally(() => setBusy(null))
  }

  return (
    <div className="flex flex-col gap-4">
      <div>
        <div className="text-[13px] font-medium">Webhook 事件订阅</div>
        <div className="text-[12px] text-inkdim mt-0.5 leading-relaxed">
          Run 完成/失败/中断、待审批与工具失败事件将 POST 到订阅地址（5s 超时，失败退避重试 3 次）。签名密钥只写不读，列表仅显示是否已配置。
        </div>
      </div>

      {/* 新增订阅表单 */}
      <div className="rounded-xl border border-edge bg-panel2/30 p-3 space-y-2.5">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[12px] font-semibold shrink-0">新增订阅</span>
          <Badge tone="blue">POST JSON</Badge>
        </div>

        <div className="flex flex-col sm:flex-row gap-2">
          <input
            className={cn(inputCls, 'flex-1 min-w-0')}
            placeholder="https://example.com/hooks/harness-go（必填）"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            disabled={busy === 'create'}
          />
          <input
            className={cn(inputCls, 'sm:w-52')}
            type="password"
            autoComplete="new-password"
            placeholder="签名密钥（可选，只写不读）"
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            disabled={busy === 'create'}
          />
          <Button variant="primary" onClick={submit} disabled={busy === 'create'}>
            <Plus size={14} />
            {busy === 'create' ? '提交中…' : '新增'}
          </Button>
        </div>

        <div className="flex items-center gap-1.5 flex-wrap">
          <span className="text-[11px] text-inkfaint shrink-0">订阅事件</span>
          {EVENT_OPTIONS.map((ev) => {
            const active = events.includes(ev.name)
            return (
              <button
                key={ev.name}
                type="button"
                onClick={() => toggleEvent(ev.name)}
                className={cn(
                  'px-2 h-6 rounded-lg border text-[11px] transition-colors',
                  active
                    ? 'border-accent/50 bg-accent/12 text-accent font-medium'
                    : 'border-edge bg-panel2 text-inkdim hover:border-edge/80',
                )}
                title={ev.name}
              >
                {ev.label}
              </button>
            )
          })}
          <span className="text-[11px] text-inkfaint">
            {events.length === 0 ? '未选择 = 订阅全部一期事件' : `已选 ${events.length} 项`}
          </span>
        </div>

        {formErr && (
          <div className="rounded-lg border border-err/40 bg-err/[0.06] px-2.5 py-1.5 text-[11px] text-err leading-relaxed">
            新增失败：{formErr}
          </div>
        )}
      </div>

      {/* 列表 */}
      <div className="flex items-center gap-2">
        <span className="text-[12px] font-semibold">订阅列表</span>
        <span className="text-[11px] text-inkfaint">共 {items.length} 条</span>
        <Button size="sm" variant="ghost" className="ml-auto" onClick={load} disabled={loading}>
          <RefreshCw size={13} />
          刷新
        </Button>
      </div>

      {error ? (
        <div className="rounded-xl border border-err/40 bg-err/[0.06] px-3 py-2.5 text-[12px] text-err leading-relaxed">
          <div className="font-medium">订阅列表加载失败</div>
          <div className="mt-1 font-mono text-[11px] break-all">{error}</div>
          <div className="mt-1.5 text-[11px] text-inkdim">
            该文案来自后端原样返回。若为「Webhook 子系统未接线」，表示服务端未注册 Webhook 用例（依赖未注入），需在服务端完成接线后重试。
          </div>
        </div>
      ) : (
        <div className="rounded-xl border border-edge overflow-x-auto">
          <table className="w-full text-[12px] min-w-[720px]">
            <thead>
              <tr className="text-inkfaint text-left border-b border-edge">
                <th className="px-3 py-2 font-medium">URL</th>
                <th className="px-3 py-2 font-medium">事件</th>
                <th className="px-3 py-2 font-medium">状态</th>
                <th className="px-3 py-2 font-medium">Secret</th>
                <th className="px-3 py-2 font-medium">创建时间</th>
                <th className="px-3 py-2 font-medium text-right">操作</th>
              </tr>
            </thead>
            <tbody>
              {loading && (
                <tr>
                  <td colSpan={6} className="px-3 py-6 text-center text-inkfaint">
                    加载中…
                  </td>
                </tr>
              )}
              {!loading && items.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-3 py-6 text-center text-inkfaint">
                    暂无 Webhook 订阅。在上方填入回调 URL 即可新增第一条。
                  </td>
                </tr>
              )}
              {items.map((w) => {
                const wait = busy === w.id
                const t = tested[w.id]
                return (
                  <tr key={w.id} className="border-b border-edge/60 last:border-b-0 align-top">
                    <td className="px-3 py-2 max-w-[260px]">
                      <div className="font-mono text-[11px] break-all">{w.url}</div>
                      {t && (
                        <div className={cn('mt-1 text-[11px] leading-relaxed', t.ok ? 'text-ok' : 'text-err')}>
                          {t.text}
                        </div>
                      )}
                    </td>
                    <td className="px-3 py-2">
                      {w.events.length === 0 ? (
                        <span className="text-[11px] text-inkfaint">全部事件</span>
                      ) : (
                        <span className="inline-flex flex-wrap gap-1">
                          {w.events.map((ev) => (
                            <span
                              key={ev}
                              className="px-1.5 py-[1px] rounded border border-edge bg-panel2 text-[10px] font-mono text-inkdim"
                            >
                              {ev}
                            </span>
                          ))}
                        </span>
                      )}
                    </td>
                    <td className="px-3 py-2">
                      <div className="flex items-center gap-2">
                        <Switch
                          checked={w.enabled}
                          disabled={wait}
                          onChange={(v) => setEnabled(w, v)}
                          title={`${w.enabled ? '停用' : '启用'}该订阅`}
                        />
                        <span className={cn('text-[11px] whitespace-nowrap', w.enabled ? 'text-ok font-medium' : 'text-inkdim')}>
                          {w.enabled ? '启用' : '停用'}
                        </span>
                      </div>
                    </td>
                    <td className="px-3 py-2">
                      {w.has_secret ? <Badge tone="green">已配置</Badge> : <Badge tone="gray">未配置</Badge>}
                    </td>
                    <td className="px-3 py-2 text-[11px] text-inkdim whitespace-nowrap font-mono">{fmtTime(w.created_at)}</td>
                    <td className="px-3 py-2">
                      <div className="flex items-center justify-end gap-1.5">
                        <Button size="sm" variant="soft" disabled={wait} onClick={() => fire(w)} title="投递一条 webhook.test 事件">
                          <Plug size={13} />
                          测试
                        </Button>
                        {confirmId === w.id ? (
                          <>
                            <Button size="sm" variant="danger" disabled={wait} onClick={() => remove(w)}>
                              {wait ? '删除中…' : '确认删除'}
                            </Button>
                            <Button size="sm" variant="ghost" disabled={wait} onClick={() => setConfirmId(null)}>
                              取消
                            </Button>
                          </>
                        ) : (
                          <Button size="sm" variant="ghost" disabled={wait} onClick={() => setConfirmId(w.id)} title="删除订阅">
                            <Trash2 size={13} />
                          </Button>
                        )}
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      <div className="rounded-lg border border-edge bg-panel2/40 px-2.5 py-2 text-[11px] text-inkfaint">
        说明：签名密钥（secret）只在创建时提交，服务端加密落库且**永不回显**，故列表与表单均不回填明文；「测试」按钮同步投递一条
        <code className="font-mono text-[11px] bg-panel2 px-1 rounded">webhook.test</code>
        事件（不重试），对端返回非 2xx 时展示失败原因。
      </div>
    </div>
  )
}
