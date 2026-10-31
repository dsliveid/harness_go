package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"harness-go/internal/llm"
	"harness-go/internal/model"
)

// ============ C-6：引擎接入会话租约 ============

// fakeLeaseManager 记录 Acquire/Renew/Release 调用序列。
type fakeLeaseManager struct {
	mu         sync.Mutex
	acquired   []string // sessionID
	renewed    []string
	released   []string
	holders    []string
	acquireErr error
	renewErr   error
	epoch      int64
}

func (f *fakeLeaseManager) Acquire(_ context.Context, sessionID, holder string) (model.SessionLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, sessionID)
	f.holders = append(f.holders, holder)
	if f.acquireErr != nil {
		return model.SessionLease{}, f.acquireErr
	}
	f.epoch++
	return model.SessionLease{SessionID: sessionID, DeviceID: holder, Epoch: f.epoch}, nil
}

func (f *fakeLeaseManager) Renew(_ context.Context, sessionID, holder string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewed = append(f.renewed, sessionID)
	f.holders = append(f.holders, holder)
	return f.renewErr
}

func (f *fakeLeaseManager) Release(_ context.Context, sessionID, holder string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, sessionID)
	f.holders = append(f.holders, holder)
	return nil
}

func (f *fakeLeaseManager) snapshot() (acquired, renewed, released []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acquired...),
		append([]string(nil), f.renewed...),
		append([]string(nil), f.released...)
}

// 引擎在 Execute 中 Acquire → 每步 Renew → 结束时 Release。
func TestEngineLeaseLifecycle(t *testing.T) {
	leases := &fakeLeaseManager{}
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "好"}}}}
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) { d.Leases = leases })

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	acquired, renewed, released := leases.snapshot()
	if len(acquired) != 1 || acquired[0] != "sess-1" {
		t.Fatalf("应 Acquire 一次 sess-1，得到 %v", acquired)
	}
	if len(renewed) != 1 {
		t.Fatalf("单步 Run 应 Renew 一次，得到 %d", len(renewed))
	}
	if len(released) != 1 || released[0] != "sess-1" {
		t.Fatalf("应 Release 一次，得到 %v", released)
	}
	// 默认 holder 为空串（服务端本地执行面）
	if leases.holders[0] != "" {
		t.Fatalf("服务端 Run 的 holder 应为空串，得到 %q", leases.holders[0])
	}
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("Run 应完成，得到 %s", sdb.run.Status)
	}
}

// RunRequest.LeaseHolder 透传到租约（端侧 Run 抢占旧端）。
func TestEngineLeaseHolderFromRequest(t *testing.T) {
	leases := &fakeLeaseManager{}
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "ok"}}}}
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) { d.Leases = leases })

	req := baseRunRequest()
	req.LeaseHolder = "dev-42"
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	leases.mu.Lock()
	holders := append([]string(nil), leases.holders...)
	leases.mu.Unlock()
	for _, h := range holders {
		if h != "dev-42" {
			t.Fatalf("holder 应全为 dev-42，得到 %v", holders)
		}
	}
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("Run 应完成，得到 %s", sdb.run.Status)
	}
}

// 租约失败绝不阻断 Run（协调信息，不是业务正确性来源）。
func TestEngineLeaseFailureIsNonFatal(t *testing.T) {
	leases := &fakeLeaseManager{
		acquireErr: errors.New("主库抖动"),
		renewErr:   errors.New("主库抖动"),
	}
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "仍然成功"}}}}
	eng, sdb, bus, _ := newTestEngine(t, provider, func(d *Deps) { d.Leases = leases })

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("租约失败不应让 Run 失败: %v", err)
	}
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("Run 应仍为 completed，得到 %s", sdb.run.Status)
	}
	// 非致命告警以 error 事件形式落事件流（Run 仍可继续）
	if _, err := bus.Subscribe("run-1", 0); err != nil {
		t.Fatalf("事件订阅失败: %v", err)
	}
}

// 未注入 Leases 时行为与 C 阶段前完全一致（零改动回归）。
func TestEngineWithoutLeaseManagerUnchanged(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "无租约"}}}}
	eng, sdb, _, _ := newTestEngine(t, provider)

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("Run 应完成，得到 %s", sdb.run.Status)
	}
	if len(sdb.messages) != 1 || sdb.messages[0].Content != "无租约" {
		t.Fatalf("消息落库异常: %+v", sdb.messages)
	}
}
