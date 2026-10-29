import { agentSops } from '../../mock/settingsData'
import { toggleDisabled, useSettings } from '../../stores/settingsStore'
import { Badge, Button, Switch } from '../../components/ui'
import { cn } from '../../components/ui'
import { ShieldCheck } from 'lucide-react'

const CATEGORY_CLS: Record<string, string> = {
  thinking: 'text-cyan-400 bg-cyan-500/12 border-cyan-500/30',
  memory: 'text-ok bg-ok/12 border-ok/30',
  orchestration: 'text-accent2 bg-accent2/12 border-accent2/30',
  quality: 'text-accent bg-accent/12 border-accent/30',
  workflow: 'text-warn bg-warn/12 border-warn/30',
}

// Agent SOP：标准作业程序规范启停（对齐桌面端「Agent SOP」）
export function SopPanel() {
  const { draft, patchDraft } = useSettings()
  if (!draft) return null
  const d = draft
  const isEnabled = (id: string) => !d.disabledSops.includes(id)
  const toggle = (id: string) => patchDraft((cur) => ({ ...cur, disabledSops: toggleDisabled(cur.disabledSops, id) }))
  const enableAll = () => patchDraft((cur) => ({ ...cur, disabledSops: [] }))
  const disabledCount = d.disabledSops.length

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="text-[13px] font-medium">Agent SOP 规范管理</div>
          <div className="text-[12px] text-inkdim mt-0.5 leading-relaxed max-w-[520px]">
            配置 Agent 在执行任务时遵循的标准作业程序规范（SOP）。禁用后模型将免除对应的硬性提示词约束与相关自动化行为。
          </div>
        </div>
        <div className="flex items-center gap-2.5 shrink-0">
          <Badge tone={disabledCount > 0 ? 'yellow' : 'green'}>已启用 {agentSops.length - disabledCount} / {agentSops.length}</Badge>
          {disabledCount > 0 && (
            <Button size="sm" variant="ghost" onClick={enableAll} className="text-accent">全部启用</Button>
          )}
        </div>
      </div>

      {/* SOP 体系说明卡片 */}
      <div className="p-3 rounded-xl border border-edge/80 bg-panel2/40 flex items-start gap-3 text-[12px] leading-relaxed">
        <ShieldCheck size={18} className="text-accent shrink-0 mt-0.5" />
        <div className="flex flex-col gap-1">
          <div className="font-medium">标准作业程序 (SOP) 与自动化规范说明</div>
          <div className="text-inkdim">
            SOP 是 Agent 保持谨慎、工程规范与高质量交付的底层约束。关闭某项规范后，Agent 将在该维度解除硬性规则束缚（例如：关闭方案先行后可直接动手编码；关闭边读边记后将不强制记录并跳过任务完成后的后台自动提炼）。
          </div>
        </div>
      </div>

      <div className="flex flex-col gap-2">
        <div className="flex items-center gap-2 text-[12px] font-semibold text-accent px-0.5">
          <span className="w-2 h-2 rounded-full bg-accent" />
          <span>标准作业程序 (SOP) 与工程落地规范</span>
          <span className="text-[11px] font-normal text-inkfaint">方案先行、任务状态自愈、协作编排、代码精读与精确编辑</span>
        </div>

        {agentSops.map((s) => {
          const enabled = isEnabled(s.id)
          return (
            <div key={s.id} className={cn(
              'border rounded-xl p-3 flex items-start justify-between gap-3 transition-colors',
              enabled ? 'border-edge bg-panel2/30' : 'border-edge/50 opacity-75',
            )}>
              <div className="flex-1 min-w-0">
                <div className="flex items-center gap-2 flex-wrap">
                  <code className="text-[13px] font-mono font-medium bg-panel2 px-1.5 py-0.5 rounded border border-edge/60">{s.id}</code>
                  <span className="text-[12px] font-medium">{s.name}</span>
                  <span className={cn('text-[11px] px-1.5 py-0.5 rounded-md border font-medium', CATEGORY_CLS[s.category])}>
                    {s.categoryLabel}
                  </span>
                </div>
                <div className="text-[12px] text-inkdim mt-1.5 leading-relaxed">{s.description}</div>
                <div className="text-[11px] text-inkfaint mt-1.5 flex items-start gap-1">
                  <span>↳</span>
                  <span>{s.disableEffect}</span>
                </div>
              </div>
              <div className="flex items-center gap-2 shrink-0 pt-0.5">
                <span className={cn('text-[12px] select-none whitespace-nowrap', enabled ? 'text-ok font-medium' : 'text-inkdim')}>
                  {enabled ? '启用' : '禁用'}
                </span>
                <Switch checked={enabled} onChange={() => toggle(s.id)} title={`${enabled ? '禁用' : '启用'} ${s.name}`} />
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )
}
