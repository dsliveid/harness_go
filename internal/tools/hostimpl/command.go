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
	if dir == "" || dir == "." {
		dir = r.opts.Root
	} else {
		// 沙箱：相对路径必须落在工作区根内（与 Workspace.Resolve 同口径）
		abs, err := resolveWithin(r.opts.Root, dir)
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

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return toolapi.CommandResult{}, fmt.Errorf("hostimpl: 创建 stdout 管道失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return toolapi.CommandResult{}, fmt.Errorf("hostimpl: 创建 stderr 管道失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return toolapi.CommandResult{}, fmt.Errorf("hostimpl: 启动命令失败: %w", err)
	}

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
	if waitErr != nil {
		if runCtx.Err() == context.DeadlineExceeded || ctx.Err() == context.DeadlineExceeded {
			res.TimedOut = true
			res.Stderr += fmt.Sprintf("\n[超时 %s，进程已终止]", timeout)
			return res, nil // 超时是结果不是执行器错误（工具结果回传模型）
		}
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, nil // 非零退出码是正常结果（模型可读 stderr 自我纠正）
		}
		return res, fmt.Errorf("hostimpl: 命令执行失败: %w", waitErr)
	}
	return res, nil
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
