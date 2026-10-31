package longtask

// long-task 插件工具单测（plugintest 内存桩；状态机/检查点持久化语义由 service 层测试覆盖）。

import (
	"encoding/json"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func invoke(t *testing.T, h *plugintest.Host, name, args string) *toolapi.Result {
	t.Helper()
	p, err := Factory().New(h, nil)
	if err != nil {
		t.Fatalf("创建插件失败: %v", err)
	}
	for _, tool := range p.Tools() {
		if tool.Spec().Name != name {
			continue
		}
		res, iErr := tool.Invoke(t.Context(), &toolapi.Call{
			CallID: name + ":1", Name: name, Args: []byte(args),
			SessionID: "sess-1", RunID: "run-1",
		})
		if iErr != nil {
			t.Fatalf("%s invoke: %v", name, iErr)
		}
		return res
	}
	t.Fatalf("插件未声明工具 %s", name)
	return nil
}

// metaOf 取 DataResult 包装的 Meta.data 结构化数据。
func metaOf(t *testing.T, res *toolapi.Result) map[string]any {
	t.Helper()
	if res.Meta == nil {
		t.Fatalf("结果无 Meta: %q", res.Content)
	}
	switch v := res.Meta["data"].(type) {
	case map[string]any:
		return v
	case json.RawMessage:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err != nil {
			t.Fatalf("Meta.data 解析失败: %v", err)
		}
		return m
	default:
		t.Fatalf("Meta.data 形状异常: %T", res.Meta["data"])
	}
	return nil
}

func TestStartAndCheckpoint(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("work.txt", "进度")

	res := invoke(t, h, "start_long_task", `{"title":"迁移数据","paths":["work.txt"]}`)
	if res.IsError {
		t.Fatalf("start 失败: %s", res.Content)
	}
	taskID, _ := metaOf(t, res)["task_id"].(string)
	if taskID == "" {
		t.Fatalf("应返回 task_id: %s", res.Content)
	}

	cp := invoke(t, h, "checkpoint_long_task", `{"task_id":"`+taskID+`","note":"第一步"}`)
	if cp.IsError {
		t.Fatalf("checkpoint 失败: %s", cp.Content)
	}
	cps := h.TaskStubCheckpoints(taskID)
	if len(cps) != 1 || cps[0].Note != "第一步" || cps[0].RunID != "run-1" {
		t.Fatalf("检查点未登记: %+v", cps)
	}
	if len(h.SnapshotRefBindings(cps[0].SnapshotID)) != 1 {
		t.Fatal("检查点快照应含声明的路径")
	}
}

// TestStartRejectsEmptyTitle 空标题显式拒绝。
func TestStartRejectsEmptyTitle(t *testing.T) {
	h := plugintest.New(t)
	res := invoke(t, h, "start_long_task", `{"title":" "}`)
	if !res.IsError {
		t.Fatal("空标题应拒绝")
	}
}

func TestControlTransitions(t *testing.T) {
	h := plugintest.New(t)
	res := invoke(t, h, "start_long_task", `{"title":"长流程"}`)
	taskID, _ := metaOf(t, res)["task_id"].(string)

	for _, tc := range []struct{ action, want string }{
		{"pause", "paused"}, {"resume", "running"}, {"cancel", "cancelled"},
	} {
		r := invoke(t, h, "long_task_control", `{"action":"`+tc.action+`","task_id":"`+taskID+`"}`)
		if r.IsError {
			t.Fatalf("%s 失败: %s", tc.action, r.Content)
		}
		if got, _ := metaOf(t, r)["state"].(string); got != tc.want {
			t.Fatalf("%s 后应 %s，得到 %s", tc.action, tc.want, got)
		}
	}
	// 终态后 resume 拒绝
	r := invoke(t, h, "long_task_control", `{"action":"resume","task_id":"`+taskID+`"}`)
	if !r.IsError {
		t.Fatal("终态 resume 应拒绝")
	}
}

func TestControlFinishAndList(t *testing.T) {
	h := plugintest.New(t)
	res := invoke(t, h, "start_long_task", `{"title":"收尾"}`)
	taskID, _ := metaOf(t, res)["task_id"].(string)

	r := invoke(t, h, "long_task_control", `{"action":"complete","task_id":"`+taskID+`","note":"完成"}`)
	if r.IsError {
		t.Fatalf("complete 失败: %s", r.Content)
	}
	list := invoke(t, h, "long_task_control", `{"action":"list"}`)
	if list.IsError || !strings.Contains(list.Content, taskID) {
		t.Fatalf("list 应含任务: %s", list.Content)
	}
}

func TestRollbackRestoresFiles(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("f.txt", "v1")
	res := invoke(t, h, "start_long_task", `{"title":"改文件","paths":["f.txt"]}`)
	taskID, _ := metaOf(t, res)["task_id"].(string)

	cp := invoke(t, h, "checkpoint_long_task", `{"task_id":"`+taskID+`"}`)
	cpMeta := metaOf(t, cp)["checkpoint"].(map[string]any)
	snapID, _ := cpMeta["snapshot_id"].(string)

	// 内容变更后回滚
	_ = h.Seed("f.txt", "v2-broken")
	r := invoke(t, h, "long_task_control", `{"action":"rollback","task_id":"`+taskID+`","checkpoint_id":"`+snapID+`"}`)
	if r.IsError {
		t.Fatalf("rollback 失败: %s", r.Content)
	}
	data, err := h.ReadFile(t.Context(), "f.txt")
	if err != nil || string(data) != "v1" {
		t.Fatalf("rollback 应物理写回 v1: %q %v", data, err)
	}
}

// TestControlUnknownAction 未知 action 显式报错。
func TestControlUnknownAction(t *testing.T) {
	h := plugintest.New(t)
	r := invoke(t, h, "long_task_control", `{"action":"boom"}`)
	if !r.IsError {
		t.Fatal("未知 action 应报错")
	}
}
