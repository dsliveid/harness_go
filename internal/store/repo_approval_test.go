package store

import (
	"context"
	"database/sql"
	"testing"
)

// C-7：审批决策审计列落库（requested_by_device / decided_by_device）。
func TestApprovalDecidePersistsDeviceAudit(t *testing.T) {
	ctx := context.Background()
	ss, err := OpenSessionStore(SessionStoreOptions{DataDir: t.TempDir(), MaxOpen: 2})
	if err != nil {
		t.Fatalf("OpenSessionStore 失败: %v", err)
	}
	defer ss.Close()

	sdb, err := ss.Acquire(ctx, "sess-c7-audit")
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}

	// ① 带跨端信息：两列都落库
	if err := sdb.Approvals().DecideWithDevices(ctx, ApprovalDecision{
		ID: "ap-dev", RunID: "r-1", ToolEventID: "te-1",
		Decision:          "allow_once",
		Reason:            "端侧确认",
		RequestedByDevice: "dev-1",
		DecidedByDevice:   "dev-1",
	}); err != nil {
		t.Fatalf("DecideWithDevices 失败: %v", err)
	}

	var (
		decision    string
		reason      sql.NullString
		requestedBy sql.NullString
		decidedBy   sql.NullString
		requestedAt int64
	)
	if err := sdb.conn().QueryRowContext(ctx,
		`SELECT decision, reason, requested_by_device, decided_by_device, requested_at
		   FROM approvals WHERE id='ap-dev'`).
		Scan(&decision, &reason, &requestedBy, &decidedBy, &requestedAt); err != nil {
		t.Fatalf("查询审批记录失败: %v", err)
	}
	if decision != "allow_once" || reason.String != "端侧确认" {
		t.Fatalf("决策/原因落库错误: %s / %v", decision, reason)
	}
	if requestedBy.String != "dev-1" || decidedBy.String != "dev-1" {
		t.Fatalf("跨端审计列落库错误: req=%v dec=%v", requestedBy, decidedBy)
	}
	if requestedAt == 0 {
		t.Fatal("requested_at 应为毫秒时间戳")
	}

	// ② 幂等覆盖：同 id 再决策，审计列按新值更新
	if err := sdb.Approvals().DecideWithDevices(ctx, ApprovalDecision{
		ID: "ap-dev", RunID: "r-1", ToolEventID: "te-1",
		Decision:          "deny",
		RequestedByDevice: "dev-1",
		DecidedByDevice:   "dev-2",
	}); err != nil {
		t.Fatalf("二次决策失败: %v", err)
	}
	var cnt int
	if err := sdb.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM approvals WHERE id='ap-dev'`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("审批记录应幂等覆盖（1 行），得到 %d", cnt)
	}
	if err := sdb.conn().QueryRowContext(ctx,
		`SELECT decision, decided_by_device FROM approvals WHERE id='ap-dev'`).
		Scan(&decision, &decidedBy); err != nil {
		t.Fatal(err)
	}
	if decision != "deny" || decidedBy.String != "dev-2" {
		t.Fatalf("覆盖后审计列错误: %s / %v", decision, decidedBy)
	}

	// ③ 既有 Decisions 调用（无跨端信息）→ 两列写 NULL，行为与 C 之前一致
	if err := sdb.Approvals().Decide(ctx, "ap-local", "r-2", "te-2", "allow_session", ""); err != nil {
		t.Fatalf("Decide 失败: %v", err)
	}
	if err := sdb.conn().QueryRowContext(ctx,
		`SELECT requested_by_device, decided_by_device FROM approvals WHERE id='ap-local'`).
		Scan(&requestedBy, &decidedBy); err != nil {
		t.Fatal(err)
	}
	if requestedBy.Valid || decidedBy.Valid {
		t.Fatalf("服务端本地审批的跨端列应为 NULL: req=%v dec=%v", requestedBy, decidedBy)
	}

	// ④ 缺必要字段必须报错（不静默写脏数据）
	if err := sdb.Approvals().DecideWithDevices(ctx, ApprovalDecision{ID: "", RunID: "r-3"}); err == nil {
		t.Fatal("缺 id 应报错")
	}
	if err := sdb.Approvals().DecideWithDevices(ctx, ApprovalDecision{ID: "ap-x", RunID: ""}); err == nil {
		t.Fatal("缺 run_id 应报错")
	}
}
