package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"harness-go/internal/model"
	"harness-go/internal/sessionbus"
)

// 领域列表变更事件（projects.changed / workspaces.changed）契约测试。
//
// 核心不变量：
//  ① 事件**不带数据**（纯信号）——帧里没有 session/items 字段，只有 type + at；
//  ② 写入路径真的会触发（埋点遗漏会让三端列表永远不刷新，且不会报错）；
//  ③ 该帧**不回查 DB** —— 所以「实体刚被删除」不会导致静默丢帧。
func TestDomainEventFrameIsSignalOnly(t *testing.T) {
	srv, bus, _, token := newSessionEventsEnv(t)
	r, cancel := openSessionSSE(t, srv, token)
	defer cancel()
	readSessionFrames(t, r, 1) // 首帧快照

	bus.PublishDomain(sessionbus.EventProjectsChanged)

	frame := readSessionFrames(t, r, 1)[0]
	if frame.Event != sessionbus.EventProjectsChanged {
		t.Fatalf("event 应为 %q，得到 %q", sessionbus.EventProjectsChanged, frame.Event)
	}
	// 纯信号：只应有 type 与 at，绝无 items/session 之类载荷。
	var payload map[string]any
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
		t.Fatalf("帧解析失败: %v", err)
	}
	if _, ok := payload["items"]; ok {
		t.Fatalf("领域事件不应带 items 载荷: %+v", payload)
	}
	if _, ok := payload["session"]; ok {
		t.Fatalf("领域事件不应带 session 载荷: %+v", payload)
	}
	if payload["type"] != sessionbus.EventProjectsChanged {
		t.Fatalf("type 字段不符: %+v", payload)
	}
	if _, ok := payload["at"]; !ok {
		t.Fatalf("应带 at 时间戳: %+v", payload)
	}
}

// 工作空间事件同样可送达。
func TestDomainEventWorkspacesReachesClient(t *testing.T) {
	srv, bus, _, token := newSessionEventsEnv(t)
	r, cancel := openSessionSSE(t, srv, token)
	defer cancel()
	readSessionFrames(t, r, 1)

	bus.PublishDomain(sessionbus.EventWorkspacesChanged)

	frame := readSessionFrames(t, r, 1)[0]
	if frame.Event != sessionbus.EventWorkspacesChanged {
		t.Fatalf("event 应为 %q，得到 %q", sessionbus.EventWorkspacesChanged, frame.Event)
	}
}

// 创建项目必须广播 projects.changed（埋点回归）。
func TestCreateProjectPublishesDomainEvent(t *testing.T) {
	srv, bus, _, token := newSessionEventsEnv(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "实时项目", "path": t.TempDir()}, http.StatusCreated, nil)

	assertDomainEvent(t, sub, sessionbus.EventProjectsChanged)
}

// 更新项目必须广播 projects.changed。
func TestUpdateProjectPublishesDomainEvent(t *testing.T) {
	srv, bus, _, token := newSessionEventsEnv(t)

	var created struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "待改", "path": t.TempDir()}, http.StatusCreated, &created)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/projects/"+created.ID, token,
		rawJSON(`{"name":"已改"}`), http.StatusNoContent, nil)

	assertDomainEvent(t, sub, sessionbus.EventProjectsChanged)
}

// 删除项目必须广播 projects.changed。
func TestDeleteProjectPublishesDomainEvent(t *testing.T) {
	srv, bus, _, token := newSessionEventsEnv(t)

	var created struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "待删", "path": t.TempDir()}, http.StatusCreated, &created)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/projects/"+created.ID, token,
		nil, http.StatusNoContent, nil)

	assertDomainEvent(t, sub, sessionbus.EventProjectsChanged)
}

// 删除工作空间必须广播 workspaces.changed（可能还伴随 projects.changed）。
func TestDeleteWorkspacePublishesDomainEvent(t *testing.T) {
	srv, bus, _, token := newSessionEventsEnv(t)

	// 构造一个非内置的空间（内置 ws-server 不可删）。
	var created struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/workspaces", token,
		map[string]any{"name": "临时空间", "kind": "server"}, http.StatusCreated, &created)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/workspaces/"+created.ID+"?migrate_refs=true", token,
		nil, http.StatusNoContent, nil)

	assertDomainEvent(t, sub, sessionbus.EventWorkspacesChanged)
}

// 更新工作空间必须广播 workspaces.changed。
func TestUpdateWorkspacePublishesDomainEvent(t *testing.T) {
	srv, bus, _, token := newSessionEventsEnv(t)

	var created struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/workspaces", token,
		map[string]any{"name": "待改名空间", "kind": "server"}, http.StatusCreated, &created)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/workspaces/"+created.ID, token,
		map[string]any{"name": "已改名空间"}, http.StatusOK, nil)

	assertDomainEvent(t, sub, sessionbus.EventWorkspacesChanged)
}

// 并发多订阅者（模拟多标签页 / 多端同时在线）互不干扰。
//
// 这是真实场景：同一账号在 Web 两个标签页 + 桌面端 + 移动端同时在线，
// 各自持有一条 SSE 连接。任何一端的连接问题都不得影响其他端。
func TestSessionEventsConcurrentSubscribers(t *testing.T) {
	srv, bus, main, token := newSessionEventsEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := main.Sessions().Create(ctx, &model.Session{ID: "s-multi", Title: "t", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	const clients = 4
	readers := make([]*bufio.Reader, 0, clients)
	for i := 0; i < clients; i++ {
		r, cancel := openSessionSSE(t, srv, token)
		defer cancel()
		readers = append(readers, r)
	}
	// 每条连接都先收首帧快照。
	for _, r := range readers {
		readSessionFrames(t, r, 1)
	}

	// 一条变更必须送达**所有**订阅者。
	bus.PublishImmediate("s-multi", sessionbus.EventUpdated)
	for i, r := range readers {
		frame := readSessionFrames(t, r, 1)[0]
		if frame.Event != sessionbus.EventUpdated {
			t.Fatalf("第 %d 个订阅者未收到 updated，得到 %q", i, frame.Event)
		}
	}
}

// 慢消费者只影响自己：一个不读数据的连接不得拖累其他正常连接。
//
// 服务端的策略是「缓冲满即断开该订阅，绝不阻塞发布方」（见 sessionbus.publish）。
// 这里验证该策略在 HTTP 层同样成立 —— 若慢消费者能拖住发布方，
// 一次列表刷新会被一个卡死的标签页拖成整个 API 卡顿。
func TestSessionEventsSlowConsumerDoesNotBlockOthers(t *testing.T) {
	srv, bus, main, token := newSessionEventsEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := main.Sessions().Create(ctx, &model.Session{ID: "s-slow", Title: "t", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	// 正常连接：读掉首帧后持续消费。
	fast, cancelFast := openSessionSSE(t, srv, token)
	defer cancelFast()
	readSessionFrames(t, fast, 1)

	// 慢连接：建立但不读（模拟卡死的标签页）。
	_, cancelSlow := openSessionSSE(t, srv, token)
	defer cancelSlow()

	// 连续推送远多于单订阅者缓冲（32）的帧数。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			bus.PublishImmediate("s-slow", sessionbus.EventUpdated)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("发布方被慢消费者阻塞：策略失效（会拖垮整个 API）")
	}

	// 正常连接仍应可继续接收（哪怕中途有丢失/重连，至少不能永久卡死）。
	// 这里只断言「连接未整体崩掉且能读到数据」，不追求帧数精确。
	if _, err := fast.Peek(1); err != nil {
		t.Fatalf("正常连接被慢消费者连累断开: %v", err)
	}
}

// assertDomainEvent 在超时内等到指定类型的领域事件（跳过其它类型）。
//
// 跳过而非严格比较首个：删除工作空间时服务端会**同时**发两个信号
// （workspaces.changed + projects.changed），顺序不作保证。
func assertDomainEvent(t *testing.T, sub sessionbus.Subscription, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, open := <-sub.C():
			if !open {
				t.Fatal("订阅通道意外关闭")
			}
			if ev.Type == want {
				return
			}
		case <-deadline:
			t.Fatalf("未收到 %q 事件（埋点遗漏或未在写库后触发）", want)
		}
	}
}

// rawJSON 把 JSON 字面量字符串包成可被 doJSON 序列化的载荷。
//
// doJSON 内部会 json.Marshal(body)，直接传 string 会被编码成 JSON 字符串字面量（而非对象），
// 故这里先反序列化成 any 再交给 doJSON。
func rawJSON(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic(err)
	}
	return v
}
