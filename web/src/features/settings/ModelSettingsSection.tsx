import { useState } from 'react'
import { ChevronDown, ChevronRight, ChevronUp, Trash2, X, RotateCw, Plus, Cpu, Check, Eye, EyeOff, Copy } from 'lucide-react'
import type { Provider, RequestProtocol } from '../../types'
import { useUI } from '../../stores/uiStore'
import {
  addModel, addProvider, removeModel, removeProvider, updateModel, updateProvider,
  PROVIDER_KEY_MASK, useSettings, type AppSettings, type CapSlot,
} from '../../stores/settingsStore'
import { api } from '../../lib/api'
import { Badge, Button, Field, Switch, inputCls } from '../../components/ui'
import { cn } from '../../components/ui'

const CAP_SLOTS: Array<{ key: CapSlot; label: string; hint: string }> = [
  { key: 'chat', label: 'chat', hint: '对话主模型（禁止 NULL）' },
  { key: 'vision', label: 'vision', hint: '视觉理解' },
  { key: 'image', label: 'image', hint: '图像生成（槽位仅登记，无调用方）' },
  { key: 'decision', label: 'decision', hint: '决策判断' },
]

const MODEL_CAPS: Array<Provider['models'][number]['capabilities'][number]> = ['chat', 'vision', 'image', 'decision']

// 请求协议选项（对齐桌面端 RequestProtocol 下拉）
const PROTOCOL_OPTIONS: Array<{ value: RequestProtocol; label: string }> = [
  { value: 'chat_completions', label: 'OpenAI Chat' },
  { value: 'messages', label: 'Claude Messages' },
  { value: 'response', label: 'OpenAI Response' },
  { value: 'systemone', label: 'SystemOne' },
]

// 模型设置：厂商卡（折叠/展开编辑）+ 获取模型列表 + 能力模型槽位，对齐桌面端「模型厂商（OpenAI 兼容）」
export function ModelSettingsSection() {
  const { draft, patchDraft } = useSettings()
  const { toast } = useUI()
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  // 连接测试三态：testing 测试中 / ok 可用 / fail 不可用（不可再用 ok 掩盖失败）
  const [tested, setTested] = useState<Record<string, 'ok' | 'fail' | 'testing'>>({})
  const [testMsg, setTestMsg] = useState<Record<string, string>>({})
  // 密钥显示：revealedKey 缓存服务端拉回的明文，避免每次切换都重复请求
  const [showKey, setShowKey] = useState<Record<string, boolean>>({})
  const [revealedKey, setRevealedKey] = useState<Record<string, string>>({})
  // 复制成功态（1.5s 后自动复位）
  const [copied, setCopied] = useState<Record<string, boolean>>({})
  // 获取模型列表：拉取中 / 已拉取结果 / 面板展开 / 搜索关键字 / 输入框中的模型 ID
  const [fetching, setFetching] = useState<Record<string, boolean>>({})
  const [fetched, setFetched] = useState<Record<string, string[]>>({})
  const [menuOpen, setMenuOpen] = useState<Record<string, boolean>>({})
  const [modelFilter, setModelFilter] = useState<Record<string, string>>({})
  const [newModelId, setNewModelId] = useState<Record<string, string>>({})

  if (!draft) return null
  const d = draft
  const patch = (fn: (cur: AppSettings) => AppSettings) => patchDraft(fn)
  const toggleExpand = (id: string) => setExpanded((e) => ({ ...e, [id]: !e[id] }))

  // 连接测试：失败必须显式落到 fail 态（旧实现把失败也置 ok，会误导用户以为可用）
  const handleTest = async (p: Provider) => {
    if (tested[p.id] === 'testing') return
    if (!p.baseUrl.trim()) {
      toast('warn', '请先填写该厂商的 Base URL')
      return
    }
    setTested((t) => ({ ...t, [p.id]: 'testing' }))
    setTestMsg((m) => ({ ...m, [p.id]: '' }))
    try {
      // 所见即所测：把草稿当前值（未保存的模型 / Base URL / 刚输入的密钥）一并传给后端，
      // 「添加模型未保存」「新厂商未落库」也能按界面所见直接测；掩码占位不得当密钥回传
      const res = await api.testProvider(p.id, {
        model: p.models[0]?.id,
        base_url: p.baseUrl,
        models: p.models.map((m) => m.id),
        ...(p.apiKeyMasked && p.apiKeyMasked !== PROVIDER_KEY_MASK ? { api_key: p.apiKeyMasked } : {}),
      })
      if (res.ok) {
        setTested((t) => ({ ...t, [p.id]: 'ok' }))
        setTestMsg((m) => ({ ...m, [p.id]: `${res.latency_ms}ms` }))
        toast('success', `${p.name} 连接成功（${res.latency_ms}ms）`)
      } else {
        const reason = res.error || res.code || '未知错误'
        setTested((t) => ({ ...t, [p.id]: 'fail' }))
        setTestMsg((m) => ({ ...m, [p.id]: reason }))
        toast('error', `${p.name} 连接失败：${reason}`)
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : '网络异常'
      setTested((t) => ({ ...t, [p.id]: 'fail' }))
      setTestMsg((m) => ({ ...m, [p.id]: msg }))
      toast('error', `连接测试失败：${msg}`)
    }
  }

  // 获取模型列表：真实请求上游 /models；失败直接报错，绝不回退本地样例
  const handleFetchModels = async (p: Provider) => {
    if (fetching[p.id]) return
    if (!p.baseUrl.trim()) {
      toast('warn', '请先填写该厂商的 Base URL')
      return
    }
    setFetching((f) => ({ ...f, [p.id]: true }))
    try {
      // 所见即所测：携带草稿里未保存的 Base URL / 刚输入的密钥，厂商未落库也能拉清单
      const res = await api.listProviderModels(p.id, {
        base_url: p.baseUrl,
        ...(p.apiKeyMasked && p.apiKeyMasked !== PROVIDER_KEY_MASK ? { api_key: p.apiKeyMasked } : {}),
      })
      const list = res.items ?? []
      setFetched((m) => ({ ...m, [p.id]: list }))
      setMenuOpen((o) => ({ ...o, [p.id]: true }))
      toast('success', `成功获取 ${list.length} 个模型`)
    } catch (e) {
      const msg = e instanceof Error ? e.message : '网络异常'
      // 失败时清空本地缓存，避免残留上一次的清单被误当作本次结果
      setFetched((m) => ({ ...m, [p.id]: [] }))
      setMenuOpen((o) => ({ ...o, [p.id]: false }))
      toast('error', `获取模型列表失败：${msg}`)
    } finally {
      setFetching((f) => ({ ...f, [p.id]: false }))
    }
  }

  // 切换密钥显示：首次显示时向服务端拉明文（列表接口恒脱敏，明文只能走 secret 端点）
  const toggleKey = async (p: Provider) => {
    const next = !showKey[p.id]
    setShowKey((s) => ({ ...s, [p.id]: next }))
    if (!next) return
    if (revealedKey[p.id] !== undefined) return
    if (!p.apiKeyMasked) return // 未配置密钥，无需拉取
    try {
      const res = await api.revealProviderKey(p.id)
      setRevealedKey((k) => ({ ...k, [p.id]: res.api_key ?? '' }))
    } catch (e) {
      const msg = e instanceof Error ? e.message : '网络异常'
      setShowKey((s) => ({ ...s, [p.id]: false }))
      toast('error', `读取密钥失败：${msg}`)
    }
  }

  // 复制密钥：优先用已拉回的明文，未拉过则先拉再复制
  const copyKey = async (p: Provider) => {
    if (!p.apiKeyMasked) {
      toast('warn', '该厂商尚未配置 API Key')
      return
    }
    let key = revealedKey[p.id]
    if (key === undefined) {
      try {
        const res = await api.revealProviderKey(p.id)
        key = res.api_key ?? ''
        setRevealedKey((k) => ({ ...k, [p.id]: key as string }))
      } catch (e) {
        const msg = e instanceof Error ? e.message : '网络异常'
        toast('error', `读取密钥失败：${msg}`)
        return
      }
    }
    if (!key) {
      toast('warn', '该厂商尚未配置 API Key')
      return
    }
    try {
      await navigator.clipboard.writeText(key)
      setCopied((c) => ({ ...c, [p.id]: true }))
      setTimeout(() => setCopied((c) => ({ ...c, [p.id]: false })), 1500)
      toast('success', 'API Key 已复制到剪贴板')
    } catch {
      toast('error', '复制失败，请手动选择文本复制')
    }
  }

  const handleSelectFetched = (p: Provider, name: string) => {
    setNewModelId((n) => ({ ...n, [p.id]: name }))
    setMenuOpen((o) => ({ ...o, [p.id]: false }))
    toast('success', `已填入模型「${name}」，点击「添加」导入`)
  }

  const handleAddModel = (p: Provider) => {
    const id = (newModelId[p.id] ?? '').trim()
    if (!id) {
      toast('warn', '请输入模型 ID，或先获取模型列表选择')
      return
    }
    const before = p.models.length
    patch((cur) => addModel(cur, p.id, id))
    const after = d.providers.find((x) => x.id === p.id)?.models.some((m) => m.id === id)
    if (after && before === d.providers.find((x) => x.id === p.id)?.models.length) {
      toast('warn', `模型「${id}」已存在，无需重复添加`)
      return
    }
    setNewModelId((n) => ({ ...n, [p.id]: '' }))
    toast('success', `已添加模型「${id}」`)
  }

  return (
    <div className="flex flex-col gap-6">
      {/* 模型厂商 */}
      <section>
        <div className="flex items-center justify-between mb-2.5">
          <div className="text-[14px] font-semibold text-ink">模型厂商（OpenAI 兼容）</div>
          <Button size="sm" variant="outline" onClick={() => patch((cur) => addProvider(cur))} className="gap-1 text-[12px]">
            <Plus size={12} />
            <span>添加厂商</span>
          </Button>
        </div>
        <div className="flex flex-col gap-2.5">
          {d.providers.map((p) => {
            const isExpanded = !!expanded[p.id]
            const list = fetched[p.id] ?? []
            const filterText = (modelFilter[p.id] ?? '').toLowerCase()
            const filtered = list.filter((m) => m.toLowerCase().includes(filterText))
            return (
              <div key={p.id} className="border border-edge rounded-xl bg-panel shadow-subtle">
                {/* 厂商头部 */}
                <div
                  className="p-3 flex items-center gap-2.5 cursor-pointer hover:bg-panel2/60 select-none transition-colors"
                  onClick={() => toggleExpand(p.id)}
                >
                  <span className="text-inkfaint w-4 h-4 flex items-center justify-center shrink-0">
                    {isExpanded ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
                  </span>
                  <div className="flex items-center gap-2 min-w-0 flex-1">
                    <span className="text-[13px] font-semibold text-ink truncate max-w-[160px]">{p.name || '未命名厂商'}</span>
                    <Badge tone={p.enabled ? 'blue' : 'gray'}>{p.enabled ? '已启用' : '已停用'}</Badge>
                    {!isExpanded && (
                      <span className="text-[12px] text-inkdim truncate ml-1 min-w-0" title={p.baseUrl}>
                        {p.models.length > 0
                          ? `模型: ${p.models.map((m) => m.id).join(', ')}`
                          : <span className="text-warn">未配置模型</span>}
                      </span>
                    )}
                  </div>
                  <div className="flex items-center gap-1.5 shrink-0" onClick={(e) => e.stopPropagation()}>
                    <Button size="sm" variant="ghost" onClick={() => toggleExpand(p.id)} className="h-7 text-[12px]">
                      {isExpanded ? '收起' : '编辑'}
                    </Button>
                    <button
                      className="w-7 h-7 rounded-lg hover:bg-err/10 flex items-center justify-center text-inkfaint hover:text-err transition-colors"
                      title="移除厂商"
                      onClick={() => patch((cur) => removeProvider(cur, p.id))}
                    >
                      <Trash2 size={13} />
                    </button>
                  </div>
                </div>

                {/* 展开编辑区 */}
                {isExpanded && (
                  <div className="px-3.5 pb-3.5 flex flex-col gap-3.5 border-t border-edge/60 pt-3.5 bg-panel2/20 rounded-b-xl">
                    <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                      <Field label="厂商名称">
                        <input className={inputCls} value={p.name} placeholder="如 DeepSeek"
                          onChange={(e) => patch((cur) => updateProvider(cur, p.id, { name: e.target.value }))} />
                      </Field>
                      <Field label="Base URL">
                        <input className={`${inputCls} font-mono`} value={p.baseUrl} placeholder="https://api.deepseek.com/v1"
                          onChange={(e) => patch((cur) => updateProvider(cur, p.id, { baseUrl: e.target.value }))} />
                      </Field>
                    </div>
                    <Field label="API Key" hint="已保存的密钥可点眼睛图标查看明文、点复制图标一键复制">
                      <div className="flex items-center gap-2">
                        <input
                          className={`${inputCls} font-mono`}
                          type={showKey[p.id] ? 'text' : 'password'}
                          value={showKey[p.id] ? (revealedKey[p.id] ?? '') : p.apiKeyMasked}
                          placeholder="sk-..."
                          readOnly={showKey[p.id]}
                          onChange={(e) => {
                            // 明文态下编辑即视为替换密钥（已拉回的明文不再是真值）
                            patch((cur) => updateProvider(cur, p.id, { apiKeyMasked: e.target.value }))
                            setRevealedKey((k) => {
                              const next = { ...k }
                              delete next[p.id]
                              return next
                            })
                          }}
                        />
                        <button
                          type="button"
                          className="shrink-0 w-8 h-8 rounded-lg bg-panel2 hover:bg-panel3 border border-edge text-inkdim hover:text-ink flex items-center justify-center transition-colors"
                          title={showKey[p.id] ? '隐藏密钥' : '显示密钥明文'}
                          onClick={() => void toggleKey(p)}
                        >
                          {showKey[p.id] ? <EyeOff size={14} /> : <Eye size={14} />}
                        </button>
                        <button
                          type="button"
                          className={`shrink-0 w-8 h-8 rounded-lg bg-panel2 hover:bg-panel3 border border-edge flex items-center justify-center transition-colors ${
                            copied[p.id] ? 'text-ok' : 'text-inkdim hover:text-ink'
                          } ${p.apiKeyMasked ? '' : 'opacity-40 cursor-not-allowed'}`}
                          title={p.apiKeyMasked ? '复制 API Key' : '尚未配置 API Key'}
                          disabled={!p.apiKeyMasked}
                          onClick={() => void copyKey(p)}
                        >
                          {copied[p.id] ? <Check size={14} /> : <Copy size={14} />}
                        </button>
                      </div>
                    </Field>

                    <div className="flex items-center gap-2.5 flex-wrap pt-1">
                      <span className="text-[13px] text-ink font-medium">启用厂商</span>
                      <Switch checked={p.enabled} onChange={(v) => patch((cur) => updateProvider(cur, p.id, { enabled: v }))} />
                    </div>

                    {/* 模型列表（去掉 overflow-hidden，避免把「可用模型」下拉弹窗裁掉；圆角由内部贴边条补） */}
                    <div className="rounded-xl border border-edge bg-panel">
                      <div className="px-3 py-2 text-[12px] text-ink font-medium border-b border-edge bg-panel2/40 rounded-t-[11px] flex items-center justify-between">
                        <span>模型列表（{p.models.length}）</span>
                      </div>
                      {p.models.length === 0 && (
                        <div className="px-3 py-3 text-[12px] text-inkfaint italic">未配置模型，可获取模型列表后选择，或手动输入 ID</div>
                      )}
                      <div className="divide-y divide-edge/60">
                        {p.models.map((m, i) => (
                          <div key={i} className="p-3 flex flex-col gap-2 sm:grid sm:grid-cols-[minmax(0,1fr)_10rem_12rem_2rem] sm:gap-2 sm:items-center">
                            {/* 模型 ID + 删除：移动端同行（flex），桌面端拆回网格第 1/4 列（sm:contents 让子元素直接进网格） */}
                            <div className="flex items-center gap-2 sm:contents">
                              <input className={`${inputCls} h-8 font-mono text-[12px] flex-1 min-w-0 sm:col-start-1 sm:row-start-1`} value={m.id}
                                placeholder="模型 ID（如 deepseek-chat）"
                                onChange={(e) => patch((cur) => updateModel(cur, p.id, i, { id: e.target.value }))} />
                              <button
                                className="w-8 h-8 rounded-lg border border-edge bg-panel2 hover:bg-err/10 hover:border-err/40 flex items-center justify-center text-inkfaint hover:text-err transition-colors shrink-0 sm:col-start-4 sm:row-start-1 sm:justify-self-center"
                                title="移除模型"
                                aria-label="移除模型"
                                onClick={() => patch((cur) => removeModel(cur, p.id, i))}
                              >
                                <X size={13} />
                              </button>
                            </div>
                            {/* 移动端（<sm）：上下文上限 / 请求协议 两列，标题在上方 */}
                            <div className="grid grid-cols-2 gap-2 sm:hidden">
                              <label className="flex flex-col gap-1 min-w-0">
                                <span className="text-[11px] text-inkfaint">上下文上限</span>
                                <input className={`${inputCls} h-8 text-[12px] w-full`} type="number" value={m.contextLimit}
                                  title="上下文上限（tokens）"
                                  onChange={(e) => patch((cur) => updateModel(cur, p.id, i, { contextLimit: Number(e.target.value) || 0 }))} />
                              </label>
                              <label className="flex flex-col gap-1 min-w-0">
                                <span className="text-[11px] text-inkfaint">请求协议</span>
                                <select
                                  className={`${inputCls} h-8 text-[12px] w-full pr-1 cursor-pointer`}
                                  value={m.protocol ?? 'chat_completions'}
                                  title="该模型使用的请求协议格式"
                                  onChange={(e) => patch((cur) => updateModel(cur, p.id, i, { protocol: e.target.value as RequestProtocol }))}
                                >
                                  {PROTOCOL_OPTIONS.map((o) => (
                                    <option key={o.value} value={o.value}>{o.label}</option>
                                  ))}
                                </select>
                              </label>
                            </div>
                            {/* 桌面端独占网格列：第 2 列上下文上限、第 3 列请求协议（字段语义由 title/placeholder 承载） */}
                            <input className={`${inputCls} h-8 text-[12px] hidden sm:block sm:col-start-2 sm:row-start-1`} type="number" value={m.contextLimit}
                              title="上下文上限（tokens）"
                              onChange={(e) => patch((cur) => updateModel(cur, p.id, i, { contextLimit: Number(e.target.value) || 0 }))} />
                            <select
                              className={`${inputCls} h-8 text-[12px] hidden sm:block sm:col-start-3 sm:row-start-1 pr-1 cursor-pointer`}
                              value={m.protocol ?? 'chat_completions'}
                              title="该模型使用的请求协议格式"
                              onChange={(e) => patch((cur) => updateModel(cur, p.id, i, { protocol: e.target.value as RequestProtocol }))}
                            >
                              {PROTOCOL_OPTIONS.map((o) => (
                                <option key={o.value} value={o.value}>{o.label}</option>
                              ))}
                            </select>
                            {/* 能力标签行：桌面端第 2 行占第 1-3 列，第 4 列留给纵向居中的删除按钮 */}
                            <div className="flex items-center gap-1.5 flex-wrap sm:col-start-1 sm:col-span-3 sm:row-start-2">
                              <span className="text-[11px] text-inkfaint mr-1">能力：</span>
                              {MODEL_CAPS.map((cap) => {
                                const on = m.capabilities.includes(cap)
                                return (
                                  <button key={cap}
                                    className={`text-[11px] px-2 py-0.5 rounded-md border transition-all ${
                                      on ? 'bg-accent2/15 text-accent2 border-accent2/30 font-medium' : 'text-inkfaint border-edge hover:text-ink hover:bg-panel2'
                                    }`}
                                    onClick={() => patch((cur) => updateModel(cur, p.id, i, {
                                      capabilities: on ? m.capabilities.filter((c) => c !== cap) : [...m.capabilities, cap],
                                    }))}
                                  >
                                    {cap}
                                  </button>
                                )
                              })}
                            </div>
                          </div>
                        ))}
                      </div>

                      {/* 添加模型工具行 */}
                      <div className="relative p-2.5 border-t border-edge bg-panel2/30 rounded-b-[11px] flex items-center gap-2">
                        <input
                          className={`${inputCls} h-8 font-mono text-[12px] flex-1 min-w-0`}
                          placeholder="模型 ID，或从「获取模型列表」中选择"
                          value={newModelId[p.id] ?? ''}
                          onChange={(e) => setNewModelId((n) => ({ ...n, [p.id]: e.target.value }))}
                        />
                        <button
                          type="button"
                          className="h-8 px-2.5 rounded-lg bg-panel2 hover:bg-panel3 border border-edge text-[12px] text-inkdim hover:text-ink flex items-center gap-1.5 shrink-0 transition-colors"
                          onClick={() => {
                            if (menuOpen[p.id]) { setMenuOpen((o) => ({ ...o, [p.id]: false })); return }
                            if (list.length > 0) setMenuOpen((o) => ({ ...o, [p.id]: true }))
                            else void handleFetchModels(p)
                          }}
                          title={menuOpen[p.id] ? '收起可用模型' : '获取模型列表'}
                        >
                          <RotateCw size={12} className={cn(fetching[p.id] && 'animate-spin text-accent')} />
                          <span>{fetching[p.id] ? '获取中…' : menuOpen[p.id] ? '可用模型' : '获取模型列表'}</span>
                          {!fetching[p.id] && (menuOpen[p.id] ? <ChevronUp size={11} className="text-inkfaint" /> : <ChevronDown size={11} className="text-inkfaint" />)}
                        </button>
                        <Button size="sm" variant="outline" onClick={() => handleAddModel(p)}>添加</Button>

                        {/* 可用模型下拉面板 */}
                        {menuOpen[p.id] && (
                          <div className="absolute right-2.5 bottom-11 z-20 w-72 rounded-xl border border-edge bg-panel shadow-modal flex flex-col overflow-hidden slide-up">
                            <div className="flex items-center justify-between px-3 py-2 border-b border-edge bg-panel2/50 text-[12px] text-inkdim">
                              <span className="font-semibold text-ink">
                                可用模型（{filtered.length}{filtered.length !== list.length ? ` / ${list.length}` : ''}）
                              </span>
                              <div className="flex items-center gap-1">
                                <button className="hover:text-ink hover:bg-panel2 px-1.5 py-0.5 rounded text-[11px] flex items-center gap-1"
                                  title="重新获取" onClick={() => handleFetchModels(p)}>
                                  <RotateCw size={10} />
                                  <span>刷新</span>
                                </button>
                                <button className="hover:text-ink hover:bg-panel2 w-5 h-5 flex items-center justify-center rounded text-[11px]"
                                  title="关闭" onClick={() => setMenuOpen((o) => ({ ...o, [p.id]: false }))}>
                                  <X size={12} />
                                </button>
                              </div>
                            </div>
                            <div className="p-2 border-b border-edge/60 bg-panel2/20">
                              <input
                                className="w-full h-7 px-2.5 text-[12px] bg-panel border border-edge rounded-lg outline-none focus:border-accent"
                                placeholder="搜索模型名称…"
                                value={modelFilter[p.id] ?? ''}
                                onChange={(e) => setModelFilter((f) => ({ ...f, [p.id]: e.target.value }))}
                              />
                            </div>
                            <div className="max-h-52 overflow-y-auto p-1.5 flex flex-col gap-0.5">
                              {filtered.length === 0 && (
                                <div className="text-[12px] text-inkdim text-center py-4">
                                  {list.length === 0 ? '未获取到模型' : '无匹配模型'}
                                </div>
                              )}
                              {filtered.map((name) => {
                                const added = p.models.some((m) => m.id === name)
                                return (
                                  <button key={name} type="button"
                                    className={cn(
                                      'w-full text-left px-2.5 py-1.5 rounded-lg text-[12px] flex items-center justify-between gap-2 transition-colors',
                                      newModelId[p.id] === name ? 'bg-accent/15 text-accent font-medium' : 'hover:bg-panel2 text-inkdim hover:text-ink',
                                    )}
                                    onClick={() => handleSelectFetched(p, name)}
                                    title={`点击选择「${name}」`}
                                  >
                                    <span className="truncate font-mono text-[12px]">{name}</span>
                                    {added && <span className="text-[10px] px-1.5 py-0.5 rounded bg-panel2 text-inkfaint shrink-0">已添加</span>}
                                  </button>
                                )
                              })}
                            </div>
                          </div>
                        )}
                      </div>
                    </div>

                    {/* 连接测试：依赖模型，故置于模型列表下方（对齐桌面端布局） */}
                    <div className="flex items-center gap-2.5 flex-wrap">
                      <Button size="sm" variant="outline" onClick={() => handleTest(p)}>
                        {tested[p.id] === 'testing' ? '测试中…' : '连接测试'}
                      </Button>
                      <span className="text-[11px] text-inkfaint">
                        用该厂商的首个模型发一次极小请求，无需保存即可测试
                      </span>
                      {tested[p.id] === 'ok' && (
                        <span className="text-ok text-[12px] flex items-center gap-1 font-medium" title={testMsg[p.id]}>
                          <Check size={12} />
                          <span>可用{testMsg[p.id] ? `（${testMsg[p.id]}）` : ''}</span>
                        </span>
                      )}
                      {tested[p.id] === 'fail' && (
                        <span className="text-err text-[12px] flex items-center gap-1 font-medium" title={testMsg[p.id]}>
                          <X size={12} />
                          <span className="max-w-[260px] truncate">不可用{testMsg[p.id] ? `：${testMsg[p.id]}` : ''}</span>
                        </span>
                      )}
                    </div>
                  </div>
                )}
              </div>
            )
          })}
        </div>
      </section>

      {/* 能力模型槽位 */}
      <section>
        <div className="text-[14px] font-semibold text-ink mb-1">能力模型槽位</div>
        <div className="text-[12px] text-inkfaint mb-3">能力模型允许「跟随全局」；对话槽位为会话主模型来源，禁止 NULL。当前仅有 chat 槽位真正参与模型路由；vision / image / decision 三个槽位只做登记，服务端无对应调用方（image 插件无生成/识别工具），设置后不会改变任何运行时行为。</div>
        <div className="flex flex-col gap-2">
          {CAP_SLOTS.map(({ key, label, hint }) => {
            const candidates = d.providers
              .filter((p) => p.enabled)
              .flatMap((p) => p.models.filter((m) => m.capabilities.includes(key as never)).map((m) => ({ ref: `${p.id}/${m.id}`, label: `${p.name} / ${m.id}` })))
            const value = d.capabilityModels[key]
            return (
              <div key={key} className="rounded-xl bg-panel border border-edge px-3.5 py-2.5 flex items-center gap-3 transition-colors hover:border-edge2 shadow-subtle">
                <span className="w-20 text-[13px] font-mono font-medium text-ink shrink-0">{label}</span>
                <select
                  className="flex-1 min-w-0 h-8 px-2.5 rounded-lg bg-panel2 border border-edge text-[12px] text-ink outline-none focus:border-accent"
                  value={value}
                  onChange={(e) => patch((cur) => ({ ...cur, capabilityModels: { ...cur.capabilityModels, [key]: e.target.value } }))}
                >
                  {candidates.length === 0 && <option value="">（暂无候选模型）</option>}
                  {candidates.map((c) => <option key={c.ref} value={c.ref}>{c.label}</option>)}
                  {/* chat 槽位也允许空值：未显式设置时如实显示，避免 select 静默展示首个候选造成「已设默认」假象 */}
                  {key === 'chat' && <option value="">（未设置）</option>}
                  {key !== 'chat' && <option value="">跟随全局</option>}
                </select>
                <span className="text-[11px] text-inkfaint w-44 shrink-0 hidden sm:block">{hint}</span>
              </div>
            )
          })}
        </div>
      </section>
    </div>
  )
}
