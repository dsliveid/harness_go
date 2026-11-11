// Command harness-go 是 harness_go 的单二进制服务端入口。
//
// 组装顺序（docs/架构设计文档.md §9.3）：
//
//	config → data 目录可写 → store(主库迁移) → 清理 blob 临时文件
//	→ events 总线 → tokenizer → prompt builder → LLM provider
//	→ agent 引擎 → 各 service → HTTP 路由 → 监听
//
// 全部依赖**显式构造器注入**（无 DI 框架，ADR-008）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"harness-go/internal/agent"
	"harness-go/internal/api"
	"harness-go/internal/approval"
	"harness-go/internal/auth"
	"harness-go/internal/config"
	"harness-go/internal/device"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/logvault"
	"harness-go/internal/market"
	"harness-go/internal/mcp"
	"harness-go/internal/memory"
	"harness-go/internal/observ"
	"harness-go/internal/prompt"
	"harness-go/internal/retrieval"
	"harness-go/internal/secret"
	"harness-go/internal/service"
	"harness-go/internal/snapshot"
	"harness-go/internal/store"
	"harness-go/internal/tokenizer"
	"harness-go/internal/tools"
	"harness-go/internal/tools/hostimpl"
	"harness-go/internal/tools/loader"
	"harness-go/internal/tools/registry"
	"harness-go/internal/tools/router"
	"harness-go/pkg/toolapi"
)

// 由构建时 ldflags 注入，见 Makefile。
var (
	version   = "0.0.0-dev"
	commit    = "none"
	buildTime = "unknown"
)

// defaultSystemBase 默认系统提示兜底。
//
// 注意：Agent 真正的「出厂内置 SOP」已外化到 internal/sop/builtin/agent-core.md
// （go:embed），运行期由 SOPService.CoreText() 提供，可被 data/sop/agent-core.md
// 覆盖并**热生效**。本常量仅在 SOP 服务未装配时作为兜底（测试/降级路径），
// 内容与 agent-core.md 保持一致，避免两条路径行为分叉。
//
// 这里只写**与运行时无关**的角色与规则。会话是否绑定工作区属于运行时事实，
// 由 service.ComposeSystemBase 按会话注入——不要在此断言「运行在用户的工作区内」，
// 否则未绑定工作区的纯对话会话会出现「顶栏说未绑定、模型却自称在工作区内」的自相矛盾。
const defaultSystemBase = `你是 harness-go 的编码助手。

规则：
1. 修改文件前先读取确认内容，不要凭猜测改动。
2. 危险操作（删除、覆盖、执行命令）需用户确认。
3. 回答简洁，先给结论再给细节。`

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness-go: %v\n", err)
		os.Exit(1)
	}
}

// isLoopbackOnly 报告监听地址是否只绑定回环网卡（即局域网内其它设备无法访问）。
//
// 判定口径（与 net.Listen 的实际行为对齐）：
//   - 空 host（`":5611"`）→ 绑定全部网卡，**不是**仅回环；
//   - 0.0.0.0 / [::] / 具体网卡 IP → **不是**仅回环；
//   - 127.0.0.1 / ::1 / localhost → 仅回环。
//
// 地址非法时保守返回 true（宁可多提示一次，也不让用户以为已经对外开放）。
func isLoopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// 允许只写端口（如 "5611"）：此时 net.Listen 等同 ":5611"，绑全部网卡。
		// ⚠️ 必须先判空：net.LookupPort("tcp", "") 会返回 0, nil（视作合法服务名），
		// 不加这层判断会把空地址误判成「已对外开放」——单测 TestIsLoopbackOnlyGarbage 守这条线。
		if strings.TrimSpace(addr) != "" {
			if _, pErr := net.LookupPort("tcp", addr); pErr == nil {
				return false
			}
		}
		return true
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false // ":5611" 绑定所有网卡
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true // 主机名：无法静态判定，保守按仅回环提示
	}
	return ip.IsLoopback()
}

func run() error {
	cfg, err := config.Load(config.Options{Args: os.Args[1:]})
	if err != nil {
		return err
	}

	dataDir, err := cfg.EnsureDataDir()
	if err != nil {
		return err
	}

	// 日志双写：stdout + data/app.log（`GET /api/v1/logs` 读取该文件族）。
	//
	// 轮转：按大小切分 app.log → app.log.1 .. app.log.N。
	// 为什么必须轮转：日志无上限增长会让「读尾行」退化为扫整个大文件的线性 IO，
	// 且最终写满磁盘；20MB/天 × 30 天 = 600MB 单文件时单次查询就要秒级。
	// 打开失败不阻断启动（只退化为 stdout），但必须打印告警。
	logWriter := io.Writer(os.Stdout)
	if rot, rErr := observ.NewRotationWriter(observ.RotationOptions{
		Path:    filepath.Join(dataDir, "app.log"),
		MaxMB:   cfg.Log.RotationMB,
		MaxKeep: cfg.Log.RotationKeep,
	}); rErr == nil {
		logWriter = io.MultiWriter(os.Stdout, rot)
		defer func() { _ = rot.Close() }()
	} else {
		fmt.Fprintf(os.Stderr, "harness-go: 无法打开 data/app.log（日志面板将为空）: %v\n", rErr)
	}

	// 日志级别用 slog.LevelVar 承载，支持运行期热调（PATCH /api/v1/logs/config）。
	//
	// 关键：控制台（os.Stdout）与 data/app.log 共用同一个 MultiWriter、同一个 logger，
	// 所以一次 SetLevel 同时作用于两者 —— 这正是「界面与控制台显示同一套」的实现基础，
	// 也是不能把两者拆成不同 handler 级别的原因（拆开会导致界面看到的与终端看到的不一致）。
	logger, logLevelVar := observ.NewLoggerWithLevel(cfg.Log.Level, cfg.Log.Format, logWriter)
	slog.SetDefault(logger)
	logger.Info("日志系统就绪",
		"level", cfg.Log.Level,
		"format", cfg.Log.Format,
		"file", filepath.Join(dataDir, "app.log"),
		"note", "级别可在「设置 → 日志查询」热调，控制台与日志文件同步生效")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ① 清理上次异常退出残留的 blob 临时文件
	if n, cErr := store.CleanupTmp(filepath.Join(dataDir, "tmp")); cErr == nil && n > 0 {
		logger.Info("已清理残留 blob 临时文件", "count", n)
	}

	// ② 主库（启动时必须迁移成功，否则拒绝启动）
	mainStore, err := store.OpenMain(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = mainStore.Close() }()

	// ③ 会话库管理器（不做全量扫描，惰性建库/迁移）
	sessionStore, err := store.OpenSessionStore(store.SessionStoreOptions{
		DataDir:   dataDir,
		MaxOpen:   cfg.Storage.MaxOpenSessions,
		IdleEvict: time.Duration(cfg.Storage.IdleEvictMin) * time.Minute,
	})
	if err != nil {
		return err
	}
	defer func() { _ = sessionStore.Close() }()

	// ④ 事件总线（per-Run 环形缓冲 500 + 订阅缓冲 256）
	bus := events.NewBus(500, 256)
	defer bus.Close()

	// ⑤ 上下文组装
	// D-4 预算：max_total>0 时历史段按 history_ratio 预裁（正常裁剪，有计数），
	// 剩余超限部分由 BudgetGate 五级降级处理（降级用尽即明确报错）。
	maxHistory := 0
	if cfg.Budget.MaxTotal > 0 {
		maxHistory = int(float64(cfg.Budget.MaxTotal) * cfg.Budget.HistoryRatio)
	}
	tokenizers := tokenizer.NewRegistry()
	builder := prompt.NewBuilder(tokenizers.For(""), prompt.Limits{
		MaxTotal:         cfg.Budget.MaxTotal,
		MaxHistory:       maxHistory,
		MaxDeferredIndex: cfg.Tools.DeferredSoftLimit,
		MaxSOP:           6,
	})
	// D-4 预算闸门：包装组装器串五级降级。max_total=0 时直通零开销（行为不变）。
	gatedBuilder := service.NewBudgetGate(builder, cfg.Budget.MaxTotal, cfg.Tools.DynamicMinTopK)

	// ⑥ 模型 provider（未配置真实端点时回退 fake，便于端到端验证）
	provider, providerDesc := buildProvider(logger)

	// ⑥' 厂商密钥 + 运行时模型解析：providers 表（B-4 界面配置）优先，未命中回退 env/fake
	secretCipher, cErr := secret.LoadOrCreate(filepath.Join(dataDir, "secret.key"))
	if cErr != nil {
		return cErr
	}
	modelResolver := service.NewProviderResolver(mainStore, secretCipher, logger, provider, providerDesc)

	// ⑦ 审批闸门（5min 超时视为拒绝）
	gate := approval.NewGate(approval.DefaultTimeout)

	// ⑦' 工具子系统（A-10 协议层）：注册表 → 插件装配 → 执行面路由 → 执行适配
	toolReg := registry.New(registry.Options{
		CoreSoftLimit:     cfg.Tools.CoreSoftLimit,
		DeferredSoftLimit: cfg.Tools.DeferredSoftLimit,
		Logger:            logger,
	})
	workDir, wErr := os.Getwd()
	if wErr != nil {
		return fmt.Errorf("获取工作区根目录失败: %w", wErr)
	}
	// Host 受控能力注入（F）：命令执行 / 模型调用 / 事件 / 嵌套 Run。
	// ChildRunner 依赖引擎（引擎又依赖工具注册表），构造后经 SetChildRunner 后置注入。
	toolHost := hostimpl.New(hostimpl.Options{WorkDir: workDir})
	pluginOK, pluginFail := registerBuiltins(ctx, toolReg, toolHost, nil, logger)
	toolRouter := router.New(router.Options{
		Executor: toolapi.ExecutorServer,
		Capabilities: map[string]bool{
			toolapi.CapWorkspaceRead:  true,
			toolapi.CapWorkspaceWrite: true,
			toolapi.CapProcess:        true,
			toolapi.CapNetwork:        true,
			toolapi.CapLLM:            true,
		},
	})
	toolLimits := loader.Limits{CoreSoft: cfg.Tools.CoreSoftLimit, DeferredSoft: cfg.Tools.DeferredSoftLimit}
	toolRunner := tools.NewRunner(toolReg, toolRouter, toolLimits)
	injection := toolRunner.Injection()
	logger.Info("工具注入预算",
		"plugins_ok", pluginOK,
		"plugins_failed", pluginFail,
		"core", len(injection.Core),
		"deferred", len(injection.DeferredIndex),
		"on_demand", len(injection.OnDemand),
		"cost_core", injection.CostCore,
		"cost_index", injection.CostIndex,
		"cost_total", injection.CostTotal,
		"core_soft_limit", injection.Limits.CoreSoft,
	)
	for _, warn := range toolReg.Warnings() {
		logger.Warn("工具注入软上限告警（不阻断启动）", "detail", warn)
	}

	// ⑦''' E：外部插件子系统（MCP 三传输 + 本地工具包，mcp.enabled 总开关）。
	//
	// 失败隔离：单个 Server/包 连接失败只记日志并标 failed（状态机在 manager 内），
	// 绝不阻断启动与对话主链路（docs/MCP接入设计.md M2）。安全边界：
	// cloud 形态 stdio 硬拒（Validate）；未签名包默认拒绝安装（出口标准）。
	var mcpSvc *service.MCPService
	var pkgSvc *service.PackageService
	if cfg.MCP.Enabled {
		trustedKeys, kErr := market.ParsePublicKeys(cfg.MCP.TrustedKeys)
		if kErr != nil {
			return kErr
		}
		mcpMgr := mcp.NewManager(mcp.Options{
			ClientName:    "harness-go",
			ClientVersion: version,
			Logger:        logger,
		}, mcp.DeploymentPolicy{Mode: string(cfg.Deployment.Mode), AllowStdio: cfg.MCP.AllowStdio})
		mcpSvc = service.NewMCPService(service.MCPServiceOptions{
			Main:     mainStore,
			Manager:  mcpMgr,
			Registry: toolReg,
			Cipher:   secretCipher,
			Dep:      mcp.DeploymentPolicy{Mode: string(cfg.Deployment.Mode), AllowStdio: cfg.MCP.AllowStdio},
			Logger:   logger,
		})
		pkgSvc = service.NewPackageService(service.PackageServiceOptions{
			Main:          mainStore,
			Manager:       mcpMgr,
			Registry:      toolReg,
			DataDir:       dataDir,
			TrustedKeys:   trustedKeys,
			AllowUnsigned: cfg.MCP.AllowUnsigned,
			Logger:        logger,
		})
		mcpSvc.StartAll(ctx)
		pkgSvc.RestoreAll(ctx)
		defer mcpMgr.StopAll()
	}

	// ⑦'' C-6：会话租约管理器（跨端单写者）。
	//
	// 必须在引擎之前构造（引擎 Execute 需要它），而推送 lease.revoked 依赖 WS 网关的
	// 在线连接表 → 用 SetHub 后置注入，打破「引擎 → 租约 → 网关 → 服务 → 引擎」的
	// 构造期循环。设备域关闭时保持 nil：引擎只走本地 mutex，行为与 C 阶段前一致。
	var deviceLeases *device.LeaseManager
	var leasePort agent.LeaseManager
	if cfg.Devices.Enabled {
		deviceLeases = service.NewLeaseAdapter(mainStore, nil)
		leasePort = deviceLeases
	}

	// ⑧ 引擎
	// 审批规则合并器（会话级「本会话允许」+ 项目级声明规则）：合并/去重/降级策略唯一实现，
	// 引擎只消费结果，不感知「项目」概念。失败一律降级为更保守的逐次审批，绝不阻断 Run。
	approvalPolicy := service.NewApprovalPolicy(mainStore, sessionStore)
	engine, err := agent.New(agent.Deps{
		Sessions:      service.NewSessionProvider(sessionStore),
		Summary:       service.NewSummaryWriter(mainStore),
		Events:        bus,
		Models:        modelResolver,
		Builder:       gatedBuilder,
		Tools:         toolRunner,
		Catalog:       toolRunner,
		RiskOverride:  toolRunner.RiskFor,
		Gate:          gate,
		Leases:        leasePort,
		MaxConcurrent: 8,
		MaxSteps:      cfg.Agent.MaxSteps,
		// 每 N 步向模型注入「当前步数 X/总步数 N」进度提示（0 = 关闭）。
		StepNoticeEvery: cfg.Agent.StepNoticeEvery,
		// 访问模式切换即时生效（对齐桌面端）：每次审批决策前实时重读会话行；
		// 读取失败回落 Run 启动快照（引擎内空值回落 confirm，与 normalizeAccessMode 同口径）。
		AccessModeProvider: func(ctx context.Context, sessionID string) (approval.AccessMode, error) {
			sess, err := mainStore.Sessions().Get(ctx, sessionID)
			if err != nil {
				return "", err
			}
			if sess.AccessMode == "" {
				return approval.ModeConfirm, nil
			}
			return approval.AccessMode(sess.AccessMode), nil
		},
		Rules: approvalPolicy,
	})
	if err != nil {
		return err
	}

	// ⑧' F：Host 能力接线（命令执行 / LLM / 事件 / 嵌套 Run）+ 记忆文件同步。
	// 核心 SOP（agent-core）：出厂内置（embed）+ 运行时覆盖（data/sop）。
	// 每 Run 动态读取 → 界面改完下一轮即生效，无需重启/重编译。
	sopSvc := service.NewSOPService(dataDir, cfg.SOP.CoreMaxTokens)
	childRunner := service.NewChildRunner(service.ChildRunnerOptions{
		Sessions:   sessionStore,
		Engine:     engine,
		Main:       mainStore,
		ToolRefs:   toolRunner.PromptRefs,
		SystemBase: defaultSystemBase,
		WorkDir:    workDir,
		// G：项目级 SOP 动态源，按**会话工作区**读取（每轮重读 → 改完即生效）。
		SOPFunc: sopBodies,
		// 核心 SOP（agent-core）动态源：子 Run 与父 Run 同口径（体感一致）。
		CoreSOPFunc: sopSvc.BindCore(),
	})
	toolHost.SetChildRunner(childRunner)
	toolHost.SetCommandRunner(hostimpl.NewCommandRunner(hostimpl.CommandOptions{
		Root:         workDir,
		EnvWhitelist: commandEnvWhitelist,
	}))
	toolHost.SetLLM(&llmAdapter{resolver: modelResolver})
	toolHost.SetEvents(&eventsAdapter{bus: bus})
	// D：图像能力（generate_image / recognize_image）。仅在配置了图像端点时注入，
	// 未配置则保持 nil —— 插件会返回「图像能力未启用」，绝不假装成功。
	if imgProvider := buildImageProvider(logger); imgProvider != nil {
		toolHost.SetImages(imgProvider)
	}
	syncMemoryFiles(ctx, workDir, mainStore, func(msg string, args ...any) {
		logger.Info(msg, args...)
	})

	// ⑨ 认证：账号引导 + 令牌（明文仅出现一次）
	authSvc := auth.NewService(auth.Options{
		Main:       mainStore,
		SessionTTL: time.Duration(cfg.Auth.SessionTTLH) * time.Hour,
	})
	if pwd, created, aErr := authSvc.EnsureBootstrapUser(ctx, cfg.Auth.InitialUser, cfg.Auth.InitialPwd); aErr != nil {
		return aErr
	} else if created {
		if pwd != "" {
			// 随机口令写入文件（0600），**不打印到日志**（日志会进 app.log）
			pwdFile := filepath.Join(dataDir, "initial_password")
			content := cfg.Auth.InitialUser + "\n" + pwd + "\n"
			if wErr := os.WriteFile(pwdFile, []byte(content), 0o600); wErr != nil {
				logger.Error("写入初始口令文件失败，请手动重置口令", "err", wErr)
			} else {
				logger.Warn("已创建初始账号，口令写入 data/initial_password（权限 0600）",
					"user", cfg.Auth.InitialUser, "password_file", pwdFile)
			}
		} else {
			logger.Info("已按配置创建初始账号", "user", cfg.Auth.InitialUser)
		}
	}
	if plain, created, tErr := authSvc.EnsureBootstrapToken(ctx); tErr != nil {
		return tErr
	} else if created {
		logger.Warn("已生成初始 API 令牌（仅显示一次，请妥善保存）", "token", plain)
	}

	// ⑩ 用例层
	sessionSvc := service.NewSessionService(mainStore, sessionStore)
	// D-3 记忆四层：四层全部落主库检索表（kind = memory_l0/l1/l2/l3），
	// 复用 retrieval_docs / retrieval_vectors 与 FTS5 触发器，零新增迁移。
	// embedding 未配置时向量通道自动停用（诚实降级：召回走关键词 + 衰减 + 重要性）。
	memoryStore := memory.New(memory.Options{Repo: mainStore.Retrieval()})
	runSvc := service.NewRunService(service.RunServiceOptions{
		Main:       mainStore,
		Sessions:   sessionStore,
		Engine:     engine,
		SystemBase: defaultSystemBase,
		WorkDir:    workDir,
		// 核心 SOP（agent-core）动态源：每 Run 读取，界面改完下一轮即生效。
		CoreSOPFunc: sopSvc.BindCore(),
		// 每轮重新取工具定义：PATCH /tools/tiers 改档后下一轮对话即生效
		ToolRefs: toolRunner.PromptRefs,
		// B-3：审批决策端点需经同一闸门提交（引擎在此登记待决项）
		Gate: gate,
		// D-2：动态工具检索 —— deferred 档平时只进索引（~18 token/条），
		// 本轮命中者才注入全量定义。检索通道失败时自动降级为「核心集 + 最近使用」，
		// 绝不让索引问题阻断对话（宽进严出：未注入的工具仍可执行）。
		ToolSelector: &service.RetrievalToolSelector{
			Repo:    mainStore.Retrieval(),
			Kind:    retrieval.KindTool,
			TopK:    cfg.Tools.DynamicTopK,
			MinTopK: cfg.Tools.DynamicMinTopK,
		},
		// D-3：记忆召回（L0/L1 稳定前缀 + L2/L3 检索置尾）
		Memory: memoryStore,
		// G：SOP 动态源（sop 插件唯一真源 <ws>/.harness/sop/*.md，全量注入）
		SOPFunc: sopBodies,
		// C-7：高风险审批是否仅限执行端设备确认（默认 false = 只审计不阻断）
		StrictApprovalDevice: cfg.Devices.StrictApprovalDevice,
		// 能力天花板过滤工具注入：toolRunner 同时实现 agent.ToolCatalog（Risk），
		// 与引擎侧用的风险源完全一致，避免两处判定漂移（协议 §4.1）。
		Catalog: toolRunner,
	})
	if n, rErr := runSvc.RecoverInterrupted(ctx); rErr == nil && n > 0 {
		logger.Warn("已将未完成 Run 置为 interrupted（不做断点续跑）", "count", n)
	}

	// ⑩'' G：S10 快照回滚 + 长任务状态机 + 会话导出包（Host 能力注入）。
	//   - 快照：全局 CAS（data/snapshots/blobs）+ 主库锚点行；fs 插件写入前/后
	//     经 Host.Snapshots 登记 before/after，回滚服务物理写回（与写事务一致）。
	//   - 长任务：会话库 tasks/task_checkpoints；检查点 = Run + 快照引用原子登记。
	//   - 导出包：data/sessions/<sid>/bundle/；导入恢复为新会话（沙箱内路径）。
	snapStore, sErr := snapshot.New(snapshot.Options{
		Main:          mainStore,
		DataDir:       dataDir,
		WorkspaceRoot: workDir,
	})
	if sErr != nil {
		return fmt.Errorf("初始化快照存储失败: %w", sErr)
	}
	sessionSvc.SetDeletedHook(func(hctx context.Context, sid string) {
		if _, fErr := snapStore.ForgetSession(hctx, sid); fErr != nil {
			logger.Warn("清理会话快照行失败", "session", sid, "err", fErr)
			return
		}
		if n, gErr := snapStore.GC(hctx); gErr == nil && n > 0 {
			logger.Info("已回收零引用快照内容", "blobs", n)
		}
	})
	taskMgr := service.NewTaskManager(service.TaskManagerOptions{
		Sessions:  sessionStore,
		Snapshots: snapStore,
		RunStop: func(hctx context.Context, _ string, runID string) error {
			return runSvc.StopRun(hctx, runID)
		},
	})
	bundleSvc := service.NewBundleService(service.BundleServiceOptions{
		Main:          mainStore,
		Sessions:      sessionStore,
		DataDir:       dataDir,
		WorkspaceRoot: workDir,
	})
	rollbackSvc := service.NewRollbackService(service.RollbackServiceOptions{
		Main:      mainStore,
		Sessions:  sessionStore,
		Snapshots: snapStore,
	})

	// ⑩''' P1：Webhook 事件订阅（HMAC 签名 + 白名单 + 重试退避）+ OpenAI 兼容端点。
	webhookSvc := service.NewWebhookService(service.WebhookServiceOptions{
		Main:   mainStore,
		Cipher: secretCipher,
		Bus:    bus,
		Logger: logger,
	})
	webhookSvc.Start(ctx)
	defer webhookSvc.Stop()
	openaiSvc := service.NewOpenAICompatService(service.OpenAICompatServiceOptions{
		Main:     mainStore,
		Sessions: sessionStore,
		Runs:     runSvc,
		Bus:      bus,
	})

	toolHost.SetSnapshots(snapStore)
	toolHost.SetTasks(taskMgr)
	toolHost.SetBundles(bundleSvc)
	logger.Info("阶段 G 能力已接线", "snapshots", "global-cas", "long_task", "state-machine", "bundles", "zip")

	maintenanceSvc := service.NewMaintenanceService(mainStore, sessionStore, runSvc, gate.PendingCount)
	toolSvc := service.NewToolService(toolRunner, mainStore)
	toolSvc.LoadOverrides(ctx)

	// ⑩' B-4：厂商（api_key 密文落库）+ 配置 + 日志尾行
	providerSvc := service.NewProviderService(service.ProviderServiceOptions{
		Main:   mainStore,
		Cipher: secretCipher,
	})
	settingsSvc := service.NewSettingsService(mainStore)
	// ⑩' B-4：厂商（api_key 密文落库）+ 配置 + 日志尾行、审计日志库族
	logSvc := service.NewLogService(filepath.Join(dataDir, "app.log"), 500)

	// ⑩'' 审计日志天分片库族（与业务库物理隔离；可丢弃，有保留期）
	//
	// 布局：data/logs/logs-YYYYMMDD.db + blob/<day>/<xx>/<sha256>
	// 铁律：写入走有界队列 + 批量事务，绝不进入 Run 关键路径。
	logRoot := cfg.Log.Dir
	if logRoot == "" {
		logRoot = filepath.Join(dataDir, "logs")
	}
	logShard, sErr := logvault.NewShard(logRoot)
	if sErr != nil {
		return sErr
	}
	defer func() { _ = logShard.Close() }()
	logBlobs := logvault.NewBlobStore(logShard)
	if n := logBlobs.CleanupTmp(); n > 0 {
		logger.Info("已清理残留日志正文临时文件", "count", n)
	}
	// 抓取档位：settings 表优先，未配置时用 config 兜底（界面可运行时切换）
	logModes := logvault.NewModeProvider(cfg.Log.SnapshotMode)
	logRetention := logvault.NewRetention(logvault.RetentionOptions{
		Shard:           logShard,
		RetentionDays:   cfg.Log.RetentionDays,
		ColdArchiveDays: cfg.Log.ColdArchiveDays,
		MaxTotalBytes:   int64(cfg.Log.MaxTotalGB) << 30,
		Logger:          logger,
	})
	logCfgSvc := service.NewLogConfigService(service.LogConfigOptions{
		Settings:             settingsSvc,
		Modes:                logModes,
		Retention:            logRetention,
		DefaultMode:          cfg.Log.SnapshotMode,
		DefaultRetentionDays: cfg.Log.RetentionDays,
		DefaultLevel:         cfg.Log.Level,
		LevelVar:             logLevelVar,
	})
	// 用 settings 中的值刷新一次（把界面上次设置的档位带进本轮启动）
	logModes.Refresh(logCfgSvc.SnapshotMode(ctx))
	// 日志级别同理：界面上次保存的级别要覆盖本轮启动默认值，否则重启会「回到 config」。
	// ApplyStoredLevel 只读库并改进程内 LevelVar，不重复写库。
	if applied := logCfgSvc.ApplyStoredLevel(ctx); applied != cfg.Log.Level {
		logger.Info("日志级别已按上次设置恢复", "level", applied, "config_default", cfg.Log.Level)
	}

	logWriterPipe := logvault.NewWriter(logvault.WriterOptions{
		Shard:     logShard,
		Blobs:     logBlobs,
		Modes:     logModes,
		Logger:    logger,
		QueueCap:  cfg.Log.QueueCap,
		BatchSize: cfg.Log.BatchSize,
		FlushMs:   cfg.Log.FlushMS,
	})
	defer func() {
		// 优雅关闭：排空队列，避免丢失尾部日志
		if cErr := logWriterPipe.Close(); cErr != nil {
			logger.Warn("关闭日志写入管道异常", "err", cErr)
		}
	}()
	// 保留期清理器在档位服务之前已构造，这里复用它执行一次启动清理
	// 启动即清理一次（把上次运行期间过期的天库回收），之后每小时检查一次
	// （用 1h 而非 24h：跨零点后用户期望尽快看到「最早可查日期」前移）
	retentionRep := logRetention.Sweep(ctx)
	logRetention.Start(ctx, time.Hour)
	logger.Info("审计日志库族已就绪",
		"root", logShard.Root(),
		"snapshot_mode", logCfgSvc.SnapshotMode(ctx),
		"retention_days", cfg.Log.RetentionDays,
		"earliest_day", retentionRep.EarliestDay,
		"latest_day", retentionRep.LatestDay,
		"total_mb", retentionRep.TotalAfterBytes>>20)
	logvaultQuery := logvault.NewQuerier(logShard, logBlobs)
	// 把审计端口注入引擎（nil 时不记日志，行为与之前完全一致）
	runSvc.SetTraceSink(service.NewTraceSink(logWriterPipe))

	// ⑩'' C-2：设备域（配对 / 列表 / 吊销）。配置来自 config.Devices 段。
	// C-6：deviceLeases 已在引擎构造前建立（⑦''），这里注入到用例层。
	deviceSvc := service.NewDeviceService(service.DeviceServiceOptions{
		Main:           mainStore,
		Enabled:        cfg.Devices.Enabled,
		PairTTL:        time.Duration(cfg.Devices.PairTTLS) * time.Second,
		Heartbeat:      time.Duration(cfg.Devices.HeartbeatS) * time.Second,
		OfflineAfter:   time.Duration(cfg.Devices.OfflineAfterS) * time.Second,
		TaskTTL:        time.Duration(cfg.Devices.TaskTTLS) * time.Second,
		StrictApproval: cfg.Devices.StrictApprovalDevice,
		Leases:         deviceLeases,
	})
	if cfg.Devices.Enabled {
		logger.Info("设备域已启用",
			"pair_ttl_s", cfg.Devices.PairTTLS,
			"heartbeat_s", cfg.Devices.HeartbeatS,
			"offline_after_s", cfg.Devices.OfflineAfterS,
			"task_ttl_s", cfg.Devices.TaskTTLS)
	}

	// ⑩''' C-3：WS 网关（在线注册表 + 心跳超时判定 + 升级/读写循环）。
	// 在线状态落库通过 Hub 回调交给 deviceSvc（同主库、单条 UPDATE，无跨库事务）。
	//
	// C-4：OnOffline 组合回调——先落库 offline，再让派发器**立即**收敛该设备的在途任务
	// （离线立即降级，绝不等到 deadline）。dispatcher 在 hub 之后构造，故用闭包前置引用。
	var deviceDispatcher *device.Dispatcher
	// 设备状态广播总线（供 /api/v1/devices/events SSE 实时推送上下线）。
	// 与 events.Bus 是**两套**总线：后者是 per-Run 序号流，设备状态是「当前态」。
	deviceState := device.NewStateBus(device.StateBusOptions{})
	deviceHub := device.NewHub(device.HubOptions{
		Log:          logger,
		OfflineAfter: time.Duration(cfg.Devices.OfflineAfterS) * time.Second,
		OnOnline: func(octx context.Context, deviceID string) {
			deviceSvc.MarkOnline(octx, deviceID)
			// 广播上线：name/kind/user_id 从设备档案取（失败也不影响上线事实，
			// 只少了展示字段 —— 绝不能让一次查询失败把上线事件吞掉）。
			info := device.DeviceState{}
			if d, err := deviceSvc.Get(octx, deviceID); err == nil && d != nil {
				info.Name = d.Name
				info.Kind = string(d.Kind)
				// 归属账号：观察侧据此按账号划分设备视图。
				// 为空（历史设备）时保持空串，由前端展示为"未绑定账号"。
				info.UserID = d.UserID
			}
			deviceState.MarkOnline(deviceID, info)
		},
		OnHeartbeat: func(octx context.Context, deviceID string, at int64, load json.RawMessage, capsHash string, proto int) {
			deviceSvc.RecordHeartbeat(octx, deviceID, at, load, capsHash, proto)
			// 心跳只刷新 last_seen，**不广播**（否则每 15s 刷屏一次无意义事件）。
			deviceState.Touch(deviceID)
		},
		OnOffline: func(octx context.Context, deviceID string) {
			deviceSvc.MarkOffline(octx, deviceID)
			deviceState.MarkOffline(deviceID)
			if deviceDispatcher != nil {
				if n := deviceDispatcher.FailDevice(deviceID, device.TaskErrorOffline); n > 0 {
					logger.Warn("设备离线，在途端侧任务已降级", "device_id", deviceID, "tasks", n)
				}
			}
		},
	})
	var deviceGateway *device.Gateway
	if cfg.Devices.Enabled {
		deviceHub.Start(ctx)
		// 吊销设备时断开其长连接（后置注入，打破构造期循环依赖）
		deviceSvc.SetDisconnector(deviceHub)
		// 解绑双向收敛：吊销（管理端 DELETE 或端侧自我解约）后从观察面移除并广播
		// device.revoked，让 Web 端列表**无需手动刷新**即消失。
		// 不挂这个钩子的话，SSE 只能等下一次重连的快照才收敛。
		deviceSvc.SetRevokedHook(func(hctx context.Context, deviceID string) {
			deviceState.Remove(deviceID)
		})
		// C-6：租约管理器接入在线连接表 → 抢占时向旧端推送 lease.revoked
		if deviceLeases != nil {
			deviceLeases.SetHub(deviceHub)
		}
		// C-4：任务派发器（task.dispatch / progress / result）
		deviceDispatcher = device.NewDispatcher(device.DispatcherOptions{
			Log:     logger,
			Hub:     deviceHub,
			TaskTTL: deviceSvc.TaskTTL(),
		})
		deviceGateway = device.NewGateway(device.GatewayOptions{
			Log:          logger,
			Hub:          deviceHub,
			Authenticate: deviceSvc.Authenticate,
			Heartbeat:    deviceSvc.Heartbeat(),
			OfflineAfter: deviceSvc.OfflineAfter(),
			Handler:      deviceDispatcher.HandleFrame,
			PendingCount: deviceDispatcher.PendingCount,
		})
		// C-5：执行面路由接线——device 优先，不可用则静默降级本地（不变量 5）。
		// 仅在设备域启用时注入；未启用时 toolRunner 的 devices 为 nil，行为与 C 之前完全一致。
		toolRunner.SetDeviceExecutor(newDeviceTaskExecutor(
			deviceHub, deviceDispatcher, publishDeviceProgress(bus)))
		logger.Info("执行面路由已接线（端侧优先，缺失静默降级）",
			"ws_path", service.DeviceWSPath, "task_ttl_s", cfg.Devices.TaskTTLS)
		logger.Info("设备长连接网关已就绪",
			"ws_path", service.DeviceWSPath, "task_ttl_s", cfg.Devices.TaskTTLS)
	}

	// ⑪ 路由
	// 工作区宿主目录列举的白名单根（L0）：默认只放开数据目录的**父级**（即本工作区根），
	// 另可用 HARNESS_FS_ROOTS（分号分隔）追加。为空 = 前端目录选择器不可用（失败即关闭）。
	fsRoots := resolveFSRoots(dataDir, os.Getenv("HARNESS_FS_ROOTS"))
	if len(fsRoots) > 0 {
		logger.Info("工作区目录列举已启用", "roots", fsRoots)
	} else {
		logger.Warn("未配置工作区目录列举根，前端目录选择将不可用（可设 HARNESS_FS_ROOTS）")
	}

	httpHandler := api.NewRouter(api.Deps{
		Version:     version,
		Log:         logger,
		Sessions:    sessionSvc,
		Runs:        runSvc,
		Maintenance: maintenanceSvc,
		Tools:       toolSvc,
		Providers:   providerSvc,
		Settings:    settingsSvc,
		Logs:        logSvc,
		LogConfig:   logCfgSvc,
		Vault:       logvaultQuery,
		LogWriter:   logWriterPipe,
		Shard:       logShard,
		Devices:     deviceSvc,
		MCP:         mcpSvc,
		Packages:    pkgSvc,
		Rollback:    rollbackSvc,
		Artifacts:   service.NewArtifactService(service.ArtifactServiceOptions{DataDir: dataDir}),
		Bundles:     bundleSvc,
		Backups:     service.NewBackupService(service.BackupServiceOptions{DataDir: dataDir}),
		Webhooks:    webhookSvc,
		OpenAI:      openaiSvc,
		Projects:    service.NewProjectService(mainStore),
		Workspaces:  service.NewWorkspaceService(mainStore, deviceHub),
		SOP:         sopSvc,
		FSRoots:     fsRoots,
		DeviceWS:    deviceWSHandler(deviceGateway),
		DeviceState: deviceState,
		Bus:         bus,
		Auth:        authSvc,
		AuthConfig: api.AuthConfig{
			SSOEnabled:   cfg.Auth.SSOEnabled,
			SSOMode:      string(cfg.Auth.SSOMode),
			SSOSecret:    cfg.Auth.SSOSecret,
			SessionTTL:   time.Duration(cfg.Auth.SessionTTLH) * time.Hour,
			SecureCookie: false, // 本地 http 部署；https 反代时置 true
			SSOLimiter:   auth.NewLimiter(5, time.Minute),
		},
		VerifyToken: authSvc.VerifyToken,
	})

	server := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           httpHandler,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout 保持 0：SSE 长连接需要
	}

	// ⑫ 启动
	// 监听口径（2026-11 起）：默认 0.0.0.0:5611（局域网开箱可访问，用户明确要求）。
	// 检测到「仅回环绑定」时反而是用户显式收窄了范围（-addr 127.0.0.1:N 等），
	// 此时打一条 Info 级说明——不是告警，只是让局域网访问需求者知道为什么别的设备连不上。
	// （历史口径曾是默认仅回环 + Warn 提示如何开放；方向已随产品决策翻转。）
	if isLoopbackOnly(cfg.Server.Addr) {
		logger.Info("当前仅本机可访问（监听回环地址）；局域网/其它设备访问请用 -addr 0.0.0.0:5611 启动（或设置 HARNESS_SERVER_ADDR）",
			"addr", cfg.Server.Addr)
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("harness-go 已启动",
			"version", version,
			"commit", commit,
			"built", buildTime,
			"go", runtime.Version(),
			"addr", cfg.Server.Addr,
			"mode", string(cfg.Deployment.Mode),
			"data_dir", dataDir,
			"provider", providerDesc,
			"protocol_version", observ.ProtocolVersion,
			"max_open_sessions", cfg.Storage.MaxOpenSessions,
		)
		if lErr := server.ListenAndServe(); lErr != nil && !errors.Is(lErr, http.ErrServerClosed) {
			errCh <- lErr
		}
	}()

	select {
	case lErr := <-errCh:
		return fmt.Errorf("HTTP 服务启动失败: %w", lErr)
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅关闭（最长 30s）")
	}

	// ⑬ 优雅关闭：停止接收 → 等待在途请求 → 断开设备长连接 → 停止插件 → 关闭存储
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if sErr := server.Shutdown(shutdownCtx); sErr != nil {
		logger.Error("HTTP 优雅关闭失败", "err", sErr)
	}
	// 先关 WS（长连接不会被 Shutdown 等待），再停插件与存储
	deviceHub.CloseAll("服务端关闭")
	if deviceDispatcher != nil {
		// 在途端侧任务全部收敛为降级（等不到端侧结果了），避免 Run 悬空
		deviceDispatcher.Close(device.FrameCodeServerClosing)
	}
	toolReg.Stop(shutdownCtx)
	logger.Info("已退出")
	return nil
}

// resolveFSRoots 计算「允许浏览的目录根」白名单（工作区宿主目录列举，L0）。
//
// 规则（docs/工作区宿主与目录选择对接约定.md §4）：
//   - 默认放开数据目录的**父级**：数据目录形如 <workspaceRoot>/data，
//     故默认可浏览范围即该工作区根及其子树；
//   - extra（HARNESS_FS_ROOTS，分号分隔）用于显式追加额外根，便于把工作区放在别处；
//   - 结果全部 Abs + Clean，去重后返回；为空表示「拒绝列举」（失败即关闭语义）。
func resolveFSRoots(dataDir, extra string) []string {
	candidates := make([]string, 0, 4)
	if parent := filepath.Dir(filepath.Clean(dataDir)); parent != "" && parent != "." {
		candidates = append(candidates, parent)
	} else {
		candidates = append(candidates, dataDir)
	}
	for _, seg := range strings.Split(extra, ";") {
		if seg = strings.TrimSpace(seg); seg != "" {
			candidates = append(candidates, seg)
		}
	}

	out := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		// 只保留真实存在的目录，避免把拼错的路径塞进白名单
		if st, sErr := os.Stat(abs); sErr != nil || !st.IsDir() {
			continue
		}
		key := abs
		if runtime.GOOS == "windows" {
			key = strings.ToLower(abs)
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, abs)
	}
	return out
}

// deviceWSHandler 把网关包装为可 nil 的 http.Handler（未启用设备域时返回 nil，
// 由 api 层 handler 直接回 503 device.disabled，避免路由指向空指针）。
func deviceWSHandler(g *device.Gateway) http.Handler {
	if g == nil {
		return nil
	}
	return g
}

// buildProvider 按环境变量构造 provider；未配置则回退 fake（便于端到端验证）。
//
//	HARNESS_LLM_BASE_URL  例：https://api.openai.com/v1
//	HARNESS_LLM_API_KEY
func buildProvider(logger *slog.Logger) (llm.Provider, string) {
	baseURL := os.Getenv("HARNESS_LLM_BASE_URL")
	apiKey := os.Getenv("HARNESS_LLM_API_KEY")
	if baseURL != "" {
		p, err := llm.NewOpenAICompat(llm.OpenAICompatConfig{
			Name:    "openai-compat",
			BaseURL: baseURL,
			APIKey:  apiKey,
		})
		if err == nil {
			return p, "openai-compat(" + baseURL + ")"
		}
		logger.Error("初始化 OpenAI 兼容 provider 失败，回退 fake", "err", err)
	}
	fake := llm.NewFakeProvider()
	return fake, fake.Describe()
}

// buildImageProvider 按环境变量构造 OpenAI 兼容图像能力；未配置返回 nil。
//
//	HARNESS_IMAGE_BASE_URL   例：https://api.openai.com/v1（缺省回退 HARNESS_LLM_BASE_URL）
//	HARNESS_IMAGE_API_KEY    缺省回退 HARNESS_LLM_API_KEY
//	HARNESS_IMAGE_MODEL      生成模型，缺省 gpt-image-1
//	HARNESS_IMAGE_VISION_MODEL 理解模型，缺省 gpt-4o-mini
//
// 未显式配置 HARNESS_IMAGE_BASE_URL 且 LLM 端点未配置时返回 nil：
// image 插件的两个工具将诚实返回「图像能力未启用」，而不是伪造结果。
func buildImageProvider(logger *slog.Logger) toolapi.ImageProvider {
	baseURL := strings.TrimSpace(os.Getenv("HARNESS_IMAGE_BASE_URL"))
	apiKey := strings.TrimSpace(os.Getenv("HARNESS_IMAGE_API_KEY"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("HARNESS_LLM_BASE_URL"))
		if apiKey == "" {
			apiKey = strings.TrimSpace(os.Getenv("HARNESS_LLM_API_KEY"))
		}
	}
	if baseURL == "" {
		logger.Info("未配置图像端点（HARNESS_IMAGE_BASE_URL），generate_image/recognize_image 将报「图像能力未启用」")
		return nil
	}
	p, err := llm.NewOpenAIImages(llm.OpenAIImagesConfig{
		Name:        "openai-images",
		BaseURL:     baseURL,
		APIKey:      apiKey,
		Model:       strings.TrimSpace(os.Getenv("HARNESS_IMAGE_MODEL")),
		VisionModel: strings.TrimSpace(os.Getenv("HARNESS_IMAGE_VISION_MODEL")),
	})
	if err != nil {
		logger.Error("初始化图像能力失败，图像工具将报「图像能力未启用」", "err", err)
		return nil
	}
	logger.Info("图像能力已启用", "base_url", baseURL)
	return p
}

// staticResolver 单 provider 解析器（A 阶段占位，B 阶段已由 service.ProviderResolver 取代）。
//
// 保留为**测试/降级用**：当需要强制单 provider（不查 providers 表）时可显式使用。
type staticResolver struct{ provider llm.Provider }

// Resolve 固定返回注入的 provider（不做模型路由）。
func (r *staticResolver) Resolve(_ context.Context, modelRef string) (llm.Provider, string, error) {
	if r.provider == nil {
		return nil, "", errors.New("未配置模型 provider")
	}
	if modelRef == "" {
		modelRef = "default"
	}
	return r.provider, modelRef, nil
}
