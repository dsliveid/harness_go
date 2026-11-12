package service

import (
	"context"
	"testing"
)

// R3 设备离线自动中断：反向索引与 CancelByDevice 的行为契约。
//
// 测试意图（为什么这些点必须锁死）：
//   - 设备离线必须中断该设备上**全部**进行中 Run（漏一个 = 用户看到卡住的对话）
//   - 服务端内置空间的 Run（deviceID 为空）**绝不**被设备离线事件波及（R5）
//   - 中断后索引必须清理干净（否则内存随运行时长单调增长）
//   - 重复中断/空设备 ID 必须安全无副作用

// newCancelTestSvc 构造只含并发状态的最小 RunService（不接引擎）。
func newCancelTestSvc() *RunService {
	return NewRunService(RunServiceOptions{})
}

// TestCancelByDeviceCancelsAllRunsOfDevice 一台设备上的多个 Run 必须一次全中断。
func TestCancelByDeviceCancelsAllRunsOfDevice(t *testing.T) {
	svc := newCancelTestSvc()

	var cancelled1, cancelled2, other int
	_, c1 := context.WithCancel(context.Background())
	svc.registerCancelForDevice("run-1", "dev-A", func() { cancelled1++ })
	svc.registerCancelForDevice("run-2", "dev-A", func() { cancelled2++ })
	svc.registerCancelForDevice("run-3", "dev-B", func() { other++ })
	_ = c1

	if n := svc.CancelByDevice("dev-A"); n != 2 {
		t.Fatalf("dev-A 应中断 2 个 Run，得到 %d", n)
	}
	if cancelled1 != 1 || cancelled2 != 1 {
		t.Fatalf("dev-A 的两个 Run 都必须被取消，得到 %d/%d", cancelled1, cancelled2)
	}
	if other != 0 {
		t.Fatalf("dev-B 的 Run 不得被误伤，却被取消了 %d 次", other)
	}
}

// TestCancelByDeviceNeverTouchesServerRuns 服务端内置空间的 Run 永不受设备离线影响（R5）。
func TestCancelByDeviceNeverTouchesServerRuns(t *testing.T) {
	svc := newCancelTestSvc()

	var serverCancelled int
	// deviceID 为空 = 服务端内置空间（ws-server）的 Run。
	svc.registerCancelForDevice("run-srv", "", func() { serverCancelled++ })

	if n := svc.CancelByDevice("dev-A"); n != 0 {
		t.Fatalf("dev-A 无 Run，应返回 0，得到 %d", n)
	}
	if serverCancelled != 0 {
		t.Fatalf("服务端 Run 绝不能被设备离线中断，却被取消了 %d 次", serverCancelled)
	}
	// 反向校验：服务端 Run 的 cancel 仍在使用表中（未被误删）。
	svc.mu.Lock()
	_, stillThere := svc.cancels["run-srv"]
	svc.mu.Unlock()
	if !stillThere {
		t.Fatal("服务端 Run 的 cancel 不应被设备离线清理")
	}
}

// TestCancelByDeviceIsIdempotentAndSafe 重复中断 / 空设备 ID 必须安全无副作用。
func TestCancelByDeviceIsIdempotentAndSafe(t *testing.T) {
	svc := newCancelTestSvc()

	var called int
	svc.registerCancelForDevice("run-1", "dev-A", func() { called++ })

	if n := svc.CancelByDevice("dev-A"); n != 1 {
		t.Fatalf("首次中断应返回 1，得到 %d", n)
	}
	// 第二次：索引已摘除 → 0，且不重复调用 cancel（避免重复副作用）。
	if n := svc.CancelByDevice("dev-A"); n != 0 {
		t.Fatalf("重复中断应返回 0，得到 %d", n)
	}
	if called != 1 {
		t.Fatalf("cancel 应只被调用一次，实际 %d 次", called)
	}
	// 空设备 ID：直接返回 0，不做任何事。
	if n := svc.CancelByDevice(""); n != 0 {
		t.Fatalf("空设备 ID 应返回 0，得到 %d", n)
	}
	if n := svc.CancelByDevice("   "); n != 0 {
		t.Fatalf("空白设备 ID 应返回 0，得到 %d", n)
	}
}

// TestUnregisterCancelCleansReverseIndex 终态注销必须清理反向索引（防内存泄漏）。
func TestUnregisterCancelCleansReverseIndex(t *testing.T) {
	svc := newCancelTestSvc()

	svc.registerCancelForDevice("run-1", "dev-A", func() {})
	svc.registerCancelForDevice("run-2", "dev-A", func() {})

	svc.unregisterCancel("run-1")
	svc.mu.Lock()
	remaining := len(svc.runDevices["dev-A"])
	svc.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("注销一个 Run 后 dev-A 应只剩 1 个，得到 %d", remaining)
	}

	// 全部注销后，设备条目本身也应被删除（避免空集合堆积）。
	svc.unregisterCancel("run-2")
	svc.mu.Lock()
	_, exists := svc.runDevices["dev-A"]
	svc.mu.Unlock()
	if exists {
		t.Fatal("设备下已无 Run，索引条目应被删除")
	}
}
