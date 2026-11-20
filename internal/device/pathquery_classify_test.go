package device

import (
	"context"
	"testing"
	"time"

	"harness-go/internal/apperr"
)

// ============ 目录列举失败归类（纯函数 + 失败快照）回归测试 ============
//
// 背景：桌面端协议壳没实现 fs.list 帧时，serde 把帧归为 Unknown 静默丢弃，
// 服务端只能等满 3 秒超时。此时「超时」与「端侧真慢」在服务端完全同形，
// 必须靠「同设备连续超时次数 + 是否收到过任何迟到应答」这对事实区分开：
//   - 端侧未实现 → 永远等不到任何应答（含迟到），连续超时次数持续增长；
//   - 端侧慢了   → 迟到应答终会到达，Deliver 失配时必须把该事实记下来。
// 一旦把「慢」误判成「未实现」，用户会被劝去升级客户端，真实可用的通道被判死。

// ① classifyPathQueryFailure 表驱动：判据必须只有这一个出口。
func TestClassifyPathQueryFailure(t *testing.T) {
	cases := []struct {
		name         string
		consecutive  int
		sawLateReply bool
		want         apperr.Code
	}{
		{"首次超时（可重试）", 1, false, apperr.CodeFSHostTimeout},
		{"连续 2 次且无迟到应答（客户端未实现）", 2, false, apperr.CodeFSHostUnsupported},
		{"连续 5 次且无迟到应答（仍判未实现）", 5, false, apperr.CodeFSHostUnsupported},
		{"连续 2 次但有迟到应答（端侧只是慢）", 2, true, apperr.CodeFSHostTimeout},
		{"连续 9 次但有迟到应答（仍判超时）", 9, true, apperr.CodeFSHostTimeout},
		{"首次超时且有迟到应答", 1, true, apperr.CodeFSHostTimeout},
		// 防御：consecutive=0 不该发生（只会在超时分支调用），保守按可重试处理。
		{"0 次的防御形态", 0, false, apperr.CodeFSHostTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPathQueryFailure(tc.consecutive, tc.sawLateReply); got != tc.want {
				t.Fatalf("consecutive=%d sawLate=%v 应为 %s，得到 %s",
					tc.consecutive, tc.sawLateReply, tc.want, got)
			}
		})
	}
}

// ② 两档文案必须可区分，且 unsupported 档不得出现「稍后重试」这类诱导重试的措辞。
func TestPathQueryFailureMessageTiers(t *testing.T) {
	timeoutMsg := pathQueryFailureMessage(apperr.CodeFSHostTimeout)
	unsupportedMsg := pathQueryFailureMessage(apperr.CodeFSHostUnsupported)

	if timeoutMsg == unsupportedMsg {
		t.Fatal("两档文案必须不同，否则用户无法区分可重试与不可重试")
	}
	if !contains(timeoutMsg, "重试") {
		t.Fatalf("超时档应提示可重试，实际 %q", timeoutMsg)
	}
	if contains(unsupportedMsg, "重试") {
		t.Fatalf("未实现档不得诱导重试，实际 %q", unsupportedMsg)
	}
	if !contains(unsupportedMsg, "未实现") {
		t.Fatalf("未实现档必须明确「未实现」，实际 %q", unsupportedMsg)
	}
}

// ③ 失败快照：连续计数递增、被正常应答清零、快照可查询、可重置。
func TestPathQueryFailureSnapshotLifecycle(t *testing.T) {
	q, _, _ := newPathQueryEnv(t, "dev-1")
	q.ResetFailureState()

	// 首次超时：计数 1。
	timedOutOnce(t, q)
	st, ok := q.FailureSnapshot("dev-1")
	if !ok || st.Consecutive != 1 {
		t.Fatalf("首次超时后连续次数应为 1，得到 %+v ok=%v", st, ok)
	}
	if st.SawLateReply {
		t.Fatal("尚未收到迟到应答，sawLateReply 必须为 false")
	}
	if st.LastFailureAt.IsZero() {
		t.Fatal("最近失败时间必须被记录")
	}

	// 第二次超时：计数 2 且归类升级为未实现。
	timedOutOnce(t, q)
	st, _ = q.FailureSnapshot("dev-1")
	if st.Consecutive != 2 {
		t.Fatalf("第二次超时后连续次数应为 2，得到 %d", st.Consecutive)
	}

	// 正常应答必须清零连续计数（避免偶发抖动永久累积成「未实现」），
	// 同时结束观察期：sawLateReply 也要清零，否则设备退化回「完全不回帧」后
	// 会被永久钉在可重试档，用户被无限劝去重试 3 秒。
	q.noteReply("dev-1")
	st, _ = q.FailureSnapshot("dev-1")
	if st.Consecutive != 0 {
		t.Fatalf("正常应答后连续次数必须清零，得到 %d", st.Consecutive)
	}
	if st.SawLateReply {
		t.Fatal("正常应答后 sawLateReply 必须清零（观察期已结束）")
	}

	// 重置入口必须彻底清空（测试隔离依赖它）。
	q.ResetFailureState()
	if _, ok := q.FailureSnapshot("dev-1"); ok {
		t.Fatal("ResetFailureState 后不应残留快照")
	}
}

// ④ 迟到应答不得误判：超时后到达的应答必须把 sawLateReply 打上，从而让后续
// 连续超时归类为「可重试的超时」而非「客户端未实现」。
func TestPathQueryLateReplyPreventsUnsupportedVerdict(t *testing.T) {
	q, c, _ := newPathQueryEnv(t, "dev-1")
	q.ResetFailureState()

	// 第一次超时（不等真 3 秒，用带 deadline 的 ctx 提前触发）。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := q.List(ctx, "dev-1", ""); err == nil {
		t.Fatal("无应答必须超时")
	}
	firstReqID := c.sent[0].ID

	// 端侧迟到的应答：Deliver 必然返回 false（等待者已清理），但事实必须被记住。
	if q.Deliver(firstReqID, FsListResultPayload{Separator: `\`}) {
		t.Fatal("等待者已超时清理，迟到应答应返回 false")
	}
	st, ok := q.FailureSnapshot("dev-1")
	if !ok || !st.SawLateReply {
		t.Fatalf("迟到应答必须标记 sawLateReply，得到 %+v ok=%v", st, ok)
	}

	// 第二次超时：有迟到应答 ⇒ 仍判 fs.host_timeout（可重试），绝不误判为未实现。
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	_, err := q.List(ctx2, "dev-1", "")
	if err == nil {
		t.Fatal("无应答必须超时")
	}
	if code := errCode(err); code != apperr.CodeFSHostTimeout {
		t.Fatalf("收到过迟到应答时应判 fs.host_timeout（可重试），得到 %s", code)
	}
	st, _ = q.FailureSnapshot("dev-1")
	if st.Consecutive != 2 {
		t.Fatalf("连续次数应为 2，得到 %d", st.Consecutive)
	}
	if !st.SawLateReply {
		t.Fatal("sawLateReply 不得因新的一次超时而丢失")
	}
}

// ⑤ 连续 2 次超时且从未有过迟到应答 → 归类升级为 fs.host_unsupported。
func TestPathQueryConsecutiveTimeoutsBecomeUnsupported(t *testing.T) {
	q, _, _ := newPathQueryEnv(t, "dev-1")
	q.ResetFailureState()

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := q.List(ctx, "dev-1", "")
		if err == nil {
			t.Fatalf("第 %d 次无应答必须超时", i+1)
		}
		code := errCode(err)
		if i == 0 && code != apperr.CodeFSHostTimeout {
			t.Fatalf("第 1 次超时应为 fs.host_timeout，得到 %s", code)
		}
		if i == 1 && code != apperr.CodeFSHostUnsupported {
			t.Fatalf("第 2 次超时应归类为 fs.host_unsupported，得到 %s", code)
		}
		if i == 1 && !contains(err.Error(), "未实现") {
			t.Fatalf("未实现档文案须含「未实现」，实际 %q", err.Error())
		}
	}
}

// ⑥ 无 waiting 且无归属的迟到应答不得 panic，也不得凭空造快照。
func TestPathQueryUnknownLateReplyIsIgnored(t *testing.T) {
	q, _, _ := newPathQueryEnv(t, "dev-1")
	q.ResetFailureState()

	if q.Deliver("fsq-unknown", FsListResultPayload{}) {
		t.Fatal("无人等待的应答应返回 false")
	}
	if _, ok := q.FailureSnapshot("dev-1"); ok {
		t.Fatal("无从归因的迟到应答不得凭空创建快照")
	}
	var nilQ *PathQuery
	if nilQ.Deliver("x", FsListResultPayload{}) {
		t.Fatal("nil RPC 必须安全返回 false")
	}
	nilQ.ResetFailureState()
	if _, ok := nilQ.FailureSnapshot("dev-1"); ok {
		t.Fatal("nil RPC 查询必须返回 ok=false")
	}
}

// timedOutOnce 制造一次「无人应答」的超时（用 ctx deadline 提前打断，不真等 3 秒）。
func timedOutOnce(t *testing.T, q *PathQuery) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := q.List(ctx, "dev-1", ""); err == nil {
		t.Fatal("无应答必须超时报错")
	}
}
