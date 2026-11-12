package service

import (
	"context"
	"errors"
	"strings"
	"sync"

	"harness-go/internal/agent"
	"harness-go/internal/apperr"
	"harness-go/internal/approval"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
	"harness-go/internal/store"
)

// ErrRunNotRunning 停止一个不在运行的 Run。
var ErrRunNotRunning = errors.New("service: Run 未在运行")

// RunServiceOptions Run 用例依赖。
type RunServiceOptions struct {
	Main       *store.MainStore
	Sessions   *store.SessionStore
	Engine     *agent.Engine
	SystemBase string
	// CoreSOPFunc 动态核心 SOP 源（agent-core）。
	//
	// 每 Run 调用一次，返回的是「SystemBase 的实质内容」——即出厂内置
	// （internal/sop/builtin/agent-core.md，embed）或运行时覆盖（data/sop/agent-core.md）。
	// 不缓存 → 界面改完**下一轮对话即生效**，无需重启/重编译。
	//
	// 为 nil 或返回空串时退回 SystemBase 静态值（降级不注入空系统提示）。
	CoreSOPFunc func() string
	// WorkDir 服务端进程工作目录（工具相对路径的实际落盘根）。
	// 仅用于在「会话未绑定工作区」时向模型如实交代边界，不冒充用户的绑定目录。
	WorkDir string
	// SOP 静态 SOP 注入（启动期固定；测试/降级用）。
	SOP []string
	// SOPFunc 动态 SOP 源（阶段 G），按**会话绑定的工作区**读取
	// <ws>/.harness/sop/*.md，返回空切片表示项目无 SOP。
	//
	// 参数 wsPath 由 RunService 在每 Run 时按当前会话传入 —— 改造前本函数不接收
	// 参数、经 host.Workspace() 读目录，而该 root 在进程启动时固化，导致切到
	// 别的项目会话仍读到旧项目 SOP 的串味问题（本次修复）。
	SOPFunc func(wsPath string) []string
	Tools   []prompt.ToolRef
	// ToolRefs 动态工具注入（优先于静态 Tools）：每轮重新取，
	// 保证 PATCH /tools/tiers 改档后「下一轮对话生效」。
	ToolRefs func() []prompt.ToolRef
	// ToolSelector 动态工具检索（D-2）：按本轮输入从索引召回 deferred 工具，
	// 命中者本轮注入全量定义。为 nil 时退化为「deferred 只进索引」（与 D 之前行为一致）。
	ToolSelector ToolSelector
	// Memory 记忆四层召回源（D-3）：每 Run 召回一次 L0/L1 常驻块与 L2/L3 检索块。
	// 为 nil 时不注入记忆（行为与 D-3 之前一致）。召回失败降级为空块，不阻断对话。
	Memory MemorySource
	// Gate 审批闸门（B-3）：引擎登记的待决项由本服务经 DecideApproval 提交，
	// 不注入时审批端点返回 unavailable（不静默成功）。
	Gate *approval.Gate
	// StrictApprovalDevice 高风险审批是否**仅限执行端设备**确认（C-7 跨端仲裁）。
	// 默认 false：只做审计不阻断（避免把浏览器端用户锁死）。
	StrictApprovalDevice bool
	// Catalog 工具风险目录（能力天花板过滤用）：nil 时无法得知工具风险级，
	// filterTools 将**保守地保留全部工具**（不能凭空判定谁危险），
	// 此时仅依赖引擎侧 ToolPolicy 兜底短路，安全性不降级。
	Catalog agent.ToolCatalog
	// Hub 设备在线判定（能力天花板用）：nil 时所有设备空间一律视为离线（失败即关闭）。
	//
	// 为什么必须注入：设备空间的能力天花板取决于「归属设备此刻是否在线」——
	// 这是运行时事实，只在设备域 Hub 里。nil Hub 绝不等于「全部在线」，
	// 而是「无法确认在線」→ 按离线处理（fail-closed），宁可降级为零工具。
	Hub HubOnline
}

// RunService Run 用例：创建（异步执行）/ 停止 / 查询 / 启动恢复。
type RunService struct {
	main       *store.MainStore
	sessions   *store.SessionStore
	engine     *agent.Engine
	systemBase string
	// coreSOPFunc 动态核心 SOP 源（agent-core）；nil 表示用静态 systemBase。
	coreSOPFunc func() string
	workDir     string
	sop         []string
	sopFunc     func(wsPath string) []string
	tools       []prompt.ToolRef
	toolRefs    func() []prompt.ToolRef
	gate        *approval.Gate
	// toolSelector 动态工具检索（D-2）；nil 表示不做动态检索。
	toolSelector ToolSelector
	// memory 记忆四层召回源（D-3）；nil 表示不注入记忆。
	memory MemorySource
	// strictApproval 跨端审批仲裁是否强制「执行端设备确认」（C-7）。
	strictApproval bool
	// catalog 工具风险目录（能力天花板过滤用）；nil 时 filterTools 保留全部工具。
	catalog agent.ToolCatalog
	// hub 设备在线判定；nil 时设备空间一律按离线处理（fail-closed）。
	hub HubOnline

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	// runDevices 设备 → 进行中 Run 的反向索引（R3 设备离线自动中断）。
	//
	// 为什么需要：cancels 是 runID → cancel 单向表，设备离线时无法反查
	// 「这台设备上还跑着哪些 Run」。缺了它就只能全表扫描 + 逐条查库判断归属。
	//
	// 只登记**执行面归属该设备**的 Run：服务端内置空间（ExecutionDeviceID 为空）
	// 的 Run 永不进索引 —— 服务端恒在线，不该被任何设备离线事件波及。
	runDevices map[string]map[string]struct{}
	// guidanceChans 运行中引导通道（§4.2）：runID → chan *model.Message（容量 16）。
	// 与 cancels 同生命周期：Run 创建时注册、终态注销；Run 已结束的引导返回 409。
	guidanceChans sync.Map
}

// NewRunService 创建 Run 用例。
func NewRunService(opts RunServiceOptions) *RunService {
	return &RunService{
		main:           opts.Main,
		sessions:       opts.Sessions,
		engine:         opts.Engine,
		systemBase:     opts.SystemBase,
		coreSOPFunc:    opts.CoreSOPFunc,
		workDir:        opts.WorkDir,
		sop:            opts.SOP,
		sopFunc:        opts.SOPFunc,
		tools:          opts.Tools,
		toolRefs:       opts.ToolRefs,
		gate:           opts.Gate,
		toolSelector:   opts.ToolSelector,
		memory:         opts.Memory,
		strictApproval: opts.StrictApprovalDevice,
		catalog:        opts.Catalog,
		hub:            opts.Hub,
		cancels:        make(map[string]context.CancelFunc),
	}
}

// SetTraceSink 后置注入审计日志端口（启动装配期调用一次）。
//
// 后置注入的原因：审计日志库族依赖 settings 服务，而引擎在 settings 之前构造。
// 禁止在 Run 运行期间热替换（调用方须保证只在启动阶段调用）。
func (s *RunService) SetTraceSink(sink agent.TraceSink) {
	if s == nil || s.engine == nil {
		return
	}
	s.engine.SetTraceSink(sink)
}

// SetHub 后置注入设备在线判定（启动装配期调用一次）。
//
// 为什么必须后置注入：设备域 Hub 的构造依赖 logger / 网关配置，晚于 RunService 装配；
// 若强行走构造注入就要重排 main.go 的装配顺序，牵连更大。
//
// 🚨 未注入（nil）时所有设备空间一律按离线处理（fail-closed → 零工具），
// 这是安全的降级方向；绝不会因为「不知道设备在不在线」就放行工具。
func (s *RunService) SetHub(hub HubOnline) {
	if s == nil {
		return
	}
	s.hub = hub
}

// currentTools 返回本轮应注入的工具定义（动态优先）。
func (s *RunService) currentTools() []prompt.ToolRef {
	if s.toolRefs != nil {
		return s.toolRefs()
	}
	return s.tools
}

// executionDeviceID 解析本次 Run 的**执行面归属设备**（D7 执行面归属铁律）。
//
// 语义：工具必须落在「会话绑定工作空间所归属的那台设备」上，而不是任意在线设备，
// 更不是服务端本地。返回空串表示「服务端内置空间」，允许本地执行。
//
// 为什么必须逐层透传而不是让派发器自己挑：`Dispatcher.candidates` 在未指定
// DeviceID 时会退化为「全部在线设备按 ID 排序取首个」——会话绑的是手机 A，
// 写文件却可能落到桌面端 B 上。这是错误执行而非功能缺失。
func (s *RunService) executionDeviceID(ctx context.Context, sess *model.Session) string {
	if s == nil || s.main == nil || sess == nil {
		return ""
	}
	wsID := strings.TrimSpace(sess.WorkspaceID)
	if wsID == "" {
		return ""
	}
	ws, err := s.main.Workspaces().Get(ctx, wsID)
	if err != nil {
		return ""
	}
	// 内置服务端空间：执行面就是服务端本地。
	if ws.ID == store.BuiltinWorkspaceServerID {
		return ""
	}
	return strings.TrimSpace(ws.DeviceID)
}

// resolveToolPolicy 判定该会话的**工具能力策略**——工作空间能力天花板的唯一裁决入口。
//
// 【唯一入口铁律】工作空间能力判定**只在本函数实现**，其他任何位置（引擎、API 层、
// 前端）都不得重复实现第二套规则，否则两处漂移会直接变成越权漏洞。
// 依据：docs/工作空间模型与三端能力收口协议.md §4.1。
//
// 规则（fail-closed：查不到一律从严）：
//   - session.WorkspaceID 为空 → none（纯对话，无执行宿主）；
//   - workspace 查不到（悬空外键/已删除）→ none；
//   - workspace 是内置服务端空间（ws-server）→ full（服务端自身恒在线，D8）；
//   - workspace.DeviceID 为空（非内置）→ none（未绑定设备，D1）；
//   - 归属设备已删除 / 已解绑（revoked）→ none（D4：空间保留但无宿主）；
//   - 归属设备不在线 → none（D3：**绝不降级到服务端本地执行**）；
//   - workspace.Kind == mobile → readonly（移动端可对话+图片识别，禁写禁命令）；
//   - kind 为 server/desktop/browser 且设备在线 → full。
//
// 第二个返回值为拒绝文案，用于 Engine 短路时展示给模型。
func (s *RunService) resolveToolPolicy(ctx context.Context, sess *model.Session) (agent.ToolPolicy, string) {
	if sess == nil {
		return agent.ToolPolicyReadonly, policyReasonNoWorkspace
	}
	wsID := strings.TrimSpace(sess.WorkspaceID)
	if wsID == "" {
		// 纯对话会话（无 workspace_id）：协议 §3.3 的「无执行面」。
		//
		// 给 readonly 而非 none 的理由：只读工具（read_file / echo / 检索）不需要
		// 执行宿主参与，纯对话里读代码上下文是正常刚需；而 write / command 必须
		// 有设备或服务端空间承接，无归属时一律拒绝。
		// 这与旧 DenyTools（只拦 command+danger）的差别在于：**write 现在也被拦下**，
		// 补上了「纯对话能偷偷改文件」的漏洞。
		return agent.ToolPolicyReadonly, policyReasonNoWorkspace
	}
	if s.main == nil {
		return agent.ToolPolicyReadonly, policyReasonNoWorkspace
	}
	ws, err := s.main.Workspaces().Get(ctx, wsID)
	if err != nil {
		// 悬空引用：宁可拒绝也不放宽（fail-closed）。
		return agent.ToolPolicyNone, policyReasonUnbound
	}
	if !ws.Kind.Valid() {
		return agent.ToolPolicyNone, policyReasonUnbound
	}
	// 内置服务端空间：恒在线（它就是服务端自身），不参与设备在线判定。
	if ws.ID == store.BuiltinWorkspaceServerID {
		if ws.Kind == model.WorkspaceKindMobile {
			return agent.ToolPolicyReadonly, policyReasonMobile
		}
		// 【R1b 纯对话无工具调用】绑服务端空间但**无项目** = 纯对话。
		//
		// 语义：纯对话只是「聊天 + 看历史」，不应具备在服务端宿主机上执行命令/写文件的能力。
		// 这与「级联删除不得把会话迁到 ws-server（防提权）」的安全铁律同源：
		// 若迁过去的会话拿到 full，等于给了绕过设备归属的后门。
		//
		// 有项目则维持 full：项目意味着用户已明确声明工作目录与执行意图。
		if strings.TrimSpace(sess.ProjectID) == "" {
			return agent.ToolPolicyReadonly, policyReasonPureChat
		}
		return agent.ToolPolicyFull, ""
	}
	// 非内置空间必须有可用设备宿主，否则任何工具都只能落到服务端本地执行（D8 明令禁止）。
	if strings.TrimSpace(ws.DeviceID) == "" {
		return agent.ToolPolicyNone, policyReasonUnbound
	}
	dev, err := s.main.Devices().Get(ctx, ws.DeviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return agent.ToolPolicyNone, policyReasonDeviceMissing
		}
		return agent.ToolPolicyNone, policyReasonUnbound
	}
	if dev.RevokedAt != nil {
		return agent.ToolPolicyNone, policyReasonDeviceRevoked
	}
	if s.hub == nil || !s.hub.Online(ws.DeviceID) {
		return agent.ToolPolicyNone, policyReasonOffline
	}
	if ws.Kind == model.WorkspaceKindMobile {
		return agent.ToolPolicyReadonly, policyReasonMobile
	}
	return agent.ToolPolicyFull, ""
}

// filterTools 按能力天花板过滤工具注入列表（协议 §4.1）。
//
// 为什么在注入阶段就过滤：让模型**根本看不到**被禁工具的 schema，
// 比"事后拒绝"更省 token、更少无效轮次；引擎侧的 ToolPolicy 短路是兜底
// （防止历史上下文或已被删除的工具名仍被模型尝试调用）。
//
// 保守原则：catalog 为 nil（无法得知风险级）时**不过滤**，宁可依赖引擎兜底，
// 也不凭工具名猜测风险——猜错会把安全工具的注入也砍掉，损害正常对话。
//
// 🚨 policy == none 时不依赖 catalog：那是「全部剔除」的绝对语义，
// 连风险级都不需要知道，因此即使 catalog 为 nil 也必须全量清空
// （否则「零工具」契约在未装配 catalog 的部署下会静默失效）。
func (s *RunService) filterTools(refs []prompt.ToolRef, policy agent.ToolPolicy) []prompt.ToolRef {
	if len(refs) == 0 {
		return refs
	}
	// none：全部剔除，与 catalog 是否装配无关。
	if policy == agent.ToolPolicyNone {
		return nil
	}
	if policy == agent.ToolPolicyFull && s.catalog == nil {
		return refs
	}
	if s.catalog == nil {
		return refs
	}
	out := make([]prompt.ToolRef, 0, len(refs))
	for _, ref := range refs {
		risk, ok := s.catalog.Risk(ref.Name)
		if !ok {
			// 风险级未知：保守保留（catalog 未覆盖 ≠ 危险）。
			out = append(out, ref)
			continue
		}
		if !policy.Allows(risk) {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// currentSOP 返回本轮 SOP 全量注入（动态源优先；空 = 项目未配置 SOP）。
//
// wsPath 传当前会话绑定的工作区，保证「读的是本会话所属项目」的 SOP ——
// 不再依赖进程启动时固化的 host root（原串味问题根因）。
func (s *RunService) currentSOP(wsPath string) []string {
	if s.sopFunc != nil {
		return s.sopFunc(wsPath)
	}
	return s.sop
}

// selectTools 动态检索本轮需注入全量定义的 deferred 工具（D-2）。
//
// 返回 nil 表示「不做动态提升」—— 此时 Builder 的行为与 D 之前完全一致
// （deferred 只进索引），保证未装配检索时零回归。
func (s *RunService) selectTools(ctx context.Context, userInput string) []string {
	if s.toolSelector == nil {
		return nil
	}
	return s.toolSelector.SelectForPrompt(ctx, userInput, s.currentTools())
}

// recallMemory 记忆召回（D-3）。失败返回空块并继续（不阻断对话），
// 错误经返回值上抛的调用方（CreateRunAs）直接丢弃 —— 与 TouchMessage 摘要回写同口径。
func (s *RunService) recallMemory(ctx context.Context, userInput string) (core, tail string) {
	if s.memory == nil || strings.TrimSpace(userInput) == "" {
		return "", ""
	}
	core, tail, err := s.memory.RecallBlocks(ctx, userInput)
	if err != nil {
		return "", "" // 召回降级：空块不阻断 Run（MemorySource 契约）
	}
	return core, tail
}

// CreateRun 写入用户消息 + 创建 Run(pending) + 异步执行。
//
// 返回后 Run 可能仍在 pending/running —— 调用方拿 run_id 去订阅 SSE。
func (s *RunService) CreateRun(ctx context.Context, sessionID, userInput, modelRef string) (*model.Run, error) {
	return s.CreateRunAs(ctx, sessionID, userInput, modelRef, "")
}

// CreateRunAs 同 CreateRun，但显式声明**执行端设备**（阶段 C-6）。
//
// deviceID 非空表示该 Run 由端侧发起/驱动：引擎会以它为会话租约持有者，
// 抢占旧端（epoch+1）并推送 lease.revoked。空串 = 服务端本地执行面。
//
// 只做传递不做校验：设备合法性由 WS 网关/执行面路由保证，这里校验会引入
// 「服务端 Run 被设备表状态卡住」的隐性耦合。
func (s *RunService) CreateRunAs(ctx context.Context, sessionID, userInput, modelRef, deviceID string) (*model.Run, error) {
	return s.CreateRunWithSystem(ctx, sessionID, userInput, modelRef, deviceID, "")
}

// CreateRunWithSystem 同 CreateRunAs，但允许覆盖本轮 SystemBase（P1 OpenAI 兼容端点：
// 客户端 system 消息优先）。空串 = 服务默认系统提示（行为不变）。
func (s *RunService) CreateRunWithSystem(ctx context.Context, sessionID, userInput, modelRef, deviceID, systemBase string) (*model.Run, error) {
	if userInput == "" {
		return nil, invalidArg("用户输入不能为空")
	}

	// 校验会话存在（只读主库）
	sess, err := s.main.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeSessionNotFound, "会话不存在")
	}
	// 【R2 对话准入·物理层拦截】归属设备离线 → 拒绝发起 Run。
	//
	// 为什么必须在 sess.Acquire 之前：一旦走到下方便会写用户消息、建会话库文件、
	// 再调模型。离线时这些都该**零发生**——消息不落库、模型不被调用、token 不消耗。
	//
	// 这是与 resolveToolPolicy（工具准入/上下文层）**不同层次**的第二道闸：
	//   - 本层挡「发起」：设备离线时整个对话不可开始，只能查看历史；
	//   - 上层挡「跑一半掉线」：Run 进行中设备离线，工具被拒 → 由 R3 自动中断。
	// 两层是纵深防御，不可互相替代。
	//
	// 判定复用 resolveToolPolicy 的权威结果（唯一入口铁律，不写第二套规则）：
	//   - 仅当策略为 none 且原因是「设备离线」时才拦。其余 none 情形
	//     （未绑定/设备已删/已解绑）沿用既有行为——它们不是「在线状态」问题，
	//     强行拦会改变历史会话的可访问性，超出本次需求范围。
	if policy, reason := s.resolveToolPolicy(ctx, sess); policy == agent.ToolPolicyNone && reason == policyReasonOffline {
		return nil, apperr.New(apperr.CodeDeviceOffline, "%s", reason)
	}
	// 自动标题（对齐桌面端）：会话仍是默认标题时，用首条用户消息前 24 字生成；
	// 用户已手动重命名则不覆盖。失败不阻断对话（标题是体验项不是正确性项）。
	if title := deriveSessionTitle(sess.Title, userInput); title != "" {
		if rErr := s.main.Sessions().Rename(ctx, sessionID, title); rErr == nil {
			sess.Title = title
		}
	}
	if modelRef == "" {
		modelRef = sess.ModelRef
	}
	if modelRef == "" {
		// 未指定模型时不阻断对话：折算为 "default"，由 ProviderResolver 按
		// agent.default_model（设置 → 能力模型槽位 chat）→ 首个启用厂商的首个模型解析；
		// 全都不可用才回退 fake provider（单二进制开箱可用），回退时会打 Warn 日志。
		modelRef = "default"
	}

	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// ① 用户消息落库（会话库）
	userMsg := &model.Message{
		ID:        newID(),
		Role:      model.RoleUser,
		Content:   userInput,
		CreatedAt: store.NowMillis(),
	}
	if err := sdb.Messages().Append(ctx, userMsg); err != nil {
		return nil, err
	}
	// ② 摘要回写主库（最终一致）
	_ = s.main.Sessions().TouchMessage(ctx, sessionID, userMsg.CreatedAt)

	// ③ 创建 Run（pending）
	executor := model.ExecutorServer
	if deviceID != "" {
		executor = model.ExecutorDevice
	}
	run := &model.Run{
		ID:        newID(),
		Status:    model.RunPending,
		ModelRef:  modelRef,
		Executor:  executor,
		DeviceID:  deviceID,
		CreatedAt: store.NowMillis(),
	}
	if err := sdb.Runs().Create(ctx, run); err != nil {
		return nil, err
	}
	// 主库投影跟随真相（单一真相铁律）：Run 一创建就写 last_run_status=pending。
	//
	// sessions.last_run_status 是 session.db.runs.status 的**投影**，此前只在 Run 终态时
	// 由 SummaryWriter.AddTokensAndStatus 回写 → Run 运行期间主库仍挂着上一轮的 completed，
	// 造成两处实测缺陷：① RecoverInterrupted 按主库筛选时漏扫，进程重启后卡死的 running
	// Run 永远不会被置 interrupted；② 会话列表的「运行中」徽标判据同样永不命中。
	// 这里补写一次即可让投影全程跟随真相，无需新增接口/表/字段。
	// 失败不阻断 Run（投影是展示项，真相在会话库），与 TouchMessage 同口径。
	_ = s.main.Sessions().AddTokensAndStatus(ctx, sessionID, 0, model.RunPending)

	// ④ 记忆召回（D-3）：每 Run 一次，core 进稳定前缀、tail 置尾。
	// 召回失败降级为空块（记忆是优化项，绝不阻断对话 —— 与工具检索同口径）。
	memCore, memTail := s.recallMemory(ctx, userInput)

	// ⑤ 异步执行：脱离 HTTP 请求 ctx（否则请求一结束 Run 就被取消）
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// 执行面归属（D7）提前解析：既用于注册反向索引（R3），也用于装配 RunRequest。
	// 空串 = 服务端内置空间（ws-server），允许本地执行。
	execDeviceID := s.executionDeviceID(ctx, sess)
	s.registerCancelForDevice(run.ID, execDeviceID, cancel)

	system := s.systemBase
	if strings.TrimSpace(systemBase) != "" {
		system = systemBase
	}
	// 核心 SOP（agent-core）动态源：每 Run 取一次 → 界面改完下一轮即生效。
	// 返回空串表示降级（服务未装配），此时保留静态提示，绝不注入空系统提示。
	if s.coreSOPFunc != nil {
		if core := strings.TrimSpace(s.coreSOPFunc()); core != "" {
			system = core
		}
	}
	// 工作区事实按会话注入：绑定/未绑定如实分叉，杜绝「未绑定却说自己在工作区内」的幻觉。
	system = ComposeSystemBase(system, sess.ProjectDir, s.workDir)
	// 能力天花板（协议 §4.1）：唯一入口在此，引擎只消费结果。
	// 零工具策略下不注入任何工具（让模型根本看不到），且引擎侧做兜底短路拒绝。
	toolPolicy, policyReason := s.resolveToolPolicy(ctx, sess)
	req := agent.RunRequest{
		RunID:           run.ID,
		SessionID:       sessionID,
		UserInput:       userInput,
		ModelRef:        modelRef,
		AccessMode:      approval.AccessMode(normalizeAccessMode(sess.AccessMode)),
		SystemBase:      system,
		SOP:             s.currentSOP(sess.ProjectDir),
		ProjectSummary:  "",
		Tools:           s.filterTools(s.currentTools(), toolPolicy),
		ActiveTools:     s.selectTools(ctx, userInput),
		MemoryCore:      memCore,
		MemoryTail:      memTail,
		LeaseHolder:     deviceID, // C-6：空串 = 服务端本地执行面
		RequesterDevice: deviceID, // C-7：审批跨端仲裁的发起端来源
		ToolPolicy:      toolPolicy,
		PolicyReason:    policyReason,
		// 执行面归属（D7）：工具必须在**会话绑定的设备**上执行，绝不落到服务端本地。
		// 空串 = 服务端内置空间（ws-server），允许本地执行。
		ExecutionDeviceID: execDeviceID,
	}
	// 运行中引导通道（§4.2）：随 Run 注册/注销，Guidance 端点推送、引擎轮次边界消费
	guidanceCh := make(chan *model.Message, 16)
	s.guidanceChans.Store(run.ID, guidanceCh)
	req.GuidanceCh = guidanceCh
	go func() {
		defer func() {
			s.unregisterCancel(run.ID)
			s.guidanceChans.Delete(run.ID)
			cancel()
		}()
		// 执行结果以数据库为准，这里只记录日志（由上层 logger 接管前先忽略）
		_ = s.engine.Execute(runCtx, req)
	}()

	return run, nil
}

// Guidance 运行中引导（需求 §4.2，对齐桌面端「待执行列表 → 引导」）。
//
// 用户消息**立即落库**（多端立即可见），并推送给引擎在**轮次边界**回灌上下文——
// 当前流结束后模型即看到该引导。Run 已结束（通道已注销）返回 409；
// 引擎通道缓冲满（16）时消息不丢（已落库），只是本轮不再回灌。
func (s *RunService) Guidance(ctx context.Context, sessionID, runID, input string) (*model.Message, error) {
	text := strings.TrimSpace(input)
	if text == "" {
		return nil, invalidArg("引导内容不能为空")
	}
	v, ok := s.guidanceChans.Load(runID)
	if !ok {
		return nil, apperr.New(apperr.CodeConflict, "Run 不在运行中，无法引导")
	}
	ch, _ := v.(chan *model.Message)
	if ch == nil {
		return nil, apperr.New(apperr.CodeConflict, "Run 不在运行中，无法引导")
	}
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	msg := &model.Message{
		ID:        newID(),
		RunID:     runID,
		Role:      model.RoleUser,
		Content:   text,
		CreatedAt: store.NowMillis(),
	}
	if err := sdb.Messages().Append(ctx, msg); err != nil {
		return nil, err
	}
	// 摘要回写（最终一致，失败不影响引导本身）
	_ = s.main.Sessions().TouchMessage(ctx, sessionID, msg.CreatedAt)
	select {
	case ch <- msg:
	default:
		// 引导密度超上限：消息已落库不丢失，仅本轮上下文不回灌（可观测，不打断对话）
	}
	return msg, nil
}

// StopRun 停止正在运行的 Run（ctx 取消 → 引擎迁移到 cancelled）。
func (s *RunService) StopRun(_ context.Context, runID string) error {
	s.mu.Lock()
	cancel, ok := s.cancels[runID]
	s.mu.Unlock()
	if !ok {
		return apperr.New(apperr.CodeRunStateConflict, "Run 未在运行")
	}
	cancel()
	return nil
}

// GetRun 查询 Run 状态。
func (s *RunService) GetRun(ctx context.Context, sessionID, runID string) (*model.Run, error) {
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	run, err := sdb.Runs().Get(ctx, runID)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeRunNotFound, "Run 不存在")
	}
	return run, nil
}

// ToolEvents 查询某 Run 的工具事件（UI 回放）。
func (s *RunService) ToolEvents(ctx context.Context, sessionID, runID string) ([]model.ToolEvent, error) {
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return sdb.ToolEvents().ListByRun(ctx, runID)
}

// ActiveRun 返回某会话当前「进行中」的 Run（无则 nil）。
//
// 用途：前端刷新（F5）后本地流式态全部丢失，但服务端 Run 仍在跑。此用例是
// UI 重建订阅的唯一事实来源——**无进行中 Run 不是错误，返回 (nil, nil)**，
// 与项目既有语义铁律「无租约不是 404」同口径（见 SessionLeaseDTO 注释）。
//
// 取 created_at 最新的一条：同一会话理论上不该并行多个未完成 Run（引擎串行），
// 但进程重启/异常残留可能留下多条，此时以最新者为准（旧的已被 FailOpenRuns 收口）。
func (s *RunService) ActiveRun(ctx context.Context, sessionID string) (*model.Run, error) {
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	runs, err := sdb.Runs().ListUnfinished(ctx)
	if err != nil {
		return nil, err
	}
	var latest *model.Run
	for i := range runs {
		if latest == nil || runs[i].CreatedAt >= latest.CreatedAt {
			latest = &runs[i]
		}
	}
	return latest, nil
}

// RunningCount 当前在运行的 Run 数（维护接口用）。
func (s *RunService) RunningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cancels)
}

// DecideApproval 提交审批决策（B-3：审批决策端点）。
//
// 校验顺序：决策合法 → Run 归属与状态（waiting_approval）→ 审批项存在 → Gate 命中。
// 「本会话允许」额外写入会话级审批规则；任何一步不满足都返回明确错误码，绝不静默成功。
func (s *RunService) DecideApproval(ctx context.Context, sessionID, runID, approvalID, decision, reason string) error {
	return s.DecideApprovalFromDevice(ctx, sessionID, runID, approvalID, decision, reason, "")
}

// DecideApprovalFromDevice 带决策端身份的审批决策（阶段 C-7 跨端仲裁）。
//
// deviceID 为空 = 浏览器/本地管理端。strict_approval_device=true 且该审批被绑定到
// 执行端设备（danger 风险 + 端侧发起）时，非该设备的决策一律 403 approval.device_mismatch；
// 仲裁在 Gate 提交前完成，被拒时待决项**不被消费**（审批仍可被正确设备确认）。
func (s *RunService) DecideApprovalFromDevice(ctx context.Context, sessionID, runID, approvalID,
	decision, reason, deviceID string) error {
	if s.gate == nil {
		return apperr.New(apperr.CodeUnavailable, "审批闸门未启用")
	}
	var d approval.Decision
	switch approval.Decision(decision) {
	case approval.AllowOnce, approval.AllowSession, approval.Deny:
		d = approval.Decision(decision)
	default:
		return invalidArg("decision 只能是 allow_once / allow_session / deny")
	}
	if approvalID == "" {
		return invalidArg("approval_id 不能为空")
	}

	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return err
	}
	run, err := sdb.Runs().Get(ctx, runID)
	if err != nil {
		return wrapStoreErr(err, apperr.CodeRunNotFound, "Run 不存在")
	}
	if run.Status != model.RunWaitingApproval {
		return apperr.New(apperr.CodeRunStateConflict,
			"Run 当前状态为 %s，不在等待审批", string(run.Status))
	}
	ev, err := sdb.ToolEvents().GetByRun(ctx, runID, approvalID)
	if err != nil {
		return wrapStoreErr(err, apperr.CodeApprovalNotFound, "审批请求不存在")
	}

	// 跨端仲裁（C-7）：strict 模式下非执行端设备的决策直接 403，且**不消费**待决项。
	chk := s.gate.CheckDevice(approvalID, deviceID, s.strictApproval)
	if !chk.Known {
		return apperr.New(apperr.CodeApprovalNotFound, "审批请求不存在或已处理")
	}
	if !chk.Allowed {
		return apperr.New(apperr.CodeApprovalDeviceMismatch, "%s", chk.Reason)
	}

	// Gate 提交：未命中说明已被处理或已超时（超时已按拒绝落库）
	if !s.gate.Resolve(approvalID, d) {
		return apperr.New(apperr.CodeApprovalNotFound, "审批请求不存在或已处理")
	}

	// 「本会话允许」→ 写会话级规则（后续同类调用免审）
	if d == approval.AllowSession {
		if err := sdb.ApprovalRules().Add(ctx, ev.ToolName, ""); err != nil {
			return err
		}
	}
	// 审计落库（best-effort：决策已生效，审计失败不回滚决策）
	// C-7：同时记「谁发起 / 谁决策」，供跨端审计与 UI 回放。
	_ = sdb.Approvals().DecideWithDevices(ctx, store.ApprovalDecision{
		ID:                approvalID,
		RunID:             runID,
		ToolEventID:       ev.ID,
		Decision:          string(d),
		Reason:            reason,
		RequestedByDevice: chk.RequestedByDevice,
		DecidedByDevice:   deviceID,
	})
	return nil
}

// RecoverInterrupted 启动恢复：把未完成 Run 置 interrupted（ADR-009 不做断点续跑）。
//
// 只扫主库里 last_run_status 非终态的会话（**不全量扫会话库**）。
func (s *RunService) RecoverInterrupted(ctx context.Context) (int, error) {
	const pageSize = 200
	sessions, err := s.main.Sessions().List(ctx, pageSize, 0)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, sess := range sessions {
		if !sess.LastRunStatus.IsUnfinished() {
			continue
		}
		sdb, err := s.sessions.Acquire(ctx, sess.ID)
		if err != nil {
			continue // 单个会话库打不开不应阻塞启动
		}
		n, err := sdb.Runs().FailOpenRuns(ctx, "process_restart")
		if err != nil {
			continue
		}
		total += n
	}
	return total, nil
}

func (s *RunService) registerCancel(runID string, cancel context.CancelFunc) {
	s.registerCancelForDevice(runID, "", cancel)
}

// registerCancelForDevice 注册 Run 的取消句柄，并按执行面设备建立反向索引（R3）。
//
// deviceID 为空表示服务端内置空间的 Run：登记 cancel 但**不进反向索引**，
// 这样任何设备离线事件都不会误伤服务端会话。
func (s *RunService) registerCancelForDevice(runID, deviceID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels[runID] = cancel
	if deviceID == "" {
		return
	}
	if s.runDevices == nil {
		s.runDevices = make(map[string]map[string]struct{})
	}
	set := s.runDevices[deviceID]
	if set == nil {
		set = make(map[string]struct{})
		s.runDevices[deviceID] = set
	}
	set[runID] = struct{}{}
}

func (s *RunService) unregisterCancel(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancels, runID)
	// 同步清理反向索引，避免设备 → Run 映射无限增长（内存泄漏）。
	for dev, set := range s.runDevices {
		delete(set, runID)
		if len(set) == 0 {
			delete(s.runDevices, dev)
		}
	}
}

// CancelByDevice 中断指定设备上所有进行中的 Run（R3 设备离线自动中断）。
//
// 语义：设备离线意味着本会话已无法在其上执行任何端侧操作，原地空等只会
// 让用户看到一串工具失败。这里直接取消，由引擎把 Run 迁移到 cancelled 终态。
//
// 不中断哪些：服务端内置空间的 Run（登记时 deviceID 为空，不在索引里），
// 以及执行面归属其它设备的 Run。返回被中断的 Run 数量，便于调用方打日志。
func (s *RunService) CancelByDevice(deviceID string) int {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return 0
	}
	s.mu.Lock()
	// 先快照并摘除索引条目，再在锁外执行 cancel：
	// cancel 会触发引擎回调并可能重入本结构，持锁调用有死锁风险。
	set := s.runDevices[deviceID]
	if len(set) == 0 {
		s.mu.Unlock()
		return 0
	}
	runIDs := make([]string, 0, len(set))
	cancels := make([]context.CancelFunc, 0, len(set))
	for runID := range set {
		runIDs = append(runIDs, runID)
		if c, ok := s.cancels[runID]; ok {
			cancels = append(cancels, c)
		}
	}
	delete(s.runDevices, deviceID)
	s.mu.Unlock()

	for _, c := range cancels {
		c()
	}
	return len(runIDs)
}

// defaultSessionTitle 会话默认标题（session_service.Create 兜底值，与前端一致）。
const defaultSessionTitle = "新对话"

// deriveSessionTitle 首条消息自动生成会话标题：仅当会话还是默认标题（空/新对话）时，
// 取输入前 24 字（与桌面端 commands.rs 口径一致）；已有自定义标题返回空串（不覆盖）。
func deriveSessionTitle(current, userInput string) string {
	cur := strings.TrimSpace(current)
	if cur != "" && cur != defaultSessionTitle {
		return ""
	}
	input := strings.TrimSpace(userInput)
	if input == "" {
		return ""
	}
	runes := []rune(input)
	if len(runes) > 24 {
		runes = runes[:24]
	}
	title := strings.TrimSpace(string(runes))
	if title == "" {
		return ""
	}
	return title
}

func normalizeAccessMode(mode string) string {
	switch mode {
	case string(approval.ModeFull):
		return string(approval.ModeFull)
	default:
		return string(approval.ModeConfirm)
	}
}

// normalizeAccessModeStrict 严格归一化访问模式：仅接受 confirm/full，
// 其余值返回空串（调用方以 400 拒绝，避免非法值静默降级为 confirm）。
func normalizeAccessModeStrict(mode string) string {
	switch strings.TrimSpace(mode) {
	case string(approval.ModeConfirm):
		return string(approval.ModeConfirm)
	case string(approval.ModeFull):
		return string(approval.ModeFull)
	default:
		return ""
	}
}
