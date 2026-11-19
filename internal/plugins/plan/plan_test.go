package plan

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func TestPlanRoundTrip(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	res, _ := readPlan(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "无执行计划") {
		t.Fatalf("空计划口径错误: %q", res.Content)
	}
	res, err := createPlan(ctx, h, &toolapi.Call{Args: []byte(
		`{"title":"重构检索","steps":[{"title":"索引层"},{"title":"选择器","done":true}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "重构检索（1/2 完成）") || !strings.Contains(res.Content, "● 选择器") {
		t.Fatalf("渲染错误: %q", res.Content)
	}
	res, _ = readPlan(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "重构检索") {
		t.Fatalf("回读错误: %q", res.Content)
	}
}

// TestUpdatePlanMarksStep update_plan 按序号推进进度（不必回传整份清单）。
//
// 这是 update_plan 的核心价值：模型只需给出序号，不会因回传整份 steps
// 而漏项或改动措辞。
func TestUpdatePlanMarksStep(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	if _, err := createPlan(ctx, h, &toolapi.Call{Args: []byte(
		`{"title":"T","steps":[{"title":"A"},{"title":"B"}]}`)}); err != nil {
		t.Fatal(err)
	}
	res, _ := updatePlan(ctx, h, &toolapi.Call{Args: []byte(`{"complete_step":1}`)})
	if !strings.Contains(res.Content, "（1/2 完成）") || !strings.Contains(res.Content, "● A") {
		t.Fatalf("标记完成失败: %q", res.Content)
	}
	// 越界序号必须显式报错（不能静默忽略）
	res, _ = updatePlan(ctx, h, &toolapi.Call{Args: []byte(`{"complete_step":9}`)})
	if !res.IsError || !strings.Contains(res.Content, "越界") {
		t.Fatalf("越界序号必须报错: %q", res.Content)
	}
}

// TestUpdatePlanAcceptsStepIndexAlias 模型最自然的 `step_index` + `status` 必须被归一。
//
// 事故会话 122559a3 实测：模型写 `{"status":"..."}` 被直接报「未知参数 "status"」，
// 白烧两步（先猜一次、再看报错、再猜）。别名归一是零成本的宽容。
func TestUpdatePlanAcceptsStepIndexAlias(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	if _, err := createPlan(ctx, h, &toolapi.Call{Args: []byte(
		`{"title":"T","steps":[{"title":"A"},{"title":"B"}]}`)}); err != nil {
		t.Fatal(err)
	}
	res, _ := updatePlan(ctx, h, &toolapi.Call{Args: []byte(`{"step_index":1,"status":"completed"}`)})
	if res.IsError {
		t.Fatalf("step_index + status 别名不应报错: %q", res.Content)
	}
	if !strings.Contains(res.Content, "（1/2 完成）") {
		t.Fatalf("别名应等价于 complete_step=1，实际: %q", res.Content)
	}
	// 反向也必须生效（status=pending → 取消完成）。
	res, _ = updatePlan(ctx, h, &toolapi.Call{Args: []byte(`{"step_index":1,"status":"pending"}`)})
	if !strings.Contains(res.Content, "（0/2 完成）") {
		t.Fatalf("status=pending 应等价于 uncomplete_step，实际: %q", res.Content)
	}
}

// TestUpdatePlanWithoutPlanRejected 无计划时更新必须报错，提示先 create_plan。
func TestUpdatePlanWithoutPlanRejected(t *testing.T) {
	h := plugintest.New(t)
	res, _ := updatePlan(context.Background(), h, &toolapi.Call{Args: []byte(`{"title":"X"}`)})
	if !res.IsError || !strings.Contains(res.Content, "create_plan") {
		t.Fatalf("无计划时更新必须报错并指引 create_plan: %q", res.Content)
	}
}

// TestListPlansSingleStore list_plans 如实反映"单计划存储"能力边界。
func TestListPlansSingleStore(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	res, _ := listPlans(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "无执行计划") {
		t.Fatalf("空态口径错误: %q", res.Content)
	}
	if _, err := createPlan(ctx, h, &toolapi.Call{Args: []byte(
		`{"title":"P1","steps":[{"title":"s"}]}`)}); err != nil {
		t.Fatal(err)
	}
	res, _ = listPlans(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "P1") || !strings.Contains(res.Content, "单计划存储") {
		t.Fatalf("列出计划错误: %q", res.Content)
	}
}

// TestPlanManifestToolNames 清单必须是四个经典工具名（与提示词一致）。
func TestPlanManifestToolNames(t *testing.T) {
	want := map[string]bool{
		"create_plan": false, "update_plan": false, "read_plan": false, "list_plans": false,
	}
	tools := Manifest().Tools
	if len(tools) != len(want) {
		t.Fatalf("工具数量应为 %d，实际 %d", len(want), len(tools))
	}
	for _, s := range tools {
		if _, ok := want[s.Name]; !ok {
			t.Fatalf("出现预期外的工具名: %s", s.Name)
		}
		want[s.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("缺少工具: %s", name)
		}
	}
}
