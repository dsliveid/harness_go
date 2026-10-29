// ============ 基础枚举 ============
export type Role = 'system' | 'user' | 'assistant' | 'tool'
export type RunStatus = 'pending' | 'running' | 'waiting_approval' | 'completed' | 'failed' | 'cancelled' | 'interrupted'
export type Risk = 'readonly' | 'write' | 'command' | 'danger'
export type AccessMode = 'confirm' | 'full'
export type ToolStatus = 'pending' | 'running' | 'ok' | 'error' | 'denied'
export type ApprovalDecision = 'allow_once' | 'allow_session' | 'deny'

// ============ 工作空间（服务端大脑 + 多客户端接入） ============
export type WorkspaceKind = 'server' | 'desktop' | 'mobile' | 'browser'

export interface Workspace {
  id: string
  /** 工作空间显示名：内置空间固定为服务端，客户端接入时可自定义 */
  name: string
  kind: WorkspaceKind
  /** 在线状态：离线空间仅可查看历史对话，不支持发起对话 */
  online: boolean
  /** 绑定的肢体数量（可执行工具的端） */
  limbCount: number
  /** 最近活跃时间（ms），离线时用于展示「多久前活跃」 */
  lastSeenAt?: number
  /** 接入协议版本 */
  protocol?: string
  /** 内置空间（服务端本身）：永远在线、不可移除 */
  builtin?: boolean
  /** 接入该空间时客户端上报的标识，如 desk-01 / phone-01 */
  clientId?: string
}

// ============ 会话 / 项目 / 消息 ============
export type PlanMode = 'standard' | 'always_plan' | 'always_proceed'

export interface Project {
  id: string
  name: string
  path: string
  pinned?: boolean
  constraints?: string
  planMode?: PlanMode
  /** 归属工作空间（缺省视为内置服务端空间） */
  workspaceId?: string
}

// 关联项目：项目设置中对另一目录的引用 + 说明（只存路径，不存对方 id）
export interface ProjectLink {
  id: string
  projectId: string
  path: string
  description: string
  createdAt: string
}

export interface Session {
  id: string
  title: string
  projectId?: string
  /** 归属工作空间（缺省按 project 推导，纯对话缺省为内置服务端空间） */
  workspaceId?: string
  workspacePath?: string
  accessMode: AccessMode
  modelRef: string
  updatedAt: number
  archivedAt?: number
  running?: boolean
  unread?: boolean
}

export interface ToolCall {
  id: string
  name: string
  args: Record<string, unknown>
  status: ToolStatus
  risk: Risk
  result?: string
  diff?: FileDiff
  durationMs?: number
  deniedReason?: string
}

export interface Message {
  id: string
  sessionId: string
  runId?: string
  seq: number
  turnSeq: number
  role: Role
  content: string
  reasoning?: string
  toolCalls?: ToolCall[]
  toolCallId?: string
  createdAt: number
  streaming?: boolean
  tokens?: number
  promptTokens?: number
  cachedTokens?: number
  durationMs?: number
  reverted?: boolean
  revertedAt?: number
  queued?: boolean
  snapshotSkipped?: boolean
}

export interface Run {
  id: string
  sessionId: string
  status: RunStatus
  startedAt?: number
  endedAt?: number
  tokensIn?: number
  tokensOut?: number
  errorCode?: string
  errorMessage?: string
}

// ============ 审批 ============
export interface Approval {
  id: string
  runId: string
  toolCallId: string
  toolName: string
  risk: Risk
  args: Record<string, unknown>
  summary: string
  externalPath?: boolean
  /** 审批类型：工具调用（默认） | 数据权限 */
  kind?: 'tool' | 'data'
  /** 数据权限审批时的数据范围 */
  dataScope?: DataScope
  createdAt: number
}

export interface ApprovalRule {
  id: string
  projectId: string
  toolName: string
  pattern: string
  createdAt: number
}

// ============ 设置（程序设置弹窗：模型/工具/网络/SOP） ============
export type ToolGroup = 'file' | 'net' | 'plan' | 'memory' | 'agent' | 'decision' | 'skill'

export interface ToolInfo {
  name: string
  title: string
  risk: Risk
  /** 系统内置 | 自演化引擎（技能库三件套） */
  category: 'builtin' | 'skill'
  /** 设置页分组（文件与命令/网络与多模态/任务与计划/长期记忆/子 Agent 编排/决策模型/自演化引擎） */
  group: ToolGroup
  description: string
}

export interface AgentSopInfo {
  id: string
  name: string
  category: 'thinking' | 'workflow' | 'orchestration' | 'quality' | 'memory'
  categoryLabel: string
  description: string
  /** 禁用后的影响说明 */
  disableEffect: string
}

export interface TokenCall {
  time: number
  prompt: number
  completion: number
  cached: number
  latencyMs: number
}

export interface TokenStatRow {
  model: string
  provider: string
  sessions: number
  prompt: number
  completion: number
  cached: number
  /** 预估/实际滚动偏差系数（1.00 = 完全一致） */
  calibRatio: number
  /** 最近调用明细（供行内展开） */
  recent?: TokenCall[]
}

export interface LogEntry {
  id: string
  time: number
  level: 'info' | 'warn' | 'error'
  source: string
  text: string
  /** 追踪 ID（详情展开展示） */
  traceId?: string
  /** 详细信息（行展开时展示） */
  detail?: string
}

// ============ 模型 ============
/** 请求协议（对齐桌面端 RequestProtocol：决定该模型走哪条协议适配器） */
export type RequestProtocol = 'chat_completions' | 'messages' | 'response' | 'systemone'

export interface ModelInfo {
  id: string
  contextLimit: number
  capabilities: Array<'chat' | 'vision' | 'image' | 'decision' | 'reasoning'>
  /** 该模型的请求协议，缺省视为 chat_completions（OpenAI 兼容） */
  protocol: RequestProtocol
}

export interface Provider {
  id: string
  name: string
  baseUrl: string
  models: ModelInfo[]
  enabled: boolean
  apiKeyMasked: string
}

// ============ 插件 / MCP / 制品 ============
export type PluginKind = 'builtin' | 'mcp' | 'http' | 'wasm'
export type PluginState = 'registered' | 'validating' | 'available' | 'enabled' | 'unavailable' | 'degraded' | 'disabled'

export interface Plugin {
  id: string
  name: string
  kind: PluginKind
  description: string
  version: string
  state: PluginState
  requires: string[]
  missing?: string[]
  tools: string[]
  riskCeiling: Risk
  defaultEnabled: boolean
  calls?: number
  failures?: number
  phase: 'M1' | 'M2' | 'M3' | 'M4'
}

export type McpTransport = 'stdio' | 'sse' | 'http'
export type McpState = 'starting' | 'ready' | 'degraded' | 'failed' | 'disabled'

export interface McpToolInfo {
  name: string
  description: string
  risk: Risk
  /** 与内置工具同名冲突：本地优先，该工具以 mcp:<server> 命名空间注入，不覆盖内置 */
  conflictWith?: string
}

export interface McpServer {
  id: string
  name: string
  /** 来源层级：系统内置 / 已安装 / 程序自我拓展 */
  origin?: ExtOrigin
  transport: McpTransport
  command?: string
  url?: string
  enabled: boolean
  state: McpState
  tools: McpToolInfo[]
  lastError?: string
  calls?: number
  failures?: number
  healthIntervalS?: number
  /** 支持的客户端平台（缺省视为全平台） */
  platforms?: PlatformSupport
}

/**
 * 能力来源层级（工具 / MCP / 技能 的二级视图统一口径）：
 * - builtin   系统内置：随服务端（Web 端）发布，可由服务端安装到客户端
 * - installed 已安装：本端已落地（本地技能、本地制品、已下载 registry 包）
 * - extended  拓展：程序在开发过程中为自身扩展出来的能力（自演化引擎产出）
 * 市场（market）不是来源层级，而是远端 registry 的候选池。
 */
export type ExtOrigin = 'builtin' | 'installed' | 'extended'

export type RegistryKind = 'mcp' | 'plugin' | 'skill'
export interface RegistryItem {
  id: string
  name: string
  version: string
  kind: RegistryKind
  description: string
  sizeMb: number
  requires: string[]
  signature: 'signed' | 'unsigned'
  installed?: boolean
  installing?: boolean
  progress?: number
  /** 支持的客户端平台（缺省视为全平台） */
  platforms?: PlatformSupport
}

// ============ 计划 / 待办 / 技能 / 记忆 ============
export interface PlanStep {
  id: string
  content: string
  status: 'pending' | 'in_progress' | 'done'
}

export interface PlanDoc {
  id: string
  title: string
  version: number
  active: boolean
  steps: PlanStep[]
  updatedAt: number
  /** 方案详细信息（create_plan 产出，浮窗详情展示用） */
  goals?: string
  architecture?: string
  files?: string[]
  verification?: string
  status?: 'in_progress' | 'completed' | 'suspended'
  /** 物理计划文档路径（.harness/plans/xxx.md） */
  path?: string
}

export interface TodoItem {
  id: string
  content: string
  status: 'pending' | 'in_progress' | 'done'
}

export interface Skill {
  id: string
  name: string
  description: string
  source: 'local' | 'registry'
  /** 来源层级：系统内置（随服务端发布）/ 已安装 / 程序自我拓展 */
  origin?: ExtOrigin
  updatedAt: number
  script?: string
  /** 支持的客户端平台（缺省视为全平台） */
  platforms?: PlatformSupport
}

// ============ 拓展（程序为自身扩展的能力）与多端协同 ============
export type ExtensionKind = 'tool' | 'mcp' | 'skill'
export type ExtensionState = 'draft' | 'installed' | 'disabled'

/** 拓展项：Agent 在开发过程中为自己生成/固化的能力，需自检 + 审批后安装 */
export interface ExtensionItem {
  id: string
  kind: ExtensionKind
  name: string
  version: string
  description: string
  /** 产出该能力的运行/会话来源，便于追溯与回滚 */
  generatedBy: string
  /** 该能力引入的风险上限 */
  risk: Risk
  state: ExtensionState
  /** 自检结果：安装前必须通过 */
  selfTest: 'passed' | 'pending' | 'failed'
  signature: 'signed' | 'unsigned'
  createdAt: number
  /** 已下发到的端 id 列表（多端协同） */
  syncedTo?: string[]
}

/** 端节点：服务端（本端）/ 桌面客户端 / 移动端，能力安装以此为落地载体 */
export type DevicePlatform = 'server' | 'desktop' | 'mobile'

/** 客户端操作系统：PC 端区分 Windows / Linux，移动端区分 iOS / Android */
export type ClientOS = 'windows' | 'linux' | 'ios' | 'android' | 'web'

/**
 * 能力支持的客户端平台矩阵（安装兼容性判定依据）：
 * PC 端必须分别声明 Windows 与 Linux，移动端合并为 mobile 一档。
 */
export interface PlatformSupport {
  windows: boolean
  linux: boolean
  mobile: boolean
}

export interface DeviceNode {
  id: string
  name: string
  platform: DevicePlatform
  /** 客户端操作系统（PC 端 Windows/Linux，移动端 iOS/Android，服务端为 web） */
  os: ClientOS
  online: boolean
  /** 本端已安装能力 key：工具名 / mcp:<server> / skill:<name> / ext:<name> / reg:<id> */
  installed: string[]
  /** 安装中的能力 key */
  installing: string[]
  lastSyncAt?: number
  /** 是否为当前操作所在客户端（客户端自行安装：安装动作只作用于本端） */
  self?: boolean
}

/** 系统内置工具的可安装性元信息（按工具分组给出默认值，可被单工具覆盖） */
export interface ToolInstallMeta {
  /** 是否可作为独立制品安装到客户端（false = 只能随服务端运行） */
  installable: boolean
  /** 安装方式：随包内置 / 按需下发 / 不可安装 */
  mode: '随包内置' | '按需下发' | '不可安装'
  /** 客户端侧运行依赖 */
  deps: string[]
  note: string
  /** 支持的客户端平台（移动端 / PC 端 Windows、Linux 分别声明） */
  platforms: PlatformSupport
}

// ============ 定时任务 ============
/** 定时任务：按 cron 在指定客户端自动拉起一次 Agent 会话 */
export interface ScheduledTask {
  id: string
  name: string
  /** 触发时下发给 Agent 的提示词 */
  prompt: string
  /** cron 表达式（五段式） */
  cron: string
  /** 人类可读调度描述 */
  scheduleText: string
  enabled: boolean
  /** 下次执行时间戳 */
  nextRunAt?: number
  lastRunAt?: number
  lastStatus?: 'success' | 'failed' | 'running' | 'never'
  /** 执行时归属的会话（复用上下文） */
  sessionTitle?: string
  /** 在哪个客户端上执行（客户端自行调度） */
  clientId: string
  /** 目标客户端绑定的访问模式 */
  accessMode: 'confirm' | 'full'
  /** 最近一次执行摘要 */
  lastSummary?: string
}

export interface MemoryItem {
  id: string
  title: string
  category: 'profile' | 'convention' | 'digest'
  updatedAt: number
}

// ============ 上下文压缩 ============
export type CompactionStrategy = 'semantic' | 'summary_only' | 'truncate'

/** 待确认的压缩请求（达到阈值或用户手动触发） */
export interface CompactionPending {
  sessionId: string
  beforeTokens: number
  limit: number
  threshold: number
  /** 预计丢弃的轮次数 */
  droppedTurns: number
  /** 预计保留的最近轮次数 */
  keptTurns: number
  /** 触发来源：自动（达阈值） | 手动 */
  trigger: 'auto' | 'manual'
}

/** 一次已完成压缩的记录（压缩历史 + 摘要展示） */
export interface CompactionRecord {
  id: string
  sessionId: string
  createdAt: number
  beforeTokens: number
  afterTokens: number
  freedTokens: number
  droppedTurns: number
  keptTurns: number
  strategy: CompactionStrategy
  summary: string
  trigger: 'auto' | 'manual'
}

// ============ 数据权限（项目级策略 + 审批） ============
/** 数据范围：工作区外路径 / 会话导出 / 数据库查询 / 外部外发 */
export type DataScope = 'fs_outside' | 'session_export' | 'db_query' | 'net_egress'
/** 策略：拒绝 / 每次询问 / 直接允许 */
export type DataScopeMode = 'deny' | 'ask' | 'allow'

export interface DataScopeGrant {
  id: string
  projectId: string
  scope: DataScope
  mode: DataScopeMode
  updatedAt: number
}

// ============ 快照 / 回滚 ============
export interface DiffLine {
  type: 'add' | 'del' | 'ctx'
  text: string
  no?: number
}

export interface DiffHunk {
  id: string
  header: string
  before: DiffLine[]
  after: DiffLine[]
  reverted?: boolean
}

export interface FileDiff {
  path: string
  hunks: DiffHunk[]
  additions: number
  deletions: number
}

export interface Snapshot {
  id: string
  sessionId: string
  messageId: string
  turnSeq: number
  toolEventId?: string
  filePath: string
  beforeHash?: string
  afterHash: string
  isNewFile: boolean
  revertedAt?: number
  skipped?: boolean
  sizeBytes: number
  createdAt: number
}

// ============ 子进程（独立进程，可停止/恢复） ============
export interface SubagentMsg {
  id: string
  role: 'user' | 'assistant'
  content: string
  streaming?: boolean
  /** 与主对话消息一致的可选指标（驱动轮次底部指标栏） */
  tokens?: number
  promptTokens?: number
  cachedTokens?: number
  durationMs?: number
  /** 步骤工具调用（有则进入执行过程抽屉，与主对话结构一致） */
  toolCalls?: ToolCall[]
}

export interface Subagent {
  id: string
  name: string
  role: string
  status: 'running' | 'completed' | 'failed' | 'interrupted' | 'cancelled'
  summary: string
  tokens: number
  startedAt: number
  messages: SubagentMsg[]
}

// ============ 运行中的命令行（run_command 派生） ============
export interface CommandRun {
  id: string
  /** 完整命令行 */
  cmd: string
  pid: number
  startedAt: number
  status: 'running' | 'exited' | 'cancelled'
  output?: string
}

// ============ UI ============
export type Breakpoint = 'mobile' | 'tablet' | 'desktop'
export interface ToastMsg {
  id: string
  type: 'info' | 'success' | 'warn' | 'error'
  text: string
}
