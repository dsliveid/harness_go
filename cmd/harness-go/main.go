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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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

// defaultSystemBase 默认系统提示（后续由 SOP/项目约束/技能扩展）。
const defaultSystemBase = `你是 harness-go 的编码助手，运行在用户的工作区内。

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

func run() error {
	cfg, err := config.Load(config.Options{Args: os.Args[1:]})
	if err != nil {
		return err
	}

	dataDir, err := cfg.EnsureDataDir()
	if err != nil {
		return err
	}

	// 日志双写：stdout + data/app.log（`GET /api/v1/logs` 尾行读取该文件）。
	// 日志文件打开失败不阻断启动（只退化为 stdout），但必须打印告警。
	logWriter := io.Writer(os.Stdout)
	if logFile, lErr := os.OpenFile(filepath.Join(dataDir, "app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); lErr == nil {
		logWriter = io.MultiWriter(os.Stdout, logFile)
		defer func() { _ = logFile.Close() }()
	} else {
		fmt.Fprintf(os.Stderr, "harness-go: 无法打开 data/app.log（日志面板将为空）: %v\n", lErr)
	}

	logger := observ.NewLogger(cfg.Log.Level, cfg.Log.Format, logWriter)
	slog.SetDefault(logger)

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
	engine, err := agent.New(agent.Deps{
		Sessions:      service.NewSessionProvider(sessionStore),
		Summary:       service.NewSummaryWriter(mainStore),
		Events:        bus,
		Models:        modelResolver,
		Builder:       gatedBuilder,
		Tools:         toolRunner,
		Catalog:       toolRunner,
		Gate:          gate,
		Leases:        leasePort,
		MaxConcurrent: 8,
		MaxSteps:      20,
	})
	if err != nil {
		return err
	}

	// ⑧' F：Host 能力接线（命令执行 / LLM / 事件 / 嵌套 Run）+ 记忆文件同步。
	childRunner := service.NewChildRunner(service.ChildRunnerOptions{
		Sessions:   sessionStore,
		Engine:     engine,
		Main:       mainStore,
		ToolRefs:   toolRunner.PromptRefs,
		SystemBase: defaultSystemBase,
		// G：SOP 动态源（sop 插件落盘 .harness/sop/*.md 后下一轮生效）
		SOPFunc: func() []string { return sopBodies(toolHost) },
	})
	toolHost.SetChildRunner(childRunner)
	toolHost.SetCommandRunner(hostimpl.NewCommandRunner(hostimpl.CommandOptions{
		Root:         workDir,
		EnvWhitelist: commandEnvWhitelist,
	}))
	toolHost.SetLLM(&llmAdapter{resolver: modelResolver})
	toolHost.SetEvents(&eventsAdapter{bus: bus})
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
		SOPFunc: func() []string { return sopBodies(toolHost) },
		// C-7：高风险审批是否仅限执行端设备确认（默认 false = 只审计不阻断）
		StrictApprovalDevice: cfg.Devices.StrictApprovalDevice,
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
	logSvc := service.NewLogService(filepath.Join(dataDir, "app.log"), 500)

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
	deviceHub := device.NewHub(device.HubOptions{
		Log:          logger,
		OfflineAfter: time.Duration(cfg.Devices.OfflineAfterS) * time.Second,
		OnOnline:     deviceSvc.MarkOnline,
		OnHeartbeat:  deviceSvc.RecordHeartbeat,
		OnOffline: func(octx context.Context, deviceID string) {
			deviceSvc.MarkOffline(octx, deviceID)
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
		Devices:     deviceSvc,
		MCP:         mcpSvc,
		Packages:    pkgSvc,
		Rollback:    rollbackSvc,
		Webhooks:    webhookSvc,
		OpenAI:      openaiSvc,
		DeviceWS:    deviceWSHandler(deviceGateway),
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
