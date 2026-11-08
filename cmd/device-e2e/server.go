package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"harness-go/internal/api"
	"harness-go/internal/auth"
	"harness-go/internal/device"
	"harness-go/internal/observ"
	"harness-go/internal/service"
	"harness-go/internal/store"

	"net/http/httptest"
)

// ============ 自包含服务端装配 ============
//
// 为什么自包含而不是「连已启动的服务端」：
//  1. 服务端目前**没有**触发任务的 HTTP 端点（dispatcher.Dispatch 只在单测里被调用），
//     而本脚本的核心目标正是「派发 → 端侧执行 → 回执收敛」这一段，只能由进程内直接调
//     Dispatcher 才能跑通 —— 因此必须有进程内装配。
//  2. 一条 `go run ./cmd/device-e2e` 就能跑，不需要用户先起服务端、先建库、先开 devices.enabled，
//     联调门槛最低（失败是脚本自己的问题，不是环境问题）。
//  3. 装配全部走**真实实现**（真实 sqlite 临时库、真实 api.NewRouter + 中间件链、真实 Gateway、
//     真实 Dispatcher），只有「数据目录」是临时的，因此断言仍然是对生产代码的断言，
//     不是对测试替身的断言。
//
// 代价：外部（真服务端）模式拿不到派发能力，所以 -external 只跑到握手/心跳（见 main.go）。

// serverOptions 装配参数（默认值对齐 config.Devices 默认：heartbeat 15s / offline 45s / pair_ttl 120s）。
type serverOptions struct {
	Username string
	Password string
	DataDir  string // 空 = 进程级临时目录（退出即删）
	// DevicesEnabled 设备域总开关（默认 true；设为 false 用于自检 503 device.disabled 提示文案）
	DevicesEnabled bool
	PairTTL        time.Duration
	Heartbeat      time.Duration
	OfflineAfter   time.Duration
	TaskTTL        time.Duration
}

// harnessServer 进程内最小可用服务端。
type harnessServer struct {
	URL      string
	Devices  *service.DeviceService
	Hub      *device.Hub
	Dispatch *device.Dispatcher

	stopFns []func()
}

// Close 逆序清理（断连 → 关服务 → 关库 → 删临时目录）。
func (h *harnessServer) Close() {
	for i := len(h.stopFns) - 1; i >= 0; i-- {
		h.stopFns[i]()
	}
}

// startHarnessServer 起一个「真实路由 + 真实网关 + 真实派发器」的最小服务端。
//
// 返回 error 时：临时目录在此就地清理（调用方拿不到 harnessServer，无法 Close）。
func startHarnessServer(opts serverOptions) (hsrv *harnessServer, err error) {
	ctx := context.Background()

	dataDir := opts.DataDir
	if dataDir == "" {
		tmp, mkErr := os.MkdirTemp("", "harness-device-e2e-*")
		if mkErr != nil {
			return nil, fmt.Errorf("创建临时数据目录失败: %w", mkErr)
		}
		dataDir = tmp
		defer func() {
			if err != nil {
				_ = os.RemoveAll(tmp)
			}
		}()
	}

	mainStore, err := store.OpenMain(ctx, dataDir)
	if err != nil {
		return nil, fmt.Errorf("打开临时主库失败（目录 %s）: %w", dataDir, err)
	}

	srv := &harnessServer{}
	srv.stopFns = append(srv.stopFns, func() { _ = mainStore.Close() })
	if opts.DataDir == "" {
		dir := dataDir
		srv.stopFns = append(srv.stopFns, func() { _ = os.RemoveAll(dir) })
	}

	// ① 账号与用户令牌（脚本用真实 /api/v1/auth/login 换取令牌，不直接签发，以覆盖中间件链）
	authSvc := auth.NewService(auth.Options{Main: mainStore, SessionTTL: time.Hour})
	if _, _, err := authSvc.EnsureBootstrapUser(ctx, opts.Username, opts.Password); err != nil {
		srv.Close()
		return nil, fmt.Errorf("创建初始账号失败: %w", err)
	}

	// ② 设备域（配对 / 鉴权 / 在线落库）
	devSvc := service.NewDeviceService(service.DeviceServiceOptions{
		Main:         mainStore,
		Enabled:      opts.DevicesEnabled,
		PairTTL:      opts.PairTTL,
		Heartbeat:    opts.Heartbeat,
		OfflineAfter: opts.OfflineAfter,
		TaskTTL:      opts.TaskTTL,
	})
	srv.Devices = devSvc

	// ③ 在线表 + 派发器（互相引用：Hub 的 OnOffline 要让派发器立即收敛在途任务，
	//    派发器又需要 Hub 查连接 → 用闭包捕获后置赋值的指针打破构造期循环）
	var disp *device.Dispatcher
	hub := device.NewHub(device.HubOptions{
		OfflineAfter: opts.OfflineAfter,
		OnOnline:     devSvc.MarkOnline,
		OnHeartbeat:  devSvc.RecordHeartbeat,
		OnOffline: func(ctx context.Context, deviceID string) {
			devSvc.MarkOffline(ctx, deviceID)
			if disp != nil {
				// 端侧掉线：在途任务立即判降级，不等 deadline（不变量 5）
				disp.FailDevice(deviceID, "")
			}
		},
	})
	srv.Hub = hub
	hub.Start(context.Background())
	srv.stopFns = append(srv.stopFns, func() { hub.CloseAll("e2e 结束") })

	disp = device.NewDispatcher(device.DispatcherOptions{
		Hub:     hub,
		TaskTTL: opts.TaskTTL,
	})
	srv.Dispatch = disp

	// ④ WS 网关：鉴权用真实 DeviceService.Authenticate（查 token_hash + 未吊销），
	//    Handler 接派发器（task.progress / task.result 才有归宿）
	gw := device.NewGateway(device.GatewayOptions{
		Hub:          hub,
		Authenticate: devSvc.Authenticate,
		Heartbeat:    opts.Heartbeat,
		OfflineAfter: opts.OfflineAfter,
		Handler:      disp.HandleFrame,
		PendingCount: disp.PendingCount,
	})
	devSvc.SetDisconnector(hub)

	// ⑤ 真实路由 + 真实中间件链（Auth / RateLimit / AccessLog 全在），
	//    这样「配对端点需用户令牌」「pair / ws 免登录」这两条契约都被真实验证。
	router := api.NewRouter(api.Deps{
		Version:     "device-e2e",
		Log:         observ.NewLogger("error", "text", os.Stderr),
		Devices:     devSvc,
		DeviceWS:    gw,
		Auth:        authSvc,
		AuthConfig:  api.AuthConfig{SessionTTL: time.Hour},
		VerifyToken: authSvc.VerifyToken,
	})

	// httptest 默认监听 127.0.0.1（**不是 localhost**，与项目约定一致）
	ts := httptest.NewServer(router)
	srv.URL = ts.URL
	srv.stopFns = append(srv.stopFns, ts.Close)

	return srv, nil
}
