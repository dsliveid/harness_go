import type {
  ApprovalRule, CommandRun, DataScopeGrant, DeviceNode, ExtensionItem, FileDiff, McpServer, MemoryItem,
  Message, PlanDoc, Plugin, Project, ProjectLink, Provider, RegistryItem, ScheduledTask, Session,
  Skill, Snapshot, Subagent, TodoItem, Workspace,
} from '../types'

const now = Date.now()
const min = 60_000
const hour = 3_600_000

// ---------------- 工作空间（服务端大脑 + 客户端接入的肢体空间） ----------------
/** 内置默认工作空间：服务端本身，永远在线、不可移除 */
export const DEFAULT_WORKSPACE_ID = 'ws-srv'

export const workspaces: Workspace[] = [
  { id: 'ws-srv', name: '服务端工作空间', kind: 'server', online: true, limbCount: 1, lastSeenAt: now - 4_000, protocol: 'v1', builtin: true },
  { id: 'ws-desk', name: '我的桌面 · desk-01', kind: 'desktop', online: true, limbCount: 3, lastSeenAt: now - 6_000, protocol: 'v1', clientId: 'desk-01' },
  { id: 'ws-office', name: '公司桌面 · desk-02', kind: 'desktop', online: false, limbCount: 2, lastSeenAt: now - 5 * hour, protocol: 'v1', clientId: 'desk-02' },
  { id: 'ws-phone', name: '随身手机 · phone-01', kind: 'mobile', online: true, limbCount: 1, lastSeenAt: now - 40 * min, protocol: 'v1', clientId: 'phone-01' },
]

// ---------------- 项目 ----------------
export const projects: Project[] = [
  { id: 'p1', name: 'harness_go', path: '/data/workspace/harness_go', pinned: true, workspaceId: 'ws-srv', constraints: '# 项目约束\n- 后端 Go 1.22+，前端 React 18\n- 先方案后编码，禁止跳步' },
  { id: 'p2', name: 'harness_mini', path: 'F:/WorkSpace/Other/harness_mini', workspaceId: 'ws-desk', constraints: '- Tauri 2 + Rust 内核\n- 桌面端，勿引入 web 运行时' },
  { id: 'p3', name: 'data-pipeline', path: 'D:/WorkSpace/data-pipeline', workspaceId: 'ws-office' },
]

// ---------------- 会话 ----------------
export const sessions: Session[] = [
  { id: 's1', title: '实现 M2 插件注册表与热加载', projectId: 'p1', workspaceId: 'ws-srv', workspacePath: '/data/workspace/harness_go', accessMode: 'confirm', modelRef: 'deepseek/deepseek-chat', updatedAt: now - 2 * min, running: true },
  { id: 's2', title: '分析桌面版 token 计算并给出改造方案', projectId: 'p1', workspaceId: 'ws-srv', workspacePath: '/data/workspace/harness_go', accessMode: 'confirm', modelRef: 'glm/glm-4-plus', updatedAt: now - 1 * hour },
  { id: 's3', title: '重构审批引擎', projectId: 'p1', workspaceId: 'ws-srv', workspacePath: '/data/workspace/harness_go', accessMode: 'full', modelRef: 'claude/claude-sonnet-4', updatedAt: now - 3 * hour },
  { id: 's4', title: 'Web 版 MCP 三传输接入调研', projectId: 'p1', workspaceId: 'ws-srv', workspacePath: '/data/workspace/harness_go', accessMode: 'confirm', modelRef: 'glm/glm-4-plus', updatedAt: now - 5 * hour, unread: true },
  { id: 's5', title: '纯对话：解释 SSE 与 WebSocket 的取舍', workspaceId: 'ws-srv', accessMode: 'confirm', modelRef: 'deepseek/deepseek-chat', updatedAt: now - 26 * hour },
  { id: 's6', title: '归档：早期架构讨论', projectId: 'p1', workspaceId: 'ws-srv', workspacePath: '/data/workspace/harness_go', accessMode: 'confirm', modelRef: 'glm/glm-4-plus', updatedAt: now - 3 * 24 * hour, archivedAt: now - 2 * 24 * hour },
  // 以下三条用于验证「每组最多 5 条 + 更多」折叠（p1 共 7 条活跃会话）
  { id: 's7', title: '快照 CAS 去重与孤儿 blob GC', projectId: 'p1', workspaceId: 'ws-srv', workspacePath: '/data/workspace/harness_go', accessMode: 'confirm', modelRef: 'deepseek/deepseek-chat', updatedAt: now - 8 * 60 * min },
  { id: 's8', title: 'MCP 健康检查与熔断参数标定', projectId: 'p1', workspaceId: 'ws-srv', workspacePath: '/data/workspace/harness_go', accessMode: 'confirm', modelRef: 'glm/glm-4-plus', updatedAt: now - 30 * hour },
  { id: 's9', title: '上下文压缩策略对比实验', projectId: 'p1', workspaceId: 'ws-srv', workspacePath: '/data/workspace/harness_go', accessMode: 'confirm', modelRef: 'claude/claude-sonnet-4', updatedAt: now - 2 * 24 * hour + 2 * hour },
  // 桌面端接入的工作空间（在线）
  { id: 's10', title: 'Tauri 2 侧车进程改造评估', projectId: 'p2', workspaceId: 'ws-desk', workspacePath: 'F:/WorkSpace/Other/harness_mini', accessMode: 'confirm', modelRef: 'deepseek/deepseek-chat', updatedAt: now - 12 * min },
  { id: 's11', title: '桌面端 Go sidecar 通信协议设计', projectId: 'p2', workspaceId: 'ws-desk', workspacePath: 'F:/WorkSpace/Other/harness_mini', accessMode: 'confirm', modelRef: 'glm/glm-4-plus', updatedAt: now - 4 * hour },
  // 另一台桌面端接入的工作空间（离线 → 仅可查看历史）
  { id: 's12', title: 'CI 流水线迁移方案', projectId: 'p3', workspaceId: 'ws-office', workspacePath: 'D:/WorkSpace/data-pipeline', accessMode: 'confirm', modelRef: 'deepseek/deepseek-chat', updatedAt: now - 6 * hour },
  { id: 's13', title: '数据清洗任务重跑与对账', projectId: 'p3', workspaceId: 'ws-office', workspacePath: 'D:/WorkSpace/data-pipeline', accessMode: 'confirm', modelRef: 'glm/glm-4-plus', updatedAt: now - 2 * 24 * hour },
  // 移动端接入的工作空间（无项目，纯对话）
  { id: 's14', title: '语音速记：移动端肢体能力边界', workspaceId: 'ws-phone', accessMode: 'confirm', modelRef: 'deepseek/deepseek-chat', updatedAt: now - 40 * min },
]

// ---------------- 模型 ----------------
export const providers: Provider[] = [
  {
    id: 'deepseek', name: 'DeepSeek', baseUrl: 'https://api.deepseek.com/v1', enabled: true, apiKeyMasked: 'sk-****9f2c',
    models: [
      { id: 'deepseek-chat', contextLimit: 64000, capabilities: ['chat', 'reasoning'], protocol: 'chat_completions' },
      { id: 'deepseek-reasoner', contextLimit: 64000, capabilities: ['chat', 'reasoning'], protocol: 'chat_completions' },
    ],
  },
  {
    id: 'glm', name: '智谱 GLM', baseUrl: 'https://open.bigmodel.cn/api/paas/v4', enabled: true, apiKeyMasked: 'glm-****a1b3',
    models: [
      { id: 'glm-4-plus', contextLimit: 128000, capabilities: ['chat', 'vision', 'decision'], protocol: 'chat_completions' },
      { id: 'glm-4v', contextLimit: 8192, capabilities: ['vision'], protocol: 'chat_completions' },
    ],
  },
  {
    id: 'claude', name: 'Anthropic', baseUrl: 'https://api.anthropic.com', enabled: false, apiKeyMasked: 'sk-ant-****77de',
    models: [{ id: 'claude-sonnet-4', contextLimit: 200000, capabilities: ['chat', 'reasoning', 'vision'], protocol: 'messages' }],
  },
]

export const defaultModels = {
  chat: 'deepseek/deepseek-chat',
  vision: 'glm/glm-4v',
  image: 'glm/glm-4-plus',
  decision: 'glm/glm-4-plus',
}

// ---------------- 差异 ----------------
export const sampleDiff: FileDiff = {
  path: 'internal/tools/registry.go',
  additions: 12,
  deletions: 3,
  hunks: [
    {
      id: 'h1', header: '@@ -18,7 +18,12 @@ func (r *Registry) Register(t Tool) error {',
      before: [
        { type: 'ctx', text: 'func (r *Registry) Register(t Tool) error {', no: 18 },
        { type: 'del', text: '    r.mu.Lock()', no: 19 },
        { type: 'del', text: '    r.tools[t.Spec().Name] = t', no: 20 },
        { type: 'del', text: '    r.mu.Unlock()', no: 21 },
        { type: 'ctx', text: '    return nil', no: 22 },
      ],
      after: [
        { type: 'ctx', text: 'func (r *Registry) Register(t Tool) error {', no: 18 },
        { type: 'add', text: '    spec := t.Spec()', no: 19 },
        { type: 'add', text: '    if spec.Name == "" {', no: 20 },
        { type: 'add', text: '        return apperr.New("TOOL_NAME_EMPTY", "工具名不能为空")', no: 21 },
        { type: 'add', text: '    }', no: 22 },
        { type: 'add', text: '    r.mu.Lock()', no: 23 },
        { type: 'add', text: '    defer r.mu.Unlock()', no: 24 },
        { type: 'add', text: '    if _, exists := r.tools[spec.Name]; exists {', no: 25 },
        { type: 'add', text: '        return apperr.New("TOOL_CONFLICT", "工具已存在: "+spec.Name)', no: 26 },
        { type: 'add', text: '    }', no: 27 },
        { type: 'add', text: '    r.tools[spec.Name] = t', no: 28 },
        { type: 'ctx', text: '    return nil', no: 29 },
      ],
    },
  ],
}

// ---------------- 消息（演示会话 s1） ----------------
export const messages: Message[] = [
  {
    id: 'm1', sessionId: 's1', runId: 'r1', seq: 1, turnSeq: 1, role: 'user', createdAt: now - 40 * min,
    content: '先看一下现在工具注册表的实现，然后加一个同名冲突检测。',
  },
  {
    id: 'm2', sessionId: 's1', runId: 'r1', seq: 2, turnSeq: 1, role: 'assistant', createdAt: now - 39 * min,
    content: '我先读取 `internal/tools/registry.go` 确认现有结构。',
    toolCalls: [{
      id: 'tc1', name: 'read_file', risk: 'readonly', status: 'ok', durationMs: 42,
      args: { path: 'internal/tools/registry.go', offset_line: 1, max_lines: 60 },
      result: '     1  package tools\n     2\n     3  type Registry struct {\n     4      mu    sync.RWMutex\n     5      tools map[string]Tool\n     6  }\n...',
    }],
    tokens: 820,
    promptTokens: 610,
    cachedTokens: 520,
    durationMs: 1240,
  },
  {
    id: 'm3', sessionId: 's1', runId: 'r1', seq: 3, turnSeq: 1, role: 'assistant', createdAt: now - 38 * min,
    content: '当前 `Register` 没有冲突检测，直接覆盖同名工具。我会加三处：空名校验、重复检测、`defer Unlock`。',
    tokens: 260,
    promptTokens: 190,
    cachedTokens: 150,
    durationMs: 860,
  },
  {
    id: 'm4', sessionId: 's1', runId: 'r2', seq: 4, turnSeq: 2, role: 'user', createdAt: now - 12 * min,
    content: '直接改，改完跑一下 go test ./internal/tools。',
  },
  {
    id: 'm5', sessionId: 's1', runId: 'r2', seq: 5, turnSeq: 2, role: 'assistant', createdAt: now - 11 * min,
    content: '正在修改 `registry.go`：',
    toolCalls: [{
      id: 'tc2', name: 'write_file', risk: 'write', status: 'ok', durationMs: 18,
      args: { path: 'internal/tools/registry.go' }, diff: sampleDiff,
      result: '已写入 internal/tools/registry.go（+12 −3）',
    }],
    tokens: 410,
    promptTokens: 300,
    cachedTokens: 240,
    durationMs: 1620,
  },
  {
    id: 'm6', sessionId: 's1', runId: 'r2', seq: 6, turnSeq: 2, role: 'assistant', createdAt: now - 10 * min,
    content: '执行测试：',
    toolCalls: [{
      id: 'tc3', name: 'run_command', risk: 'command', status: 'ok', durationMs: 3200,
      args: { command: 'go test ./internal/tools/...', timeout_ms: 120000 },
      result: 'ok   harness_go/internal/tools   0.412s\nPASS\n\ncoverage: 78.3% of statements',
    }],
    tokens: 190,
    promptTokens: 140,
    cachedTokens: 110,
    durationMs: 3410,
  },
  {
    id: 'm7', sessionId: 's1', runId: 'r2', seq: 7, turnSeq: 2, role: 'assistant', createdAt: now - 9 * min,
    content: '改完并通过测试。新增了 3 个单测覆盖空名与冲突分支，覆盖率 78.3%。',
    tokens: 120,
    promptTokens: 80,
    cachedTokens: 60,
  },
  {
    id: 'm8', sessionId: 's1', runId: 'r3', seq: 8, turnSeq: 3, role: 'user', createdAt: now - 4 * min,
    content: '把这次改造拆成计划，并列出待办。',
  },
  {
    id: 'm9', sessionId: 's1', runId: 'r3', seq: 9, turnSeq: 3, role: 'assistant', createdAt: now - 4 * min + 5000,
    content: '已创建计划并登记待办：',
    toolCalls: [
      {
        id: 'tc4', name: 'create_plan', risk: 'write', status: 'ok', durationMs: 25,
        args: { title: '插件注册表热加载改造', steps: 4 },
        result: '已创建 .harness/plans/plugin-registry-hot-reload.md（4 步）',
      },
      {
        id: 'tc5', name: 'todo', risk: 'readonly', status: 'ok', durationMs: 6,
        args: { items: 4 },
        result: '任务清单已更新（4 项）',
      },
      {
        id: 'tc6', name: 'spawn_subagent', risk: 'write', status: 'running', durationMs: 120,
        args: { subagentId: 'sa1', role: '前端开发', task: '实现统一三端布局骨架' },
        result: '已派生子进程「前端专家」（独立上下文与工具环境）',
      },
    ],
    tokens: 300,
    promptTokens: 210,
    cachedTokens: 170,
    durationMs: 1180,
  },
  {
    id: 'm10', sessionId: 's1', runId: 'r3', seq: 10, turnSeq: 3, role: 'assistant', createdAt: now - 3 * min,
    content: '计划已落盘，待办 4 项。要现在开始第一步吗？',
    tokens: 90,
    promptTokens: 60,
    cachedTokens: 45,
  },
]

// ---------------- 计划 / 待办 ----------------
export const plans: PlanDoc[] = [
  {
    id: 'pl1', title: '插件注册表热加载改造', version: 2, active: true, updatedAt: now - 3 * min,
    status: 'in_progress',
    path: '.harness/plans/plugin-registry-hot-reload.md',
    goals: '插件启用/禁用与参数变更后无需重启进程即可生效，同时保证热更新期间正在执行的调用不中断、失败可回滚。',
    architecture: '采用「写时复制 + 原子替换」方案：注册表每次变更生成不可变快照，通过 RCU 指针切换；依赖探测在加载前完成，缺失依赖自动降级为 disabled 并记录原因；tools.changed 事件广播给所有已打开会话以刷新工具白名单。',
    files: [
      '[MODIFY] internal/plugin/registry.go — 快照结构与原子替换',
      '[MODIFY] internal/plugin/manifest.go — Manifest 依赖声明',
      '[NEW] internal/plugin/hotreload.go — 热加载编排与回滚',
      '[MODIFY] internal/events/tools.go — tools.changed 事件',
    ],
    verification: 'go test ./internal/plugin/... 全量回归；手工验证：运行中会话内禁用插件后工具白名单即时刷新，无进行中调用中断。',
    steps: [
      { id: 'ps1', content: '定义 Plugin Manifest 与状态机', status: 'done' },
      { id: 'ps2', content: '实现依赖探测与降级（git/chromium）', status: 'in_progress' },
      { id: 'ps3', content: '注册表写时复制 + 热更新原子替换', status: 'pending' },
      { id: 'ps4', content: 'tools.changed 事件与生效边界', status: 'pending' },
    ],
  },
  {
    id: 'pl2', title: 'Web 版 MCP 三传输接入', version: 1, active: false, updatedAt: now - 26 * hour,
    status: 'suspended',
    path: '.harness/plans/web-mcp-transports.md',
    goals: 'Web 版支持 stdio / SSE / Streamable HTTP 三种 MCP 传输，与桌面版行为对齐。',
    architecture: 'stdio 由服务端子进程代理转 WebSocket；SSE/HTTP 直连。熔断器按 server 维度计数，连续失败 3 次进入 30s 冷却。',
    files: ['[MODIFY] internal/mcp/client.go', '[NEW] internal/mcp/transport_sse.go'],
    verification: 'go test ./internal/mcp/...；对本地 echo MCP Server 做三传输连通性手测。',
    steps: [
      { id: 'ps5', content: 'stdio 传输与进程组 kill', status: 'done' },
      { id: 'ps6', content: 'SSE / Streamable HTTP', status: 'done' },
      { id: 'ps7', content: '熔断与自动重启', status: 'pending' },
    ],
  },
]

export const todos: TodoItem[] = [
  { id: 't1', content: '定义 Manifest 与状态机', status: 'done' },
  { id: 't2', content: '依赖探测与能力降级', status: 'in_progress' },
  { id: 't3', content: '注册表热更新', status: 'pending' },
  { id: 't4', content: '生效边界与事件广播', status: 'pending' },
]

// ---------------- 插件 ----------------
export const plugins: Plugin[] = [
  { id: 'fs', name: '文件操作', kind: 'builtin', description: '工作区文件读写、检索与结构大纲', version: '1.0.0', state: 'enabled', requires: ['fs_read', 'fs_write'], tools: ['read_file', 'list_dir', 'file_outline', 'write_file', 'edit_file', 'glob', 'grep'], riskCeiling: 'write', defaultEnabled: true, calls: 128, failures: 0, phase: 'M2' },
  { id: 'command', name: '命令执行', kind: 'builtin', description: 'Shell 命令执行，流式输出与进程树终止', version: '1.0.0', state: 'enabled', requires: ['fs_write'], tools: ['run_command'], riskCeiling: 'danger', defaultEnabled: true, calls: 34, failures: 1, phase: 'M2' },
  { id: 'web-fetch', name: '网页抓取', kind: 'builtin', description: '抓取网页并转纯文本', version: '1.0.0', state: 'enabled', requires: ['net'], tools: ['fetch_web_page'], riskCeiling: 'readonly', defaultEnabled: true, calls: 7, failures: 0, phase: 'M2' },
  { id: 'todo', name: '任务清单', kind: 'builtin', description: '维护任务清单并在 UI 呈现', version: '1.0.0', state: 'enabled', requires: [], tools: ['todo'], riskCeiling: 'readonly', defaultEnabled: true, calls: 22, failures: 0, phase: 'M2' },
  { id: 'plan', name: '计划管理', kind: 'builtin', description: '物理计划文档 + 看板投影', version: '1.0.0', state: 'enabled', requires: ['fs_write'], tools: ['create_plan', 'update_plan', 'switch_plan', 'read_plan', 'list_plans'], riskCeiling: 'write', defaultEnabled: true, calls: 41, failures: 0, phase: 'M2' },
  { id: 'decision', name: '决策工具', kind: 'builtin', description: 'judge / choice / score 决策模型调用', version: '1.0.0', state: 'enabled', requires: ['model:decision'], tools: ['judge_decision', 'choice_decision', 'score_decision'], riskCeiling: 'readonly', defaultEnabled: false, calls: 12, failures: 0, phase: 'M2' },
  { id: 'memory', name: '长期记忆', kind: 'builtin', description: '项目记忆读写（profile/conventions/digest）', version: '1.0.0', state: 'enabled', requires: ['fs_write'], tools: ['record_memory', 'read_memory'], riskCeiling: 'write', defaultEnabled: true, calls: 9, failures: 0, phase: 'M3' },
  { id: 'skill', name: '技能库', kind: 'builtin', description: '技能保存、列举与执行', version: '1.0.0', state: 'enabled', requires: ['fs_read', 'fs_write'], tools: ['list_skills', 'save_skill', 'run_skill'], riskCeiling: 'write', defaultEnabled: true, calls: 5, failures: 0, phase: 'M3' },
  { id: 'image', name: '多模态', kind: 'builtin', description: '图片生成与视觉识别', version: '1.0.0', state: 'degraded', requires: ['model:vision', 'model:image', 'fs_write'], missing: ['model:image 未配置'], tools: ['generate_image', 'recognize_image'], riskCeiling: 'write', defaultEnabled: false, calls: 3, failures: 3, phase: 'M3' },
  { id: 'browser', name: '浏览器自动化', kind: 'builtin', description: '无头 Chromium 导航、交互、截图', version: '1.0.0', state: 'unavailable', requires: ['chromium', 'net'], missing: ['未检测到 Chromium（可改用 Playwright MCP）'], tools: ['browser_navigate', 'browser_interact', 'browser_scroll', 'browser_screenshot', 'browser_get_debug_state', 'browser_close'], riskCeiling: 'write', defaultEnabled: false, phase: 'M3' },
  { id: 'subagent', name: '子 Agent', kind: 'builtin', description: '派生、等待、停止子 Agent 与子进程', version: '1.0.0', state: 'enabled', requires: [], tools: ['spawn_subagent', 'wait_subagents', 'get_subagent_status', 'stop_subagent', 'spawn_subprocess', 'wait_subprocesses', 'stop_subprocess', 'resume_subprocess'], riskCeiling: 'write', defaultEnabled: true, calls: 18, failures: 0, phase: 'M3' },
  { id: 'collaborator', name: '协作器', kind: 'builtin', description: '常驻角色会话与增量汇报', version: '1.0.0', state: 'disabled', requires: [], tools: ['dispatch_collaborator', 'wait_collaborators', 'get_collaborators'], riskCeiling: 'write', defaultEnabled: false, calls: 2, failures: 0, phase: 'M3' },
  { id: 'session-bundle', name: '会话打包', kind: 'builtin', description: '会话与产物导出/导入（数据面板「导出会话包」即其入口）', version: '1.0.0', state: 'enabled', requires: ['fs_read', 'fs_write'], tools: ['export_session', 'import_session'], riskCeiling: 'write', defaultEnabled: true, phase: 'M4' },
  { id: 'sop', name: 'SOP', kind: 'builtin', description: '项目级标准作业流程（设置 → Agent SOP 为其开关面）', version: '1.0.0', state: 'disabled', requires: ['fs_read', 'fs_write'], tools: ['run_workspace_sop'], riskCeiling: 'write', defaultEnabled: false, phase: 'M4' },
]

// ---------------- MCP ----------------
// origin：builtin = 服务端随程序内置（可下发给客户端）；installed = 本端已安装；extended = 程序自我拓展
export const mcpServers: McpServer[] = [
  {
    id: 'mcp-fs', name: 'filesystem', origin: 'builtin', transport: 'stdio', command: 'npx -y @modelcontextprotocol/server-filesystem /workspace',
    enabled: true, state: 'ready', calls: 56, failures: 0, healthIntervalS: 60,
    platforms: { windows: true, linux: true, mobile: false },
    tools: [
      { name: 'read_file', description: '读取文件内容', risk: 'readonly', conflictWith: 'read_file' },
      { name: 'list_directory', description: '列出目录', risk: 'readonly' },
      { name: 'write_file', description: '写入文件', risk: 'write', conflictWith: 'write_file' },
    ],
  },
  {
    id: 'mcp-fetch', name: 'fetch', origin: 'builtin', transport: 'http', url: 'http://127.0.0.1:7790/mcp',
    enabled: true, state: 'ready', calls: 18, failures: 0, healthIntervalS: 60,
    platforms: { windows: true, linux: true, mobile: true },
    tools: [
      { name: 'fetch_url', description: '抓取网页并转纯文本', risk: 'readonly' },
      { name: 'extract_links', description: '抽取页面链接', risk: 'readonly' },
    ],
  },
  {
    id: 'mcp-pg', name: 'postgres', origin: 'installed', transport: 'sse', url: 'http://127.0.0.1:7788/sse',
    enabled: true, state: 'degraded', lastError: '连续 3 次调用失败：connection refused', calls: 12, failures: 3, healthIntervalS: 60,
    platforms: { windows: true, linux: true, mobile: false },
    tools: [{ name: 'query', description: '执行只读 SQL', risk: 'readonly' }],
  },
  {
    id: 'mcp-playwright', name: 'playwright', origin: 'installed', transport: 'stdio', command: 'npx -y @playwright/mcp@latest',
    enabled: false, state: 'disabled', calls: 0, failures: 0,
    platforms: { windows: true, linux: true, mobile: false },
    tools: [
      { name: 'browser_navigate', description: '打开网页', risk: 'write' },
      { name: 'browser_click', description: '点击元素', risk: 'write' },
      { name: 'browser_snapshot', description: '页面快照', risk: 'readonly' },
    ],
  },
  {
    id: 'mcp-repo-graph', name: 'repo-graph', origin: 'extended', transport: 'stdio', command: 'node .harness/extensions/repo-graph/server.js',
    enabled: true, state: 'ready', calls: 4, failures: 0, healthIntervalS: 120,
    platforms: { windows: true, linux: true, mobile: false },
    tools: [
      { name: 'symbol_graph', description: '构建符号依赖图（Agent 自建）', risk: 'readonly' },
      { name: 'impact_analysis', description: '改动影响面分析', risk: 'readonly' },
    ],
  },
]

// ---------------- 制品 registry ----------------
// platforms：制品支持的客户端平台（PC 端分 Windows / Linux；缺省视为全平台）
export const registryItems: RegistryItem[] = [
  { id: 'reg-playwright', name: '@playwright/mcp', version: '0.0.42', kind: 'mcp', description: '浏览器自动化 MCP（需 Node + Chromium）', sizeMb: 152, requires: ['node>=18', 'chromium'], signature: 'signed', installed: true, platforms: { windows: true, linux: true, mobile: false } },
  { id: 'reg-pg', name: '@modelcontextprotocol/server-postgres', version: '0.6.2', kind: 'mcp', description: 'PostgreSQL 只读查询 MCP', sizeMb: 8, requires: ['node>=18'], signature: 'signed', installed: true, platforms: { windows: true, linux: true, mobile: false } },
  { id: 'reg-gh', name: 'mcp-server-github', version: '1.4.0', kind: 'mcp', description: 'GitHub Issue / PR 能力（HTTP 传输，全端可用）', sizeMb: 12, requires: ['node>=18'], signature: 'signed', installed: false, platforms: { windows: true, linux: true, mobile: true } },
  { id: 'reg-pdf', name: 'pdf-extract', version: '0.3.1', kind: 'plugin', description: 'PDF 文本与表格抽取（单文件二进制）', sizeMb: 24, requires: [], signature: 'signed', installed: true, platforms: { windows: true, linux: true, mobile: true } },
  { id: 'reg-csv', name: 'csv-clean', version: '1.2.0', kind: 'plugin', description: 'CSV 清洗与去重归类工具', sizeMb: 1.5, requires: [], signature: 'signed', installed: false, platforms: { windows: true, linux: true, mobile: true } },
  { id: 'reg-code-review', name: 'code-review-skill', version: '1.1.0', kind: 'skill', description: '代码审查流程与检查清单', sizeMb: 0.2, requires: [], signature: 'signed', installed: true, platforms: { windows: true, linux: true, mobile: true } },
  { id: 'reg-sql-opt', name: 'sql-optimizer-skill', version: '0.4.0', kind: 'skill', description: 'SQL 优化建议与执行计划解读', sizeMb: 0.3, requires: [], signature: 'signed', installed: false, platforms: { windows: true, linux: true, mobile: true } },
  { id: 'reg-community-db', name: 'community-db-tool', version: '0.9.0', kind: 'plugin', description: '社区源示例（未签名，仅 PC 端）', sizeMb: 3, requires: [], signature: 'unsigned', installed: false, platforms: { windows: true, linux: true, mobile: false } },
]

// ---------------- 技能 / 记忆 ----------------
// origin：builtin = 随服务端内置；installed = 本端已安装（本地/registry 下载）；extended = 程序自我拓展
// platforms：技能支持的客户端平台（脚本执行类仅 PC 端）
export const skills: Skill[] = [
  { id: 'sk-builtin-web-test', name: 'web-autonomous-test', description: '无头浏览器语义测试与异常自愈（内置 SOP）', source: 'local', origin: 'builtin', updatedAt: now - 30 * min, platforms: { windows: true, linux: true, mobile: false } },
  { id: 'sk-builtin-plan', name: 'plan-first-workflow', description: '大改动计划先行：create_plan → 自动推进 → 验收门禁', source: 'local', origin: 'builtin', updatedAt: now - 45 * min, platforms: { windows: true, linux: true, mobile: true } },
  { id: 'sk1', name: 'build-check', description: '构建与静态检查流水线', source: 'local', origin: 'installed', updatedAt: now - 2 * hour, script: 'npm run build && go vet ./...', platforms: { windows: true, linux: true, mobile: false } },
  { id: 'sk2', name: 'code-review-skill', description: '代码审查流程与检查清单', source: 'registry', origin: 'installed', updatedAt: now - 26 * hour, platforms: { windows: true, linux: true, mobile: true } },
  { id: 'sk3', name: 'release-notes', description: '根据提交记录生成发布说明', source: 'local', origin: 'installed', updatedAt: now - 3 * 24 * hour, platforms: { windows: true, linux: true, mobile: true } },
  { id: 'sk-ext-pdf', name: 'pdf-table-ocr', description: 'PDF 表格抽取流程（由 Agent 在会话中自建并自检）', source: 'local', origin: 'extended', updatedAt: now - 6 * hour, platforms: { windows: true, linux: true, mobile: false } },
]

// ---------------- 拓展：程序在开发过程中为自身扩展的能力 ----------------
// 由自演化引擎产出：先草稿（自检中）→ 用户审批 → 安装；安装后进入各页「已安装」
export const extensions: ExtensionItem[] = [
  {
    id: 'ext-tool-git',
    kind: 'tool',
    name: 'git_smart_commit',
    version: '0.1.0',
    description: '按改动语义自动切分提交并生成 Conventional Commits 文案（由 Agent 在会话中自建）',
    generatedBy: 'session: 提交规范改造',
    risk: 'write',
    state: 'installed',
    selfTest: 'passed',
    signature: 'signed',
    createdAt: now - 5 * hour,
    syncedTo: ['client-win'],
  },
  {
    id: 'ext-tool-graph',
    kind: 'tool',
    name: 'code_graph_query',
    version: '0.2.1',
    description: '本地符号图查询（SQLite + FTS5），用于低成本定位实现位置',
    generatedBy: 'session: 大型代码库检索优化',
    risk: 'readonly',
    state: 'draft',
    selfTest: 'pending',
    signature: 'unsigned',
    createdAt: now - 3 * hour,
  },
  {
    id: 'ext-mcp-graph',
    kind: 'mcp',
    name: 'repo-graph',
    version: '0.1.2',
    description: 'Agent 自建 MCP Server：符号依赖图与改动影响面分析',
    generatedBy: 'session: 跨模块重构评估',
    risk: 'readonly',
    state: 'installed',
    selfTest: 'passed',
    signature: 'signed',
    createdAt: now - 26 * hour,
    syncedTo: ['client-win'],
  },
  {
    id: 'ext-mcp-notify',
    kind: 'mcp',
    name: 'notify-hub',
    version: '0.0.9',
    description: '长任务完成/失败时向桌面端与移动端推送通知（MCP over HTTP）',
    generatedBy: 'session: 长跑任务提醒',
    risk: 'write',
    state: 'disabled',
    selfTest: 'passed',
    signature: 'unsigned',
    createdAt: now - 2 * 24 * hour,
  },
  {
    id: 'ext-skill-pdf',
    kind: 'skill',
    name: 'pdf-table-ocr',
    version: '0.1.0',
    description: '把 PDF 表格抽取固化为可复用技能，含失败重试与字段校验',
    generatedBy: 'session: 报表解析',
    risk: 'readonly',
    state: 'installed',
    selfTest: 'passed',
    signature: 'signed',
    createdAt: now - 6 * hour,
  },
  {
    id: 'ext-skill-review',
    kind: 'skill',
    name: 'review-diff-guard',
    version: '0.1.0',
    description: '提交前自动跑差异审查清单（含破坏性变更检测）',
    generatedBy: 'session: 提交前校验',
    risk: 'write',
    state: 'draft',
    selfTest: 'failed',
    signature: 'unsigned',
    createdAt: now - 90 * min,
  },
]

// ---------------- 客户端节点（客户端自行安装：每个端各自安装自己的能力） ----------------
// os 决定平台兼容性：PC 端区分 Windows / Linux，移动端 iOS / Android；self = 当前操作所在客户端。
export const devices: DeviceNode[] = [
  {
    id: 'client-win', name: '桌面端 · Windows', platform: 'desktop', os: 'windows', online: true, self: true,
    installed: ['read_file', 'write_file', 'edit_file', 'run_command', 'fetch_web_page', 'mcp:filesystem', 'skill:build-check', 'reg:reg-pdf'],
    installing: [], lastSyncAt: now - 2 * min,
  },
  {
    id: 'client-linux', name: '桌面端 · Linux', platform: 'desktop', os: 'linux', online: true,
    installed: ['read_file', 'list_dir', 'grep', 'run_command', 'record_memory', 'mcp:repo-graph'],
    installing: [], lastSyncAt: now - 18 * min,
  },
  {
    id: 'client-ios', name: '移动端 · iOS', platform: 'mobile', os: 'ios', online: true,
    installed: ['read_file', 'fetch_web_page', 'skill:web-autonomous-test'],
    installing: [], lastSyncAt: now - 3 * hour,
  },
  {
    id: 'client-android', name: '移动端 · Android', platform: 'mobile', os: 'android', online: false,
    installed: ['read_file', 'todo'],
    installing: [], lastSyncAt: now - 3 * 24 * hour,
  },
  {
    id: 'dev-server', name: 'Web 服务端', platform: 'server', os: 'web', online: true,
    installed: ['read_file', 'write_file', 'run_command', 'fetch_web_page', 'spawn_subagent'],
    installing: [], lastSyncAt: now - 2 * min,
  },
]

// ---------------- 定时任务（客户端本地调度，到点自行拉起一次会话） ----------------
export const scheduledTasks: ScheduledTask[] = [
  {
    id: 'sch1', name: '每日构建健康巡检', prompt: '跑一遍 npm run build 与 go vet，失败则定位根因并给出修复方案',
    cron: '0 9 * * 1-5', scheduleText: '工作日 09:00', enabled: true,
    nextRunAt: now + 6 * hour, lastRunAt: now - 18 * hour, lastStatus: 'success',
    sessionTitle: '构建巡检', clientId: 'client-win', accessMode: 'confirm',
    lastSummary: '构建通过，0 error / 2 warning（沿用上次结论）',
  },
  {
    id: 'sch2', name: '依赖漏洞周报', prompt: '扫描依赖漏洞并生成周报，高危项需附修复命令',
    cron: '0 10 * * 1', scheduleText: '每周一 10:00', enabled: true,
    nextRunAt: now + 2 * 24 * hour, lastRunAt: now - 5 * 24 * hour, lastStatus: 'failed',
    sessionTitle: '依赖治理', clientId: 'client-linux', accessMode: 'confirm',
    lastSummary: '上次失败：npm audit 网络超时（可重试）',
  },
  {
    id: 'sch3', name: '夜间回归自测', prompt: '对本地 5611 端口跑无头浏览器全链路自测，输出失败用例',
    cron: '30 2 * * *', scheduleText: '每天 02:30', enabled: false,
    lastRunAt: now - 3 * 24 * hour, lastStatus: 'never',
    sessionTitle: '自动回归', clientId: 'client-win', accessMode: 'full',
  },
  {
    id: 'sch4', name: '移动端待办收口', prompt: '汇总今日会话中未完成的待办，推送到移动端',
    cron: '0 20 * * *', scheduleText: '每天 20:00', enabled: true,
    nextRunAt: now + 4 * hour, lastRunAt: now - 20 * hour, lastStatus: 'success',
    sessionTitle: '待办收口', clientId: 'client-ios', accessMode: 'confirm',
    lastSummary: '推送 3 条未完成待办',
  },
]

export const memories: MemoryItem[] = [
  { id: 'me1', title: 'harness_go 基础架构基线', category: 'profile', updatedAt: now - 2 * hour },
  { id: 'me2', title: '工程避坑清单（前端渲染/前缀缓存）', category: 'convention', updatedAt: now - 5 * hour },
  { id: 'me3', title: '桌面版 token 计算机制与缺陷', category: 'digest', updatedAt: now - 26 * hour },
]

// ---------------- 快照 ----------------
export const snapshots: Snapshot[] = [
  { id: 'sn1', sessionId: 's1', messageId: 'm5', turnSeq: 2, toolEventId: 'tc2', filePath: 'internal/tools/registry.go', beforeHash: 'a1b2', afterHash: 'c3d4', isNewFile: false, sizeBytes: 4211, createdAt: now - 11 * min },
  { id: 'sn2', sessionId: 's1', messageId: 'm5', turnSeq: 2, toolEventId: 'tc2', filePath: 'internal/tools/registry_test.go', beforeHash: undefined, afterHash: 'e5f6', isNewFile: true, sizeBytes: 1880, createdAt: now - 11 * min },
  { id: 'sn3', sessionId: 's1', messageId: 'm5', turnSeq: 2, toolEventId: 'tc2', filePath: 'internal/tools/fixtures/large.json', beforeHash: 'aa11', afterHash: 'bb22', isNewFile: false, sizeBytes: 7_340_032, skipped: true, createdAt: now - 11 * min },
  { id: 'sn4', sessionId: 's1', messageId: 'm2', turnSeq: 1, toolEventId: 'tc1', filePath: 'internal/tools/registry.go', beforeHash: 'old1', afterHash: 'a1b2', isNewFile: false, sizeBytes: 3980, createdAt: now - 39 * min, revertedAt: now - 38 * min },
]

// ---------------- 运行中的命令行 ----------------
export const commands: CommandRun[] = [
  { id: 'cmd1', cmd: 'go test ./internal/tools/... -race', pid: 31241, startedAt: now - 8 * min, status: 'running' },
  { id: 'cmd2', cmd: 'npm run build && npm run lint', pid: 30988, startedAt: now - 2 * min, status: 'running' },
]

// ---------------- 子进程（独立进程，可停止/恢复；含详细对话转录） ----------------
export const subagents: Subagent[] = [
  {
    id: 'sa1', name: '前端专家', role: '前端开发', status: 'running', summary: '正在实现三端布局骨架', tokens: 12400, startedAt: now - 22 * min,
    messages: [
      { id: 'sa1m1', role: 'user', content: '实现统一三端布局骨架：PC 侧栏可收起、平板侧栏常驻、移动端侧栏抽屉化。使用 useBreakpoint 分别渲染三套骨架。' },
      {
        id: 'sa1m2', role: 'assistant', tokens: 680, promptTokens: 520, cachedTokens: 410, durationMs: 1240,
        content: '我已经阅读了 AppShell 与 Sidebar 的现有实现，确认断点变化由 useBreakpoint 驱动。\n\n- PC：侧栏列宽随 sidebarOpen 切换 0/264px\n- 平板：常驻 264px\n- 移动端：Drawer 抽屉\n\n现在开始改 `src/layout/AppShell.tsx` 的骨架分支，完成后跑 tsc 校验。',
        toolCalls: [{
          id: 'sa1t1', name: 'read_file', risk: 'readonly', status: 'ok', durationMs: 36,
          args: { path: 'src/layout/AppShell.tsx' },
          result: '已读取骨架定义：桌面/平板/移动三套分支与 useBreakpoint 接线确认一致。',
        }],
      },
      { id: 'sa1m3', role: 'user', content: '注意不要破坏 TopBar 的模型选择器，移动端要保留。' },
      {
        id: 'sa1m4', role: 'assistant', tokens: 240, promptTokens: 180, cachedTokens: 140, durationMs: 640,
        content: '收到。模型选择器已在窄屏下做了 max-w 与隐藏处理（`110px → 180px` 响应式收窄），不会破坏。骨架分支正在编写中…',
      },
    ],
  },
  {
    id: 'sa2', name: '测试专家', role: '测试验证', status: 'completed', summary: '已补齐注册表单测，覆盖率 78.3%', tokens: 8100, startedAt: now - 48 * min,
    messages: [
      { id: 'sa2m1', role: 'user', content: '为 internal/tools 注册表补齐单测：空名校验、同名冲突检测、死锁路径。禁止修改生产代码。' },
      {
        id: 'sa2m2', role: 'assistant', tokens: 920, promptTokens: 700, cachedTokens: 560, durationMs: 1820,
        content: '已按三个分支补齐 `registry_test.go`：\n\n1. Register("") → 断言返回 ErrEmptyName\n2. Register 同名两次 → 断言 ErrConflict\n3. 并发注册 → 用 -race 断言无死锁\n\n`go test ./internal/tools/` 全部通过，覆盖率 **78.3%**。',
        toolCalls: [{
          id: 'sa2t1', name: 'read_file', risk: 'readonly', status: 'ok', durationMs: 41,
          args: { path: 'internal/tools/registry.go' },
          result: '已读取现有断言与注册表实现，确认三个待覆盖分支。',
        }],
      },
      { id: 'sa2m3', role: 'user', content: '确认没有改动生产代码？' },
      {
        id: 'sa2m4', role: 'assistant', tokens: 130, promptTokens: 90, cachedTokens: 70, durationMs: 420,
        content: '确认。本次仅新增测试文件，`git status` 显示未触碰任何 src 文件。',
      },
    ],
  },
  {
    id: 'sa3', name: '文档编写', role: '文档', status: 'interrupted', summary: '进程重启导致中断，可重试', tokens: 3200, startedAt: now - 2 * hour,
    messages: [
      { id: 'sa3m1', role: 'user', content: '基于 docs/架构设计文档.md 梳理一份分层依赖表，输出到 docs/分工.md。' },
      {
        id: 'sa3m2', role: 'assistant', tokens: 310, promptTokens: 220, cachedTokens: 170, durationMs: 780,
        content: '正在阅读架构文档的分层与上下文组装章节，准备起草依赖单向表…',
        toolCalls: [{
          id: 'sa3t1', name: 'read_file', risk: 'readonly', status: 'ok', durationMs: 33,
          args: { path: 'docs/架构设计文档.md' },
          result: '已读取分层架构与上下文组装章节（M0–M1 范围）。',
        }],
      },
    ],
  },
]

// ---------------- 审批规则（项目级：在「项目设置 → 审批规则」中管理） ----------------
export const approvalRules: ApprovalRule[] = [
  { id: 'ar1', projectId: 'p1', toolName: 'run_command', pattern: 'go test', createdAt: now - 2 * hour },
  { id: 'ar2', projectId: 'p1', toolName: 'write_file', pattern: 'internal/tools/', createdAt: now - 40 * min },
  { id: 'ar3', projectId: 'p2', toolName: 'run_command', pattern: 'cargo check', createdAt: now - 3 * hour },
]

// ---------------- 关联项目（项目设置中对另一目录的引用 + 说明） ----------------
export const projectLinks: ProjectLink[] = [
  { id: 'pl1', projectId: 'p1', path: 'F:/WorkSpace/Other/harness_mini', description: '桌面版同源项目。共用需求说明与架构文档；改共用约定时两边同步。', createdAt: '' },
  { id: 'pl2', projectId: 'p1', path: 'F:/WorkSpace/data-pipeline', description: '数据管道项目，无已知依赖，仅作参考。', createdAt: '' },
]

// ---------------- 数据权限（项目级：在「项目设置 → 数据权限」中管理） ----------------
export const dataGrants: DataScopeGrant[] = [
  { id: 'dg1', projectId: 'p1', scope: 'fs_outside', mode: 'ask', updatedAt: now - 3 * hour },
  { id: 'dg2', projectId: 'p1', scope: 'session_export', mode: 'allow', updatedAt: now - 2 * hour },
  { id: 'dg3', projectId: 'p1', scope: 'db_query', mode: 'ask', updatedAt: now - hour },
  { id: 'dg4', projectId: 'p1', scope: 'net_egress', mode: 'deny', updatedAt: now - 30 * min },
]

// ---------------- 运行态 ----------------
export const runningSessionId = 's1'
export const contextLimit = 64000
export const usedTokens = 18700

// ---------------- 长会话（用于验证虚拟列表：320 条消息 / 160 轮） ----------------
function genLongMessages(): Message[] {
  const out: Message[] = []
  const base = now - 6 * hour
  for (let t = 1; t <= 160; t++) {
    out.push({
      id: `lm-${t}-u`,
      sessionId: 's2',
      seq: t * 2 - 1,
      turnSeq: t,
      role: 'user',
      content: `第 ${t} 轮：检查 internal/module_${t}/handler.go 的实现，指出错误处理与并发保护方面的问题。`,
      createdAt: base + t * 60_000,
    })
    out.push({
      id: `lm-${t}-a`,
      sessionId: 's2',
      seq: t * 2,
      turnSeq: t,
      role: 'assistant',
      content: `第 ${t} 轮分析：\n\n- 当前实现在 \`internal/module_${t}/handler.go\`\n- 问题：错误未包装、缺少并发保护、无单测\n\n建议先补测试再重构。`,
      tokens: 160 + (t % 40),
      createdAt: base + t * 60_000 + 3_000,
    })
  }
  return out
}

export const longSessionMessages = genLongMessages()
