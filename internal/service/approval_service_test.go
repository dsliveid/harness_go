package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/approval"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ C-7：审批跨端仲裁（decide 端点 device_id + strict 403） ============

// newApprovalTestRig 建真实主库 + 会话库 + 闸门，并造一个「等待审批」的 Run。
//
// 返回：RunService、注册待决审批用的闸门、sessionID/runID/approvalID。
func newApprovalTestRig(t *testing.T, strict bool, requestedBy, required string) (
	svc *RunService, gate *approval.Gate, sid, rid, aid string,
) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })

	ss, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 4})
	if err != nil {
		t.Fatalf("打开会话存储失败: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	gate = approval.NewGate(time.Second)
	svc = NewRunService(RunServiceOptions{
		Main:                 main,
		Sessions:             ss,
		Gate:                 gate,
		StrictApprovalDevice: strict,
	})

	// 会话（主库）+ 会话库文件（首次 Acquire 惰性建库）
	sess := &model.Session{ID: "sess-c7", Title: "审批仲裁", ModelRef: "fake-model"}
	if err := main.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	sdb, err := ss.Acquire(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Acquire 会话库失败: %v", err)
	}

	// Run：pending → waiting_approval
	rid = "run-c7"
	if err := sdb.Runs().Create(ctx, &model.Run{ID: rid, ModelRef: "fake-model"}); err != nil {
		t.Fatalf("创建 Run 失败: %v", err)
	}
	if err := sdb.Runs().Transition(ctx, rid, model.RunWaitingApproval, "", "", ""); err != nil {
		t.Fatalf("Run 置 waiting_approval 失败: %v", err)
	}

	// 工具事件（审批项 = 工具事件 ID）
	aid = "te-c7"
	if err := sdb.ToolEvents().Insert(ctx, &model.ToolEvent{
		ID: aid, RunID: rid, ToolName: "run_command",
		ArgsJSON: []byte(`{"cmd":"rm -rf /"}`), Risk: model.RiskDanger,
	}); err != nil {
		t.Fatalf("插入工具事件失败: %v", err)
	}

	// 闸门登记（带跨端归属）
	if _, err := gate.RegisterWithDevice(aid, requestedBy, required); err != nil {
		t.Fatalf("登记待决审批失败: %v", err)
	}
	return svc, gate, sess.ID, rid, aid
}

// strict 模式 + 非执行端设备 → 403 approval.device_mismatch，且待决项不被消费。
func TestDecideApprovalStrictDeviceMismatch(t *testing.T) {
	svc, gate, sid, rid, aid := newApprovalTestRig(t, true, "dev-1", "dev-1")
	ctx := context.Background()

	for _, deviceID := range []string{"", "dev-2"} {
		err := svc.DecideApprovalFromDevice(ctx, sid, rid, aid, "allow_once", "", deviceID)
		if err == nil {
			t.Fatalf("strict 模式非执行端设备（device=%q）应拒绝", deviceID)
		}
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code != apperr.CodeApprovalDeviceMismatch {
			t.Fatalf("错误码应为 %s，得到 %v", apperr.CodeApprovalDeviceMismatch, err)
		}
		if ae.Status() != 403 {
			t.Fatalf("HTTP 状态应为 403，得到 %d", ae.Status())
		}
	}
	// 被拒后审批仍在等待 → 执行端设备可正常决策
	if gate.PendingCount() != 1 {
		t.Fatalf("被拒不得消费待决项，PendingCount=%d", gate.PendingCount())
	}
	if err := svc.DecideApprovalFromDevice(ctx, sid, rid, aid, "allow_once", "端侧确认", "dev-1"); err != nil {
		t.Fatalf("执行端设备应可决策: %v", err)
	}
	if gate.PendingCount() != 0 {
		t.Fatalf("决策后应无待决项，PendingCount=%d", gate.PendingCount())
	}
}

// 非 strict（默认）→ 只审计不阻断：浏览器端（device 空）也能决策。
func TestDecideApprovalNonStrictAllowsBrowser(t *testing.T) {
	svc, gate, sid, rid, aid := newApprovalTestRig(t, false, "dev-1", "dev-1")
	ctx := context.Background()

	if err := svc.DecideApprovalFromDevice(ctx, sid, rid, aid, "deny", "浏览器拒绝", ""); err != nil {
		t.Fatalf("非 strict 模式应放行: %v", err)
	}
	if gate.PendingCount() != 0 {
		t.Fatalf("决策后应无待决项，PendingCount=%d", gate.PendingCount())
	}

	// 审计列落库：发起端 dev-1，决策端为空（本地管理端）
	sdb, err := svc.sessions.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	rec, err := sdb.Approvals().Get(ctx, aid)
	if err != nil {
		t.Fatalf("查询审批记录失败: %v", err)
	}
	if rec.RequestedByDevice != "dev-1" {
		t.Fatalf("requested_by_device 应为 dev-1，得到 %q", rec.RequestedByDevice)
	}
	if rec.DecidedByDevice != "" {
		t.Fatalf("decided_by_device 应为空（本地管理端），得到 %q", rec.DecidedByDevice)
	}
	if rec.Decision != "deny" || rec.Reason != "浏览器拒绝" {
		t.Fatalf("决策/原因落库错误: %q / %q", rec.Decision, rec.Reason)
	}
}

// 未登记的审批项 → approval.not_found（403 不适用，先判 Known）。
func TestDecideApprovalUnknownApproval(t *testing.T) {
	svc, _, sid, rid, _ := newApprovalTestRig(t, true, "dev-1", "dev-1")
	err := svc.DecideApprovalFromDevice(context.Background(), sid, rid, "not-registered",
		"allow_once", "", "dev-1")
	if err == nil {
		t.Fatal("未登记审批应报错")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.CodeApprovalNotFound {
		t.Fatalf("错误码应为 %s，得到 %v", apperr.CodeApprovalNotFound, err)
	}
}
