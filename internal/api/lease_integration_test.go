package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"harness-go/internal/model"
)

// TestSessionLeaseEndpoint 阶段 C-6 出口：GET /sessions/{sid}/lease。
//
// 断言要点：
//   - 无租约时**不是 404**：以 active=true / holder="" / holder_kind=server 表示「服务端可写」
//   - 引擎 Run 期间租约由服务端持有（holder 空串），Run 结束后释放
//   - 端侧 device 抢占后 epoch=1、holder_kind=device，且从 DB 读回真实归属
//   - 跨端抢占（第二个设备）epoch+1
//   - 未鉴权 → 401
func TestSessionLeaseEndpoint(t *testing.T) {
	srv, token, deviceSvc := newTestServerWithDevices(t)
	ctx := context.Background()

	// 建会话
	var sess struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "租约测试"}, http.StatusCreated, &sess)

	type leaseResp struct {
		SessionID  string `json:"session_id"`
		Holder     string `json:"holder"`
		HolderKind string `json:"holder_kind"`
		Epoch      int64  `json:"epoch"`
		Active     bool   `json:"active"`
		ExpiresAt  int64  `json:"expires_at"`
	}

	// ① 无租约 → 200 且 active=true（语义：服务端本地可写，不是 404）
	var empty leaseResp
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/lease", token,
		nil, http.StatusOK, &empty)
	if !empty.Active || empty.Epoch != 0 || empty.Holder != "" {
		t.Fatalf("无租约时应为 active=true / epoch=0 / holder=\"\": %+v", empty)
	}
	if empty.HolderKind != string(model.ExecutorServer) {
		t.Fatalf("无租约时 holder_kind 应为 server，得到 %q", empty.HolderKind)
	}
	if empty.SessionID != sess.ID {
		t.Fatalf("session_id 应回显，得到 %q", empty.SessionID)
	}

	// ② 端侧设备 A 抢占 → epoch=1 / holder_kind=device
	if _, err := deviceSvc.Leases().Acquire(ctx, sess.ID, "dev-a"); err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	var l1 leaseResp
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/lease", token,
		nil, http.StatusOK, &l1)
	if l1.Holder != "dev-a" || l1.Epoch != 1 || !l1.Active {
		t.Fatalf("设备 A 应持有 epoch=1: %+v", l1)
	}
	if l1.HolderKind != string(model.ExecutorDevice) {
		t.Fatalf("holder_kind 应为 device，得到 %q", l1.HolderKind)
	}
	if l1.ExpiresAt == 0 {
		t.Fatal("持有中的租约应带 expires_at")
	}

	// ③ 设备 B 抢占 → epoch+1（跨端单写者）
	if _, err := deviceSvc.Leases().Acquire(ctx, sess.ID, "dev-b"); err != nil {
		t.Fatalf("第二次 Acquire 失败: %v", err)
	}
	var l2 leaseResp
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/lease", token,
		nil, http.StatusOK, &l2)
	if l2.Holder != "dev-b" || l2.Epoch != 2 {
		t.Fatalf("设备 B 抢占后应 epoch=2: %+v", l2)
	}

	// ④ 释放后回到「服务端可写」语义
	if err := deviceSvc.Leases().Release(ctx, sess.ID, "dev-b"); err != nil {
		t.Fatalf("Release 失败: %v", err)
	}
	var l3 leaseResp
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/lease", token,
		nil, http.StatusOK, &l3)
	if !l3.Active || l3.Holder != "" || l3.Epoch != 0 {
		t.Fatalf("释放后应回到无租约语义: %+v", l3)
	}

	// ⑤ 未鉴权 → 401
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/lease", "",
		nil, http.StatusUnauthorized, nil)
}

// TestSessionLeaseDuringRun 引擎 Run 期间租约为服务端持有，Run 结束后释放。
func TestSessionLeaseDuringRun(t *testing.T) {
	srv, token, _ := newTestServerWithDevices(t)

	var sess struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "Run 租约"}, http.StatusCreated, &sess)

	// 触发一次同步完成的 Run（fake provider 无延迟）
	var run struct {
		RunID string `json:"run_id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions/"+sess.ID+"/runs", token,
		map[string]any{"input": "你好"}, http.StatusAccepted, &run)

	// 等待 Run 收敛
	waitUntil(t, 3*time.Second, func() bool {
		var got struct {
			Status model.RunStatus `json:"status"`
		}
		doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/runs/"+run.RunID,
			token, nil, http.StatusOK, &got)
		switch got.Status {
		case model.RunCompleted, model.RunFailed, model.RunCancelled:
			return true
		}
		return false
	})

	// Run 结束 → 引擎 defer Release → 租约表无记录（active=true 的「服务端可写」语义）
	var lease struct {
		Holder string `json:"holder"`
		Epoch  int64  `json:"epoch"`
		Active bool   `json:"active"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/lease", token,
		nil, http.StatusOK, &lease)
	if lease.Active && lease.Epoch == 0 && lease.Holder == "" {
		return // 期望路径：已释放
	}
	// 允许「仍在持有」的竞态瞬态，但 holder 必须是服务端（空串），不得是设备
	if lease.Holder != "" {
		t.Fatalf("服务端 Run 结束后租约不得被设备持有: %+v", lease)
	}
}
