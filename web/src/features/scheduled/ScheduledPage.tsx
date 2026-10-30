import { useState } from 'react'
import { Clock, Plus, Play, Trash2, Terminal, CheckCircle2, XCircle, Loader2, PauseCircle } from 'lucide-react'
import { useSchedule } from '../../stores/scheduleStore'
import { usePlugins } from '../../stores/pluginStore'
import { useUI } from '../../stores/uiStore'
import { Badge, Button, Empty, Modal, Switch, cn } from '../../components/ui'
import { PromptModal, type PromptField } from '../../components/PromptModal'
import { RealModeNotice } from '../../components/RealModeNotice'
import type { ScheduledTask } from '../../types'

const fmtTime = (t?: number) =>
  t ? new Date(t).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '—'

function StatusBadge({ t }: { t: ScheduledTask }) {
  if (t.lastStatus === 'running') {
    return (
      <Badge tone="blue">
        <span className="inline-flex items-center gap-1">
          <Loader2 size={10} className="animate-spin" /> 执行中
        </span>
      </Badge>
    )
  }
  if (t.lastStatus === 'success') {
    return (
      <Badge tone="green">
        <span className="inline-flex items-center gap-1">
          <CheckCircle2 size={10} /> 上次成功
        </span>
      </Badge>
    )
  }
  if (t.lastStatus === 'failed') {
    return (
      <Badge tone="red">
        <span className="inline-flex items-center gap-1">
          <XCircle size={10} /> 上次失败
        </span>
      </Badge>
    )
  }
  return <Badge tone="gray">尚未执行</Badge>
}

// 【布局定稿】定时任务改为覆盖式弹窗：不再替换右侧主内容，对话区始终保留。
// 弹窗高度固定（min 720px / 视口 86dvh），内容切换与任务增删都不改变外框高度，仅内部列表滚动。
export function ScheduledModal() {
  const { tasks, toggle, runNow, remove, create } = useSchedule()
  const { devices } = usePlugins()
  const { scheduledOpen, setScheduled, toast } = useUI()
  const [prompt, setPrompt] = useState<PromptField[] | null>(null)
  const [confirmId, setConfirmId] = useState<string | null>(null)

  const clientName = (id: string) => devices.find((d) => d.id === id)?.name ?? '未知客户端'
  const enabledCount = tasks.filter((t) => t.enabled).length

  const openCreate = () => setPrompt([
    { key: 'name', label: '任务名称', value: '', placeholder: '如：每日构建健康巡检' },
    { key: 'cron', label: '调度表达式（cron）', value: '0 9 * * 1-5', placeholder: '0 9 * * 1-5', hint: '五段式：分 时 日 月 周；示例 0 9 * * 1-5 = 工作日 09:00' },
    { key: 'prompt', label: '执行提示词', value: '', placeholder: '到点后下发给 Agent 的任务描述' },
  ])

  const submitCreate = (v: Record<string, string>) => {
    if (!v.name?.trim() || !v.prompt?.trim()) {
      toast('error', '任务名称与提示词不能为空')
      return
    }
    create({ name: v.name.trim(), cron: v.cron?.trim() || '0 9 * * *', prompt: v.prompt.trim() })
    setPrompt(null)
    toast('success', `定时任务「${v.name.trim()}」已创建，由本客户端本地调度`)
  }

  return (
    <>
      <Modal
        open={scheduledOpen}
        onClose={() => setScheduled(false)}
        title={
          <span className="flex items-center gap-2">
            <Clock size={15} className="text-accent" />
            <span>定时任务</span>
            <Badge tone={enabledCount > 0 ? 'green' : 'gray'}>启用 {enabledCount} / {tasks.length}</Badge>
          </span>
        }
        width="max-w-3xl"
        height="h-[min(720px,92dvh)] sm:h-[min(720px,86dvh)]"
      >
        <div className="flex flex-col h-full min-h-0">
          <RealModeNotice text="定时任务仍是本地演示数据：调度器与运行记录属阶段 G，尚未接入服务端。" />
          {/* 固定头：调度说明 + 新建（不随列表滚动、不改变弹窗高度） */}
          <div className="shrink-0 border-b border-edge/60 bg-panel2/30 px-4 py-2.5">
            <div className="flex items-start gap-2">
              <p className="flex-1 min-w-0 text-[11.5px] text-inkfaint leading-relaxed">
                定时任务由<span className="text-inkdim">客户端本地调度</span>：每条任务绑定一个客户端，到点由该端自行拉起一次会话执行提示词，
                不依赖服务端常驻轮询。离线端到点未执行会在上线后补跑一次。
              </p>
              <Button size="sm" variant="primary" onClick={openCreate} className="gap-1.5 shrink-0">
                <Plus size={13} />
                <span>新建任务</span>
              </Button>
            </div>
          </div>

          {/* 滚动列表区 */}
          <div className="flex-1 overflow-y-auto min-h-0 px-4 py-3">
            {tasks.length === 0 ? (
              <Empty icon={<Clock size={22} className="text-inkfaint" />} title="暂无定时任务" hint="点击右上角「新建任务」按 cron 定时拉起 Agent 会话" />
            ) : (
              <div className="space-y-2">
                {tasks.map((t) => (
                  <div
                    key={t.id}
                    className={cn(
                      'rounded-xl border bg-panel p-3.5 shadow-subtle transition-all',
                      t.enabled ? 'border-edge hover:border-edge2' : 'border-edge/50 opacity-80',
                    )}
                  >
                    <div className="flex items-start gap-3">
                      <div className={cn('w-8 h-8 rounded-lg flex items-center justify-center shrink-0 mt-0.5', t.enabled ? 'bg-accent/12 text-accent' : 'bg-panel2 text-inkfaint')}>
                        {t.enabled ? <Clock size={15} /> : <PauseCircle size={15} />}
                      </div>

                      <div className="flex-1 min-w-0">
                        <div className="flex items-center gap-2 flex-wrap">
                          <span className="text-[13px] font-semibold text-ink truncate">{t.name}</span>
                          <code className="text-[11px] font-mono bg-panel2 border border-edge/70 px-1.5 py-0.5 rounded">{t.cron}</code>
                          <Badge tone="blue">{t.scheduleText}</Badge>
                          <StatusBadge t={t} />
                        </div>

                        <div className="text-[12px] text-inkdim mt-1.5 leading-relaxed line-clamp-2">{t.prompt}</div>

                        <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-inkfaint">
                          <span className="inline-flex items-center gap-1">
                            <Terminal size={10} />
                            {clientName(t.clientId)}
                          </span>
                          <span>下次执行 {t.enabled ? fmtTime(t.nextRunAt) : '已暂停'}</span>
                          <span>上次执行 {fmtTime(t.lastRunAt)}</span>
                          {t.sessionTitle && <span>会话：{t.sessionTitle}</span>}
                          <span>访问模式 {t.accessMode === 'full' ? '完全访问' : '变更前确认'}</span>
                        </div>
                        {t.lastSummary && (
                          <div className="mt-1.5 text-[11px] text-inkfaint leading-relaxed">最近结果：{t.lastSummary}</div>
                        )}
                      </div>

                      <div className="flex items-center gap-1.5 shrink-0">
                        <Button
                          size="sm"
                          variant="outline"
                          className="gap-1"
                          disabled={t.lastStatus === 'running'}
                          onClick={() => { runNow(t.id); toast('info', `「${t.name}」已在本机立即执行`) }}
                        >
                          <Play size={11} />
                          <span className="hidden sm:inline">立即运行</span>
                        </Button>
                        <Switch
                          checked={t.enabled}
                          onChange={() => { toggle(t.id); toast('info', t.enabled ? `已暂停「${t.name}」` : `已启用「${t.name}」`) }}
                          title={t.enabled ? '暂停任务' : '启用任务'}
                        />
                        <Button
                          size="sm"
                          variant={confirmId === t.id ? 'danger' : 'ghost'}
                          className={cn('gap-1', confirmId === t.id ? '' : 'text-inkdim hover:text-err')}
                          onClick={() => {
                            if (confirmId !== t.id) {
                              setConfirmId(t.id)
                              window.setTimeout(() => setConfirmId((c) => (c === t.id ? null : c)), 6000)
                              return
                            }
                            remove(t.id)
                            setConfirmId(null)
                            toast('info', `已删除「${t.name}」`)
                          }}
                          title="删除任务"
                        >
                          <Trash2 size={11} />
                          {confirmId === t.id && <span>确认</span>}
                        </Button>
                      </div>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>
      </Modal>

      {/* 新建任务表单：置于弹窗外层，避免被父级动画 transform / overflow 限制定位 */}
      <PromptModal
        open={Boolean(prompt)}
        title="新建定时任务"
        confirmText="创建"
        fields={prompt ?? []}
        onCancel={() => setPrompt(null)}
        onSubmit={submitCreate}
      />
    </>
  )
}
