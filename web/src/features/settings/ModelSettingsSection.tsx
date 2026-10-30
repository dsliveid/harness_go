import { useState } from 'react'
import { ChevronDown, ChevronRight, ChevronUp, Trash2, X, RotateCw, Plus, Cpu, Check } from 'lucide-react'
import type { Provider, RequestProtocol } from '../../types'
import { useUI } from '../../stores/uiStore'
import {
  addModel, addProvider, removeModel, removeProvider, updateModel, updateProvider,
  useSettings, type AppSettings, type CapSlot,
} from '../../stores/settingsStore'
import { fetchProviderModels } from '../../mock/settingsData'
import { api, apiMode } from '../../lib/api'
import { Badge, Button, Field, Switch, inputCls } from '../../components/ui'
import { cn } from '../../components/ui'

const CAP_SLOTS: Array<{ key: CapSlot; label: string; hint: string }> = [
  { key: 'chat', label: 'chat', hint: '对话主模型（禁止 NULL）' },
  { key: 'vision', label: 'vision', hint: '视觉理解' },
  { key: 'image', label: 'image', hint: '图像生成' },
  { key: 'decision', label: 'decision', hint: '决策判断' },
]

const MODEL_CAPS: Array<Provider['models'][number]['capabilities'][number]> = ['chat', 'reasoning', 'vision', 'image', 'decision']

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
  const [tested, setTested] = useState<Record<string, 'ok' | 'testing'>>({})
  const [showKey, setShowKey] = useState<Record<string, boolean>>({})
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

  const handleTest = async (p: Provider) => {
    if (tested[p.id] === 'testing') return
    if (!p.baseUrl.trim()) {
      toast('warn', '请先填写该厂商的 Base URL')
      return
    }
    setTested((t) => ({ ...t, [p.id]: 'testing' }))
    // mock 模式：demo 的模拟延迟；real 模式：由服务端用该厂商发一次极小请求
    if (apiMode === 'mock') {
      setTimeout(() => {
        setTested((t) => ({ ...t, [p.id]: 'ok' }))
        toast('success', `${p.name} 连接成功（模拟 218ms）`)
      }, 500)
      return
    }
    try {
      const res = await api.testProvider(p.id)
      if (res.ok) {
        setTested((t) => ({ ...t, [p.id]: 'ok' }))
        toast('success', `${p.name} 连接成功（${res.latency_ms}ms）`)
      } else {
        setTested((t) => ({ ...t, [p.id]: 'ok' }))
        toast('error', `${p.name} 连接失败：${res.error || res.code || '未知错误'}`)
      }
    } catch (e) {
      setTested((t) => ({ ...t, [p.id]: 'ok' }))
      const msg = e instanceof Error ? e.message : '网络异常'
      toast('error', `连接测试失败：${msg}${p.id.startsWith('pv-') ? '（请先保存后再测试）' : ''}`)
    }
  }

  // 模拟 GET {baseUrl}/models：按厂商名匹配目录，600ms 延迟
  const handleFetchModels = (p: Provider) => {
    if (fetching[p.id]) return
    if (!p.baseUrl.trim()) {
      toast('warn', '请先填写该厂商的 Base URL')
      return
    }
    setFetching((f) => ({ ...f, [p.id]: true }))
    setTimeout(() => {
      const list = fetchProviderModels({ name: p.name, baseUrl: p.baseUrl })
      setFetched((m) => ({ ...m, [p.id]: list }))
      setMenuOpen((o) => ({ ...o, [p.id]: true }))
      setFetching((f) => ({ ...f, [p.id]: false }))
      toast('success', `成功获取 ${list.length} 个模型`)
    }, 600)
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
            const isDefault = d.defaultProviderId === p.id
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
                    {isDefault && <Badge tone="green">默认对话</Badge>}
                    {!isDefault && <Badge tone={p.enabled ? 'blue' : 'gray'}>{p.enabled ? '已启用' : '已停用'}</Badge>}
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
                    <Field label="API Key" hint="保存后仅以掩码形式展示（原型模拟）">
                      <div className="flex items-center gap-2">
                        <input
                          className={`${inputCls} font-mono`}
                          type={showKey[p.id] ? 'text' : 'password'}
                          value={p.apiKeyMasked}
                          placeholder="sk-..."
                          onChange={(e) => patch((cur) => updateProvider(cur, p.id, { apiKeyMasked: e.target.value }))}
                        />
                        <Button size="sm" variant="ghost" onClick={() => setShowKey((s) => ({ ...s, [p.id]: !s[p.id] }))} className="shrink-0">
                          {showKey[p.id] ? '隐藏' : '显示'}
                        </Button>
                      </div>
                    </Field>

                    <div className="flex items-center gap-2.5 flex-wrap pt-1">
                      <span className="text-[13px] text-ink font-medium">启用厂商</span>
                      <Switch checked={p.enabled} onChange={(v) => patch((cur) => updateProvider(cur, p.id, { enabled: v }))} />
                      <span className="flex-1" />
                      <Button size="sm" variant={isDefault ? 'soft' : 'outline'} title="设为默认对话厂商"
                        onClick={() => { patch((cur) => ({ ...cur, defaultProviderId: p.id })); toast('success', `已将 ${p.name} 设为默认对话厂商`) }}>
                        默认对话
                      </Button>
                      <Button size="sm" variant="outline" onClick={() => handleTest(p)}>
                        {tested[p.id] === 'testing' ? '测试中…' : '连接测试'}
                      </Button>
                      {tested[p.id] === 'ok' && (
                        <span className="text-ok text-[12px] flex items-center gap-1 font-medium">
                          <Check size={12} />
                          <span>可用</span>
                        </span>
                      )}
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
                            else handleFetchModels(p)
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
        <div className="text-[12px] text-inkfaint mb-3">能力模型允许「跟随全局」；对话槽位为会话主模型来源，禁止 NULL。</div>
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
