import { useUI } from '../../stores/uiStore'
import { useChat } from '../../stores/chatStore'
import { Button, Switch } from '../../components/ui'
import { promptInstall, usePwa } from '../../lib/pwa'

// 数据与存储面板：原设置弹窗「数据与存储」标签页迁入「高级」弹窗
export function DataPanel() {
  const { toast, setAdvanced } = useUI()
  const { pushDataApproval } = useChat()
  const pwa = usePwa()

  return (
    <div className="p-3 space-y-4">
      <div className="grid grid-cols-2 gap-3">
        <Stat label="数据目录" value="data/" hint="可整体拷贝迁移" />
        <Stat label="数据库" value="18.4 MB" hint="SQLite WAL" />
        <Stat label="快照 blob" value="126 MB" hint="CAS 去重后" />
        <Stat label="会话 / 消息" value="6 / 42" hint="—" />
        <button
          type="button"
          onClick={() =>
            toast(
              'info',
              `主屏安装（PWA）：${pwa.installAvailable ? '可安装，见下方「安装到主屏」' : pwa.registered ? '已安装 / 暂无安装提示' : pwa.supported ? '未注册' : '不支持'}；离线不缓存（服务端执行）`,
            )
          }
          className="rounded-lg border border-edge p-2.5 text-left hover:bg-panel2"
        >
          <div className="text-[11px] text-inkfaint">主屏安装（PWA）</div>
          <div className="text-[14px] font-medium mt-0.5">
            {pwa.installAvailable ? '可安装' : pwa.registered ? '已就绪' : pwa.supported ? '未注册' : '不支持'}
          </div>
          <div className="text-[11px] text-inkfaint mt-0.5">
            仅更新与安装 · 离线不缓存
          </div>
        </button>
      </div>
      <div className="flex flex-wrap gap-2">
        <Button variant="outline" onClick={() => toast('success', 'VACUUM 完成，回收 3.2 MB')}>压缩数据库（VACUUM）</Button>
        <Button
          variant="outline"
          title="导出会话数据受「项目设置 → 数据权限」控制，越界会触发审批"
          onClick={() => {
            setAdvanced(false)
            pushDataApproval({
              scope: 'session_export',
              toolName: 'export_session',
              summary: '导出 6 个会话的全部消息、工具结果与 12 个制品，打包为 data/export/session-bundle.zip',
              args: {
                scope: 'session_export',
                target: 'data/export/session-bundle.zip',
                sessions: 6,
                artifacts: 12,
              },
            })
            toast('info', '已发起数据权限审批，请在对话区处置')
          }}
        >
          导出会话包
        </Button>
        <Button variant="outline" onClick={() => toast('info', '快照保留最近 50 轮，超出自动淘汰')}>查看快照策略</Button>
        {pwa.installAvailable && (
          <Button
            variant="outline"
            onClick={async () => {
              const ok = await promptInstall()
              toast(ok ? 'success' : 'info', ok ? '已开始安装到主屏' : '已取消安装')
            }}
          >
            安装到主屏
          </Button>
        )}
      </div>
      <div className="rounded-lg border border-edge p-3 text-[12px] text-inkfaint space-y-1">
        <div>· 快照：单文件 &gt; 5MB 跳过并标记「不可回滚」</div>
        <div>· 保留：最近 50 轮（可配），孤儿 blob 24h 后 GC</div>
        <div>· run_command 产生的文件变更不纳入快照（需自行用 git）</div>
      </div>
    </div>
  )
}

// 外观面板：原设置弹窗「外观」标签页迁入「高级」弹窗
const TOKEN_COLORS: Record<string, string> = {
  bg: 'var(--bg)',
  panel: 'var(--panel)',
  panel2: 'var(--panel2)',
  edge: 'var(--edge)',
  accent: 'var(--accent)',
  accent2: 'var(--accent2)',
  ok: 'var(--ok)',
  warn: 'var(--warn)',
  err: 'var(--err)',
}

export function ThemePanel() {
  const { theme, toggleTheme } = useUI()

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between rounded-xl border border-edge bg-panel p-3.5 shadow-subtle">
        <div>
          <div className="text-[14px] font-medium text-ink">深色主题</div>
          <div className="text-[12px] text-inkfaint mt-0.5">默认；也可跟随系统（原型用开关模拟）</div>
        </div>
        <Switch checked={theme === 'dark'} onChange={toggleTheme} />
      </div>
      <div className="rounded-xl border border-edge bg-panel p-3.5 shadow-subtle">
        <div className="text-[13px] font-medium text-ink mb-2.5">设计令牌预览</div>
        <div className="flex gap-2.5 flex-wrap">
          {Object.entries(TOKEN_COLORS).map(([name, cssVar]) => (
            <div key={name} className="text-center">
              <div
                className="w-10 h-10 rounded-xl border border-edge/80 shadow-subtle transition-transform hover:scale-105"
                style={{ background: cssVar }}
              />
              <div className="text-[10px] font-mono text-inkfaint mt-1">{name}</div>
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}

function Stat({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <div className="rounded-lg border border-edge p-2.5">
      <div className="text-[11px] text-inkfaint">{label}</div>
      <div className="text-[14px] font-medium mt-0.5">{value}</div>
      <div className="text-[11px] text-inkfaint mt-0.5">{hint}</div>
    </div>
  )
}
