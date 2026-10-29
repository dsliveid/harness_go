import { Archive, Database, Palette, MonitorSmartphone, Blocks } from 'lucide-react'
import { useUI } from '../../stores/uiStore'
import { useSessions } from '../../stores/sessionStore'
import { Button, Empty, Modal, Tabs } from '../../components/ui'
import { DataPanel, ThemePanel } from './DataThemePanels'
import { WorkspacePanel } from './WorkspacePanel'

// 高级弹窗：归档 / 工作空间 / 数据与存储 / 外观
// 工具 / MCP / 技能 已迁移至侧栏「插件市场」（客户端自行安装），此处不再重复。
export function AdvancedModal() {
  const { advancedOpen, advancedTab, setAdvanced, setPlugins, toast } = useUI()
  const { sessions, unarchive, select } = useSessions()
  const archived = sessions.filter((s) => s.archivedAt).sort((a, b) => (b.archivedAt ?? 0) - (a.archivedAt ?? 0))

  return (
    <Modal
      open={advancedOpen}
      onClose={() => setAdvanced(false)}
      title="高级功能与扩展"
      width="max-w-4xl"
      height="h-[min(620px,92dvh)] sm:h-[min(620px,86dvh)]"
    >
      <div className="flex flex-col sm:flex-row h-full min-h-0">
        <div className="sm:w-44 border-b sm:border-b-0 sm:border-r border-edge/80 p-2.5 shrink-0 flex flex-col bg-panel2/20">
          <Tabs
            items={[
              { key: 'workspace', label: <span className="flex items-center gap-2"><MonitorSmartphone size={13} className="text-emerald-400" /><span>工作空间</span></span> },
              { key: 'archive', label: <span className="flex items-center gap-2"><Archive size={13} className="text-inkdim" /><span>归档会话</span></span> },
              { key: 'data', label: <span className="flex items-center gap-2"><Database size={13} className="text-sky-400" /><span>数据与存储</span></span> },
              { key: 'theme', label: <span className="flex items-center gap-2"><Palette size={13} className="text-purple-400" /><span>外观主题</span></span> },
            ]}
            value={advancedTab}
            onChange={(v) => setAdvanced(true, v)}
            className="sm:flex-col sm:items-stretch"
          />
        </div>

        <div className="flex-1 overflow-y-auto p-4 sm:p-5 min-h-0">
          {/* 能力管理入口指引：工具 / MCP / 技能 已迁出 */}
          <button
            className="w-full mb-4 flex items-center gap-2 rounded-xl border border-edge bg-panel2/50 px-3 py-2 text-left text-[12px] text-inkdim hover:border-accent/40 hover:text-ink transition-colors"
            onClick={() => { setAdvanced(false); setPlugins(true) }}
            title="打开插件市场"
          >
            <Blocks size={13} className="text-accent shrink-0" />
            <span>工具 / MCP / 技能 已迁移至<span className="text-accent font-medium">侧栏「插件市场」</span>，由各客户端自行安装</span>
          </button>

          {advancedTab === 'workspace' && <WorkspacePanel />}
          {advancedTab === 'data' && <DataPanel />}
          {advancedTab === 'theme' && <ThemePanel />}

          {advancedTab === 'archive' && (
            <div className="space-y-3">
              <div className="rounded-xl border border-edge bg-panel2/50 p-3 text-[12px] text-inkfaint leading-relaxed">
                归档的会话不出现在侧栏列表中；归档不占用上下文。取消归档后回到原项目分组并选中。
              </div>

              {archived.length === 0 ? (
                <Empty icon={<Archive size={22} className="text-inkdim" />} title="暂无归档会话" hint="会话菜单中的「归档」会把会话安全移入此处" />
              ) : (
                <div className="space-y-2">
                  {archived.map((s) => (
                    <div key={s.id} className="flex items-center gap-3 rounded-xl border border-edge bg-panel p-3 shadow-subtle hover:border-edge2 transition-all">
                      <div className="min-w-0 flex-1">
                        <div className="text-[13px] font-medium text-ink truncate">{s.title}</div>
                        <div className="text-[11px] text-inkfaint mt-0.5">
                          归档于 {new Date(s.archivedAt ?? 0).toLocaleDateString('zh-CN')} · {s.workspacePath || '纯对话'}
                        </div>
                      </div>
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => { unarchive(s.id); select(s.id); toast('success', `已恢复会话「${s.title}」`) }}
                      >
                        取消归档
                      </Button>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}
        </div>
      </div>
    </Modal>
  )
}