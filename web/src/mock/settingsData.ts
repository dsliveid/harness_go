import type { AgentSopInfo, LogEntry, TokenStatRow, ToolGroup, ToolInfo, ToolInstallMeta } from '../types'

const now = Date.now()
const min = 60_000
const hour = 3_600_000

// ---------------- Agent 工具清单（对齐桌面端：标题 + 风险 + 类别） ----------------
export const toolList: ToolInfo[] = [
  // ---------- 文件与检索 ----------
  { group: 'file', name: 'read_file', title: '读取文件', risk: 'readonly', category: 'builtin', description: '读取工作区内文本文件的局部或全部内容，支持按行偏移切片。' },
  { group: 'file', name: 'list_dir', title: '列出目录', risk: 'readonly', category: 'builtin', description: '列出目录下的文件与子目录（含大小）。' },
  { group: 'file', name: 'file_outline', title: '提取大纲', risk: 'readonly', category: 'builtin', description: '提取代码文件的结构骨架（类/函数签名与行号），低成本掌握全貌。' },
  { group: 'file', name: 'glob', title: '查找文件', risk: 'readonly', category: 'builtin', description: '按 glob 模式（支持 ** 与 *）查找文件，返回相对路径列表。' },
  { group: 'file', name: 'grep', title: '搜索内容', risk: 'readonly', category: 'builtin', description: '按正则在工作区文件内容中搜索，支持上下文行展开。' },
  { group: 'file', name: 'write_file', title: '写入文件', risk: 'write', category: 'builtin', description: '新建文件或整体覆盖写入完整内容；仅用于新文件。' },
  { group: 'file', name: 'edit_file', title: '编辑文件', risk: 'write', category: 'builtin', description: '基于精确原文的最小化替换修改文件。' },
  { group: 'file', name: 'run_command', title: '执行命令', risk: 'danger', category: 'builtin', description: '在工作区目录执行 shell 命令（构建、测试、git 等），禁止破坏性命令。' },
  // ---------- 网络与多模态 ----------
  { group: 'net', name: 'fetch_web_page', title: '网页抓取', risk: 'readonly', category: 'builtin', description: '通过 HTTP/HTTPS 抓取网页并清洗为纯文本 / Markdown。' },
  { group: 'net', name: 'generate_image', title: '图片生成', risk: 'write', category: 'builtin', description: '按提示词生成图片或图标并保存至本地，结果直接在界面渲染。' },
  { group: 'net', name: 'recognize_image', title: '视觉识别', risk: 'readonly', category: 'builtin', description: '调用视觉模型识别、分析本地图片文件内容。' },
  // ---------- 任务与计划 ----------
  { group: 'plan', name: 'todo', title: '任务清单', risk: 'readonly', category: 'builtin', description: '维护多步任务清单，随进展更新各项状态。' },
  { group: 'plan', name: 'create_plan', title: '计划管理', risk: 'write', category: 'builtin', description: '在 .harness/plans/ 建立结构化计划文档，作为权威执行锚点。' },
  { group: 'plan', name: 'update_plan', title: '计划更新', risk: 'write', category: 'builtin', description: '需求变更或阶段推进时更新计划正文与分步状态。' },
  { group: 'plan', name: 'switch_plan', title: '切换计划', risk: 'readonly', category: 'builtin', description: '在多个历史计划之间切换，将目标计划重新唤醒为当前活动计划。' },
  { group: 'plan', name: 'read_plan', title: '读取计划', risk: 'readonly', category: 'builtin', description: '查阅指定计划或当前活动计划的完整方案文档正文。' },
  { group: 'plan', name: 'list_plans', title: '计划清单', risk: 'readonly', category: 'builtin', description: '列出工作区已建立的全部任务计划及其推进状态与版本。' },
  // ---------- 长期记忆 ----------
  { group: 'memory', name: 'record_memory', title: '记录记忆', risk: 'write', category: 'builtin', description: '将技术栈、架构约定与阶段结论沉淀至项目知识库。' },
  { group: 'memory', name: 'read_memory', title: '读取记忆', risk: 'readonly', category: 'builtin', description: '查阅项目已积累的技术大盘与主题碎记。' },
  // ---------- 子 Agent 与子进程编排 ----------
  { group: 'agent', name: 'spawn_subagent', title: '派生子 Agent', risk: 'readonly', category: 'builtin', description: '创建独立上下文的子 Agent 并行协作开发，避免污染主会话。' },
  { group: 'agent', name: 'get_subagent_status', title: '子 Agent 状态', risk: 'readonly', category: 'builtin', description: '查询子 Agent 进程的执行状态与最新进展输出摘要。' },
  { group: 'agent', name: 'wait_subagents', title: '等待子 Agent', risk: 'readonly', category: 'builtin', description: '等待子 Agent 执行完毕并汇总其结论与改动文件清单。' },
  { group: 'agent', name: 'stop_subagent', title: '停止子 Agent', risk: 'write', category: 'builtin', description: '停止正在运行的子 Agent 协作进程。' },
  { group: 'agent', name: 'spawn_subprocess', title: '派生子进程', risk: 'readonly', category: 'builtin', description: '创建临时子进程执行聚焦的探索、排查或独立测试任务。' },
  { group: 'agent', name: 'wait_subprocesses', title: '等待子进程', risk: 'readonly', category: 'builtin', description: '等待临时子进程执行完毕并汇总其结论与改动文件清单。' },
  { group: 'agent', name: 'stop_subprocess', title: '停止子进程', risk: 'write', category: 'builtin', description: '强制停止正在运行的临时子进程。' },
  { group: 'agent', name: 'resume_subprocess', title: '恢复子进程', risk: 'write', category: 'builtin', description: '在已有断点处恢复推进被中断或失败的临时子进程。' },
  // ---------- 决策模型 ----------
  { group: 'decision', name: 'judge_decision', title: '决策判断', risk: 'readonly', category: 'builtin', description: '调用决策模型进行高精度二元判断。' },
  { group: 'decision', name: 'choice_decision', title: '决策选择', risk: 'readonly', category: 'builtin', description: '在多个备选项中做出最优选择与分类。' },
  { group: 'decision', name: 'score_decision', title: '决策评分', risk: 'readonly', category: 'builtin', description: '对意图理解与方案合理性做量化百分制评分与多维度质检。' },
  { group: 'skill', name: 'list_skills', title: '查询技能库', risk: 'readonly', category: 'skill', description: '列出工作区 .harness/skills/ 下已定义的所有项目技能。' },
  { group: 'skill', name: 'save_skill', title: '固化项目技能', risk: 'write', category: 'skill', description: '将高频或复杂的组合脚本固化为可重复调用的项目技能。' },
  { group: 'skill', name: 'run_skill', title: '执行项目技能', risk: 'write', category: 'skill', description: '执行已固化的项目技能脚本并返回输出。' },
]

// ---------------- 系统内置工具的可安装性（供「插件市场 → 工具」展示与客户端自装） ----------------
// 关键约定：内置工具必须「可安装」，且必须区分平台支持 —— 移动端 / PC 端（Windows、Linux 分开）。
// 服务端（Web 端）只是其中一端：客户端自行把能力装到自己所在的机器上执行；
// 纯服务端能力（如子 Agent / 子进程编排）标注不可安装，客户端仅通过 API 代理调用。
export const toolInstallMeta: Record<ToolGroup, ToolInstallMeta> = {
  file: { installable: true, mode: '随包内置', deps: ['仅标准库'], note: '纯本地文件系统操作，随客户端包体发布，无需额外下载。', platforms: { windows: true, linux: true, mobile: true } },
  net: { installable: true, mode: '按需下发', deps: ['network', 'model:vision'], note: '网页抓取随包内置；图片生成 / 视觉识别需客户端已配置对应模型能力。', platforms: { windows: true, linux: true, mobile: true } },
  plan: { installable: true, mode: '随包内置', deps: ['fs_write'], note: '计划文档落本地 .harness/plans/，各端共用同一目录约定。', platforms: { windows: true, linux: true, mobile: true } },
  memory: { installable: true, mode: '随包内置', deps: ['fs_write'], note: '记忆落本地 .harness/memory/，多端通过同步协议合并，避免双写冲突。', platforms: { windows: true, linux: true, mobile: true } },
  agent: { installable: false, mode: '不可安装', deps: [], note: '子 Agent / 子进程编排依赖服务端调度与并发信号量，任何客户端都不安装，仅通过 API 代理调用。', platforms: { windows: false, linux: false, mobile: false } },
  decision: { installable: true, mode: '按需下发', deps: ['model:decision'], note: '决策工具是薄壳，客户端需配置决策模型后才可安装启用。', platforms: { windows: true, linux: true, mobile: true } },
  skill: { installable: true, mode: '按需下发', deps: ['fs_read', 'fs_write'], note: '技能库三件套随包内置；具体技能按需安装到客户端 .harness/skills/。', platforms: { windows: true, linux: true, mobile: true } },
}

/** 单工具覆盖（缺省沿用分组元信息）：平台差异集中在此声明，便于逐工具校准 */
export const toolInstallOverrides: Record<string, {
  mode?: ToolInstallMeta['mode']
  deps?: string[]
  platforms?: ToolInstallMeta['platforms']
  note?: string
}> = {
  // 命令执行：PC 端依赖 shell（Windows PowerShell / Linux bash），移动端沙箱不提供
  run_command: {
    mode: '随包内置',
    deps: ['shell'],
    platforms: { windows: true, linux: true, mobile: false },
    note: 'Windows 走 PowerShell、Linux 走 bash；移动端不提供 shell 执行能力。',
  },
  // 写入类工具：PC 端全支持，移动端仅提供受限沙箱写入
  write_file: { platforms: { windows: true, linux: true, mobile: true }, note: '移动端写入受沙箱目录限制（仅工作区根内）。' },
  edit_file: { platforms: { windows: true, linux: true, mobile: true }, note: '移动端写入受沙箱目录限制（仅工作区根内）。' },
  browser_navigate: { mode: '按需下发', deps: ['chromium'], platforms: { windows: true, linux: true, mobile: false }, note: '无头 Chromium 仅 PC 端可安装；移动端走服务端代理。' },
  generate_image: { mode: '按需下发', deps: ['model:image'], platforms: { windows: true, linux: true, mobile: true }, note: '需客户端已配置生图模型。' },
  recognize_image: { mode: '按需下发', deps: ['model:vision'], platforms: { windows: true, linux: true, mobile: true }, note: '需客户端已配置视觉模型。' },
  run_skill: { platforms: { windows: true, linux: true, mobile: false }, note: '技能脚本执行（py/sh/ps1）在移动端不可用，仅可读取技能说明。' },
  save_skill: { platforms: { windows: true, linux: true, mobile: false }, note: '技能固化需本地脚本运行时，移动端仅支持安装现成技能。' },
}

export function toolInstallInfo(name: string, group: ToolGroup): ToolInstallMeta {
  const base = toolInstallMeta[group]
  const o = toolInstallOverrides[name]
  return o
    ? {
        ...base,
        mode: o.mode ?? base.mode,
        deps: o.deps ?? base.deps,
        platforms: o.platforms ?? base.platforms,
        note: o.note ?? base.note,
      }
    : base
}

// ---------------- Agent SOP（对齐桌面端 AGENT_SOPS 六条规范） ----------------
export const agentSops: AgentSopInfo[] = [
  {
    id: 'plan_first',
    name: '方案先行与计划中枢规范',
    category: 'thinking',
    categoryLabel: '思考范式',
    description: '面对大改动任务（3 个以上文件）或多轮需求，在 .harness/plans/ 自动建立结构化计划 MD 文档；需求变更时优先更新计划，后续严格根据计划分步执行，防止多轮需求失真与遗忘。',
    disableEffect: '禁用后：Agent 接到任务后可直接编写代码，不强制在 .harness/plans/ 建立结构化方案文档。',
  },
  {
    id: 'todo_lifecycle',
    name: '任务清单全生命周期规范',
    category: 'workflow',
    categoryLabel: '任务规划',
    description: '接到多步任务时先用 todo 工具列出计划，并随进展更新各项状态；在执行完最后一步、给出最终回复前，务必调用 todo 工具将已完成任务的状态更新为 done，切勿遗留 in_progress 状态。',
    disableEffect: '禁用后：允许 Agent 自由多步推进，不强制调用 todo 工具维护状态清单。',
  },
  {
    id: 'subagent_orchestration',
    name: '子进程协作与架构编排规范',
    category: 'orchestration',
    categoryLabel: '协同体系',
    description: '面对复杂需求、多模块并发开发（如前后端分离、多业务模块并行）时，总架构师按标准流程（规划拆解 → 派生子 Agent → 等待汇聚 → 全局验收）采用子进程协同提升效率与模块隔离度。',
    disableEffect: '禁用后：停用多子进程协作编排指引，Agent 倾向于在单主进程内串行处理全部任务。',
  },
  {
    id: 'surgical_code_reading',
    name: '精益代码研读规范',
    category: 'quality',
    categoryLabel: '工程质量',
    description: '探索业务与代码时遵循漏斗式渐进探索（目录/清单 → file_outline 骨架 → grep 线索 → 局部靶向精读）；严禁对大型文件进行多轮无休止的分页切片式逐行通读；修改前只精读最小必要上下文。',
    disableEffect: '禁用后：允许 Agent 自由阅读大段源码，解除对连续分页通读与精益读取的强约束。',
  },
  {
    id: 'safe_code_edit',
    name: '代码克制与精确修改规范',
    category: 'quality',
    categoryLabel: '工程质量',
    description: '修改文件前必须先用 read_file 读取相关内容，用 edit_file 做基于精确原文的最小化修改；新文件才用 write_file；动手前探索代码结构，修改后尽量用 run_command 运行构建或测试验证。',
    disableEffect: '禁用后：移除代码最小化精确修改与构建验证的强指引约束。',
  },
  {
    id: 'memory_distill',
    name: '边读边记与认知沉淀规范',
    category: 'memory',
    categoryLabel: '知识资产',
    description: '内置工作区长期认知记忆（.harness/memory/）；常见技术栈提问优先秒级召回；深入探索或阅读多个文件后强制调用 record_memory 沉淀技术大盘与主题碎记。',
    disableEffect: '禁用后：移除强制调用 record_memory 沉淀约束，且会话结束后不触发后台自动提炼。',
  },
]

// ---------------- 厂商模型目录（模拟「获取模型列表」：/v1/models 返回） ----------------
const MODEL_CATALOG: Array<{ match: RegExp; models: string[] }> = [
  { match: /deepseek/i, models: ['deepseek-chat', 'deepseek-reasoner', 'deepseek-coder', 'deepseek-vl'] },
  { match: /glm|bigmodel|zhipu/i, models: ['glm-4-plus', 'glm-4-air', 'glm-4-flash', 'glm-4v', 'glm-4v-plus', 'cogview-3'] },
  { match: /anthropic|claude/i, models: ['claude-sonnet-4', 'claude-opus-4', 'claude-haiku-3-5'] },
  { match: /openai/i, models: ['gpt-4o', 'gpt-4o-mini', 'o3-mini'] },
]
export const fallbackModelCatalog = ['qwen-max', 'qwen-plus', 'deepseek-v3', 'llama-3.1-70b', 'mistral-large']

export function fetchProviderModels(provider: { name: string; baseUrl: string }): string[] {
  const hay = `${provider.name} ${provider.baseUrl}`
  const hit = MODEL_CATALOG.find((c) => c.match.test(hay))
  return hit ? hit.models : fallbackModelCatalog
}

// ---------------- Token 统计（演示数据：精确用量 + 预估校准系数 + 最近调用明细） ----------------
const d7 = 7 * 24 * hour
export const tokenStats: TokenStatRow[] = [
  {
    model: 'deepseek-chat', provider: 'DeepSeek', sessions: 23, prompt: 184_320, completion: 41_260, cached: 52_400, calibRatio: 1.02,
    recent: [
      { time: now - 8 * min, prompt: 19_640, completion: 4_210, cached: 8_300, latencyMs: 812 },
      { time: now - 5 * hour, prompt: 12_380, completion: 2_940, cached: 4_120, latencyMs: 644 },
      { time: now - 2 * d7 / 7, prompt: 15_720, completion: 5_130, cached: 6_050, latencyMs: 937 },
      { time: now - d7 + 40 * min, prompt: 22_010, completion: 6_880, cached: 9_340, latencyMs: 1_021 },
    ],
  },
  {
    model: 'glm-4-plus', provider: '智谱 GLM', sessions: 11, prompt: 96_150, completion: 22_880, cached: 18_300, calibRatio: 0.97,
    recent: [
      { time: now - 26 * min, prompt: 9_820, completion: 2_460, cached: 2_900, latencyMs: 1_184 },
      { time: now - 9 * hour, prompt: 7_430, completion: 1_980, cached: 1_560, latencyMs: 988 },
      { time: now - 3 * d7 / 7, prompt: 11_260, completion: 3_020, cached: 2_240, latencyMs: 1_302 },
      { time: now - 5 * d7 / 7, prompt: 8_640, completion: 2_110, cached: 1_880, latencyMs: 1_046 },
    ],
  },
  {
    model: 'claude-sonnet-4', provider: 'Anthropic', sessions: 4, prompt: 51_040, completion: 12_600, cached: 9_200, calibRatio: 1.05,
    recent: [
      { time: now - 3 * hour, prompt: 13_820, completion: 3_640, cached: 3_100, latencyMs: 1_470 },
      { time: now - 4 * d7 / 7, prompt: 10_260, completion: 2_480, cached: 1_940, latencyMs: 1_288 },
      { time: now - 6 * d7 / 7, prompt: 14_480, completion: 3_920, cached: 2_680, latencyMs: 1_652 },
    ],
  },
]

export const tokenSummary = {
  today: 38_420,
  week: 408_950,
  cacheHitRate: 0.312,
}

// ---------------- 日志查询（演示数据：分布在近 7 日，含 traceId 与详情） ----------------
const d1 = 24 * hour
export const logEntries: LogEntry[] = [
  { id: 'l1', time: now - 2 * min, level: 'info', source: 'api', text: 'POST /api/sessions/s1/runs 202 Accepted（run=r7 排队中）', traceId: 'tr-9f21c4', detail: '请求体 218B，全局并发 5/8，Run 进入 pending 队列；预期调度延迟 < 200ms。' },
  { id: 'l2', time: now - 6 * min, level: 'info', source: 'agent', text: 'run=r6 completed，4 里程碑事件落库，消息 token 缓存命中', traceId: 'tr-9f21c0', detail: '耗时 8.4s；工具事件 3 个（read/write/command），prompt 缓存命中 41%，最终内容以 DB 消息快照为准。' },
  { id: 'l3', time: now - 18 * min, level: 'warn', source: 'sse', text: '订阅通道缓冲已满，主动断开慢消费者（等待 resync）', traceId: 'tr-9f21ba', detail: '订阅者 channel 缓冲 256 已满且持续 >2s 未消费；已主动断开以免阻塞 Run goroutine，客户端重连后走 resync + 消息快照对齐。' },
  { id: 'l4', time: now - 25 * min, level: 'error', source: 'mcp', text: 'postgres：连续 3 次调用失败 connection refused，进入降级', traceId: 'tr-9f21a7', detail: '传输方式 SSE（http://127.0.0.1:7788/sse）；熔断计数 3/3，健康检查间隔 60s；降级期间该 MCP 工具不再注入模型。' },
  { id: 'l5', time: now - 40 * min, level: 'info', source: 'store', text: 'SQLite 迁移完成：schema_migrations v7，integrity_check ok', traceId: 'tr-9f2192', detail: '迁移耗时 96ms；WAL checkpoint 完成，pages flushed 42；外键约束开启。' },
  { id: 'l6', time: now - 3 * hour, level: 'warn', source: 'llm', text: 'deepseek-chat 流式响应超时一次，已自动重试（第 1 次）', traceId: 'tr-9f2155', detail: '首字超时阈值 20s；重试携带 stream_options.include_usage=true；重试后首字延迟 812ms。' },
  { id: 'l7', time: now - 8 * hour, level: 'info', source: 'tools', text: '快照链校验通过：4 个快照，1 个 skipped（>5MB 大文件）', traceId: 'tr-9f2110', detail: '跳过 fixtures/large.json（7.0MB）；hash 校验 a1b2→c3d4 全部匹配，撤销链完整。' },
  { id: 'l8', time: now - 20 * hour, level: 'error', source: 'api', text: 'POST /api/runs 401 Unauthorized：api_token 过期，客户端已重放', traceId: 'tr-9f2088', detail: '令牌前缀 ak-****；过期时间 2026-10-26T03:12Z；重放后取得新令牌，请求成功转发。' },
  { id: 'l9', time: now - 2 * d1, level: 'info', source: 'agent', text: '审批等待 5min 超时：视为拒绝，原因已回传模型', traceId: 'tr-9f1f02', detail: 'write_file internal/store/migrate.go 等待审批超时；Run 继续执行，模型改用只读路径完成任务。' },
  { id: 'l10', time: now - 4 * d1, level: 'warn', source: 'tokenizer', text: 'o200k_base 缓存未命中，按需下载完成（4.1MB → data/tokenizers/）', traceId: 'tr-9f1c30', detail: '首次调用 GPT-4o 计数触发下载；下载耗时 1.9s，之后 LRU 命中率 100%。' },
  { id: 'l11', time: now - 6 * d1, level: 'error', source: 'store', text: '写入冲突：messages UNIQUE(session_id,seq) 违反，已重试成功', traceId: 'tr-9f1980', detail: '并发双写同序号；busy_timeout 重试 2 次后成功，最终 seq 由服务端单调分配。' },
  { id: 'l12', time: now - 7 * d1 + 3 * hour, level: 'info', source: 'api', text: '优雅关闭演练：Shutdown(30s) 全部 Run 收敛，落库 12 条里程碑', traceId: 'tr-9f1700', detail: '取消 2 个运行中 Run → 等待 4.2s 收敛 → 事件落库 → 关闭 DB；全程无丢消息。' },
]
