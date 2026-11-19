//go:build windows

package hostimpl

// Windows 进程树管理：Job Object + taskkill 双保险。
//
// 选择 Windows 系统调用而不是纯 taskkill 的原因：
//   - taskkill /T 依赖遍历父子关系，父子链路一旦断裂（中间进程已退出）就漏杀；
//   - Job Object 是内核级容器，AssignProcessToJobObject 之后整个树被内核追踪，
//     即使中间进程退出，孙进程仍在 Job 里，KILL_ON_JOB_CLOSE 可一次性清空。
//
// 两者互补：Job 负责"整棵树一起死"，taskkill 负责"立刻死"（Job 关闭有延迟）。

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateJobObjectW          = modkernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = modkernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject  = modkernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess               = modkernel32.NewProc("OpenProcess")
	procTerminateProcess          = modkernel32.NewProc("TerminateProcess")
	procCloseHandle               = modkernel32.NewProc("CloseHandle")
)

const (
	// PROCESS_TERMINATE 允许 TerminateProcess。
	processTerminate = 0x0001
	// PROCESS_SET_QUOTA 允许 AssignProcessToJobObject。
	processSetQuota = 0x0100
	// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE：Job 句柄全部关闭时内核杀掉所有成员。
	jobObjectLimitKillOnJobClose = 0x2000
	// JobObjectExtendedLimitInformation 的信息类别编号。
	jobObjectExtendedLimitInformationClass = 9
)

// ioCounters 对应 Win32 IO_COUNTERS。
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// jobObjectBasicLimitInformation 对应 Win32 JOBOBJECT_BASIC_LIMIT_INFORMATION。
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// jobObjectExtendedLimitInformation 对应 Win32 JOBOBJECT_EXTENDED_LIMIT_INFORMATION。
type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// globalJob 是进程级共享的 Job Object。
//
// 为什么共享一个而非每个命令一个：每条命令建一个 Job 会在高频工具调用下
// 积累大量句柄。共享 Job + KILL_ON_JOB_CLOSE 已经能满足"服务端退出时
// 级联清理"的目标；单条命令的强杀由 terminateTree 负责。
var (
	globalJobOnce sync.Once
	globalJob     uintptr
)

// ensureGlobalJob 惰性创建全局 Job Object，失败时返回 0（降级为纯 taskkill）。
func ensureGlobalJob() uintptr {
	globalJobOnce.Do(func() {
		job, _, _ := procCreateJobObjectW.Call(0, 0)
		if job == 0 {
			return
		}
		var info jobObjectExtendedLimitInformation
		info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
		ok, _, _ := procSetInformationJobObject.Call(
			job,
			uintptr(jobObjectExtendedLimitInformationClass),
			uintptr(unsafe.Pointer(&info)),
			unsafe.Sizeof(info),
		)
		if ok == 0 {
			procCloseHandle.Call(job)
			return
		}
		globalJob = job
	})
	return globalJob
}

// attachProcessTree 把刚启动的子进程纳入 Job Object，并返回生命周期守卫。
//
// 调用时机必须紧跟 cmd.Start()：越早 Assign，越不会漏掉早期派生的孙进程
// （进程启动后第一件事可能就是 fork 子进程）。
func attachProcessTree(pid int) *ProcessTree {
	if pid <= 0 {
		return newProcessTree(0, nil)
	}
	assignToJob(pid)
	return newProcessTree(pid, nil)
}

// assignToJob 把进程加入全局 Job（尽力而为，失败不影响功能）。
func assignToJob(pid int) {
	job := ensureGlobalJob()
	if job == 0 {
		return
	}
	h, _, _ := procOpenProcess.Call(
		uintptr(processSetQuota|processTerminate),
		0,
		uintptr(pid),
	)
	if h == 0 {
		return
	}
	defer procCloseHandle.Call(h)
	procAssignProcessToJobObject.Call(job, h)
}

// killTree 终止整棵进程树。
//
// 顺序：先 TerminateProcess 立即断根（微秒级，立刻停止继续产出），
// 再异步 taskkill /T 清理深层孙进程（内核遍历，稍慢但彻底）。
//
// 为什么先 TerminateProcess：如果先 taskkill，遍历期间命令可能还在写管道，
// 导致 Wait 继续被拖住。先断根能让输出立即停止，缩短取消的收敛时间。
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	h, _, _ := procOpenProcess.Call(uintptr(processTerminate), 0, uintptr(pid))
	if h != 0 {
		procTerminateProcess.Call(h, 1)
		procCloseHandle.Call(h)
	}
	terminateTree(pid)
}

// terminateTree 用 taskkill /F /T 递归清理子进程树。
//
// 必须**异步**启动，绝不能用 cmd.Run()/Output() 同步等待：
// taskkill 自身偶尔会被待杀进程阻塞数秒，同步调用会把整个工具调用拖住，
// 这正是"取消后还要等 50 秒"的另一种成因。
func terminateTree(pid int) {
	bin := "taskkill"
	if root := os.Getenv("SystemRoot"); root != "" {
		bin = root + `\System32\taskkill.exe`
	}
	c := exec.Command(bin, "/F", "/T", "/PID", itoa(pid))
	setNoWindow(c)
	if err := c.Start(); err != nil {
		return
	}
	// 不等待：回收交给操作系统，避免阻塞调用方。
	go func() { _ = c.Wait() }()
}

// setNoWindow 让辅助进程不弹黑框（Windows GUI 宿主下必需）。
func setNoWindow(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	const createNoWindow = 0x08000000
	c.SysProcAttr.CreationFlags |= createNoWindow
}

// setProcessGroup 在 Windows 下无需设置进程组。
//
// 进程树归属由 Job Object 承担（内核容器语义强于 Unix 进程组），
// 保留同签名是为了让 command.go 保持平台无关。
func setProcessGroup(*exec.Cmd) {}

// itoa 是 strconv.Itoa 的极简替身，避免为一次转换引入 import。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
