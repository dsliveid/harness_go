package router

import (
	"errors"
	"testing"

	"harness-go/pkg/toolapi"
)

func TestCanRunExecutorAndCaps(t *testing.T) {
	r := New(Options{
		Executor: toolapi.ExecutorServer,
		Capabilities: map[string]bool{
			toolapi.CapWorkspaceRead: true,
		},
	})
	if err := r.CanRun(toolapi.Spec{Name: "a", Executor: toolapi.ExecutorServer}); err != nil {
		t.Fatalf("同执行面应可运行: %v", err)
	}
	if err := r.CanRun(toolapi.Spec{Name: "b", Executor: toolapi.ExecutorAny}); err != nil {
		t.Fatalf("any 执行面应可运行: %v", err)
	}
	err := r.CanRun(toolapi.Spec{Name: "c", Executor: toolapi.ExecutorDevice})
	if !errors.Is(err, toolapi.ErrUnsupported) {
		t.Fatalf("端侧工具在服务端应 ErrUnsupported，得到 %v", err)
	}
	err = r.CanRun(toolapi.Spec{Name: "d", Executor: toolapi.ExecutorServer, Capabilities: []string{toolapi.CapNetwork}})
	if !errors.Is(err, toolapi.ErrUnsupported) {
		t.Fatalf("缺能力应 ErrUnsupported，得到 %v", err)
	}
}

func TestRequiresMissing(t *testing.T) {
	r := New(Options{
		Executor:     toolapi.ExecutorServer,
		Capabilities: map[string]bool{toolapi.CapProcess: true},
		HasRequire:   func(name string) bool { return name != "chromium" },
	})
	err := r.CanRun(toolapi.Spec{Name: "browser", Executor: toolapi.ExecutorServer, Requires: []string{"chromium"}})
	if !errors.Is(err, toolapi.ErrUnavailable) {
		t.Fatalf("缺运行期依赖应 ErrUnavailable，得到 %v", err)
	}
	if err := r.CanRun(toolapi.Spec{Name: "git", Requires: []string{"git"}}); err != nil {
		t.Fatalf("依赖满足应可运行: %v", err)
	}
}

func TestFilter(t *testing.T) {
	r := New(Options{Executor: toolapi.ExecutorServer})
	run, skipped := r.Filter([]toolapi.Spec{
		{Name: "ok", Executor: toolapi.ExecutorServer},
		{Name: "device-only", Executor: toolapi.ExecutorDevice},
	})
	if len(run) != 1 || run[0].Name != "ok" {
		t.Fatalf("可运行集合错误: %+v", run)
	}
	if _, ok := skipped["device-only"]; !ok {
		t.Fatalf("不可运行工具应记录原因: %+v", skipped)
	}
	if got := r.Capabilities(); len(got) != 0 {
		t.Fatalf("无能力时应为空，得到 %v", got)
	}
}
