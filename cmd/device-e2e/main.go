// Command device-e2e 是「服务端 ↔ 桌面端」跨仓联调脚本，从**服务端视角**把设备协同主链
// 真实跑一遍：配对 → WS 升级 → welcome → 心跳 → 任务派发 → 端侧执行 → 结果回执收敛。
//
// 设计要点（为什么这样写）：
//   - 默认**自包含**：脚本进程内起真实 Router + 真实 Gateway + 真实 Dispatcher + 临时 sqlite 库，
//     一条命令即可跑通，不需要用户先启动服务端。原因见 server.go 顶部注释。
//   - 端侧全部由脚本扮演（client.go），脚本只依赖 HTTP/WS 线上契约。
//   - device_token 属长期凭证，输出一律打码为「前 8 位 + ***」。
//
// 用法：go run ./cmd/device-e2e [-mode full|pair-only] [-external -base-url http://127.0.0.1:5611]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"harness-go/internal/device"
	"harness-go/internal/model"
)

// stepOutcome 单步结论（用于最终 PASS/FAIL 汇总）。
type stepOutcome struct {
	Name    string
	OK      bool
	Skipped bool
	Detail  string
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("device-e2e", flag.ContinueOnError)
	baseURL := fs.String("base-url", "http://127.0.0.1:5611",
		"外部模式下的服务端地址（**必须用 127.0.0.1，不用 localhost**）")
	user := fs.String("user", "e2e-admin", "登录用户名（自包含模式自动创建该账号）")
	password := fs.String("password", "e2e-password-123", "登录口令")
	mode := fs.String("mode", "full",
		"full=跑到任务派发与回执；pair-only=只跑到配对+WS 握手+心跳；"+
			"runner=拉起真实桌面端二进制做两端真实对撞（R1~R5；单包形态下调起 hd-host.exe 并自动注入 --runner）")
	external := fs.Bool("external", false,
		"连已启动的服务端（true）还是进程内自起（false，默认）。外部模式无派发端点，仅跑到心跳")
	runnerBin := fs.String("runner-bin", "",
		"runner 模式下的 hd-host 可执行文件路径（单包分发后不再有 hd-runner.exe）："+
			"空=自动探测 ../harness_desktop/target/{debug,release}/hd-host[.exe]，"+
			"启动时脚本会自动追加 --runner 让它以端侧身份运行")
	runnerNoDev := fs.Bool("runner-no-dev", false,
		"runner 模式不给二进制传 --dev（默认传，因为本地自包含服务端是 ws:// 明文）")
	dataDir := fs.String("data-dir", "", "自包含模式的数据目录（空=系统临时目录，退出即删）")
	verbose := fs.Bool("v", false, "打印每一帧的原始类型与关键负载（令牌已打码）")
	disableDevices := fs.Bool("disable-devices", false,
		"自包含模式下故意关闭设备域（devices.enabled=false），用于自检 503 device.disabled 的提示文案")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *mode != "full" && *mode != "pair-only" && *mode != "runner" {
		fmt.Fprintf(os.Stderr, "❌ -mode 只能是 full / pair-only / runner，当前 %q\n", *mode)
		return 2
	}
	if *mode == "runner" && *external {
		// 外部模式下拿不到 h.Hub / h.Dispatch（那是进程内装配才有的），
		// 而 runner 模式的核心断言恰恰是「服务端在线表 + 进程内派发」，
		// 所以这两者组合无法成立 —— 提前拦掉，别等到跑了 3 步才报 nil panic。
		fmt.Fprintln(os.Stderr,
			"❌ -mode runner 必须配合自包含服务端（不能与 -external 同用）："+
				"runner 模式需要读取进程内 Hub 在线表并直接调用 Dispatcher")
		return 2
	}

	// Ctrl+C 也能干净退出（临时库/临时目录必须被清理）
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cli := &http.Client{Timeout: 15 * time.Second}

	fmt.Println("========================================")
	fmt.Println(" 服务端 ↔ 桌面端 设备协同联调（device-e2e）")
	fmt.Println("========================================")

	// ---------- 前置：起服务端（或连外部服务端） ----------
	var (
		harness *harnessServer
		target  string
	)
	if *external {
		target = strings.TrimRight(*baseURL, "/")
		fmt.Printf("▶ 目标：外部服务端 %s（外部模式）\n", target)
		if err := pingHealth(ctx, cli, target); err != nil {
			// 外部模式最常见的失败就是「忘了先起服务端」，这里提前给出可执行建议，
			// 而不是让用户看到一条 dial tcp refused 的底层错误。
			fmt.Fprintf(os.Stderr, "❌ 无法连通 %s：%v\n", target, err)
			fmt.Fprintf(os.Stderr, "   请先启动服务端：go run ./cmd/harness-go（默认 127.0.0.1:5611），"+
				"并确认 -base-url 与监听地址一致（**用 127.0.0.1，不要用 localhost**）\n")
			return 1
		}
		fmt.Println("  ✓ /healthz 探活通过")
		fmt.Println("  ⚠ 外部模式没有 HTTP 派发端点，任务派发/回执步骤自动跳过（SKIP）")
	} else {
		fmt.Println("▶ 目标：进程内自包含服务端（真实 Router + 真实 Gateway + 真实 Dispatcher + 临时 sqlite）")
		if *dataDir != "" {
			fmt.Printf("  数据目录：%s\n", *dataDir)
		}
		srv, err := startHarnessServer(serverOptions{
			Username:       *user,
			Password:       *password,
			DataDir:        *dataDir,
			DevicesEnabled: !*disableDevices,
			PairTTL:        120 * time.Second,
			Heartbeat:      15 * time.Second,
			OfflineAfter:   45 * time.Second,
			TaskTTL:        10 * time.Second,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ 自包含服务端启动失败：%v\n", err)
			return 1
		}
		harness = srv
		defer harness.Close()
		target = srv.URL
		fmt.Printf("  监听：%s（127.0.0.1，非 localhost）\n", target)
	}

	r := &runner{outcomes: make([]stepOutcome, 0, 16), verbose: *verbose}
	r.runAll(ctx, cli, target, *user, *password, *mode, *external, harness, runnerFlags{
		BinPath: *runnerBin,
		Dev:     !*runnerNoDev,
	})
	return r.summary()
}

// runner 串起所有步骤并收集结论。
type runner struct {
	outcomes []stepOutcome
	verbose  bool
}

func (r *runner) pass(name, detail string) {
	r.outcomes = append(r.outcomes, stepOutcome{Name: name, OK: true, Detail: detail})
	fmt.Printf("  ✅ %s —— %s\n", name, detail)
}

func (r *runner) fail(name, detail string) {
	r.outcomes = append(r.outcomes, stepOutcome{Name: name, OK: false, Detail: detail})
	fmt.Printf("  ❌ %s —— %s\n", name, detail)
}

func (r *runner) skip(name, detail string) {
	r.outcomes = append(r.outcomes, stepOutcome{Name: name, Skipped: true, Detail: detail})
	fmt.Printf("  ⏭  %s —— SKIP：%s\n", name, detail)
}

// summary 打印 PASS/FAIL 汇总并返回进程退出码。
func (r *runner) summary() int {
	failed, skipped, passed := 0, 0, 0
	for _, o := range r.outcomes {
		switch {
		case o.Skipped:
			skipped++
		case o.OK:
			passed++
		default:
			failed++
		}
	}
	fmt.Println()
	fmt.Println("----------------------------------------")
	fmt.Printf("汇总：PASS %d / FAIL %d / SKIP %d\n", passed, failed, skipped)
	if failed > 0 {
		fmt.Println("结论：FAIL ❌")
		fmt.Println("下列步骤失败：")
		for _, o := range r.outcomes {
			if !o.OK && !o.Skipped {
				fmt.Printf("  - %s：%s\n", o.Name, o.Detail)
			}
		}
		return 1
	}
	if skipped > 0 {
		// 全 PASS 但有 SKIP 时，必须显式声明「哪些能力没被本次运行覆盖」，
		// 否则读者会把「退出码 0」误当成「全链已验证」。
		fmt.Println("提示：存在 SKIP 步骤，本次运行**未**覆盖上述能力（详见上方 ⏭ 行）")
		fmt.Println("结论：PASS（部分能力 SKIPPED）✅ 已执行的步骤全部通过")
		return 0
	}
	fmt.Println("结论：PASS ✅ 设备协同主链（配对 → WS → 心跳 → 派发 → 回执）全部通过")
	return 0
}

// runAll 按「配对 → 握手 → 心跳 → 派发 → 回执」顺序推进；任一步失败即停止后续步骤。
//
// -mode runner 时前 3 步（登录 / 签发配对码 / 配对拿令牌）完全复用，之后**分支**到 runner.go：
// 后续握手、心跳、派发、回执全部由**真实桌面端进程**（单包 hd-host.exe + `--runner`）完成，
// 脚本只做服务端视角断言。
// 这样两种模式共享同一份前置代码，避免出现「两套配对逻辑各写一遍然后悄悄漂移」。
func (r *runner) runAll(ctx context.Context, cli *http.Client, baseURL, user, password, mode string,
	external bool, harness *harnessServer, flags runnerFlags) {

	// ---------- 前置：登录拿用户令牌（管理端接口的凭据） ----------
	fmt.Println("\n[0] 登录拿用户令牌")
	userToken, err := login(ctx, cli, baseURL, user, password)
	if err != nil {
		r.fail("登录", explainHTTP(err))
		return
	}
	r.pass("登录", "已取得用户令牌 "+maskToken(userToken))

	// ---------- ① 签发配对码 ----------
	fmt.Println("\n[1] 签发配对码（管理端，需用户令牌）")
	pc, err := issuePairCode(ctx, cli, baseURL, userToken, "device-e2e 桌面端", "desktop")
	if err != nil {
		r.fail("签发配对码", explainHTTP(err))
		return
	}
	if len(pc.Code) != 6 {
		r.fail("签发配对码", fmt.Sprintf("配对码应为 6 位，实际 %q（长度 %d）", pc.Code, len(pc.Code)))
		return
	}
	if pc.TTLSec <= 0 {
		r.fail("签发配对码", fmt.Sprintf("ttl_s 应 > 0，实际 %d", pc.TTLSec))
		return
	}
	r.pass("签发配对码", fmt.Sprintf("code=%s ttl_s=%d expires_at=%d",
		pc.Code, pc.TTLSec, pc.ExpiresAt))

	// ---------- ② 配对换 device_token ----------
	fmt.Println("\n[2] 端侧凭码配对（免登录，令牌明文仅此一次）")
	peerName := "device-e2e 端侧（脚本扮演）"
	if mode == "runner" {
		peerName = "device-e2e 端侧（真实 hd-host --runner 进程）"
	}
	paired, err := pairDeviceNamed(ctx, cli, baseURL, pc.Code, peerName)
	if err != nil {
		r.fail("配对", explainHTTP(err))
		return
	}
	if !strings.HasPrefix(paired.DeviceToken, "hgd_") {
		// 令牌前缀是跨仓约定（桌面端/移动端据此识别「这是设备令牌不是用户令牌」）
		r.fail("配对", fmt.Sprintf("device_token 前缀不符合约定 hgd_，实际 %s", maskToken(paired.DeviceToken)))
		return
	}
	if paired.ProtocolVersion <= 0 {
		r.fail("配对", fmt.Sprintf("protocol_version 应 > 0，实际 %d", paired.ProtocolVersion))
		return
	}
	r.pass("配对", fmt.Sprintf("device_id=%s token=%s ws_path=%s protocol_version=%d",
		paired.DeviceID, maskToken(paired.DeviceToken), paired.WSPath, paired.ProtocolVersion))

	// ---------- 分支：真实 Runner 二进制模式 ----------
	// 必须在这里就分叉：下面 [3] 会用**同一 device_id** 再建一条脚本自己的 WS 连接，
	// 而 Hub.Register 对同 device_id 是「替换」语义 —— 两条连接会互相顶掉，
	// 导致真实 Runner 被判离线，联调结论彻底失真。
	if mode == "runner" {
		if harness == nil {
			// 理论上 main 已拦掉 -external，这里兜底防止未来有人放宽校验。
			r.skip("真实 Runner 二进制", "runner 模式需要进程内服务端（Hub/Dispatcher），当前无自包含装配")
			return
		}
		r.runRunnerMode(ctx, harness, baseURL, paired, flags)
		return
	}

	// ---------- ③ WS 升级 ----------
	fmt.Println("\n[3] WS 升级（设备令牌经 ?token= 传入，升级前鉴权）")
	wsCtx, wsCancel := context.WithCancel(ctx)
	defer wsCancel()
	conn, status, err := dialDevice(wsCtx, baseURL, paired.DeviceToken, paired.ProtocolVersion)
	if err != nil {
		r.fail("WS 升级", err.Error())
		return
	}
	defer conn.closeNormally()
	r.pass("WS 升级", fmt.Sprintf("HTTP %d Switching Protocols（101）", status))

	// ---------- ④ welcome ----------
	fmt.Println("\n[4] 收 welcome（服务端下发心跳节奏与协议版本）")
	var welcome device.WelcomePayload
	env, err := conn.waitFrame(device.MsgWelcome, 5*time.Second, r.frameLogger("跳过"))
	if err != nil {
		r.fail("收 welcome", err.Error())
		return
	}
	if err := env.Decode(&welcome); err != nil {
		r.fail("收 welcome", "解析 welcome 负载失败："+err.Error())
		return
	}
	if welcome.HeartbeatS <= 0 {
		r.fail("收 welcome", fmt.Sprintf("heartbeat_s 应 > 0，实际 %d", welcome.HeartbeatS))
		return
	}
	if welcome.OfflineAfterS < welcome.HeartbeatS {
		r.fail("收 welcome", fmt.Sprintf("offline_after_s(%d) 必须 >= heartbeat_s(%d)，否则端侧必然误判断线",
			welcome.OfflineAfterS, welcome.HeartbeatS))
		return
	}
	if welcome.ProtocolVersion != device.CurrentProtocolVersion {
		r.fail("收 welcome", fmt.Sprintf("protocol_version 期望 %d，实际 %d",
			device.CurrentProtocolVersion, welcome.ProtocolVersion))
		return
	}
	if welcome.DeviceID != paired.DeviceID {
		r.fail("收 welcome", fmt.Sprintf("welcome.device_id(%s) 与配对返回的 device_id(%s) 不一致",
			welcome.DeviceID, paired.DeviceID))
		return
	}
	r.pass("收 welcome", fmt.Sprintf("heartbeat_s=%d offline_after_s=%d protocol_version=%d pending=%d",
		welcome.HeartbeatS, welcome.OfflineAfterS, welcome.ProtocolVersion, welcome.Pending))

	// ---------- ⑤ ping → pong ----------
	fmt.Println("\n[5] 发 ping → 收 pong（断言 pong 回带同一 id）")
	pingID := fmt.Sprintf("ping-%d", time.Now().UnixNano())
	if err := conn.sendEnvelope(device.MsgPing, pingID, device.PingPayload{
		ProtocolVersion:  device.CurrentProtocolVersion,
		CapabilitiesHash: "e2e-caps-hash",
		Load:             json.RawMessage(`{"inflight":0}`),
	}); err != nil {
		r.fail("ping/pong", "发送 ping 失败："+err.Error())
		return
	}
	pongEnv, err := conn.waitFrame(device.MsgPong, 5*time.Second, r.frameLogger("跳过"))
	if err != nil {
		r.fail("ping/pong", err.Error())
		return
	}
	var pong device.PongPayload
	if err := pongEnv.Decode(&pong); err != nil {
		r.fail("ping/pong", "解析 pong 负载失败："+err.Error())
		return
	}
	if pongEnv.ID != pingID {
		r.fail("ping/pong", fmt.Sprintf("pong.id(%q) 与 ping.id(%q) 不一致：端侧无法做请求应答配对",
			pongEnv.ID, pingID))
		return
	}
	r.pass("ping/pong", fmt.Sprintf("id=%s 原样回带，server_time=%d pending=%d protocol_version=%d",
		pongEnv.ID, pong.ServerTime, pong.Pending, pong.ProtocolVersion))

	if mode == "pair-only" {
		r.skip("任务派发", "-mode=pair-only，按参数要求在心跳后停止")
		r.skip("端侧执行回执", "-mode=pair-only")
		return
	}
	if external || harness == nil {
		r.skip("任务派发", "外部模式：服务端目前没有触发 dispatcher.Dispatch 的 HTTP 端点，无法远程派发（见 README）")
		r.skip("端侧执行回执", "外部模式：同上")
		return
	}

	// ---------- ⑥ 派发任务 → 收 task.dispatch ----------
	fmt.Println("\n[6] 服务端派发任务 → 端侧收 task.dispatch")
	const toolName = "fs.list"
	taskID := fmt.Sprintf("te-e2e-%d", time.Now().UnixNano())
	runID := fmt.Sprintf("run-e2e-%d", time.Now().UnixNano())

	type dispatchOutcome struct {
		res device.TaskResult
		err error
	}
	dispatchCh := make(chan dispatchOutcome, 1)
	go func() {
		// 注意：服务端**无 HTTP 派发端点**（Dispatch 只在单测里被调用过），
		// 故这里直接在进程内调用派发器。这是本脚本自包含装配的根本原因。
		res, derr := harness.Dispatch.Dispatch(ctx, device.TaskRequest{
			TaskID:      taskID,
			RunID:       runID,
			ToolEventID: taskID, // 跨仓约定：task_id 恒等于 tool_event_id（端侧据此做幂等）
			ToolName:    toolName,
			Args:        json.RawMessage(`{"path":"."}`),
			Timeout:     10 * time.Second,
		})
		dispatchCh <- dispatchOutcome{res: res, err: derr}
	}()

	dispEnv, err := conn.waitFrame(device.MsgTaskDispatch, 5*time.Second, r.frameLogger("跳过"))
	if err != nil {
		r.fail("派发任务", err.Error())
		return
	}
	var task device.TaskDispatchPayload
	if err := dispEnv.Decode(&task); err != nil {
		r.fail("派发任务", "解析 task.dispatch 负载失败："+err.Error())
		return
	}
	if task.TaskID != taskID {
		r.fail("派发任务", fmt.Sprintf("task.dispatch.task_id(%q) 与请求 task_id(%q) 不一致", task.TaskID, taskID))
		return
	}
	if task.ToolEventID != task.TaskID {
		r.fail("派发任务", fmt.Sprintf("不变量被破坏：task_id(%q) 必须等于 tool_event_id(%q)",
			task.TaskID, task.ToolEventID))
		return
	}
	if task.ToolName != toolName {
		r.fail("派发任务", fmt.Sprintf("tool_name 期望 %q，实际 %q", toolName, task.ToolName))
		return
	}
	if task.Deadline <= time.Now().UnixMilli() {
		r.fail("派发任务", fmt.Sprintf("deadline(%d) 已过期，端侧会直接放弃执行", task.Deadline))
		return
	}
	r.pass("派发任务", fmt.Sprintf("task_id=%s tool=%s attempt=%d deadline=%s",
		task.TaskID, task.ToolName, task.Attempt, time.UnixMilli(task.Deadline).Format("15:04:05")))

	// ---------- ⑦ 端侧执行 + progress + result ----------
	fmt.Println("\n[7] 端侧执行（脚本扮演）→ 回 task.progress / task.result")
	if err := conn.sendEnvelope(device.MsgTaskProgress, taskID, device.TaskProgressPayload{
		TaskID: taskID, RunID: task.RunID, ToolEventID: task.ToolEventID, Chunk: "执行中 50%",
	}); err != nil {
		r.fail("端侧执行回执", "发送 task.progress 失败："+err.Error())
		return
	}
	if err := conn.sendEnvelope(device.MsgTaskResult, taskID, device.TaskResultPayload{
		TaskID:      taskID,
		RunID:       task.RunID,
		ToolEventID: task.ToolEventID,
		Status:      device.TaskStatusSuccess,
		Result:      json.RawMessage(`{"entries":["README.md","go.mod"],"count":2}`),
		// 故意谎称 trusted：服务端必须把它覆盖成 untrusted（不变量 4）
		SourceTrust: string(model.TrustTrusted),
	}); err != nil {
		r.fail("端侧执行回执", "发送 task.result 失败："+err.Error())
		return
	}
	r.pass("端侧执行回执", "task.progress + task.result 已上行（progress 不落库，result 收敛终态）")

	// ---------- ⑧ 服务端侧收敛终态 ----------
	fmt.Println("\n[8] 服务端侧收敛终态（Dispatch 返回值）")
	select {
	case out := <-dispatchCh:
		if out.err != nil {
			r.fail("服务端终态", "Dispatch 返回错误："+out.err.Error())
			return
		}
		res := out.res
		if res.Status != model.ToolOK {
			r.fail("服务端终态", fmt.Sprintf("期望 status=ok，实际 %s（error=%q timeout=%v degraded=%v）",
				res.Status, res.Error, res.Timeout, res.Degraded))
			return
		}
		if res.SourceTrust != model.TrustUntrusted {
			r.fail("服务端终态", fmt.Sprintf("端侧结果必须被强制为 untrusted，实际 %s", res.SourceTrust))
			return
		}
		if res.DeviceID != paired.DeviceID {
			r.fail("服务端终态", fmt.Sprintf("收敛设备(%s) 与配对设备(%s) 不一致", res.DeviceID, paired.DeviceID))
			return
		}
		var body map[string]any
		if err := json.Unmarshal(res.Result, &body); err != nil {
			r.fail("服务端终态", "结果 JSON 解析失败："+err.Error())
			return
		}
		r.pass("服务端终态", fmt.Sprintf("status=%s source_trust=%s device=%s 耗时=%s result=%s",
			res.Status, res.SourceTrust, res.DeviceID,
			res.Duration.Round(time.Millisecond), truncate(string(res.Result), 80)))
	case <-time.After(15 * time.Second):
		r.fail("服务端终态", "等待 Dispatch 收敛超时（15s）：端侧已回执但服务端未收敛，检查 Gateway.Handler 是否接了派发器")
		return
	}

	// ---------- 幂等兜底：迟到/重复回执不得改变已收敛终态 ----------
	fmt.Println("\n[9] 幂等校验：重复上报同一 task.result 不得改变终态")
	if err := conn.sendEnvelope(device.MsgTaskResult, taskID, device.TaskResultPayload{
		TaskID: taskID, RunID: runID, ToolEventID: taskID,
		Status: device.TaskStatusFailed, Error: "重复上报（应被幂等忽略）",
	}); err != nil {
		r.fail("幂等校验", "发送重复 task.result 失败："+err.Error())
		return
	}
	// 给服务端读循环一点时间去处理这一帧（本帧不产生回执，只能等）
	time.Sleep(300 * time.Millisecond)
	res2, err := harness.Dispatch.Dispatch(ctx, device.TaskRequest{
		TaskID: taskID, RunID: runID, ToolEventID: taskID, ToolName: toolName,
		Args: json.RawMessage(`{"path":"."}`),
	})
	if err != nil {
		r.fail("幂等校验", "重派同一 task_id 返回错误："+err.Error())
		return
	}
	if res2.Status != model.ToolOK {
		r.fail("幂等校验", fmt.Sprintf("重复回执污染了终态：期望仍是 ok，实际 %s（error=%q）",
			res2.Status, res2.Error))
		return
	}
	r.pass("幂等校验", "同一 task_id 重派直接命中幂等缓存，终态仍为 ok")
}

// frameLogger 返回「跳过无关帧」的调试输出（-v 时才打印）。
func (r *runner) frameLogger(prefix string) func(*device.Envelope) {
	if !r.verbose {
		return nil
	}
	return func(env *device.Envelope) {
		fmt.Printf("  · %s帧 type=%s id=%s v=%d\n", prefix, env.Type, env.ID, env.V)
	}
}
