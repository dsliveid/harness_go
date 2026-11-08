package collaborator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

// ---------- 测试桩 ----------

// fakeChild 可控 ChildRun 桩：Wait 返回预置结果或错误。
type fakeChild struct {
	id      string
	status  toolapi.ChildStatus
	res     toolapi.ChildResult
	waitErr error
}

func (c *fakeChild) ID() string                  { return c.id }
func (c *fakeChild) Status() toolapi.ChildStatus { return c.status }
func (c *fakeChild) Cancel() error               { return nil }

func (c *fakeChild) Wait(context.Context) (toolapi.ChildResult, error) {
	if c.waitErr != nil {
		return toolapi.ChildResult{}, c.waitErr
	}
	return c.res, nil
}

// nilSpawnHost 覆写 Spawn() 返回 nil（模拟内核「嵌套 Run 未接线」）。
// plugintest.Host.Spawn() 恒返回适配器，故用一层覆写壳表达该分支。
type nilSpawnHost struct{ *plugintest.Host }

func (nilSpawnHost) Spawn() toolapi.ChildRunner { return nil }

// ---------- 用例 ----------

// TestAskCollaboratorNilSpawn Spawn() 为 nil 时必须显式报「嵌套 Run 能力未接线」。
func TestAskCollaboratorNilSpawn(t *testing.T) {
	h := nilSpawnHost{plugintest.New(t)}
	res, err := invokeAsk(context.Background(), h, &toolapi.Call{Args: []byte(`{"role":"reviewer","question":"这段代码有问题吗"}`)})
	if err != nil {
		t.Fatalf("invokeAsk 不应向上抛错: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("未接线必须返回 IsError 结果: %+v", res)
	}
	if !strings.Contains(res.Content, "嵌套 Run 能力未接线") {
		t.Fatalf("错误文案缺失接线提示: %q", res.Content)
	}
}

// TestAskCollaboratorCompleted 正常回复 → 结果含角色名与回复正文，格式 [role 的意见]\n...
func TestAskCollaboratorCompleted(t *testing.T) {
	h := plugintest.New(t)
	h.SpawnFn = func(context.Context, toolapi.SpawnRequest) (toolapi.ChildRun, error) {
		return &fakeChild{
			id:     "run-child-1",
			status: toolapi.ChildCompleted,
			res: toolapi.ChildResult{
				RunID:  "run-child-1",
				Status: toolapi.ChildCompleted,
				Output: "建议先补 SSRF 防护单测",
			},
		}, nil
	}
	res, err := invokeAsk(context.Background(), h, &toolapi.Call{
		RunID: "run-parent-1", SessionID: "sess-1",
		Args: []byte(`{"role":"reviewer","question":"阶段A还缺什么"}`),
	})
	if err != nil {
		t.Fatalf("invokeAsk 不应向上抛错: %v", err)
	}
	if res.IsError {
		t.Fatalf("子 Run 完成不应标记失败: %q", res.Content)
	}
	if !strings.HasPrefix(res.Content, "[reviewer 的意见]\n") {
		t.Fatalf("回复格式应为 [role 的意见]+换行: %q", res.Content)
	}
	if !strings.Contains(res.Content, "建议先补 SSRF 防护单测") {
		t.Fatalf("结果应含子 Run 回复正文: %q", res.Content)
	}
}

// TestAskCollaboratorChildNotCompleted 子 Run 非成功终态 → IsError + Meta 携带子 Run 信息。
func TestAskCollaboratorChildNotCompleted(t *testing.T) {
	cases := []struct {
		name       string
		status     toolapi.ChildStatus
		childErr   string
		wantDetail string
	}{
		{name: "failed 带错误详情", status: toolapi.ChildFailed, childErr: "配额耗尽", wantDetail: "配额耗尽"},
		{name: "interrupted 无详情回退状态名", status: toolapi.ChildInterrupted, wantDetail: string(toolapi.ChildInterrupted)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := plugintest.New(t)
			h.SpawnFn = func(context.Context, toolapi.SpawnRequest) (toolapi.ChildRun, error) {
				return &fakeChild{
					id:     "run-child-2",
					status: tc.status,
					res: toolapi.ChildResult{
						RunID:  "run-child-2",
						Status: tc.status,
						Error:  tc.childErr,
					},
				}, nil
			}
			res, err := invokeAsk(context.Background(), h, &toolapi.Call{Args: []byte(`{"role":"reviewer","question":"x"}`)})
			if err != nil {
				t.Fatalf("invokeAsk 不应向上抛错: %v", err)
			}
			if !res.IsError {
				t.Fatalf("状态 %s 必须判为失败: %q", tc.status, res.Content)
			}
			if !strings.Contains(res.Content, tc.wantDetail) {
				t.Fatalf("失败文案应含 %q，实得: %q", tc.wantDetail, res.Content)
			}
			if got := fmt.Sprint(res.Meta["child_run_id"]); got != "run-child-2" {
				t.Fatalf("Meta.child_run_id 错误: %q", got)
			}
			if got := fmt.Sprint(res.Meta["child_status"]); got != string(tc.status) {
				t.Fatalf("Meta.child_status 错误: %q（期望 %s）", got, tc.status)
			}
		})
	}
}

// TestAskCollaboratorBadArgs 参数 JSON 非法 → 解码失败提示，且不触达 Spawn。
func TestAskCollaboratorBadArgs(t *testing.T) {
	h := plugintest.New(t)
	spawned := false
	h.SpawnFn = func(context.Context, toolapi.SpawnRequest) (toolapi.ChildRun, error) {
		spawned = true
		return nil, errors.New("不应被调用")
	}
	res, err := invokeAsk(context.Background(), h, &toolapi.Call{Args: []byte(`not-json`)})
	if err != nil {
		t.Fatalf("invokeAsk 不应向上抛错: %v", err)
	}
	if !res.IsError {
		t.Fatalf("非法参数必须返回 IsError: %+v", res)
	}
	if !strings.Contains(res.Content, "ask_collaborator") || !strings.Contains(res.Content, "参数解析失败") {
		t.Fatalf("应提示参数解析失败: %q", res.Content)
	}
	if spawned {
		t.Fatal("参数非法时不得派生协作者 Run")
	}
}

// TestAskCollaboratorSpawnError Spawn 返回 error → 包装为 IsError（不 panic、不上抛）。
func TestAskCollaboratorSpawnError(t *testing.T) {
	h := plugintest.New(t)
	h.SpawnFn = func(context.Context, toolapi.SpawnRequest) (toolapi.ChildRun, error) {
		return nil, errors.New("同会话在途子 Run 已达上限 4")
	}
	res, err := invokeAsk(context.Background(), h, &toolapi.Call{Args: []byte(`{"role":"reviewer","question":"x"}`)})
	if err != nil {
		t.Fatalf("Spawn 失败不应上抛为 error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("Spawn 失败必须返回 IsError: %+v", res)
	}
	if !strings.Contains(res.Content, "在途子 Run 已达上限") {
		t.Fatalf("应保留底层错误原因: %q", res.Content)
	}
}

// TestAskCollaboratorWaitError child.Wait 返回 error → 「等待回复失败」。
func TestAskCollaboratorWaitError(t *testing.T) {
	h := plugintest.New(t)
	h.SpawnFn = func(context.Context, toolapi.SpawnRequest) (toolapi.ChildRun, error) {
		return &fakeChild{id: "run-child-3", status: toolapi.ChildRunning, waitErr: errors.New("子 Run 超时")}, nil
	}
	res, err := invokeAsk(context.Background(), h, &toolapi.Call{Args: []byte(`{"role":"reviewer","question":"x"}`)})
	if err != nil {
		t.Fatalf("Wait 失败不应上抛为 error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("Wait 失败必须返回 IsError: %+v", res)
	}
	if !strings.Contains(res.Content, "等待回复失败") || !strings.Contains(res.Content, "子 Run 超时") {
		t.Fatalf("Wait 失败文案错误: %q", res.Content)
	}
}

// TestAskCollaboratorRolePassthrough role→AgentRole（与 subagent 的唯一实质差异）、
// question→Input、model_ref→ModelRef、call.RunID/SessionID 透传。
func TestAskCollaboratorRolePassthrough(t *testing.T) {
	h := plugintest.New(t)
	var got toolapi.SpawnRequest
	h.SpawnFn = func(_ context.Context, req toolapi.SpawnRequest) (toolapi.ChildRun, error) {
		got = req
		return &fakeChild{status: toolapi.ChildCompleted,
			res: toolapi.ChildResult{RunID: "run-child-4", Status: toolapi.ChildCompleted, Output: "ok"}}, nil
	}
	res, err := invokeAsk(context.Background(), h, &toolapi.Call{
		RunID:     "run-parent-9",
		SessionID: "sess-9",
		Args:      []byte(`{"role":"security-auditor","question":"这个设计安全吗","model_ref":"gpt-4o"}`),
	})
	if err != nil {
		t.Fatalf("invokeAsk 不应向上抛错: %v", err)
	}
	if got.AgentRole != "security-auditor" {
		t.Fatalf("args.Role 未透传为 AgentRole: %q", got.AgentRole)
	}
	if got.Input != "这个设计安全吗" {
		t.Fatalf("Input 未透传 question: %q", got.Input)
	}
	if got.ModelRef != "gpt-4o" {
		t.Fatalf("ModelRef 未透传 model_ref: %q", got.ModelRef)
	}
	if got.ParentRunID != "run-parent-9" {
		t.Fatalf("ParentRunID 未取 call.RunID: %q", got.ParentRunID)
	}
	if got.SessionID != "sess-9" {
		t.Fatalf("SessionID 未取 call.SessionID: %q", got.SessionID)
	}
	// 结果里的角色名应取用户传入的 role（非固定 "collaborator"）
	if !strings.Contains(res.Content, "[security-auditor 的意见]") {
		t.Fatalf("结果角色名应来自 args.Role: %q", res.Content)
	}
}

// TestAskCollaboratorWiring 工厂装配路径：manifest 声明 ask_collaborator 且经 Plugin.Tools 可调。
func TestAskCollaboratorWiring(t *testing.T) {
	h := plugintest.New(t)
	h.SpawnFn = func(context.Context, toolapi.SpawnRequest) (toolapi.ChildRun, error) {
		return &fakeChild{status: toolapi.ChildCompleted,
			res: toolapi.ChildResult{RunID: "run-child-5", Status: toolapi.ChildCompleted, Output: "装配成功"}}, nil
	}
	if m := Manifest(); m.ID != "collaborator" {
		t.Fatalf("manifest id 错误: %q", m.ID)
	}
	p, err := Factory().New(h, nil)
	if err != nil {
		t.Fatalf("工厂装配失败: %v", err)
	}
	var target toolapi.Tool
	for _, tl := range p.Tools() {
		if tl.Spec().Name == "ask_collaborator" {
			target = tl
		}
	}
	if target == nil {
		t.Fatal("未注册 ask_collaborator 工具")
	}
	res, err := target.Invoke(context.Background(), &toolapi.Call{Args: []byte(`{"role":"reviewer","question":"x"}`)})
	if err != nil {
		t.Fatalf("Invoke 不应向上抛错: %v", err)
	}
	if res.IsError || !strings.Contains(res.Content, "装配成功") {
		t.Fatalf("装配路径调用结果异常: %+v", res)
	}
}
