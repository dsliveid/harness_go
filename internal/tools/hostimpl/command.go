package hostimpl

// 受审批约束的子进程执行器（阶段 F / 需求 §10-4「执行命令（实时输出）」）。
//
// 安全边界：
//   - Dir 相对工作区根（经 Workspace.Resolve 沙箱校验，越界拒绝）；
//   - 输出上限（stdout/stderr 各 64KB，防巨型输出撑爆上下文与内存）；
//   - 超时硬杀（默认 60s，上限 600s）；
//   - 增量输出经 Progress 回调节流推送（200ms），由插件转发 tool.progress 事件。
//   - 审批在本层之外：工具声明 risk=command → 引擎审批闸门裁决后才进到这里。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"harness-go/pkg/toolapi"
)

// 命令执行边界。
const (
	DefaultCommandTimeout = 60 * time.Second
	MaxCommandTimeout     = 600 * time.Second
	MaxOutputBytes        = 64 << 10
	ProgressThrottle      = 200 * time.Millisecond
)

// CommandOptions 命令执行器参数。
type CommandOptions struct {
	// Root 工作区绝对根（Dir 相对路径的沙箱基准）。
	Root string
	// Env 白名单继承的宿主环境变量键（其余透传请求 Env）。
	EnvWhitelist []string
}

// NewCommandRunner 创建真实命令执行器。
func NewCommandRunner(opts CommandOptions) toolapi.CommandRunner {
	return &commandRunner{opts: opts}
}

type commandRunner struct{ opts CommandOptions }

// Run 实现 toolapi.CommandRunner。
func (r *commandRunner) Run(ctx context.Context, req toolapi.CommandRequest) (toolapi.CommandResult, error) {
	if strings.TrimSpace(req.Cmd) == "" {
		return toolapi.CommandResult{}, fmt.Errorf("%w: cmd 不能为空", toolapi.ErrInvalidArgs)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	if timeout > MaxCommandTimeout {
		timeout = MaxCommandTimeout
	}
	dir := strings.TrimSpace(req.Dir)
	// 会话执行根优先（2026-11 目录选择放开）：ctx 无根时回落进程级 Root，
	// 保证既有调用点行为不变。命令的 cmd.Dir 必须落在**生效根**之内。
	root := r.opts.Root
	if ctxRoot, ok := toolapi.WorkspaceRootFrom(ctx); ok {
		root = ctxRoot
	}
	if dir == "" || dir == "." {
		dir = root
	} else {
		// 沙箱：相对路径必须落在工作区根内（与 Workspace.Resolve 同口径）
		abs, err := resolveWithin(root, dir)
		if err != nil {
			return toolapi.CommandResult{}, err
		}
		dir = abs
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, req.Cmd, req.Args...)
	cmd.Dir = dir
	cmd.Env = childCommandEnv(req.Env, r.opts.EnvWhitelist)
	// Unix 下把子进程放进独立进程组，让 killTree 的 kill(-pid) 能覆盖整棵树，
	// 同时保证绝不会误杀服务端自身（服务端不在该组）。Windows 由 Job Object 负责。
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return toolapi.CommandResult{}, fmt.Errorf("hostimpl: 创建 stdout 管道失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return toolapi.CommandResult{}, fmt.Errorf("hostimpl: 创建 stderr 管道失败: %w", err)
	}
	// 关键顺序：先 Start 拿到 PID，再 attach（越早越不会漏掉早期派生的孙进程）。
	// 随后立刻接管 cmd.Cancel —— 这一步把"只杀直接子进程"改成"杀整棵进程树"，
	// 是修复"取消后仍持续产出、终态后消息继续入库"的核心。
	if err := cmd.Start(); err != nil {
		return toolapi.CommandResult{}, fmt.Errorf("hostimpl: 启动命令失败: %w", err)
	}
	tree := attachProcessTree(cmd.Process.Pid)
	cmd.Cancel = cancelProcessTree(tree)
	// killTree=true 的兜底：无论从哪个 return 分支离开（含 panic），
	// 只要本次调用没有正常走完，就回收整棵树，绝不留孤儿。
	normalExit := false
	defer func() { quitProcessTree(tree, !normalExit) }()

	var (
		mu          sync.Mutex
		outBuf      strings.Builder
		errBuf      strings.Builder
		lastPush    time.Time
		pushPending strings.Builder
	)
	push := func(chunk string) {
		if req.Progress == nil {
			return
		}
		mu.Lock()
		pushPending.WriteString(chunk)
		now := time.Now()
		due := now.Sub(lastPush) >= ProgressThrottle
		if due {
			lastPush = now
		}
		payload := pushPending.String()
		if due {
			pushPending.Reset()
		}
		mu.Unlock()
		if due && payload != "" {
			req.Progress(payload)
		}
	}
	collect := func(dst *strings.Builder, label string, rd io.Reader) {
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 0, 64<<10), 512<<10)
		for sc.Scan() {
			line := sc.Text()
			mu.Lock()
			full := dst.Len()+len(line) < MaxOutputBytes
			if full {
				dst.WriteString(line + "\n")
			}
			mu.Unlock()
			push(label + line + "\n")
			if !full {
				// 超限后不再收集，但继续消费管道防阻塞
				continue
			}
		}
	}
	done := make(chan struct{}, 2)
	go func() { collect(&outBuf, "", stdout); done <- struct{}{} }()
	go func() { collect(&errBuf, "[stderr] ", stderr); done <- struct{}{} }()
	<-done
	<-done
	// 尾包推送（不足节流间隔的剩余输出）
	if req.Progress != nil {
		mu.Lock()
		tail := pushPending.String()
		pushPending.Reset()
		mu.Unlock()
		if tail != "" {
			req.Progress(tail)
		}
	}

	waitErr := cmd.Wait()
	res := toolapi.CommandResult{
		Stdout: outBuf.String(),
		Stderr: errBuf.String(),
	}
	// ---- 三种异常必须分流（此前统一落到通用错误，模型无法分辨）----
	//
	// 旧实现在这里只判断 DeadlineExceeded，于是「用户停止 / 上层取消」
	// （context.Canceled）会一路掉到最后的通用分支，产出
	//   run_command: 执行失败: hostimpl: 启动命令失败: context canceled
	// —— 这句话对模型毫无信息量：它既不知道是超时、被用户停、还是工具坏了，
	// 也读不出「我该换策略还是该放弃」。三类结论必须各自可读。
	//
	// 且三者都必须是**结果**（返回 nil error）而不是执行器错误：
	// 超时与取消是正常业务结局，需要把已产出的部分输出交给模型判断。
	normalExit = true

	if waitErr != nil {
		switch {
		case runCtx.Err() == context.DeadlineExceeded:
			res.TimedOut = true
			res.Stderr += fmt.Sprintf("\n[命令超时（%s），进程树已强制终止]", timeout)

		case ctx.Err() == context.Canceled || ctx.Err() == context.DeadlineExceeded:
			// 上层取消（用户点停止 / 设备离线 / 请求断开）：与超时严格区分，
			// 因为这是"用户不要这个结果了"，而不是"命令太慢"。
			res.Cancelled = true
			res.Stderr += "\n[命令已被取消（用户停止或上游中断），进程树已强制终止]"

		case isExitError(waitErr):
			// 非零退出码是正常结果：模型需要读 stderr 自我纠正，不能当执行器故障。
			res.ExitCode = exitCodeOf(waitErr)

		default:
			// 真正的执行器故障（管道断开、进程创建失败等）。
			return res, fmt.Errorf("hostimpl: 命令执行失败: %w", waitErr)
		}
	}
	return res, nil
}

// isExitError 判断是否为「命令跑了但返回非零」。
//
// 抽成独立函数而非内联类型断言：Windows 与 Unix 下 exec 返回的具体类型
// 可能不同，集中在这里便于将来适配，调用点不必关心平台差异。
func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// exitCodeOf 取出退出码（无法获取时返回 -1）。
func exitCodeOf(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// childCommandEnv 组装子进程环境：宿主白名单 + 请求 Env。
// 白名单为空时仅透传请求 Env（最小暴露）。
func childCommandEnv(reqEnv map[string]string, whitelist []string) []string {
	var out []string
	keep := map[string]bool{}
	for _, k := range whitelist {
		keep[k] = true
	}
	if len(keep) > 0 {
		for _, kv := range os.Environ() {
			if i := strings.IndexByte(kv, '='); i > 0 && keep[kv[:i]] {
				out = append(out, kv)
			}
		}
	}
	for k, v := range reqEnv {
		out = append(out, k+"="+v)
	}
	return out
}

// resolveWithin 把相对路径约束在工作区根内（与 workspace.Resolve 同口径）。
func resolveWithin(root, rel string) (string, error) {
	clean := strings.ReplaceAll(rel, "\\", "/")
	for strings.HasPrefix(clean, "./") {
		clean = clean[2:]
	}
	if strings.HasPrefix(clean, "../") || clean == ".." || filepath.IsAbs(clean) {
		return "", fmt.Errorf("%w: 命令工作目录越界 %q", toolapi.ErrInvalidArgs, rel)
	}
	abs := root
	if clean != "" && clean != "." {
		abs = filepath.Join(root, filepath.FromSlash(clean))
	}
	return abs, nil
}
