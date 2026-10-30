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
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/observ"
	"harness-go/internal/prompt"
	"harness-go/internal/secret"
	"harness-go/internal/service"
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
	tokenizers := tokenizer.NewRegistry()
	builder := prompt.NewBuilder(tokenizers.For(""), prompt.Limits{
		MaxDeferredIndex: cfg.Tools.DeferredSoftLimit,
		MaxSOP:           6,
	})

	// ⑥ 模型 provider（未配置真实端点时回退 fake，便于端到端验证）
	provider, providerDesc := buildProvider(logger)

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

	// ⑧ 引擎
	engine, err := agent.New(agent.Deps{
		Sessions:      service.NewSessionProvider(sessionStore),
		Summary:       service.NewSummaryWriter(mainStore),
		Events:        bus,
		Models:        &staticResolver{provider: provider},
		Builder:       builder,
		Tools:         toolRunner,
		Catalog:       toolRunner,
		Gate:          gate,
		MaxConcurrent: 8,
		MaxSteps:      20,
	})
	if err != nil {
		return err
	}

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
	runSvc := service.NewRunService(service.RunServiceOptions{
		Main:       mainStore,
		Sessions:   sessionStore,
		Engine:     engine,
		SystemBase: defaultSystemBase,
		// 每轮重新取工具定义：PATCH /tools/tiers 改档后下一轮对话即生效
		ToolRefs: toolRunner.PromptRefs,
		// B-3：审批决策端点需经同一闸门提交（引擎在此登记待决项）
		Gate: gate,
	})
	if n, rErr := runSvc.RecoverInterrupted(ctx); rErr == nil && n > 0 {
		logger.Warn("已将未完成 Run 置为 interrupted（不做断点续跑）", "count", n)
	}
	maintenanceSvc := service.NewMaintenanceService(mainStore, sessionStore, runSvc, gate.PendingCount)
	toolSvc := service.NewToolService(toolRunner, mainStore)
	toolSvc.LoadOverrides(ctx)

	// ⑩' B-4：厂商（api_key 密文落库）+ 配置 + 日志尾行
	secretCipher, cErr := secret.LoadOrCreate(filepath.Join(dataDir, "secret.key"))
	if cErr != nil {
		return cErr
	}
	providerSvc := service.NewProviderService(service.ProviderServiceOptions{
		Main:   mainStore,
		Cipher: secretCipher,
	})
	settingsSvc := service.NewSettingsService(mainStore)
	logSvc := service.NewLogService(filepath.Join(dataDir, "app.log"), 500)

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

	// ⑬ 优雅关闭：停止接收 → 等待在途请求 → 停止插件 → 关闭存储
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if sErr := server.Shutdown(shutdownCtx); sErr != nil {
		logger.Error("HTTP 优雅关闭失败", "err", sErr)
	}
	toolReg.Stop(shutdownCtx)
	logger.Info("已退出")
	return nil
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

// staticResolver 单 provider 解析器（A 阶段不做多 provider 路由）。
type staticResolver struct{ provider llm.Provider }

func (r *staticResolver) Resolve(_ context.Context, modelRef string) (llm.Provider, string, error) {
	if r.provider == nil {
		return nil, "", errors.New("未配置模型 provider")
	}
	if modelRef == "" {
		modelRef = "default"
	}
	return r.provider, modelRef, nil
}
