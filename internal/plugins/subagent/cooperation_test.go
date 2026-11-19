package subagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

// ============ 协作闭环测试桩 ============
//
// 既有测试桩的 spawnAdapter.Get 恒返回 (nil,false)，无法支撑
// status/wait/stop/reject 这组"需要按 ID 找回到在途句柄"的工具。
// 这里自建一个能登记句柄的 ChildRunner：Spawn 把 child 存进 map，
// Get 可按 ID 取回——这正是生产实现（child_runner）的真实语义。

// trackingRunner 可登记句柄的 ChildRunner 桩。
type trackingRunner struct {
	mu      sync.Mutex
	children map[string]*trackedChild
	onSpawn func(req toolapi.SpawnRequest)
}

func newTrackingRunner() *trackingRunner {
	return &trackingRunner{children: make(map[string]*trackedChild)}
}

func (r *trackingRunner) Spawn(_ context.Context, req toolapi.SpawnRequest) (toolapi.ChildRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.onSpawn != nil {
		r.onSpawn(req)
	}
	id := "sub-" + itoa(len(r.children)+1)
	c := &trackedChild{id: id, status: toolapi.ChildRunning, result: toolapi.ChildResult{
		RunID: id, Status: toolapi.ChildCompleted, Output: "已完成：" + req.Input,
	}}
	r.children[id] = c
	return c, nil
}

func (r *trackingRunner) Get(id string) (toolapi.ChildRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.children[id]
	return c, ok
}

// trackedChild 可被外部推进状态的 ChildRun 桩。
type trackedChild struct {
	mu       sync.Mutex
	id       string
	status   toolapi.ChildStatus
	result   toolapi.ChildResult
	waitErr  error
	cancelled bool
}

func (c *trackedChild) ID() string { return c.id }
func (c *trackedChild) Status() toolapi.ChildStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}
func (c *trackedChild) Cancel() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled = true
	c.status = toolapi.ChildCancelled
	return nil
}
func (c *trackedChild) Wait(context.Context) (toolapi.ChildResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waitErr != nil {
		return toolapi.ChildResult{}, c.waitErr
	}
	c.status = c.result.Status
	return c.result, nil
}

// runnerHost 让 Host.Spawn() 返回指定的 ChildRunner。
type runnerHost struct {
	*plugintest.Host
	runner toolapi.ChildRunner
}

func (h runnerHost) Spawn() toolapi.ChildRunner { return h.runner }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ============ 用例 ============

// TestSpawnNonBlockingReturnsID wait=false 必须立即返回 ID 而不是等结果。
//
// 这是整个协作闭环的前提：改造前 spawn 强制阻塞，父代理永远拿不到在途句柄，
// 后续 wait/stop/reject 全都无从谈起。
func TestSpawnNonBlockingReturnsID(t *testing.T) {
	r := newTrackingRunner()
	h := runnerHost{Host: plugintest.New(t), runner: r}
	res, err := spawnSubagent(context.Background(), h, &toolapi.Call{
		Args: []byte(`{"task":"调研 A","wait":false}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "sub-1") || !strings.Contains(res.Content, "running") {
		t.Fatalf("非阻塞应返回 ID 与 running 状态: %q", res.Content)
	}
	// 记录必须落盘（reject 返工依赖原任务描述）
	rec, _ := loadRecord(context.Background(), h, "sub-1")
	if rec == nil || rec.Task != "调研 A" {
		t.Fatalf("派发记录未落盘: %+v", rec)
	}
}

// TestSpawnDefaultsToBlocking wait 缺省必须是阻塞（保持既有行为零变化）。
func TestSpawnDefaultsToBlocking(t *testing.T) {
	r := newTrackingRunner()
	h := runnerHost{Host: plugintest.New(t), runner: r}
	res, _ := spawnSubagent(context.Background(), h, &toolapi.Call{Args: []byte(`{"task":"X"}`)})
	if !strings.Contains(res.Content, "子代理完成") {
		t.Fatalf("缺省应阻塞并返回结果: %q", res.Content)
	}
}

// TestGetStatusReflectsRuntime get_subagent_status 状态以运行时为准。
func TestGetStatusReflectsRuntime(t *testing.T) {
	r := newTrackingRunner()
	h := runnerHost{Host: plugintest.New(t), runner: r}
	_, _ = spawnSubagent(context.Background(), h, &toolapi.Call{Args: []byte(`{"task":"A","wait":false}`)})

	res, _ := getSubagentStatus(context.Background(), h, &toolapi.Call{Args: []byte(`{"subagent_id":"sub-1"}`)})
	if !strings.Contains(res.Content, "running") {
		t.Fatalf("应显示 running: %q", res.Content)
	}
	// 未知 ID 必须如实报告（不能假装成功）
	res, _ = getSubagentStatus(context.Background(), h, &toolapi.Call{Args: []byte(`{"subagent_id":"nope"}`)})
	if !strings.Contains(res.Content, "未找到") {
		t.Fatalf("未知 ID 应如实报告: %q", res.Content)
	}
}

// TestWaitSubagentsAggregates wait_subagents 汇总结果并落盘状态。
func TestWaitSubagentsAggregates(t *testing.T) {
	r := newTrackingRunner()
	h := runnerHost{Host: plugintest.New(t), runner: r}
	_, _ = spawnSubagent(context.Background(), h, &toolapi.Call{Args: []byte(`{"task":"A","wait":false}`)})
	_, _ = spawnSubagent(context.Background(), h, &toolapi.Call{Args: []byte(`{"task":"B","wait":false}`)})

	res, _ := waitSubagents(context.Background(), h, &toolapi.Call{Args: []byte(`{}`)})
	if !strings.Contains(res.Content, "子代理汇总") ||
		!strings.Contains(res.Content, "sub-1") || !strings.Contains(res.Content, "sub-2") {
		t.Fatalf("汇总结果不符: %q", res.Content)
	}
	rec, _ := loadRecord(context.Background(), h, "sub-1")
	if rec == nil || rec.Status != "completed" {
		t.Fatalf("等待后状态应落盘为 completed: %+v", rec)
	}
}

// TestWaitSubagentsReportsRunning wait 未完成时必须提示"严禁提前输出结论"。
func TestWaitSubagentsReportsRunning(t *testing.T) {
	r := newTrackingRunner()
	h := runnerHost{Host: plugintest.New(t), runner: r}
	_, _ = spawnSubagent(context.Background(), h, &toolapi.Call{Args: []byte(`{"task":"A","wait":false}`)})
	// 让子代理等待超时（模拟仍在运行）
	r.Get2("sub-1").waitErr = errors.New("仍在运行")

	res, _ := waitSubagents(context.Background(), h, &toolapi.Call{Args: []byte(`{"timeout_secs":1}`)})
	if !strings.Contains(res.Content, "严禁提前输出最终业务结论") {
		t.Fatalf("未完成时必须显式警示: %q", res.Content)
	}
}

// TestRejectSubagentRedispatches reject 必须带整改要求重新派发。
func TestRejectSubagentRedispatches(t *testing.T) {
	r := newTrackingRunner()
	var seen []toolapi.SpawnRequest
	r.onSpawn = func(req toolapi.SpawnRequest) { seen = append(seen, req) }
	h := runnerHost{Host: plugintest.New(t), runner: r}
	_, _ = spawnSubagent(context.Background(), h, &toolapi.Call{Args: []byte(`{"task":"实现 X","wait":false}`)})

	res, _ := rejectSubagent(context.Background(), h, &toolapi.Call{Args: []byte(
		`{"subagent_id":"sub-1","feedback":"缺少边界检查"}`)})
	if !strings.Contains(res.Content, "已驳回") || !strings.Contains(res.Content, "第 2 次尝试") {
		t.Fatalf("驳回结果不符: %q", res.Content)
	}
	// 重新派发的任务必须同时含原任务与整改要求
	if len(seen) != 2 || !strings.Contains(seen[1].Input, "实现 X") ||
		!strings.Contains(seen[1].Input, "缺少边界检查") {
		t.Fatalf("返工任务未包含原任务与整改要求: %+v", seen)
	}
	// 原记录应标记为已驳回（可追溯）
	prev, _ := loadRecord(context.Background(), h, "sub-1")
	if prev == nil || prev.Status != "rejected" {
		t.Fatalf("原记录应标记 rejected: %+v", prev)
	}
}

// TestRejectWithoutTaskRejected 找不到原任务且未给 task 时必须报错。
func TestRejectWithoutTaskRejected(t *testing.T) {
	h := runnerHost{Host: plugintest.New(t), runner: newTrackingRunner()}
	res, _ := rejectSubagent(context.Background(), h, &toolapi.Call{Args: []byte(
		`{"subagent_id":"ghost","feedback":"f"}`)})
	if !res.IsError || !strings.Contains(res.Content, "找不到原任务描述") {
		t.Fatalf("无原任务必须报错: %q", res.Content)
	}
}

// TestStopSubagentCancels stop 必须真的调用 Cancel 并落盘 cancelled。
func TestStopSubagentCancels(t *testing.T) {
	r := newTrackingRunner()
	h := runnerHost{Host: plugintest.New(t), runner: r}
	_, _ = spawnSubagent(context.Background(), h, &toolapi.Call{Args: []byte(`{"task":"A","wait":false}`)})

	res, _ := stopSubagent(context.Background(), h, &toolapi.Call{Args: []byte(`{"subagent_id":"sub-1"}`)})
	if !strings.Contains(res.Content, "已停止") {
		t.Fatalf("停止结果不符: %q", res.Content)
	}
	if !r.Get2("sub-1").cancelled {
		t.Fatal("必须真的调用 Cancel")
	}
	rec, _ := loadRecord(context.Background(), h, "sub-1")
	if rec == nil || rec.Status != "cancelled" {
		t.Fatalf("状态应落盘为 cancelled: %+v", rec)
	}
}

// TestSubagentManifestToolFamily 清单必须声明完整协作闭环的五个工具。
func TestSubagentManifestToolFamily(t *testing.T) {
	want := []string{"spawn_subagent", "get_subagent_status", "wait_subagents", "reject_subagent", "stop_subagent"}
	got := map[string]bool{}
	for _, s := range Manifest().Tools {
		got[s.Name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("缺少工具 %s（实际: %v）", name, got)
		}
	}
}

// Get2 取桩句柄（测试内需要推进其状态）。
func (r *trackingRunner) Get2(id string) *trackedChild {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.children[id]
}
