//go:build !windows

package hostimpl

// Unix 进程树管理：进程组（setpgid）+ 组信号。
//
// 为什么用进程组而不是逐个杀：
//   - Unix 的进程树没有统一容器（Linux 的 cgroup 需要 root 或 systemd 配合），
//     但**进程组**是 POSIX 标准能力，且 kill(-pgid) 一次能覆盖整组；
//   - 前提是启动时把子进程放进独立进程组（setpgid），否则它会留在服务端的
//     进程组里，kill(-pgid) 会误杀服务端自己 —— 这是绝对不能出现的事故。
//
// 因此 command.go 必须在 Unix 下为子进程设置 Setpgid，这里依赖那个前提。

import (
	"os/exec"
	"syscall"
)

// attachProcessTree 返回 Unix 下的进程树守卫。
//
// Unix 无需额外句柄：进程组由 PID 隐含（pgid == 首进程 PID），
// 因此 release 为空操作。前置条件是启动时已 Setpgid=true。
func attachProcessTree(pid int) *ProcessTree {
	return newProcessTree(pid, nil)
}

// killTree 向整个进程组发 SIGKILL。
//
// kill(-pid) 表示"发给以 pid 为首进程的进程组"，这正是 setpgid 建立的组。
// 随后再对 pid 本身发一次，覆盖 setpgid 失败（极少数平台/权限场景）的兜底。
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	// 负号 = 整个进程组。若子进程未成功进入独立组，这一步会返回 ESRCH
	// 或 EPERM，而不会误杀服务端（服务端不在该组内）。
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// setNoWindow 在 Unix 下无对应概念（保留同签名以便跨平台调用）。
func setNoWindow(*exec.Cmd) {}

// setProcessGroup 把子进程放进**独立的进程组**。
//
// 这是 killTree 的前置条件，缺了它会造成严重后果：
//   - 不设 Setpgid 时子进程继承服务端的进程组；
//   - 此时 kill(-pid) 要么无效（ESRCH），要么在极端情况下波及服务端自身。
//
// 设置后 pgid == 子进程 PID，kill(-pid) 精确覆盖该命令派生的整棵树。
func setProcessGroup(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
}
