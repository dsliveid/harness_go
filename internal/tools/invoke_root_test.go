package tools

import (
	"context"
	"errors"
	"testing"

	"harness-go/internal/tools/loader"
	"harness-go/pkg/toolapi"
)

// ============ 会话执行根注入（2026-11 目录选择放开）============
//
// Runner 是「会话 project_dir」与「工具沙箱根」之间的桥梁：
// 它必须把解析到的根注入 ctx，且**解析失败时绝不注入**（回落进程根，
// 也就是保守失败），否则越界目录会被当成合法执行根 —— 等于白名单失效。

// fakeRoots 可控的根解析器桩。
type fakeRoots struct {
	root string
	err  error
	// called 记录被调用的 sessionID，用于断言「没有 sessionID 时不应查询」。
	called []string
}

func (f *fakeRoots) ResolveExecutionRoot(_ context.Context, sessionID string) (string, error) {
	f.called = append(f.called, sessionID)
	return f.root, f.err
}

func testLimits() loader.Limits { return loader.Limits{CoreSoft: 8, DeferredSoft: 60} }

// ① 解析成功 → 根被注入 ctx。
func TestSessionRootInjected(t *testing.T) {
	fr := &fakeRoots{root: "/sess/root"}
	r := NewRunner(nil, nil, testLimits()).WithExecutionRoots(fr)

	ctx := toolapi.WithWorkspaceRoot(context.Background(), r.sessionRoot(context.Background(), "s-1"))
	got, ok := toolapi.WorkspaceRootFrom(ctx)
	if !ok || got != "/sess/root" {
		t.Fatalf("会话根应被注入 ctx，得到 %q（ok=%v）", got, ok)
	}
	if len(fr.called) != 1 || fr.called[0] != "s-1" {
		t.Fatalf("应按 sessionID 查询一次，实际 %v", fr.called)
	}
}

// ② 解析报错（越界/不存在）→ 不注入（回落进程根，绝不放行未校验目录）。
func TestSessionRootErrorIsNotInjected(t *testing.T) {
	fr := &fakeRoots{root: "/should/be/ignored", err: errors.New("越界")}
	r := NewRunner(nil, nil, testLimits()).WithExecutionRoots(fr)

	ctx := toolapi.WithWorkspaceRoot(context.Background(), r.sessionRoot(context.Background(), "s-2"))
	if _, ok := toolapi.WorkspaceRootFrom(ctx); ok {
		t.Fatal("解析失败时绝不能注入执行根")
	}
}

// ③ 空 sessionID（纯对话/直调）→ 不查询也不注入。
func TestSessionRootEmptySessionSkipsResolver(t *testing.T) {
	fr := &fakeRoots{root: "/x"}
	r := NewRunner(nil, nil, testLimits()).WithExecutionRoots(fr)

	if got := r.sessionRoot(context.Background(), ""); got != "" {
		t.Fatalf("空 sessionID 应返回空根，得到 %q", got)
	}
	if len(fr.called) != 0 {
		t.Fatalf("空 sessionID 不应触发查询，实际 %v", fr.called)
	}
}

// ④ 未注入解析器 → 空根（纯增量改造，默认行为不变）。
func TestSessionRootWithoutResolver(t *testing.T) {
	r := NewRunner(nil, nil, testLimits())
	if got := r.sessionRoot(context.Background(), "s-1"); got != "" {
		t.Fatalf("未注入解析器时应返回空根，得到 %q", got)
	}
}
