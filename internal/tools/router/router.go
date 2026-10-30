// Package router 判定工具能否在本端执行（executor × capabilities × 运行期依赖）。
//
// 只做「能否执行」判定，不做注入决策（注入由 tier 决定，见 registry / loader）。
// 未通过判定的工具**仍保持注册**（宽进严出），调用时才明确报错。
package router

import (
	"fmt"
	"sort"

	"harness-go/pkg/toolapi"
)

// Options 本端能力声明。
type Options struct {
	// Executor 本端执行面（server / device）。
	Executor toolapi.Executor
	// Capabilities 本端具备的能力标签（见 toolapi.Cap*）。
	Capabilities map[string]bool
	// HasRequire 运行期依赖探测（如 chromium、git）；nil 表示视为全部满足。
	HasRequire func(name string) bool
}

// Router 执行面路由器。
type Router struct {
	opts Options
}

// New 创建路由器（Executor 为空按 server 处理）。
func New(opts Options) *Router {
	if opts.Executor == "" {
		opts.Executor = toolapi.ExecutorServer
	}
	return &Router{opts: opts}
}

// CanRun 判定能否执行：nil 可执行；否则 ErrUnsupported / ErrUnavailable。
func (r *Router) CanRun(spec toolapi.Spec) error {
	s := spec.Normalized()
	switch s.Executor {
	case toolapi.ExecutorAny:
	case r.opts.Executor:
	case "":
	default:
		return fmt.Errorf("%w: %s 需在 %s 执行面运行", toolapi.ErrUnsupported, s.Name, s.Executor)
	}
	var missing []string
	for _, cap := range s.Capabilities {
		if !r.opts.Capabilities[cap] {
			missing = append(missing, cap)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s 缺少能力 %v", toolapi.ErrUnsupported, s.Name, missing)
	}
	if r.opts.HasRequire != nil {
		for _, req := range s.Requires {
			if !r.opts.HasRequire(req) {
				return fmt.Errorf("%w: %s 缺少依赖 %q", toolapi.ErrUnavailable, s.Name, req)
			}
		}
	}
	return nil
}

// Filter 分组：runnable 可执行；skipped 为 name → 原因。
func (r *Router) Filter(specs []toolapi.Spec) (runnable []toolapi.Spec, skipped map[string]string) {
	skipped = make(map[string]string)
	for _, s := range specs {
		if err := r.CanRun(s); err != nil {
			skipped[s.Name] = err.Error()
			continue
		}
		runnable = append(runnable, s)
	}
	return runnable, skipped
}

// Capabilities 返回本端能力列表（排序后，便于稳定输出）。
func (r *Router) Capabilities() []string {
	out := make([]string, 0, len(r.opts.Capabilities))
	for k, v := range r.opts.Capabilities {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Executor 返回本端执行面。
func (r *Router) Executor() toolapi.Executor { return r.opts.Executor }
