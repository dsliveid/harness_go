package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"harness-go/internal/model"
)

func newTestStores(t *testing.T) (context.Context, *MainStore, *SessionStore) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	m, err := OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("OpenMain 失败: %v", err)
	}
	ss, err := OpenSessionStore(SessionStoreOptions{DataDir: dir, MaxOpen: 8})
	if err != nil {
		t.Fatalf("OpenSessionStore 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = ss.Close()
		_ = m.Close()
	})
	return ctx, m, ss
}

func TestSessionRepoCRUD(t *testing.T) {
	ctx, m, _ := newTestStores(t)
	repo := m.Sessions()

	sess := &model.Session{ID: "s1", Title: "第一个会话", ModelRef: "p/m"}
	if err := repo.Create(ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	got, err := repo.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	if got.Title != "第一个会话" || got.AccessMode != "confirm" {
		t.Fatalf("会话字段错误: %+v", got)
	}
	if got.MessageCount != 0 || got.TotalTokens != 0 {
		t.Fatalf("摘要列初值应为 0: %+v", got)
	}

	// 摘要回写：消息 +1，token 累加，Run 终态
	if err := repo.TouchMessage(ctx, "s1", 1700000000000); err != nil {
		t.Fatalf("回写消息摘要失败: %v", err)
	}
	if err := repo.AddTokensAndStatus(ctx, "s1", 123, model.RunCompleted); err != nil {
		t.Fatalf("回写 Run 摘要失败: %v", err)
	}
	got, _ = repo.Get(ctx, "s1")
	if got.MessageCount != 1 {
		t.Fatalf("message_count 应为 1，得到 %d", got.MessageCount)
	}
	if got.TotalTokens != 123 {
		t.Fatalf("total_tokens 应为 123，得到 %d", got.TotalTokens)
	}
	if got.LastRunStatus != model.RunCompleted {
		t.Fatalf("last_run_status 应为 completed，得到 %s", got.LastRunStatus)
	}
	if got.LastMessageAt == nil || *got.LastMessageAt != 1700000000000 {
		t.Fatalf("last_message_at 未写入: %+v", got.LastMessageAt)
	}

	// 列表与统计
	if err := repo.Create(ctx, &model.Session{ID: "s2", Title: "第二个"}); err != nil {
		t.Fatal(err)
	}
	list, err := repo.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("列出会话失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应列出 2 个会话，得到 %d", len(list))
	}
	n, _ := repo.Count(ctx)
	if n != 2 {
		t.Fatalf("会话数应为 2，得到 %d", n)
	}

	// 重命名 + 软删
	if err := repo.Rename(ctx, "s2", "改名了"); err != nil {
		t.Fatalf("重命名失败: %v", err)
	}
	if err := repo.SoftDelete(ctx, "s2"); err != nil {
		t.Fatalf("软删失败: %v", err)
	}
	if _, err := repo.Get(ctx, "s2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("软删后应返回 ErrNotFound，得到 %v", err)
	}
	n, _ = repo.Count(ctx)
	if n != 1 {
		t.Fatalf("软删后会话数应为 1，得到 %d", n)
	}
}

func TestUserAndTokenRepo(t *testing.T) {
	ctx, m, _ := newTestStores(t)
	users := m.Users()
	tokens := m.Tokens()

	n, err := users.Count(ctx)
	if err != nil || n != 0 {
		t.Fatalf("初始账号数应为 0: n=%d err=%v", n, err)
	}

	u := &model.User{ID: "u1", Username: "admin", PasswordHash: "argon2id$xxx", DisplayName: "管理员"}
	if err := users.Create(ctx, u); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	got, err := users.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("查询账号失败: %v", err)
	}
	if got.Role != "owner" {
		t.Fatalf("默认角色应为 owner，得到 %s", got.Role)
	}
	if _, err := users.GetByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在账号应返回 ErrNotFound，得到 %v", err)
	}

	exp := NowMillis() + 3600_000
	tk := &model.Token{ID: "t1", UserID: "u1", Name: "web", Kind: model.TokenKindWebSession,
		TokenHash: "hash-abc", Scope: "write", ExpiresAt: &exp}
	if err := tokens.Create(ctx, tk); err != nil {
		t.Fatalf("创建令牌失败: %v", err)
	}
	gotTk, err := tokens.GetByHash(ctx, "hash-abc")
	if err != nil {
		t.Fatalf("查询令牌失败: %v", err)
	}
	if gotTk.Kind != model.TokenKindWebSession || gotTk.ExpiresAt == nil {
		t.Fatalf("令牌字段错误: %+v", gotTk)
	}
	if err := tokens.Revoke(ctx, "t1", NowMillis()); err != nil {
		t.Fatalf("吊销令牌失败: %v", err)
	}
	gotTk, _ = tokens.GetByHash(ctx, "hash-abc")
	if gotTk.RevokedAt == nil {
		t.Fatal("吊销时间未写入")
	}
	// 重复吊销应返回 ErrNotFound（已吊销）
	if err := tokens.Revoke(ctx, "t1", NowMillis()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复吊销应返回 ErrNotFound，得到 %v", err)
	}
}

func TestSettingsRepo(t *testing.T) {
	ctx, m, _ := newTestStores(t)
	s := m.Settings()

	if _, err := s.Get(ctx, "tools.tiers"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的配置应返回 ErrNotFound，得到 %v", err)
	}
	val := json.RawMessage(`{"read_file":"core","browser_click":"on_demand"}`)
	if err := s.Set(ctx, "tools.tiers", val); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
	got, err := s.Get(ctx, "tools.tiers")
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if string(got) != string(val) {
		t.Fatalf("配置值不一致: %s", got)
	}
	// 覆盖写
	if err := s.Set(ctx, "tools.tiers", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("覆盖配置失败: %v", err)
	}
	got, _ = s.Get(ctx, "tools.tiers")
	if string(got) != "{}" {
		t.Fatalf("覆盖后配置值错误: %s", got)
	}
}

func TestMessageRepoAppendAndList(t *testing.T) {
	ctx, _, ss := newTestStores(t)
	sdb, err := ss.Acquire(ctx, "sess-msg")
	if err != nil {
		t.Fatal(err)
	}
	repo := sdb.Messages()

	for i := 0; i < 3; i++ {
		m := &model.Message{ID: "m" + string(rune('1'+i)), Role: model.RoleUser, Content: "hi"}
		if err := repo.Append(ctx, m); err != nil {
			t.Fatalf("追加消息失败: %v", err)
		}
		if m.Seq != int64(i+1) {
			t.Fatalf("seq 应递增为 %d，得到 %d", i+1, m.Seq)
		}
	}
	list, err := repo.List(ctx, 0, 10)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("应有 3 条消息，得到 %d", len(list))
	}
	if list[0].Seq != 1 || list[2].Seq != 3 {
		t.Fatalf("消息顺序错误: %+v", list)
	}

	// 增量读取
	after, err := repo.List(ctx, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Seq != 3 {
		t.Fatalf("afterSeq 分页错误: %+v", after)
	}

	n, _ := repo.Count(ctx)
	if n != 3 {
		t.Fatalf("消息数应为 3，得到 %d", n)
	}

	if err := repo.UpdateTokenCount(ctx, "m1", 42); err != nil {
		t.Fatalf("更新 token 数失败: %v", err)
	}
	first, _ := repo.List(ctx, 0, 1)
	if first[0].TokenCount != 42 {
		t.Fatalf("token_count 未更新: %d", first[0].TokenCount)
	}
}

func TestRunRepoStateMachine(t *testing.T) {
	ctx, _, ss := newTestStores(t)
	sdb, err := ss.Acquire(ctx, "sess-run")
	if err != nil {
		t.Fatal(err)
	}
	repo := sdb.Runs()

	run := &model.Run{ID: "r1", ModelRef: "p/m"}
	if err := repo.Create(ctx, run); err != nil {
		t.Fatalf("创建 Run 失败: %v", err)
	}
	got, err := repo.Get(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.RunPending || got.Executor != model.ExecutorServer {
		t.Fatalf("默认状态错误: %+v", got)
	}

	// pending → running
	if err := repo.Transition(ctx, "r1", model.RunRunning, "", "", ""); err != nil {
		t.Fatalf("迁移到 running 失败: %v", err)
	}
	got, _ = repo.Get(ctx, "r1")
	if got.StartedAt == nil {
		t.Fatal("running 应写入 started_at")
	}

	// running → waiting_approval → completed
	if err := repo.Transition(ctx, "r1", model.RunWaitingApproval, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.Transition(ctx, "r1", model.RunCompleted, "", "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.Get(ctx, "r1")
	if got.Status != model.RunCompleted || got.EndedAt == nil {
		t.Fatalf("终态错误: %+v", got)
	}

	// 终态不可再迁移（单点收口）
	if err := repo.Transition(ctx, "r1", model.RunRunning, "", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("终态迁移应被拒绝，得到 %v", err)
	}
	// 非法状态值
	if err := repo.Transition(ctx, "r1", model.RunStatus("bogus"), "", "", ""); err == nil {
		t.Fatal("非法状态应报错")
	}
}

// 进程重启：未完成 Run 全部置 interrupted（ADR-009 不做断点续跑）。
func TestFailOpenRunsOnRestart(t *testing.T) {
	ctx, _, ss := newTestStores(t)
	sdb, err := ss.Acquire(ctx, "sess-restart")
	if err != nil {
		t.Fatal(err)
	}
	repo := sdb.Runs()

	mustCreate := func(id string, status model.RunStatus) {
		if err := repo.Create(ctx, &model.Run{ID: id, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	mustCreate("r-pending", model.RunPending)
	mustCreate("r-running", model.RunRunning)
	mustCreate("r-waiting", model.RunWaitingApproval)
	mustCreate("r-done", model.RunCompleted)

	unfinished, err := repo.ListUnfinished(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfinished) != 3 {
		t.Fatalf("未完成 Run 应为 3，得到 %d", len(unfinished))
	}

	n, err := repo.FailOpenRuns(ctx, "process_restart")
	if err != nil {
		t.Fatalf("标记中断失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("应标记 3 个 Run 为中断，得到 %d", n)
	}

	got, _ := repo.Get(ctx, "r-running")
	if got.Status != model.RunInterrupted {
		t.Fatalf("应置为 interrupted，得到 %s", got.Status)
	}
	if got.StopReason != "process_restart" {
		t.Fatalf("stop_reason 错误: %s", got.StopReason)
	}
	// 已完成的不受影响
	done, _ := repo.Get(ctx, "r-done")
	if done.Status != model.RunCompleted {
		t.Fatalf("已完成 Run 不应被改动，得到 %s", done.Status)
	}
	if n2, _ := repo.FailOpenRuns(ctx, ""); n2 != 0 {
		t.Fatalf("再次调用应无影响，实际 %d", n2)
	}
}

func TestToolEventRepo(t *testing.T) {
	ctx, _, ss := newTestStores(t)
	sdb, err := ss.Acquire(ctx, "sess-tool")
	if err != nil {
		t.Fatal(err)
	}
	repo := sdb.ToolEvents()

	ev := &model.ToolEvent{ID: "e1", RunID: "r1", ToolName: "echo",
		ArgsJSON: json.RawMessage(`{"msg":"hi"}`), Risk: model.RiskReadonly}
	if err := repo.Insert(ctx, ev); err != nil {
		t.Fatalf("记录工具事件失败: %v", err)
	}
	// 端侧结果默认不可信标记需由调用方显式设置；这里验证默认值
	list, err := repo.ListByRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 条事件，得到 %d", len(list))
	}
	if list[0].Namespace != "builtin" || list[0].SourceTrust != model.TrustTrusted {
		t.Fatalf("默认字段错误: %+v", list[0])
	}

	if err := repo.Finish(ctx, "e1", model.ToolOK, json.RawMessage(`{"ok":true}`), "", ""); err != nil {
		t.Fatalf("结束工具事件失败: %v", err)
	}
	list, _ = repo.ListByRun(ctx, "r1")
	if list[0].Status != model.ToolOK || list[0].EndedAt == nil {
		t.Fatalf("终态错误: %+v", list[0])
	}
	if string(list[0].ResultJSON) != `{"ok":true}` {
		t.Fatalf("结果未写入: %s", list[0].ResultJSON)
	}
}
