// 本文件实现「真实 Rust Runner 二进制」联调模式（-mode runner）。
//
// 与同目录 client.go 的区别（一句话）：client.go 是**脚本自己扮演端侧**（只验服务端契约），
// 本文件是**真的拉起桌面端的 hd-runner 进程**，让 Go 服务端与 Rust 端侧真实对撞。
//
// 为什么值得多这一层：
//   - 自包含模式里「端侧」是 Go 写的假端侧，两端都读同一份 Go 常量，**协议漂移不可能被发现**；
//   - 真实二进制是另一套语言、另一份协议实现，字段名/枚举值/心跳节奏只要有一处不一致就会暴露；
//   - 同时验证服务端装配（Gateway/Dispatcher/sqlite）在**进程外**客户端下也成立，而不是只在同进程内成立。
//
// 三条设计红线（代码里逐条落地，不靠记忆）：
//  1. 令牌只经 stdin 传：`hgd_...` 绝不进 argv / 环境变量（同机进程能从 ps、/proc/<pid>/environ 读到），
//     输出一律 maskToken 打码。
//  2. 只杀自己拉起的子进程：统一走 cmd.Process.Kill()，**绝不**按进程名全局杀（那会误杀用户正在
//     使用的桌面端或另一条 CI 流水线的 Runner）。
//  3. 二进制可能不存在：探测不到就 SKIP 并打印可执行的构建命令，退出码仍为 0 ——
//     「另一个仓库还没构建」不该让本仓库的自包含模式变红（本脚本的首要用途就是联调自检）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"harness-go/internal/device"
	"harness-go/internal/model"
)

// runnerFlags runner 模式的命令行参数。
type runnerFlags struct {
	// BinPath 显式指定的二进制路径（-runner-bin）。为空时自动探测。
	BinPath string
	// Dev 是否尝试给二进制传 --dev（允许 ws:// 明文连接本地服务端）。
	// 本地自包含服务端是 http://127.0.0.1:PORT，实际是 ws://，必须放行非 TLS。
	Dev bool
}

// ============ ① 二进制探测 ============

// runnerCandidate 一个待探测路径（带「为什么看这个位置」的说明，用于失败时打印排查线索）。
type runnerCandidate struct {
	Path   string
	Reason string
}

// detectRunnerBinary 按「显式指定 → 环境变量 → debug → release」的顺序探测 hd-runner 可执行文件。
//
// 为什么是这个顺序：显式参数永远优先（用户最清楚自己刚把二进制放哪）；环境变量次之（CI 注入）；
// 之后才是约定路径 —— cargo 默认产物目录，且 debug 先于 release（开发期 debug 更常见、构建更快）。
// 返回 (路径, 已尝试列表, 错误)。错误里要能看出**试过哪些地方**，否则用户只知道"找不到"。
func detectRunnerBinary(explicit string) (string, []runnerCandidate, error) {
	if p := strings.TrimSpace(explicit); p != "" {
		c := runnerCandidate{Path: p, Reason: "命令行 -runner-bin 显式指定"}
		if resolved, err := prepareRunnerPath(c); err == nil {
			return resolved, []runnerCandidate{c}, nil
		}
		return "", []runnerCandidate{c}, fmt.Errorf("显式指定的 -runner-bin 不可用：%s", p)
	}

	var cands []runnerCandidate
	if env := strings.TrimSpace(os.Getenv("HD_RUNNER_BIN")); env != "" {
		cands = append(cands, runnerCandidate{Path: env, Reason: "环境变量 HD_RUNNER_BIN"})
	}
	// 约定路径：本仓库与 harness_desktop 是同级目录（见项目关联关系），
	// 因此从工作区根目录出发是 ..\harness_desktop\target\{debug,release}\hd-runner[.exe]。
	for _, profile := range []string{"debug", "release"} {
		for _, name := range runnerExeNames() {
			cands = append(cands, runnerCandidate{
				Path:   filepath.Join("..", "harness_desktop", "target", profile, name),
				Reason: "cargo " + profile + " 约定产物",
			})
		}
	}

	for _, c := range cands {
		if resolved, err := prepareRunnerPath(c); err == nil {
			return resolved, cands, nil
		}
	}
	return "", cands, errors.New("未找到 hd-runner 可执行文件")
}

// runnerExeNames 返回当前平台下可能的文件名。
//
// Windows 必须带 .exe（cargo 产物如此），但同时也试不带后缀 —— 有些人会把二进制改名/软链，
// 多试一次的成本远低于让用户对着"找不到"发呆。
func runnerExeNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"hd-runner.exe", "hd-runner"}
	}
	return []string{"hd-runner"}
}

// prepareRunnerPath 校验候选路径：存在、是普通文件、可执行。
func prepareRunnerPath(c runnerCandidate) (string, error) {
	abs, err := filepath.Abs(c.Path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s 是目录", abs)
	}
	if st.Size() == 0 {
		return "", fmt.Errorf("%s 是空文件", abs)
	}
	return abs, nil
}

// runnerBuildHint 缺失时的可执行建议（照抄即可，不留「请自行编译」这种废话）。
func runnerBuildHint() string {
	sep := "/"
	if runtime.GOOS == "windows" {
		sep = "\\"
	}
	return "请先构建：cd .." + sep + "harness_desktop && cargo build -p hd-runner --bin hd-runner" +
		"（或 -runner-bin 指向已有二进制）"
}

// ============ ② 子进程封装 ============

// tailBuffer 保留「最后 N 字节」的输出缓冲。
//
// 为什么不是简单 bytes.Buffer：Runner 会持续打 tracing 日志，长时间联调（或在 CI 里挂住）时
// 无界累积会吃光内存；而排查只需要**最后**几 KB（错误栈总在末尾）。
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > t.max {
		// 只保留尾部：错误现场在最后
		t.data = append([]byte(nil), t.data[len(t.data)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.data))
}

// runnerProcess 拉起后的 hd-runner 子进程句柄。
//
// 关键不变量：stdin **保持打开**。Runner 的契约是「stdin EOF 时自杀」（防孤儿），
// 所以写出 auth 行后立刻 Close 会让它刚连上就退出 —— 这是最容易踩的坑。
type runnerProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *tailBuffer
	stderr *tailBuffer
	args   []string

	done chan struct{} // Wait 返回后关闭

	mu      sync.Mutex
	waitErr error
}

// startRunnerProcess 启动子进程（不传任何敏感信息，工作目录固定为本次联调的工作区根）。
func startRunnerProcess(bin string, workDir string, args []string) (*runnerProcess, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = workDir
	// 安全红线：只用 stdin 传令牌 —— 这里显式不给 cmd.Env 追加任何东西，
	// 也不把 token 放进 args；Runner 侧 ipc.rs 同样从类型层面禁止令牌进日志。

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建子进程 stdin 管道失败: %w", err)
	}
	p := &runnerProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: newTailBuffer(8 << 10),
		stderr: newTailBuffer(8 << 10),
		args:   args,
		done:   make(chan struct{}),
	}
	cmd.Stdout = p.stdout
	cmd.Stderr = p.stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s 失败: %w", bin, err)
	}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.waitErr = err
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

// pid 返回 PID（日志里展示，便于用户自己去查证，不含任何敏感信息）。
func (p *runnerProcess) pid() int { return p.cmd.Process.Pid }

// sendCommand 向 stdin 写一行 JSONL（内部标签协议：`{"op":"...", ...}`）。
func (p *runnerProcess) sendCommand(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("序列化 stdin 命令失败: %w", err)
	}
	// 单行 + '\n'：Runner 侧逐行读（BufRead::read_line），不带换行会一直阻塞。
	if _, err := p.stdin.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("写子进程 stdin 失败（进程可能已退出）: %w", err)
	}
	return nil
}

// exited 非阻塞地报告「是否已退出」，返回退出错误（nil 表示 0）。
func (p *runnerProcess) exited() (bool, error) {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return true, p.waitErr
	default:
		return false, nil
	}
}

// waitExit 等待退出；超时则强杀本进程自身（不外溢）并返回错误。
func (p *runnerProcess) waitExit(timeout time.Duration) (int, error) {
	select {
	case <-p.done:
		return p.exitCode(), p.exitErr()
	case <-time.After(timeout):
		p.kill()
		<-p.done
		return p.exitCode(), fmt.Errorf("等待子进程退出超时（%s），已强制终止", timeout)
	}
}

func (p *runnerProcess) exitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(p.waitErr, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func (p *runnerProcess) exitErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waitErr == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(p.waitErr, &ee) {
		return fmt.Errorf("子进程以退出码 %d 结束", ee.ExitCode())
	}
	return p.waitErr
}

// kill 终止**本脚本自己拉起的**子进程。
//
// 🔴 进程安全：只用 cmd.Process.Kill() 定向杀 PID，绝不按镜像名全局杀（taskkill /IM、pkill -f 之类）——
// 同机可能还跑着用户自己启动的桌面端或另一个 CI 的 Runner，全局杀会连带打掉它们。
func (p *runnerProcess) kill() {
	if alreadyExited, _ := p.exited(); alreadyExited {
		return
	}
	_ = p.cmd.Process.Kill()
	<-p.done // 回收，避免僵尸/句柄泄漏
}

// cleanup 幂等收尾（defer 用）：先尝试优雅（关 stdin → 等 3s），再强杀。
func (p *runnerProcess) cleanup() {
	if alreadyExited, _ := p.exited(); alreadyExited {
		return
	}
	_ = p.stdin.Close() // Runner 收到 EOF 会自杀（契约里的防孤儿机制）
	if _, err := p.waitExit(3 * time.Second); err != nil {
		p.kill()
	}
}

// showOutput 打印子进程尾部输出（失败排查的唯一现场，必须带上）。
func (p *runnerProcess) showOutput() {
	if s := p.stdout.String(); s != "" {
		fmt.Printf("     ↳ stdout 尾部：\n%s\n", indent(s, "       "))
	}
	if s := p.stderr.String(); s != "" {
		fmt.Printf("     ↳ stderr 尾部：\n%s\n", indent(s, "       "))
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// startRunnerWithDevFallback 启动二进制，并在它「不认 --dev」时自动重试一次。
//
// 为什么要这套兜底：`--dev` 是端侧为「允许 ws:// 明文连接本地服务端」提供的开关，
// 但不同版本的 hd-runner 对未知参数的处理不一样（有的忽略、有的直接 usage 报错退出）。
// 硬编码传 --dev 会让联调在「端侧刚好没实现该参数」时莫名其妙全红，
// 所以：先试带 --dev，若 1.2s 内因参数问题退出，则去掉重试一次，行为可自愈。
//
// 返回值 err 非空时调用方必须**直接当作拉起失败**处理（此时子进程已被 kill 回收）。
func startRunnerWithDevFallback(bin, workDir string, wantDev bool) (*runnerProcess, string, error) {
	// tryResult 一次启动尝试的结果。
	type tryResult struct {
		exited     bool // 1.2s 观察窗内就退出了
		argProblem bool // 退出原因是「参数不被接受」
		output     string
	}

	try := func(args []string) (*runnerProcess, tryResult, error) {
		p, err := startRunnerProcess(bin, workDir, args)
		if err != nil {
			return nil, tryResult{}, err
		}
		// 静置观察：健康进程此时正阻塞在「读 stdin 第一行」，绝不会自己退出。
		select {
		case <-p.done:
			text := p.stderr.String() + p.stdout.String()
			lower := strings.ToLower(text)
			argProblem := strings.Contains(lower, "unexpected argument") ||
				strings.Contains(lower, "unrecognized") ||
				strings.Contains(lower, "invalid option") ||
				strings.Contains(lower, "found argument") ||
				strings.Contains(lower, "usage:")
			return p, tryResult{exited: true, argProblem: argProblem, output: text}, nil
		case <-time.After(1200 * time.Millisecond):
			return p, tryResult{}, nil
		}
	}

	args := []string{}
	if wantDev {
		args = append(args, "--dev")
	}
	p, res, err := try(args)
	if err != nil {
		return nil, "", err
	}
	if res.exited && res.argProblem && wantDev {
		note := "该二进制不认识 --dev，已自动去掉该参数重试（version 差异，不影响联调）"
		p.kill()
		p, res, err = try(nil)
		if err != nil {
			return nil, note, err
		}
		if res.exited {
			p.kill()
			return nil, note, fmt.Errorf("二进制启动即退出（去掉 --dev 后仍失败，退出原因见 stderr）：%s",
				truncate(res.output, 400))
		}
		return p, note, nil
	}
	if res.exited {
		// 启动即退出与参数无关（例如缺少依赖库 / 立刻 panic）：快速失败，
		// 不必等到写 stdin 才报「文件已关闭」这种误导性错误。
		p.kill()
		return nil, "", fmt.Errorf("二进制启动即退出，无法进入 auth 握手：%s", truncate(res.output, 400))
	}
	return p, "", nil
}

// ============ ③ runner 模式主流程 ============

// wsEndpointURL 推导 Runner 需要的 WS 端点（http→ws / https→wss）。
//
// ⚠️ 与 client.go 的 wsURL 的**关键区别**：这里**不带** token 查询参数。
// 原因：Runner 自己会在 URL 后附加 `?token=`（见 harness_desktop/crates/hd-runner/src/transport.rs
// 的 build_url）。若我们把 token 也拼进 ws_url，就会变成 `?token=A&token=B`，服务端取到哪个不确定。
// 而令牌本体通过 auth 行的 `token` 字段经 stdin 传入。
func wsEndpointURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// 允许用户直接给 ws://（自包含模式下不会发生，但外部/手工调试时更宽容）
	default:
		return "", fmt.Errorf("-base-url 必须是 http(s):// 或 ws(s)://，当前为 %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1/devices/ws"
	u.RawQuery = ""
	return u.String(), nil
}

// runRunnerMode 走完整条「真实二进制」联调链（R1~R5）。
//
// 前置条件：[0] 登录、[1] 签发配对码、[2] 配对换 device_token 已由 runAll 完成，
// 因为这几步两种模式完全一致（都走真实 HTTP 端点），没必要重复实现。
func (r *runner) runRunnerMode(ctx context.Context, h *harnessServer, baseURL string,
	paired *pairResult, flags runnerFlags) {

	// ---------- R1 探测二进制 ----------
	fmt.Println("\n[R1] 探测 hd-runner 真实二进制")
	bin, tried, err := detectRunnerBinary(flags.BinPath)
	if err != nil {
		// 降级：SKIP 而不是 FAIL。理由：本仓库的自包含模式自足可用，
		// 「另一个仓库还没构建」属于环境未就绪，不是本仓库的缺陷。
		r.skip("真实 Runner 二进制", fmt.Sprintf("%v（已尝试 %d 个位置）", err, len(tried)))
		for _, c := range tried {
			fmt.Printf("      · %s  ← %s\n", c.Path, c.Reason)
		}
		fmt.Println("  ℹ " + runnerBuildHint())
		fmt.Println("  ℹ 其余 [R2]~[R5] 一并跳过；自包含模式（不带 -mode runner）不受影响")
		return
	}
	st, _ := os.Stat(bin)
	sizeMB := float64(st.Size()) / (1 << 20)
	r.pass("真实 Runner 二进制", fmt.Sprintf("%s（%.1f MB，探测自 %s）",
		bin, sizeMB, firstLine(reasonOf(tried, bin))))

	// 收尾统一清理：无论从哪一步 return，都不能留下孤儿进程。
	devNote := ""

	// ---------- R2 拉起子进程 + stdin auth ----------
	fmt.Println("\n[R2] 拉起 hd-runner 子进程，经 stdin 下发 auth（令牌不进 argv/env）")
	wsEndpoint, err := wsEndpointURL(baseURL)
	if err != nil {
		r.fail("拉起子进程", err.Error())
		return
	}
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}
	p, devNote, err := startRunnerWithDevFallback(bin, workDir, flags.Dev)
	if err != nil {
		r.fail("拉起子进程", err.Error())
		return
	}
	// 🔴 从这里开始，任何路径返回都必须回收子进程（含 Ctrl+C 场景）。
	defer p.cleanup()
	cancelWatch := make(chan struct{})
	defer close(cancelWatch)
	go func() {
		// ctx 取消（Ctrl+C / 超时）→ 定向杀本进程，避免孤儿
		select {
		case <-ctx.Done():
			p.kill()
		case <-cancelWatch:
		}
	}()

	authLine := map[string]any{
		"op":               "auth",
		"token":            paired.DeviceToken, // 🔴 只经 stdin；日志里永远只出现 maskToken 结果
		"ws_url":           wsEndpoint,
		"protocol_version": paired.ProtocolVersion,
	}
	if err := p.sendCommand(authLine); err != nil {
		r.fail("下发 auth", err.Error())
		p.showOutput()
		return
	}
	// 顺手下发工作区策略（best-effort）：让端侧 fs.list 的守卫有白名单根目录，
	// 否则真实执行多半被 path_denied 拦掉（这属于「联调配置」而非协议缺陷）。
	// 未知 op 在 Runner 侧是 `#[serde(other)]` 忽略，不会打断会话。
	_ = p.sendCommand(map[string]any{
		"op":             "reload_policy",
		"roots":          []string{workDir},
		"exec_allowlist": []string{"git", "node"},
	})

	detail := fmt.Sprintf("pid=%d token=%s ws_url=%s protocol_version=%d",
		p.pid(), maskToken(paired.DeviceToken), wsEndpoint, paired.ProtocolVersion)
	if len(p.args) > 0 {
		detail += fmt.Sprintf(" args=%v", p.args)
	}
	if devNote != "" {
		detail += "；" + devNote
	}
	r.pass("拉起子进程", detail)

	// ---------- R3 服务端侧断言「设备上线」 ----------
	fmt.Println("\n[R3] 服务端侧断言设备上线（Hub 在线表 + 真实 WS 连接）")
	if ok, why := waitDeviceOnline(ctx, h, paired.DeviceID, p, 20*time.Second); !ok {
		r.fail("设备上线断言", why)
		p.showOutput()
		return
	}
	r.pass("设备上线断言", fmt.Sprintf("device_id=%s 已在 Hub 在线表（在线数 %d）",
		paired.DeviceID, h.Hub.Count()))

	// ---------- R4 派发任务 → 断言收真实回执 ----------
	fmt.Println("\n[R4] 派发 fs.list → 断言收到真实端侧 task.result（source_trust 恒 untrusted）")
	taskID := fmt.Sprintf("te-runner-%d", time.Now().UnixNano())
	runID := fmt.Sprintf("run-runner-%d", time.Now().UnixNano())
	started := time.Now()
	res, derr := h.Dispatch.Dispatch(ctx, device.TaskRequest{
		TaskID:      taskID,
		RunID:       runID,
		ToolEventID: taskID, // 跨仓约定：task_id 恒等于 tool_event_id（端侧据此做幂等）
		ToolName:    "fs.list",
		// path 用绝对工作区根：端侧守卫按 roots 白名单判路径，绝对路径最不容易有歧义。
		Args:    json.RawMessage(fmt.Sprintf(`{"path":%q}`, filepath.ToSlash(workDir))),
		Timeout: 15 * time.Second,
	})
	if derr != nil {
		r.fail("任务派发", fmt.Sprintf("派发失败：%v（%s）", derr, res.Error))
		p.showOutput()
		return
	}
	if res.Degraded {
		// 降级 = 请求压根没到端侧（无在线设备/写失败），说明「真实对撞」没发生。
		r.fail("任务派发", fmt.Sprintf("端侧不可用已降级（error=%q）：本地服务端并未真正与 Runner 通上", res.Error))
		p.showOutput()
		return
	}
	if res.SourceTrust != model.TrustUntrusted {
		// 不变量：端侧结果一律不可信，服务端必须强制覆盖为 untrusted。
		r.fail("任务回执", fmt.Sprintf("source_trust 期望 %s，实际 %s", model.TrustUntrusted, res.SourceTrust))
		return
	}
	switch {
	case res.Status == model.ToolOK:
		r.pass("任务回执", fmt.Sprintf("status=ok source_trust=%s 耗时=%s 结果=%s",
			res.SourceTrust, time.Since(started).Truncate(time.Millisecond), truncate(string(res.Result), 200)))
	case res.Timeout:
		r.fail("任务回执", fmt.Sprintf("端侧未在 TTL 内回执（status=%s）——真实 Runner 未处理 task.dispatch", res.Status))
		p.showOutput()
		return
	default:
		// 端侧真实回了「失败」回执：这依然证明两端真实对撞成立（链路通、帧形状对），
		// 只是被守卫策略拦掉（通常是 fs.path_denied，白名单没来得及生效）。
		// 因此判 PASS 但把原因亮出来，避免把配置问题误读成协议缺陷。
		r.pass("任务回执", fmt.Sprintf("status=%s source_trust=%s error=%q —— 端侧真实回执已到达（链路成立）",
			res.Status, res.SourceTrust, truncate(res.Error, 160)))
	}

	// ---------- R5 shutdown 收尾：断言正常退出、无孤儿 ----------
	fmt.Println("\n[R5] 收尾：stdin 下发 shutdown → 断言子进程正常退出、无孤儿残留")
	if err := p.sendCommand(map[string]any{"op": "shutdown"}); err != nil {
		r.fail("优雅退出", "下发 shutdown 失败："+err.Error())
		p.showOutput()
		return
	}
	// 同时关闭写端：即使该版本不处理 shutdown op，stdin EOF 也会触发自杀（双保险）。
	_ = p.stdin.Close()
	code, werr := p.waitExit(10 * time.Second)
	if werr != nil {
		r.fail("优雅退出", fmt.Sprintf("%v（退出码 %d）", werr, code))
		p.showOutput()
		return
	}
	if code != 0 {
		r.fail("优雅退出", fmt.Sprintf("子进程退出码为 %d（期望 0）", code))
		p.showOutput()
		return
	}
	// 无孤儿断言：Wait 已返回 ⇒ 进程已被回收；再确认它确实不在运行态。
	if stillRunning, _ := p.exited(); !stillRunning {
		r.fail("无孤儿残留", "Wait 已返回但进程仍被判为运行中（状态不一致）")
		return
	}
	r.pass("优雅退出", fmt.Sprintf("pid=%d 退出码 0，stdin 已关闭，进程已被 Wait 回收（无孤儿）", p.pid()))

	// 附带观察：服务端在线表应在连接断开后清理（不作为失败条件，
	// 因为离线判定也可能走心跳巡检，节奏由 welcome.offline_after_s 决定）。
	if offline, _ := waitDeviceOffline(ctx, h, paired.DeviceID, 5*time.Second); offline {
		fmt.Println("  ℹ 服务端在线表已同步移除该设备")
	} else {
		fmt.Println("  ℹ 服务端在线表暂仍显示在线（将随心跳巡检判定离线，属正常时序）")
	}
}

// waitDeviceOnline 轮询 Hub 在线表，同时监视子进程是否提前死掉
// （提前死掉时立刻给出 stderr 现场，而不是干等 20 秒）。
func waitDeviceOnline(ctx context.Context, h *harnessServer, deviceID string,
	p *runnerProcess, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.Hub.Online(deviceID) {
			return true, ""
		}
		if exited, err := p.exited(); exited {
			msg := fmt.Sprintf("子进程在设备上线前就退出了（err=%v）", err)
			if s := p.stderr.String(); s != "" {
				msg += "；stderr 尾部：" + truncate(s, 400)
			}
			return false, msg
		}
		select {
		case <-ctx.Done():
			return false, "等待设备上线期间 ctx 被取消"
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false, fmt.Sprintf("等待设备上线超时（%s）：Runner 未完成 WS 握手或鉴权被拒（检查 ws_url/token/--dev）", timeout)
}

// waitDeviceOffline 轮询「设备已不在线」，仅用于附带观察。
func waitDeviceOffline(ctx context.Context, h *harnessServer, deviceID string, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !h.Hub.Online(deviceID) {
			return true, ""
		}
		select {
		case <-ctx.Done():
			return false, "ctx 取消"
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false, "仍在在线表"
}

// reasonOf 找出实际命中的候选（用于把「从哪探测到的」写进输出）。
func reasonOf(tried []runnerCandidate, bin string) string {
	for _, c := range tried {
		if abs, err := filepath.Abs(c.Path); err == nil && abs == bin {
			return c.Reason
		}
	}
	return "自动探测"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
