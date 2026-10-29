import { useEffect, useRef, useState, type ReactNode } from 'react'
import { FileText, Shield, FolderGit2, ShieldAlert, Settings, Zap, Folder, Database } from 'lucide-react'
import type { DataScope, DataScopeMode, PlanMode, Project } from '../../types'
import { Button, Modal, Tabs, cn, inputCls } from '../../components/ui'
import { MarkdownLite } from '../../components/MarkdownLite'
import { DATA_MODE_META, DATA_SCOPES } from '../../lib/dataScopes'
import { useSessions } from '../../stores/sessionStore'
import { useUI } from '../../stores/uiStore'

type Tab = 'constraints' | 'policy' | 'links' | 'approvals' | 'data'

/** 与桌面端 estimate_tokens 同口径的粗估：非 ASCII 按字计（×0.7）、ASCII 4 字符 1 token */
function estimateTokens(s: string): number {
  let cjk = 0
  let other = 0
  for (const ch of s) {
    if (ch.charCodeAt(0) <= 0x7f) other++
    else cjk++
  }
  return Math.floor(cjk * 0.7) + Math.floor(other / 4)
}

const normPath = (p: string) => p.trim().replace(/\\/g, '/').toLowerCase().replace(/\/+$/, '')

// ---------------- 项目约束页（草稿由外壳统一保存） ----------------
function ConstraintsPane({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const [mode, setMode] = useState<'edit' | 'preview'>(value.trim() ? 'preview' : 'edit')
  const toggleCls = (active: boolean) =>
    `px-3 py-1.5 rounded-lg text-[12px] font-medium transition-all ${active ? 'bg-panel2 text-ink shadow-subtle' : 'text-inkfaint hover:text-ink'}`

  return (
    <div className="flex flex-col h-full min-h-0">
      <div className="flex items-center gap-2 mb-2 shrink-0">
        <div className="font-semibold text-[14px] text-ink">项目约束</div>
        <span className="text-[11px] text-inkdim truncate hidden sm:block">
          该项目下每个新对话自动阅读；对话进行中修改，下一条消息生效
        </span>
        <div className="ml-auto flex gap-1 shrink-0 p-0.5 rounded-lg bg-panel border border-edge">
          <button className={toggleCls(mode === 'edit')} onClick={() => setMode('edit')}>编辑</button>
          <button className={toggleCls(mode === 'preview')} onClick={() => setMode('preview')}>预览</button>
        </div>
      </div>

      {mode === 'edit' ? (
        <>
          <textarea
            className="flex-1 min-h-[220px] sm:min-h-0 bg-panel2 border border-edge rounded-xl p-3.5 text-[13px] font-mono leading-relaxed outline-none focus:border-accent resize-none text-ink"
            value={value}
            placeholder="使用 Markdown 撰写本项目的规范、注意事项、编码约定等…"
            onChange={(e) => onChange(e.target.value)}
          />
          <div className="shrink-0 mt-1.5 text-[11px] text-inkfaint text-right">
            ≈ {estimateTokens(value)} tokens · {value.length} 字符（约束随 system prompt 常驻上下文，建议精炼）
          </div>
        </>
      ) : value.trim() ? (
        <div className="flex-1 min-h-0 overflow-y-auto bg-panel2 border border-edge rounded-xl p-3.5">
          <MarkdownLite text={value} />
        </div>
      ) : (
        <div className="flex-1 flex items-center justify-center text-inkfaint text-[13px]">
          尚未设置项目约束，切换到「编辑」撰写本项目的规范与注意事项
        </div>
      )}
    </div>
  )
}

// ---------------- 规划与执行策略页（草稿由外壳统一保存） ----------------
const policyOptions: Array<{ id: PlanMode; title: string; badge: string; desc: string; detail: string; icon: ReactNode }> = [
  {
    id: 'standard',
    title: '标准模式 (Standard)',
    badge: '默认模式',
    desc: '灵活自决。面对复杂大改动由 Agent 根据通用准则自主决定，常规任务直接处理。',
    detail: '适合日常对话与普通编码任务。不作强制中断与前置门禁，维持轻量灵活体验。',
    icon: <Settings size={14} className="text-accent" />,
  },
  {
    id: 'always_plan',
    title: '强计划确认模式 (Always Plan)',
    badge: '严格安全风控',
    desc: '凡对项目进行较大改动（预计改动 3+ 文件、架构重构、新增功能），必须先生成计划，由您确认后才可实施。',
    detail: '阶段严格解耦：只读调研 → 物理生成计划并落盘 → 强制停步等待人工确认。未获确认前严格禁止调用任何写入或修改工具。',
    icon: <Shield size={14} className="text-purple-400" />,
  },
  {
    id: 'always_proceed',
    title: '自动推进模式 (Always Proceed)',
    badge: '敏捷连续交付',
    desc: '面对较大改动同样必须生成规范计划，但在生成计划后会自动连续编写代码，无需停步等待确认。',
    detail: '兼顾规范与速度：若您在指令中明确说明「先不改动代码 / 仅出方案」，则仅输出方案；否则出具计划后自动无缝实施修改。',
    icon: <Zap size={14} className="text-amber-400" />,
  },
]

function PolicyPane({ value, onChange }: { value: PlanMode; onChange: (v: PlanMode) => void }) {
  return (
    <div className="flex flex-col h-full min-h-0">
      <div className="flex items-center gap-2 mb-3 shrink-0">
        <div className="font-medium text-[14px]">规划与执行策略</div>
        <span className="text-[11px] text-inkdim truncate hidden sm:block">
          控制复杂工程任务中任务方案拟定与代码落地的交互方式
        </span>
      </div>
      <div className="flex-1 min-h-0 overflow-y-auto flex flex-col gap-2.5 pr-1">
        {policyOptions.map((opt) => {
          const selected = value === opt.id
          return (
            <div
              key={opt.id}
              onClick={() => onChange(opt.id)}
              className={`p-3.5 rounded-xl border cursor-pointer transition-all flex flex-col gap-1.5 ${
                selected
                  ? 'border-accent bg-accent/10 ring-1 ring-accent/30'
                  : 'border-edge bg-panel2/40 hover:bg-panel2'
              }`}
            >
              <div className="flex items-center gap-2.5">
                <span className="text-[15px] shrink-0">{opt.icon}</span>
                <span className="text-[13px] font-medium text-ink">{opt.title}</span>
                <span
                  className={`text-[10px] px-2 py-0.5 rounded-full font-medium ${
                    selected ? 'bg-accent/20 text-accent' : 'bg-panel2 text-inkfaint'
                  }`}
                >
                  {opt.badge}
                </span>
                <div className="ml-auto">
                  <div
                    className={`w-4 h-4 rounded-full border flex items-center justify-center transition-colors ${
                      selected ? 'border-accent bg-accent' : 'border-edge bg-panel'
                    }`}
                  >
                    {selected && <div className="w-1.5 h-1.5 rounded-full bg-white" />}
                  </div>
                </div>
              </div>
              <p className="text-[12px] text-ink leading-relaxed sm:pl-6">{opt.desc}</p>
              <p className="text-[11px] text-inkfaint leading-relaxed sm:pl-6">{opt.detail}</p>
            </div>
          )
        })}
      </div>
    </div>
  )
}

// ---------------- 关联项目页（行内表单确认后立即落库，与原型 store 直写一致） ----------------
function LinksPane({
  project,
  onOpenConstraints,
}: {
  project: Project
  onOpenConstraints: () => void
}) {
  const { projects, projectLinks, addProjectLink, updateProjectLink, deleteProjectLink } = useSessions()
  const toast = useUI((s) => s.toast)
  const links = projectLinks.filter((l) => l.projectId === project.id)
  // 行内表单：id 缺省 = 新增，否则为编辑该条
  const [form, setForm] = useState<{ id?: string; path: string; description: string } | null>(null)
  const pathRef = useRef<HTMLInputElement | null>(null)
  const descRef = useRef<HTMLTextAreaElement | null>(null)

  // Web 端无本地目录选择插件，改为手动输入目录路径
  const startAdd = () => setForm({ path: '', description: '' })

  const commitForm = () => {
    if (!form) return
    // 以 DOM 值为准做兜底，避免受控 state 未同步时丢输入
    const path = (pathRef.current?.value ?? form.path).trim()
    const description = descRef.current?.value ?? form.description
    if (!path) { toast('warn', '请填写关联目录路径'); return }
    if (project.path && normPath(project.path) === normPath(path)) {
      toast('warn', '不能关联项目自身的目录')
      return
    }
    const dup = links.find((l) => normPath(l.path) === normPath(path) && l.id !== form.id)
    if (dup) {
      toast('warn', '该目录已关联')
      return
    }
    if (form.id) updateProjectLink(form.id, path, description)
    else addProjectLink(project.id, path, description)
    setForm(null)
    toast('success', form.id ? '关联项目已更新' : '关联项目已添加')
  }

  const remove = (l: { id: string; path: string }) => {
    const name = l.path.split(/[\\/]/).filter(Boolean).pop() || l.path
    if (!window.confirm(`移除对「${name}」的关联？`)) return
    deleteProjectLink(l.id)
    if (form?.id === l.id) setForm(null)
    toast('success', '关联已移除')
  }

  // 关联目录若对应本工具中的项目且已设约束 → 注入时会带上其自身约束
  const linkedHasConstraints = (path: string) =>
    projects.some((p) => normPath(p.path) === normPath(path) && (p.constraints ?? '').trim().length > 0)

  return (
    <div className="flex flex-col h-full min-h-0">
      <div className="flex items-center gap-2 mb-2 shrink-0">
        <div className="font-medium text-[14px]">关联项目</div>
        <span className="text-[11px] text-inkdim truncate hidden sm:block">
          新对话会一并注入关联说明；与本项目约束冲突时，以关联项目为准
        </span>
        <Button size="sm" variant="soft" className="ml-auto shrink-0" onClick={startAdd}>+ 添加关联项目</Button>
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto flex flex-col gap-2 pr-1">
        {/* 添加 / 编辑表单 */}
        {form && (
          <div className="border border-accent/40 rounded-xl p-3 bg-panel2/40 flex flex-col gap-2 shrink-0">
            <label className="block">
              <div className="text-[12px] text-inkdim mb-1">关联目录</div>
              <input
                ref={pathRef}
                className={inputCls}
                placeholder="输入关联项目的目录路径，如 F:/WorkSpace/Other/harness_mini"
                value={form.path}
                onChange={(e) => setForm({ ...form, path: e.target.value })}
              />
              <div className="text-[11px] text-inkfaint mt-1">Web 端无法打开本地目录选择器，请手动输入路径</div>
            </label>
            <label className="block">
              <div className="text-[12px] text-inkdim mb-1">说明</div>
              <textarea
                ref={descRef}
                className="w-full min-h-[80px] p-2.5 rounded-lg bg-panel2 border border-edge text-[13px] leading-relaxed outline-none focus:border-accent resize-none"
                placeholder="说明该关联项目：用途、依赖关系、目录约定、注意事项等（Markdown）…"
                value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })}
              />
            </label>
            <div className="flex justify-end gap-2">
              <Button size="sm" variant="ghost" onClick={() => setForm(null)}>取消</Button>
              <Button size="sm" variant="primary" onClick={commitForm}>确定</Button>
            </div>
          </div>
        )}

        {/* 当前项目：列表首行，只读 */}
        <div className="border border-edge rounded-xl p-3 bg-panel2/60 shrink-0">
          <div className="flex items-center gap-2">
            <span className="text-[13px] font-medium truncate">{project.name}</span>
            <span className="text-[11px] text-inkdim font-mono truncate flex-1" title={project.path}>
              {project.path || '未绑定目录'}
            </span>
            <span className="text-[11px] text-inkdim shrink-0 hidden sm:block">当前项目 · 只读</span>
            {(project.constraints ?? '').trim() ? (
              <button
                className="text-[11px] text-accent hover:underline shrink-0"
                title="查看项目约束"
                onClick={onOpenConstraints}
              >
                已设约束
              </button>
            ) : null}
          </div>
        </div>

        {/* 关联项目条目 */}
        {links.map((l) => (
          <div key={l.id} className="border border-edge rounded-xl p-3 bg-panel2/30 shrink-0">
            <div className="flex items-center gap-2">
              <Folder size={14} className="text-accent shrink-0" />
              <span className="text-[13px] font-medium shrink-0">
                {l.path.split(/[\\/]/).filter(Boolean).pop() || l.path}
              </span>
              <span className="text-[11px] text-inkdim font-mono truncate flex-1" title={l.path}>{l.path}</span>
              {linkedHasConstraints(l.path) && (
                <span className="text-[11px] text-accent shrink-0 hidden sm:block" title="该目录对应的项目已设置约束，注入时一并带上">
                  已注入该项目约束
                </span>
              )}
              <Button size="sm" variant="ghost" className="shrink-0" onClick={() => setForm({ id: l.id, path: l.path, description: l.description })}>编辑</Button>
              <Button size="sm" variant="ghost" className="text-err hover:text-err shrink-0" onClick={() => remove(l)}>删除</Button>
            </div>
            {l.description.trim() && (
              <div className="text-[12px] text-inkdim mt-1.5 whitespace-pre-wrap break-words max-h-20 overflow-y-auto">
                {l.description}
              </div>
            )}
          </div>
        ))}

        {links.length === 0 && !form && (
          <div className="flex-1 flex items-center justify-center text-inkfaint text-[13px]">
            暂无关联项目。点击「添加关联项目」并撰写说明
          </div>
        )}
      </div>
    </div>
  )
}

// ---------------- 审批规则页（原设置弹窗「审批规则」迁入，项目级） ----------------
function ApprovalsPane({ projectId }: { projectId: string }) {
  const { approvalRules, addApprovalRule, removeApprovalRule } = useSessions()
  const toast = useUI((s) => s.toast)
  const rules = approvalRules.filter((r) => r.projectId === projectId)
  const [toolName, setToolName] = useState('')
  const [pattern, setPattern] = useState('')
  const toolRef = useRef<HTMLInputElement | null>(null)
  const patternRef = useRef<HTMLInputElement | null>(null)

  const submit = () => {
    // 以 DOM 值为准做兜底，避免受控 state 未同步时丢输入
    const t = (toolRef.current?.value ?? toolName).trim()
    const p = (patternRef.current?.value ?? pattern).trim()
    if (!t || !p) {
      toast('warn', '请填写工具名与路径/命令模式')
      return
    }
    addApprovalRule(projectId, t, p)
    setToolName('')
    setPattern('')
    toast('success', '审批规则已添加')
  }

  return (
    <div className="flex flex-col h-full min-h-0">
      <div className="flex items-center gap-2 mb-2 shrink-0">
        <div className="font-medium text-[14px]">审批规则</div>
        <span className="text-[11px] text-inkdim truncate hidden sm:block">
          该项目下会话的工具放行规则；审批时选「本会话允许」也会自动生成
        </span>
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto flex flex-col gap-3 pr-1">
        {/* 添加表单 */}
        <div className="rounded-xl border border-edge bg-panel2/40 p-3 flex flex-col sm:flex-row gap-2 shrink-0">
          <input
            ref={toolRef}
            className={`${inputCls} sm:w-48 font-mono`}
            placeholder="工具名，如 run_command"
            value={toolName}
            onChange={(e) => setToolName(e.target.value)}
          />
          <input
            ref={patternRef}
            className={`${inputCls} flex-1 font-mono`}
            placeholder="路径 / 命令模式，如 go test"
            value={pattern}
            onChange={(e) => setPattern(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter') submit() }}
          />
          <Button variant="primary" className="shrink-0" onClick={submit}>添加规则</Button>
        </div>

        {/* 规则列表 */}
        <div className="space-y-1.5">
          {rules.map((r) => (
            <div key={r.id} className="flex items-center gap-2 rounded-lg border border-edge px-2.5 py-2 bg-panel2/30">
              <span className="font-mono text-[12px] text-inkdim shrink-0">{r.toolName}</span>
              <span className="font-mono text-[12px] text-inkfaint flex-1 truncate">{r.pattern}</span>
              <Button size="sm" variant="ghost" onClick={() => removeApprovalRule(r.id)}>删除</Button>
            </div>
          ))}
          {rules.length === 0 && <div className="text-[12px] text-inkfaint">暂无规则（审批时选「本会话允许」会自动生成）</div>}
        </div>

        <div className="rounded-lg border border-edge p-3 text-[12px] text-inkfaint space-y-1 shrink-0">
          <div>· 只读工具自动放行；写入/命令默认需审批；高危命令强制逐次确认</div>
          <div>· 外部插件（MCP/HTTP）默认按 write 风险处理</div>
          <div>· 拒绝原因会作为工具结果回传模型</div>
        </div>
      </div>
    </div>
  )
}

// ---------------- 数据权限页：数据范围 × 拒绝/询问/允许 ----------------
const MODE_ACTIVE_CLS: Record<DataScopeMode, string> = {
  deny: 'border-err/45 bg-err/10 text-err font-medium',
  ask: 'border-warn/45 bg-warn/10 text-warn font-medium',
  allow: 'border-ok/45 bg-ok/10 text-ok font-medium',
}

function DataScopePane({ projectId }: { projectId: string }) {
  const { dataGrants, setDataScope } = useSessions()
  const toast = useUI((s) => s.toast)

  const modeOf = (scope: DataScope): DataScopeMode =>
    dataGrants.find((g) => g.projectId === projectId && g.scope === scope)?.mode ?? 'ask'

  return (
    <div className="flex flex-col h-full min-h-0">
      <div className="flex items-center gap-2 mb-2 shrink-0">
        <div className="font-semibold text-[14px] text-ink">数据权限</div>
        <span className="text-[11px] text-inkdim truncate hidden sm:block">
          独立于工具审批：控制 Agent 能触达哪些数据，越界一律进入审批闸门
        </span>
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto pr-1 flex flex-col gap-2">
        {DATA_SCOPES.map((s) => {
          const mode = modeOf(s.key)
          return (
            <div key={s.key} className="rounded-xl border border-edge bg-panel2/30 p-2.5">
              <div className="flex items-start gap-2">
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-1.5 flex-wrap">
                    <span className="text-[13px] font-medium text-ink">{s.label}</span>
                    <code className="font-mono text-[10px] text-inkfaint bg-panel2 px-1 py-0.5 rounded border border-edge/70">
                      {s.key}
                    </code>
                  </div>
                  <div className="text-[11px] text-inkfaint mt-0.5 leading-relaxed">{s.hint}</div>
                  <div className="text-[11px] text-inkfaint/85 mt-0.5 font-mono truncate" title={s.example}>
                    {s.example}
                  </div>
                </div>
              </div>

              <div className="mt-2 flex gap-1">
                {DATA_MODE_META.map((m) => {
                  const active = mode === m.key
                  return (
                    <button
                      key={m.key}
                      className={cn(
                        'flex-1 rounded-lg border px-2 py-1 text-[11px] transition-all duration-150',
                        active
                          ? MODE_ACTIVE_CLS[m.key]
                          : 'border-edge bg-panel text-inkfaint hover:text-ink hover:border-edge2',
                      )}
                      title={m.desc}
                      onClick={() => {
                        setDataScope(projectId, s.key, m.key)
                        toast('success', `「${s.label}」策略已更新为「${m.label}」`)
                      }}
                    >
                      {m.label}
                    </button>
                  )
                })}
              </div>
            </div>
          )
        })}

        <div className="rounded-lg border border-edge p-2.5 text-[11px] text-inkfaint space-y-1 shrink-0">
          <div>· 工具审批管「能不能执行动作」，数据权限管「这次能触达哪些数据」</div>
          <div>· 「询问」= 每次触达弹审批条；「允许」= 自动放行并记审计日志；「拒绝」= 直接回传失败</div>
          <div>· 审批时选「本项目允许」会自动把对应范围置为「允许」，可在本页回收</div>
        </div>
      </div>
    </div>
  )
}

// ---------------- 弹窗外壳：左侧菜单 + 右侧内容 + 统一操作条（对齐桌面端） ----------------
export function ProjectModal({ project, onClose }: { project: Project | null; onClose: () => void }) {
  const { setProjectConstraints, setProjectPlanMode } = useSessions()
  const toast = useUI((s) => s.toast)
  const [tab, setTab] = useState<Tab>('constraints')
  // 草稿：约束文本 + 计划模式；打开时装载，「应用 / 保存」时统一落库（关联项目 / 审批规则为行内即时提交）
  const [constraints, setConstraints] = useState('')
  const [planMode, setPlanMode] = useState<PlanMode>('standard')

  // 打开（或切换项目）时重置页面与草稿
  useEffect(() => {
    if (!project) return
    setTab('constraints')
    setConstraints(project.constraints ?? '')
    setPlanMode(project.planMode ?? 'standard')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [project?.id])

  if (!project) return null
  const dirty =
    constraints !== (project.constraints ?? '') ||
    planMode !== (project.planMode ?? 'standard')

  // 应用：把草稿统一写入 store（约束 + 计划模式），不关闭窗口
  const apply = () => {
    if (constraints !== (project.constraints ?? '')) setProjectConstraints(project.id, constraints)
    if (planMode !== (project.planMode ?? 'standard')) setProjectPlanMode(project.id, planMode)
    toast('success', '项目设置已应用')
  }

  // 保存：保存数据并关闭窗口
  const save = () => {
    if (dirty) apply()
    onClose()
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={
        <span className="flex items-center gap-2 min-w-0">
          <span className="shrink-0">项目设置</span>
          <span className="text-[13px] text-inkdim font-normal truncate">
            · {project.name}（{project.path || '未绑定目录'}）
          </span>
        </span>
      }
      width="max-w-4xl"
      height="h-[min(600px,92dvh)] sm:h-[min(600px,86dvh)]"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button variant="outline" disabled={!dirty} onClick={apply}>应用</Button>
          <Button variant="primary" onClick={save}>保存</Button>
        </>
      }
    >
      <div className="flex flex-col sm:flex-row h-full min-h-0">
        <div className="sm:w-44 border-b sm:border-b-0 sm:border-r border-edge/80 p-2.5 shrink-0 flex flex-col bg-panel2/20">
          <Tabs
            items={[
              { key: 'constraints', label: <span className="flex items-center gap-2"><FileText size={13} className="text-accent" /><span>项目约束</span></span> },
              { key: 'policy', label: <span className="flex items-center gap-2"><Shield size={13} className="text-purple-400" /><span>规划与执行</span></span> },
              { key: 'links', label: <span className="flex items-center gap-2"><FolderGit2 size={13} className="text-emerald-400" /><span>关联项目</span></span> },
              { key: 'approvals', label: <span className="flex items-center gap-2"><ShieldAlert size={13} className="text-warn" /><span>审批规则</span></span> },
              { key: 'data', label: <span className="flex items-center gap-2"><Database size={13} className="text-sky-400" /><span>数据权限</span></span> },
            ]}
            value={tab}
            onChange={(v) => setTab(v as Tab)}
            className="sm:flex-col sm:items-stretch"
          />
        </div>

        <div className="flex-1 p-3 sm:p-4 min-w-0 min-h-0">
          {tab === 'constraints' && <ConstraintsPane value={constraints} onChange={setConstraints} />}
          {tab === 'policy' && <PolicyPane value={planMode} onChange={setPlanMode} />}
          {tab === 'links' && <LinksPane project={project} onOpenConstraints={() => setTab('constraints')} />}
          {tab === 'approvals' && <ApprovalsPane projectId={project.id} />}
          {tab === 'data' && <DataScopePane projectId={project.id} />}
        </div>
      </div>
    </Modal>
  )
}
