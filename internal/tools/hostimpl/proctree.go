package hostimpl

// 进程树生命周期管理（跨平台公共部分）。
//
// # 为什么需要这一层
//
// os/exec 的 exec.CommandContext 在 ctx 取消时只对**直接子进程**发 Kill。
// 但真实命令几乎总是带管道与派生的：
//
//	powershell -Command "go run ./cmd/x | Out-String"
//
// 这里 powershell 是直接子进程，go run 编译出的可执行文件是孙进程。
// 杀掉 powershell 后，孙进程继续存活并继续往管道里写——管道未关闭，
// cmd.Wait() 就无法返回。
//
// 真实事故（会话 9ee34e00）：Run 在 ended_at=…266409 已是 cancelled 终态，
// 但两条工具消息仍在 …320130/…320132（迟 53.7 秒）落库。根因就是这个孤儿
// 进程树：终态之后命令仍在产出，结果仍被写入。
//
// # 三层保障（对齐 harness_mini 的实战设计）
//
//  1. cmd.Cancel 自定义：ctx 取消时先杀整棵进程树，而不是只杀直接子进程；
//  2. tree.Kill() 显式调用：Wait 之前如果已判定取消/超时，再补一刀；
//  3. defer tree.Release()：即使前面逻辑提前 return，也保证回收。
//
// Windows 额外用 Job Object（KILL_ON_JOB_CLOSE）：服务端进程自身崩溃时，
// 内核会级联杀掉整棵进程树，不留孤儿占着端口与文件句柄。
//
// 平台差异全部收敛在 proctree_windows.go / proctree_unix.go，
// 本文件只保留与平台无关的守卫逻辑与统一接口。

import (
	"os/exec"
	"sync"
)

// ProcessTree 是一个已启动子进程及其派生进程树的生命周期守卫。
//
// 用法铁律：
//
//	cmd.Start()
//	tree := attachProcessTree(cmd.Process.Pid)   // 越早越好，越早越不会漏
//	defer tree.Release()
//	... 正常流程 ...
//	tree.Kill()  // 需要强制终止时（超时/取消/输出超限）
//
// Kill 与 Release 都是**幂等**的：重复调用不会 panic，也不会误杀已被
// 系统回收的 PID（平台实现里靠句柄而非 PID 兜底）。
type ProcessTree struct {
	pid     int
	mu      sync.Mutex
	killed  bool
	release func()
}

// newProcessTree 由 attachProcessTree 内部调用，绑定平台资源释放回调。
func newProcessTree(pid int, release func()) *ProcessTree {
	return &ProcessTree{pid: pid, release: release}
}

// PID 返回根进程 PID（日志与排障用）。
func (t *ProcessTree) PID() int {
	if t == nil {
		return 0
	}
	return t.pid
}

// Kill 强制终止整棵进程树（幂等）。
//
// 语义上等价于「用户按下停止」，会同时杀直接子进程与所有孙进程。
// 必须在 cmd.Wait() **之前或之后**都能安全调用：Windows 实现用 taskkill /T
// 按 PID 递归，Unix 实现按进程组发信号。
func (t *ProcessTree) Kill() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.killed {
		t.mu.Unlock()
		return
	}
	t.killed = true
	t.mu.Unlock()
	killTree(t.pid)
}

// Release 释放平台资源（Windows 关闭 Job Object 句柄）。
//
// 注意：Release **不**杀进程——正常结束的进程不需要被杀。
// 需要兜底强杀时，调用方应在 defer 里先 Kill 再 Release，或直接用
// deferKill 辅助函数。
func (t *ProcessTree) Release() {
	if t == nil {
		return
	}
	t.mu.Lock()
	rel := t.release
	t.release = nil
	t.mu.Unlock()
	if rel != nil {
		rel()
	}
}

// quitProcessTree 结束一棵进程树：先杀（若需要），再释放平台资源。
//
// 这是 command.go 里 `defer` 的标准用法，把「是否要杀」的判断收敛在一处，
// 避免每个 return 分支各写一遍。
func quitProcessTree(tree *ProcessTree, kill bool) {
	if tree == nil {
		return
	}
	if kill {
		tree.Kill()
	}
	tree.Release()
}

// cancelProcessTree 是 cmd.Cancel 的实现载体。
//
// 与 exec.CommandContext 默认行为的关键差异：默认只 Kill 直接子进程，
// 这里杀整棵树。返回 nil 让标准库继续走它的 Wait 流程。
func cancelProcessTree(tree *ProcessTree) func() error {
	return func() error {
		tree.Kill()
		return nil
	}
}

// 编译期断言：确保 exec.Cmd 的 Cancel 字段签名与本文件假设一致。
// 若标准库改变签名，这里会先于调用点报错，避免静默失效。
var _ = func(c *exec.Cmd, tree *ProcessTree) { c.Cancel = cancelProcessTree(tree) }
