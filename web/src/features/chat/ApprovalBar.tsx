import { useState } from 'react'
import { ShieldAlert, Check, CheckCheck, X, Database, Smartphone, Terminal } from 'lucide-react'
import { useChat } from '../../stores/chatStore'
import { Badge, Button, inputCls } from '../../components/ui'
import { DATA_SCOPE_HINT, DATA_SCOPE_LABEL } from '../../lib/dataScopes'
import { canDecideApproval } from '../../lib/mobileCapabilities'
import { buildCommandLine, commandDir, isCommandTool } from '../../lib/commandLine'

export function ApprovalBar() {
  const { approval, respond } = useChat()
  const [denying, setDenying] = useState(false)
  const [reason, setReason] = useState('')

  if (!approval) return null

  // 移动端壳能力声明（§4.7.1 审批只读）：false → 卡片照常展示，但不提供 允许/拒绝 按钮。
  // 未注入声明（桌面端 / 浏览器）时该值为 true，渲染路径与改动前完全一致。
  // 注意：这不是安全边界，服务端仍会独立校验（协议 §5.3）。
  const canDecide = canDecideApproval()

  const isData = approval.kind === 'data'
  const riskTone = approval.risk === 'danger' ? 'red' : approval.risk === 'command' ? 'yellow' : 'blue'
  const scope = approval.dataScope
  // 命令类审批的完整命令行（后端只下发 {cmd, args[]}，命令行需展示层拼装）
  const isCmd = isCommandTool(approval.toolName, approval.risk)
  const cmdLine = isCmd ? buildCommandLine(approval.args) : ''
  const cmdDir = isCmd ? commandDir(approval.args) : ''
  // 命令类参数已由命令行块完整呈现，chip 不再重复罗列 cmd/args/dir/timeout_s
  const CMD_ARG_KEYS = new Set(['cmd', 'command', 'args', 'dir', 'timeout_s'])
  const argEntries = Object.entries(approval.args || {}).filter(
    ([k]) => !(isCmd && CMD_ARG_KEYS.has(k)),
  )

  return (
    <div className="mx-3 sm:mx-5 mb-2.5 rounded-2xl border border-warn/40 bg-panel p-3.5 slide-up">
      <div className="flex items-center gap-2 flex-wrap">
        <div className="w-6 h-6 rounded-lg bg-warn/15 text-warn flex items-center justify-center shrink-0">
          {isData ? <Database size={14} /> : <ShieldAlert size={14} />}
        </div>
        <span className="text-[13px] font-semibold text-ink">{isData ? '数据权限审批' : '安全操作审批'}</span>
        {isData ? (
          <Badge tone="purple">数据范围</Badge>
        ) : (
          <Badge tone={riskTone}>
            {approval.risk === 'danger' ? '高危' : approval.risk === 'command' ? '执行命令' : '写入文件'}
          </Badge>
        )}
        <span className="font-mono text-[12px] bg-panel2 border border-edge px-1.5 py-0.5 rounded text-accent font-medium">
          {approval.toolName}
        </span>
        {scope && (
          <span className="text-[11px] px-1.5 py-0.5 rounded-md bg-accent2/12 text-accent2 border border-accent2/30 font-medium">
            {DATA_SCOPE_LABEL[scope]}
          </span>
        )}
        {approval.externalPath && <Badge tone="yellow">外部工作区路径</Badge>}
      </div>

      <div className="mt-2 text-[13px] text-ink leading-relaxed font-medium">
        {approval.summary}
      </div>

      {/* 命令类审批：审批点就是「要不要跑这条命令」，摘要（如「执行命令」）不足以判断，
          必须把完整命令行摆出来（折行不截断），让用户看清参数后再决定允许/拒绝。 */}
      {cmdLine && (
        <div className="mt-2 rounded-lg border border-warn/30 bg-[#090d14] overflow-hidden">
          <div className="flex items-center gap-1.5 px-2.5 h-7 border-b border-edge/60 bg-panel2/60 text-[10.5px] text-inkfaint font-mono select-none">
            <Terminal size={10} className="text-amber-400" />
            完整命令行
          </div>
          <code className="block px-2.5 py-2 font-mono text-[11.5px] leading-5 text-[#e6edf3] whitespace-pre-wrap break-all select-text">
            {cmdLine}
          </code>
          {cmdDir && (
            <div className="px-2.5 py-1 border-t border-edge/40 text-[10.5px] text-inkfaint font-mono">
              cwd: {cmdDir}
            </div>
          )}
        </div>
      )}

      {isData && scope && (
        <div className="mt-1.5 text-[11px] text-inkfaint leading-relaxed">
          该请求触达「{DATA_SCOPE_LABEL[scope]}」：{DATA_SCOPE_HINT[scope]}。
        </div>
      )}

      {argEntries.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-1.5 text-[11px] font-mono">
          {argEntries.map(([k, v]) => (
            <span key={k} className="inline-flex items-center gap-1 rounded-md bg-panel2 px-2 py-0.5 border border-edge/80">
              <span className="text-inkfaint">{k}:</span>
              <span className="text-ink font-medium max-w-[240px] truncate" title={String(v)}>{String(v)}</span>
            </span>
          ))}
        </div>
      )}

      {!canDecide ? (
        /* 移动端壳：审批只读（§4.7.1）。卡片保留展示让用户知道有待处理审批，
           但明确告知去电脑端确认，而不是留一个点不动的空壳。 */
        <div className="mt-3 pt-2.5 border-t border-edge/60 flex items-start gap-2 text-[12px] text-inkdim leading-relaxed">
          <Smartphone size={13} className="text-warn shrink-0 mt-0.5" />
          <span>
            移动端为大模型操作的<b className="font-medium text-ink">只读审批</b>：此审批需在电脑端确认后才能继续执行。
          </span>
        </div>
      ) : denying ? (
        <div className="mt-3 space-y-2 border-t border-edge/60 pt-2.5">
          <input
            className={inputCls}
            placeholder={isData ? '填写拒绝原因（将回传给调用方）' : '填写拒绝原因（将回传给模型，引导其调整方案）'}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            autoFocus
          />
          <div className="flex justify-end gap-2">
            <Button size="sm" variant="ghost" onClick={() => setDenying(false)}>取消</Button>
            <Button
              size="sm"
              variant="danger"
              onClick={() => {
                respond('deny', reason || '用户拒绝')
                setDenying(false)
                setReason('')
              }}
            >
              确认拒绝
            </Button>
          </div>
        </div>
      ) : (
        <div className="mt-3 pt-2.5 border-t border-edge/60 flex flex-wrap gap-2">
          <Button size="sm" variant="primary" onClick={() => respond('allow_once')} className="gap-1.5">
            <Check size={13} />
            <span>允许一次</span>
          </Button>
          <Button size="sm" variant="outline" onClick={() => respond('allow_session')} className="gap-1.5">
            <CheckCheck size={13} />
            <span>{isData ? '本项目允许' : '本会话均允许'}</span>
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setDenying(true)} className="gap-1.5 text-inkdim hover:text-err">
            <X size={13} />
            <span>拒绝并说明原因</span>
          </Button>
        </div>
      )}
    </div>
  )
}
