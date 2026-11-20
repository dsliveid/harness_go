// Command fake-runner 是阶段 C 的**假 Runner**（端侧执行面替身），用于端到端验证
// 设备派发全链路：配对 → WS 长连接 → task.dispatch → task.progress → task.result。
//
// 用法（端到端验收 dev/e2e-c.ps1 走这条路径）：
//
//	# ① 管理端取配对码（需用户令牌）
//	curl -X POST -H "Authorization: Bearer <token>" http://127.0.0.1:18080/api/v1/devices/pair-codes
//	# ② 起假 Runner（凭码配对，令牌缓存到 .harness/tmp/fake-runner-token）
//	go run ./cmd/fake-runner -server http://127.0.0.1:18080 -code 482913
//	# ③ 重连复用已配对令牌（无需再配对）
//	go run ./cmd/fake-runner -token hgd_xxx
//
// 行为约定（与 internal/device 协议严格对齐，勿擅自扩展）：
//   - 首帧由服务端下发 welcome，本程序据此取心跳节奏
//   - 每 heartbeat 发 ping{load, capabilities_hash, protocol_version}
//   - 收到 task.dispatch → 先回 task.progress（分块）→ 再回 task.result
//   - **幂等**：已完成 task_id 缓存在内存，重复派发直接回缓存结果且不重复执行
//   - 结果一律上报 source_trust=untrusted（端侧结果不可信，由服务端裁决）
//   - 被服务端以 device.replaced / device.revoked 关闭时不再重连（避免抢连接）
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"harness-go/internal/device"
	"harness-go/internal/model"
)

// runnerConfig 假 Runner 运行参数。
type runnerConfig struct {
	server     string
	code       string
	token      string
	tokenFile  string
	name       string
	kind       string
	platform   string
	caps       string
	heartbeat  time.Duration
	taskDelay  time.Duration
	failTools  string
	fsRoots    string
	once       bool
	maxRetries int
}

// fakeRunner 假 Runner 主状态。
type fakeRunner struct {
	cfg  runnerConfig
	log  func(format string, args ...any)
	http *http.Client

	deviceID string
	token    string
	caps     []model.Capability

	mu     sync.Mutex
	done   map[string]device.TaskResultPayload // 幂等缓存：task_id → 终态
	tasks  int
	errors int
}

func main() {
	cfg := runnerConfig{}
	flag.StringVar(&cfg.server, "server", "http://127.0.0.1:18080", "服务端地址（http/https）")
	flag.StringVar(&cfg.code, "code", "", "6 位配对码（首次配对；与 -token 二选一）")
	flag.StringVar(&cfg.token, "token", "", "已配对的设备令牌（重连复用；与 -code 二选一）")
	flag.StringVar(&cfg.tokenFile, "token-file", "", "配对成功后把设备令牌写入该文件（可选）")
	flag.StringVar(&cfg.name, "name", "假 Runner", "设备名称（配对时上报）")
	flag.StringVar(&cfg.kind, "kind", "desktop", "设备形态：desktop|tablet|mobile|cli|browser")
	flag.StringVar(&cfg.platform, "platform", "", "平台标识（默认 runtime.GOOS）")
	flag.StringVar(&cfg.caps, "caps", "fs.read,fs.write,run_command,workspace.list", "能力项（逗号分隔，可带 @版本）")
	flag.DurationVar(&cfg.heartbeat, "heartbeat", 15*time.Second, "心跳间隔（welcome 未下发时的兜底）")
	flag.DurationVar(&cfg.taskDelay, "task-delay", 200*time.Millisecond, "模拟单任务执行耗时")
	flag.StringVar(&cfg.failTools, "fail-tools", "", "指定工具名一律返回失败（逗号分隔，便于验证失败路径）")
	flag.StringVar(&cfg.fsRoots, "fs-roots", "", "目录列举白名单根（逗号分隔；默认为当前工作目录）")
	flag.BoolVar(&cfg.once, "once", false, "完成 1 个任务后退出（脚本验收用）")
	flag.IntVar(&cfg.maxRetries, "max-retries", 5, "断线重连次数上限（<0 表示无限重连）")
	flag.Parse()

	if cfg.fsRoots == "" {
		// 默认用 cwd：既能让服务端链路端到端验证，又不会把整机文件系统随意暴露出去。
		if wd, wErr := os.Getwd(); wErr == nil {
			cfg.fsRoots = wd
		}
	}

	cfg.server = strings.TrimRight(strings.TrimSpace(cfg.server), "/")
	if cfg.server == "" {
		fatalf("-server 不能为空")
	}
	if cfg.token == "" && cfg.code == "" {
		// 允许从 token 文件恢复，便于脚本二次运行
		if cfg.tokenFile != "" {
			if b, err := os.ReadFile(cfg.tokenFile); err == nil {
				cfg.token = strings.TrimSpace(string(b))
			}
		}
	}
	if cfg.token == "" && cfg.code == "" {
		fatalf("必须提供 -code（首次配对）或 -token（已配对令牌）")
	}
	if cfg.platform == "" {
		cfg.platform = defaultPlatform()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	r := &fakeRunner{
		cfg:  cfg,
		log:  func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
		http: &http.Client{Timeout: 10 * time.Second},
		caps: parseCapabilities(cfg.caps),
		done: make(map[string]device.TaskResultPayload),
	}
	if err := r.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fatalf("%v", err)
	}
	r.log("FAKE_RUNNER_EXIT tasks=%d errors=%d", r.tasks, r.errors)
}

// run 主循环：配对（如需要）→ 连接 → 断线按指数退避重连。
func (r *fakeRunner) run(ctx context.Context) error {
	if r.cfg.token == "" {
		if err := r.pair(ctx); err != nil {
			return err
		}
	} else {
		r.token = r.cfg.token
		r.log("使用既有设备令牌连接（device_id 待 welcome 回填）")
	}

	retries := 0
	backoff := time.Second
	for {
		err := r.session(ctx)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return context.Canceled
		}
		if errors.Is(err, errTerminal) {
			r.log("服务端要求本设备停止重连：%v", err)
			return nil
		}
		if r.cfg.once && r.tasks > 0 {
			return nil
		}
		if r.cfg.maxRetries >= 0 && retries >= r.cfg.maxRetries {
			return fmt.Errorf("重连次数已达上限（%d）：%w", retries, err)
		}
		retries++
		r.log("连接中断（%v），%s 后重连（第 %d 次）", err, backoff, retries)
		select {
		case <-ctx.Done():
			return context.Canceled
		case <-time.After(backoff):
		}
		backoff = time.Duration(math.Min(float64(backoff)*2, float64(30*time.Second)))
	}
}

// errTerminal 服务端明确要求不再重连（被替换/被吊销/协议不兼容）。
var errTerminal = errors.New("fake-runner: 连接被服务端终止，不再重连")

// pair 凭配对码换取设备令牌（明文仅返回一次，随后只落内存/可选文件）。
func (r *fakeRunner) pair(ctx context.Context) error {
	// 服务端 decodeJSON 拒绝未知字段 → 能力项只能上报 {name, version}
	caps := make([]map[string]string, 0, len(r.caps))
	for _, c := range r.caps {
		caps = append(caps, map[string]string{"name": c.Name, "version": c.Version})
	}
	body := map[string]any{
		"code":         strings.TrimSpace(r.cfg.code),
		"name":         r.cfg.name,
		"kind":         r.cfg.kind,
		"platform":     r.cfg.platform,
		"app_version":  "fake-runner/1.0",
		"capabilities": caps,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.server+"/api/v1/devices/pair", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("配对请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("配对失败（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		DeviceID        string `json:"device_id"`
		DeviceToken     string `json:"device_token"`
		WSPath          string `json:"ws_path"`
		ProtocolVersion int    `json:"protocol_version"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("解析配对响应失败: %w", err)
	}
	if out.DeviceID == "" || out.DeviceToken == "" {
		return fmt.Errorf("配对响应缺少 device_id / device_token")
	}
	r.deviceID = out.DeviceID
	r.token = out.DeviceToken
	r.log("FAKE_RUNNER_PAIRED device_id=%s protocol_version=%d", out.DeviceID, out.ProtocolVersion)
	if r.cfg.tokenFile != "" {
		if mkErr := os.MkdirAll(filepath.Dir(r.cfg.tokenFile), 0o750); mkErr == nil {
			if wErr := os.WriteFile(r.cfg.tokenFile, []byte(out.DeviceToken), 0o600); wErr != nil {
				r.log("警告：写入令牌文件失败：%v", wErr)
			}
		}
	}
	return nil
}

// session 一次完整连接会话（返回后由 run 决定是否重连）。
func (r *fakeRunner) session(ctx context.Context) error {
	wsURL := strings.Replace(r.cfg.server, "http", "ws", 1) +
		"/api/v1/devices/ws?token=" + r.token
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			// 令牌已失效（设备被吊销或换库）→ 明确终止，避免无限重试刷日志
			return fmt.Errorf("%w: 设备令牌已失效（HTTP 401）", errTerminal)
		}
		return fmt.Errorf("WS 拨号失败: %w", err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	r.log("WS 已连接：%s", wsURL)

	hb := r.cfg.heartbeat
	// 独立 ctx 管理本会话读写与心跳
	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	// 写串行化：心跳协程与结果回传协程都会写同一连接
	var writeMu sync.Mutex
	writeFrame := func(env device.Envelope) error {
		raw, mErr := json.Marshal(env)
		if mErr != nil {
			return mErr
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		wCtx, wCancel := context.WithTimeout(sessCtx, 10*time.Second)
		defer wCancel()
		return ws.Write(wCtx, websocket.MessageText, raw)
	}

	heartbeatOnce := func() {
		env := device.MustEnvelope(device.MsgPing, "", device.PingPayload{
			Load:             json.RawMessage(fmt.Sprintf(`{"tasks":%d}`, r.taskCount())),
			CapabilitiesHash: device.CapabilitiesHash(r.caps),
			ProtocolVersion:  device.CurrentProtocolVersion,
		})
		if wErr := writeFrame(env); wErr != nil {
			r.log("心跳发送失败：%v", wErr)
		}
	}

	// 心跳协程（welcome 到达后按其节奏覆盖）
	go func() {
		ticker := time.NewTicker(hb)
		defer ticker.Stop()
		for {
			select {
			case <-sessCtx.Done():
				return
			case <-ticker.C:
				heartbeatOnce()
			}
		}
	}()

	for {
		readCtx, readCancel := context.WithTimeout(sessCtx, 3*hb+30*time.Second)
		_, data, rErr := ws.Read(readCtx)
		readCancel()
		if rErr != nil {
			if sessCtx.Err() != nil || ctx.Err() != nil {
				return context.Canceled
			}
			return fmt.Errorf("读取中断: %w", rErr)
		}

		var env device.Envelope
		if uErr := json.Unmarshal(data, &env); uErr != nil {
			r.log("收到非法帧（忽略）：%v", uErr)
			continue
		}

		switch env.Type {
		case device.MsgWelcome:
			var wp device.WelcomePayload
			_ = env.Decode(&wp)
			if r.deviceID == "" {
				r.deviceID = wp.DeviceID
			}
			if wp.HeartbeatS > 0 {
				hb = time.Duration(wp.HeartbeatS) * time.Second
			}
			r.log("FAKE_RUNNER_READY device_id=%s server_v=%d heartbeat_s=%d offline_after_s=%d pending=%d",
				wp.DeviceID, wp.ProtocolVersion, wp.HeartbeatS, wp.OfflineAfterS, wp.Pending)
			heartbeatOnce()

		case device.MsgPong:
			// 心跳应答：无需处理（服务端已刷新 last_seen_at）

		case device.MsgTaskDispatch:
			var dp device.TaskDispatchPayload
			if dErr := env.Decode(&dp); dErr != nil {
				r.log("task.dispatch 负载非法：%v", dErr)
				continue
			}
			go r.handleTask(sessCtx, writeFrame, dp)

		case device.MsgFsList:
			// 目录列举（L1）：最小实现，只列目录、不读文件内容。
			// 目的：让服务端链路（API → PathQuery → 帧 → 应答 → DTO）在没有桌面端
			// 参与时也能在本仓端到端验证 —— 此前 fs.list 无人应答，只能等满 3 秒超时。
			var fp device.FsListPayload
			if dErr := env.Decode(&fp); dErr != nil {
				r.log("fs.list 负载非法：%v", dErr)
				continue
			}
			// 回帧必须同步由本协程写出（不另起 goroutine）：列举极轻，
			// 且帧 ID 必须逐字回传请求的 id，否则服务端 reqID 配不上只能超时。
			resp := device.MustEnvelope(device.MsgFsListResult, env.ID, r.fsList(fp))
			if wErr := writeFrame(resp); wErr != nil {
				r.log("fs.list.result 发送失败：%v", wErr)
			}

		case device.MsgError:
			var ep device.ErrorPayload
			_ = env.Decode(&ep)
			r.log("服务端错误帧：code=%s msg=%s fatal=%v", ep.Code, ep.Message, ep.Fatal)
			switch ep.Code {
			case device.FrameCodeReplaced, device.FrameCodeRevoked, device.FrameCodeProtocolMismatch:
				return fmt.Errorf("%w: %s", errTerminal, ep.Code)
			}
			if ep.Fatal {
				return fmt.Errorf("服务端以 fatal 关闭连接：%s", ep.Code)
			}

		case device.MsgLeaseRevoked:
			var lp device.LeaseRevokedPayload
			_ = env.Decode(&lp)
			r.log("会话租约被抢占：session=%s epoch=%d reason=%s", lp.SessionID, lp.Epoch, lp.Reason)

		default:
			r.log("忽略未知帧：%s", env.Type)
		}
	}
}

// handleTask 执行一个任务并回传进度与终态（幂等：同一 task_id 只执行一次）。
func (r *fakeRunner) handleTask(ctx context.Context, writeFrame func(device.Envelope) error, p device.TaskDispatchPayload) {
	taskID := strings.TrimSpace(p.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(p.ToolEventID)
	}
	// 幂等铁律：task_id = tool_event_id，重复派发（断线重派）必须回放同一结果
	if cached, ok := r.cachedResult(taskID); ok {
		r.log("TASK_REPLAY task_id=%s（幂等回放，不重复执行）", taskID)
		env := device.MustEnvelope(device.MsgTaskResult, taskID, cached)
		env.RunID = p.RunID
		if err := writeFrame(env); err != nil {
			r.log("回放结果发送失败：%v", err)
		}
		return
	}

	r.log("TASK_START task_id=%s tool=%s attempt=%d deadline=%d run_id=%s",
		taskID, p.ToolName, p.Attempt, p.Deadline, p.RunID)

	// ① 分块进度（端侧只产事件，服务端不落库）
	steps := []string{"准备参数", "执行中", "整理结果"}
	for i, step := range steps {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.cfg.taskDelay):
		}
		env := device.MustEnvelope(device.MsgTaskProgress, taskID, device.TaskProgressPayload{
			TaskID:      taskID,
			RunID:       p.RunID,
			ToolEventID: p.ToolEventID,
			Chunk:       fmt.Sprintf("%d/%d %s", i+1, len(steps), step),
		})
		env.RunID = p.RunID
		if err := writeFrame(env); err != nil {
			r.log("进度发送失败：%v", err)
			return
		}
	}

	// ② 终态（端侧结果一律 untrusted）
	result := r.buildResult(p)
	if _, fail := r.failSet()[p.ToolName]; fail {
		result = device.TaskResultPayload{
			TaskID:      taskID,
			RunID:       p.RunID,
			ToolEventID: p.ToolEventID,
			Status:      device.TaskStatusFailed,
			Error:       "fake-runner: 按 -fail-tools 指定返回失败",
			SourceTrust: string(model.TrustUntrusted),
		}
		r.bumpErrors()
	}

	r.storeResult(taskID, result)
	env := device.MustEnvelope(device.MsgTaskResult, taskID, result)
	env.RunID = p.RunID
	if err := writeFrame(env); err != nil {
		r.log("结果发送失败：%v", err)
		return
	}
	r.bumpTasks()
	r.log("TASK_DONE task_id=%s status=%s tool=%s", taskID, result.Status, p.ToolName)
	if r.cfg.once {
		// 由 run 循环在会话返回后退出；这里直接取消会话即可
		os.Exit(exitCodeOK)
	}
}

// exitCodeOK 单任务模式正常退出码。
const exitCodeOK = 0

// fsList 最小目录列举实现（应答 fs.list）。
//
// ## 必须守住的四条契约（对齐 docs/工作区宿主与目录选择对接约定.md §3.3）
//
//  1. **separator 用宿主真实分隔符**（filepath.Separator）：前端据此切面包屑，
//     猜错平台会让路径被错误切分；
//  2. **parent 到根层必须是 nil**，绝不返回 ".."（前端会把它当可点入口，
//     一旦点了就跑到白名单之外）；
//  3. **dirs 只含目录**，绝不读文件内容（这是目录选择器，不是文件浏览器）；
//  4. **只列白名单根之内**：越界一律回 code=path_denied，不存在回 path_not_found，
//     与真实端侧的错误语义保持一致，从而服务端的 translate 分支也能被真实覆盖。
//
// 另外 with_roots 为真时（服务端 path 传空串 = 未定位）回 roots 让用户从根开始浏览。
func (r *fakeRunner) fsList(p device.FsListPayload) device.FsListResultPayload {
	roots := r.fsRootPaths()
	sep := string(filepath.Separator)
	res := device.FsListResultPayload{Separator: sep, Path: ""}
	if p.WithRoots || strings.TrimSpace(p.Path) == "" {
		res.Roots = fsEntriesOf(roots)
		res.Dirs = []device.FsDirEntry{}
		return res
	}

	target := filepath.Clean(strings.TrimSpace(p.Path))
	if !fsWithinAnyRoot(target, roots) {
		// 越界：如实回错，绝不降级成空列表（否则用户以为目录是空的）。
		res.Error = "路径不在允许列举的白名单内"
		res.Code = "path_denied"
		return res
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		res.Error = "目录不存在或不可读"
		res.Code = "path_not_found"
		return res
	}

	res.Path = target
	dirs := make([]device.FsDirEntry, 0, len(entries))
	for _, e := range entries {
		// 只收目录：e.IsDir() 对符号链接恒为 false（不跟随），
		// 因此软链不会泄漏到目录列表里（与服务端 L0 的 fsListSubdirs 口径一致）。
		if !e.IsDir() {
			continue
		}
		dirs = append(dirs, device.FsDirEntry{Name: e.Name(), Path: filepath.Join(target, e.Name())})
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	// 上限与端侧 fsMaxDirs 对齐（500），超出即截断并标记。
	if len(dirs) > fsListMaxDirs {
		dirs = dirs[:fsListMaxDirs]
		res.Truncated = true
	}
	res.Dirs = dirs
	// parent 计算：已到白名单根时返回 nil，且上级必须仍在白名单内才给出。
	for _, root := range roots {
		if filepath.Clean(root) == target {
			res.Parent = nil
			return res
		}
	}
	up := filepath.Dir(target)
	if up != target && fsWithinAnyRoot(up, roots) {
		res.Parent = &up
	}
	return res
}

// fsListMaxDirs 单次列举返回的目录上限（与桌面端 hd-core 的 FS_MAX_DIRS 一致）。
const fsListMaxDirs = 500

// fsRootPaths 解析 -fs-roots 为白名单根（均已 Clean + Abs 化）。
func (r *fakeRunner) fsRootPaths() []string {
	var out []string
	seen := make(map[string]struct{})
	for _, raw := range strings.Split(r.cfg.fsRoots, ",") {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	return out
}

// fsEntriesOf 把路径列表转成目录项（显示名取末级名，盘符根取 "C:"）。
func fsEntriesOf(paths []string) []device.FsDirEntry {
	out := make([]device.FsDirEntry, 0, len(paths))
	for _, p := range paths {
		name := filepath.Base(p)
		if name == "" || name == "." || name == string(filepath.Separator) {
			name = filepath.VolumeName(p)
			if name == "" {
				name = p
			}
		}
		out = append(out, device.FsDirEntry{Name: name, Path: p})
	}
	return out
}

// fsWithinAnyRoot 判断路径是否在任一白名单根内（大小写敏感按平台原生语义）。
func fsWithinAnyRoot(target string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, target)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)) {
			return true
		}
	}
	return false
}

// buildResult 构造端侧结果负载（假执行：原样回显工具名与参数）。
func (r *fakeRunner) buildResult(p device.TaskDispatchPayload) device.TaskResultPayload {
	payload := map[string]any{
		"executor":  "fake-runner",
		"device_id": r.deviceID,
		"tool":      p.ToolName,
		"echo":      json.RawMessage(p.Args),
		"finished":  time.Now().UnixMilli(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = json.RawMessage(`{"executor":"fake-runner"}`)
	}
	return device.TaskResultPayload{
		TaskID:      p.TaskID,
		RunID:       p.RunID,
		ToolEventID: p.ToolEventID,
		Status:      device.TaskStatusSuccess,
		Result:      raw,
		SourceTrust: string(model.TrustUntrusted),
	}
}

// cachedResult 幂等缓存查询。
func (r *fakeRunner) cachedResult(taskID string) (device.TaskResultPayload, bool) {
	if taskID == "" {
		return device.TaskResultPayload{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.done[taskID]
	return v, ok
}

// storeResult 幂等缓存写入（上限 256 条，超出随机淘汰最旧无意义，这里简单清理半数）。
func (r *fakeRunner) storeResult(taskID string, p device.TaskResultPayload) {
	if taskID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.done) >= 256 {
		dropped := 0
		for k := range r.done {
			delete(r.done, k)
			dropped++
			if dropped >= 128 {
				break
			}
		}
	}
	r.done[taskID] = p
}

// taskCount 已完成任务数。
func (r *fakeRunner) taskCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasks
}

// bumpTasks 累加完成计数。
func (r *fakeRunner) bumpTasks() {
	r.mu.Lock()
	r.tasks++
	r.mu.Unlock()
}

// bumpErrors 累加失败计数。
func (r *fakeRunner) bumpErrors() {
	r.mu.Lock()
	r.errors++
	r.mu.Unlock()
}

// failSet 解析 -fail-tools（每次调用开销极小，避免维护额外状态）。
func (r *fakeRunner) failSet() map[string]struct{} {
	out := make(map[string]struct{})
	for _, name := range strings.Split(r.cfg.failTools, ",") {
		if n := strings.TrimSpace(name); n != "" {
			out[n] = struct{}{}
		}
	}
	return out
}

// parseCapabilities 解析 -caps：`fs.read,run_command@1.2` → []Capability。
func parseCapabilities(s string) []model.Capability {
	var out []model.Capability
	seen := make(map[string]struct{})
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, version := item, "1.0"
		if idx := strings.Index(item, "@"); idx > 0 {
			name = strings.TrimSpace(item[:idx])
			version = strings.TrimSpace(item[idx+1:])
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, model.Capability{Name: name, Version: version})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// defaultPlatform 默认平台标识（端侧自述用）。
func defaultPlatform() string {
	return "fake"
}

// fatalf 打印错误并退出（端侧工具，无需返回错误链）。
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fake-runner: "+format+"\n", args...)
	os.Exit(2)
}
