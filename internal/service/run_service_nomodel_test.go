package service

import (
	"encoding/json"
	"testing"
	"time"

	"harness-go/internal/events"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// 「未选择模型」的发送口径（2026-11 产品决策）：
//
//	消息照常落库（消息记录保留）→ Run 立即 completed（发送成功）→ 本轮**不产生回复**。
//
// 这组用例守住四件事，缺一即回归：
//  1. 用户消息必须落库 —— 否则刷新后消息凭空消失，「保留记录」诉求直接失效；
//  2. Run 终态必须是 completed（不是 failed）—— 这是「发送成功只是没回复」的语义根基；
//  3. 会话库内**不得出现 assistant 消息** —— 旧实现会把空 model_ref 折算成 "default"，
//     一路落到 fake provider 产出假回复，这正是本需求要消除的行为；
//  4. 终态事件必须下发 —— 前端已乐观上屏空气泡，没有终态就会永远转圈。

// newNoModelEnv 最小环境：装引擎 + RunService（服务端内置空间，恒在线可对话）。
func newNoModelEnv(t *testing.T) (*runAdmissionEnv, string) {
	t.Helper()
	env := newRunAdmissionEnv(t)
	sess := &model.Session{
		ID: "sess-nomodel", Title: "未选模型会话",
		// 服务端内置空间：不受任何设备在线状态影响（R5），排除离线干扰项。
		WorkspaceID: store.BuiltinWorkspaceServerID,
		AccessMode:  "full",
		// 🚨 关键：会话自身也不选模型（ModelRef 空串）
		ModelRef: "",
	}
	if err := env.main.Sessions().Create(env.ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	return env, sess.ID
}

// TestCreateRunWithoutModelCompletesImmediately 未选模型 → 立即 completed，不进引擎。
func TestCreateRunWithoutModelCompletesImmediately(t *testing.T) {
	env, sid := newNoModelEnv(t)

	run, err := env.svc.CreateRun(env.ctx, sid, "这条消息只应被保存", "")
	if err != nil {
		t.Fatalf("未选模型时应照常受理发送，却报错: %v", err)
	}
	if run == nil || run.ID == "" {
		t.Fatal("应返回有效 Run")
	}
	// 断言 1：建库即终态（同步返回的 Run 就是 completed，不存在 pending 空窗）
	if run.Status != model.RunCompleted {
		t.Fatalf("未选模型的 Run 应直接 completed，实际 %s", run.Status)
	}

	// 断言 2：用户消息已落库（同步写，无需轮询）
	sdb, err := env.sessions.Acquire(env.ctx, sid)
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	msgs, err := sdb.Messages().List(env.ctx, 0, 100)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("未选模型应只保存 1 条用户消息，实际 %d 条（不得产生助手回复）", len(msgs))
	}
	if msgs[0].Role != model.RoleUser {
		t.Fatalf("唯一一条消息应为 user，实际 %s", msgs[0].Role)
	}
	if msgs[0].Content != "这条消息只应被保存" {
		t.Fatalf("用户消息正文被改动: %q", msgs[0].Content)
	}

	// 断言 3：DB 里的 Run 也是终态（启动恢复不会把它当成卡死的 running Run）
	stored, err := sdb.Runs().Get(env.ctx, run.ID)
	if err != nil {
		t.Fatalf("读取 Run 失败: %v", err)
	}
	if stored.Status != model.RunCompleted {
		t.Fatalf("库中 Run 状态应 completed，实际 %s", stored.Status)
	}

	// 断言 4：没有进引擎 → 主库投影不得停在 pending/running（会话列表徽标不粘住）
	s, err := env.main.Sessions().Get(env.ctx, sid)
	if err != nil {
		t.Fatalf("读取会话摘要失败: %v", err)
	}
	if s.LastRunStatus == model.RunPending || s.LastRunStatus == model.RunRunning {
		t.Fatalf("主库 last_run_status 不得停留在进行中，实际 %s", s.LastRunStatus)
	}
}

// TestCreateRunWithoutModelPublishesTerminalEvent 未选模型 → 下发 completed 终态事件。
//
// 为什么单独守：这一轮不进引擎，没有引擎替它发 run.status。若服务层静默返回，
// 前端 startRun 的乐观空气泡会永远转圈（「界面无反应」类缺陷的复发点）。
func TestCreateRunWithoutModelPublishesTerminalEvent(t *testing.T) {
	env, sid := newNoModelEnv(t)

	run, err := env.svc.CreateRun(env.ctx, sid, "你好", "")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	// 从 seq=0 补发（服务层在返回前已发布并 CloseRun，历史仍在环形缓冲里）
	sub, err := env.bus.Subscribe(run.ID, 0)
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer env.bus.Unsubscribe(sub)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-sub.C():
			if !ok {
				t.Fatalf("事件流已关闭，未收到 run.status 终态事件")
			}
			if ev.Type != events.TypeRunStatus {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatalf("解析事件载荷失败: %v", err)
			}
			if payload["status"] != string(model.RunCompleted) {
				t.Fatalf("终态事件应为 completed，实际 %v", payload["status"])
			}
			if payload["stop_reason"] != "no_model_selected" {
				t.Fatalf("stop_reason 应如实标注 no_model_selected，实际 %v", payload["stop_reason"])
			}
			return
		case <-deadline:
			t.Fatal("等待 run.status 终态事件超时（前端会永远转圈）")
		}
	}
}

// TestCreateRunWithModelStillRunsEngine 有模型时既有流程零回归（反向守卫）。
//
// 断言「真的进了引擎」：必须产出助手消息。若上面的 noModel 分支误伤了正常路径，
// 本用例会第一时间失败。
func TestCreateRunWithModelStillRunsEngine(t *testing.T) {
	env, sid := newNoModelEnv(t)
	if _, err := env.svc.CreateRun(env.ctx, sid, "有模型的正常对话", "fake-model"); err != nil {
		t.Fatalf("有模型时应正常受理: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sdb, aErr := env.sessions.Acquire(env.ctx, sid); aErr == nil {
			if msgs, lErr := sdb.Messages().List(env.ctx, 0, 100); lErr == nil {
				for _, m := range msgs {
					if m.Role == model.RoleAssistant {
						// 引擎产出过助手消息 → 正常路径未被 noModel 分支吞掉
						return
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("有模型时应进引擎并产出助手消息（noModel 分支不得误伤正常对话）")
}
