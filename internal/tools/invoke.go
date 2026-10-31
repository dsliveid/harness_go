// Package tools 汇聚工具内核侧能力：注册表执行适配（agent.ToolRunner）与注入视图转换。
//
// 依赖方向：internal/tools → pkg/toolapi / internal/{agent,model,prompt}；
// **反向禁止**：agent 只依赖自己定义的 ToolRunner 端口，不 import 本包（depguard 校验）。
package tools

import (
	"context"
	"encoding/json"
	"errors"

	"harness-go/internal/agent"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
	"harness-go/internal/tools/loader"
	"harness-go/internal/tools/registry"
	"harness-go/internal/tools/router"
	"harness-go/pkg/toolapi"
)

// Runner 实现 agent.ToolRunner：解析工具 → 执行面判定 → 调用 → 结果翻译。
//
// 落库与事件由引擎既有路径负责（tool_events 只在引擎里写），Runner 不碰存储。
type Runner struct {
	reg     *registry.Registry
	router  *router.Router
	limits  loader.Limits
	devices DeviceExecutor // 可为 nil（未接设备域 = 只用本地执行）
}

// NewRunner 创建执行适配器。
func NewRunner(reg *registry.Registry, rtr *router.Router, limits loader.Limits) *Runner {
	return &Runner{reg: reg, router: rtr, limits: limits}
}

// 编译期断言：Runner 必须满足引擎端口。
var (
	_ agent.ToolRunner  = (*Runner)(nil)
	_ agent.ToolCatalog = (*Runner)(nil)
)

// Run 实现 agent.ToolRunner。
//
// 执行面路由（C-5，多端协同 HALO）：
//  1. spec.Executor ∈ {device, any} 且有在线设备 → 端侧派发（结果 untrusted）
//  2. 端侧不可用（无设备/断连/写帧失败）→ **静默降级**本地执行（不变量 5，用户无感）
//  3. 端侧真实执行过（含失败/超时）→ 直接采用端侧终态，**不重跑**（避免二次副作用）
//  4. spec.Executor == server（或降级后）→ 本地执行（结果 trusted）
//
// 失败一律通过 Status/Error 表达（不返回 error），保证引擎只落一次 tool_events。
func (r *Runner) Run(ctx context.Context, req agent.ToolRunRequest) (agent.ToolRunResult, error) {
	tool, spec, err := r.reg.Resolve(req.ToolName)
	if err != nil {
		return failure(model.ToolError, "工具未注册: "+req.ToolName), nil
	}
	// ① 端侧优先（executor ∈ {device, any}）：不可用则返回 ok=false 继续走本地
	if res, ok := r.tryDevice(ctx, spec, req); ok {
		return res, nil
	}
	// ② 本地执行面判定（端侧降级也须过本端能力/依赖校验，不绕开宽进严出）
	if runErr := r.router.CanRun(spec); runErr != nil {
		return failure(model.ToolError, runErr.Error()), nil
	}

	res, invokeErr := tool.Invoke(ctx, &toolapi.Call{
		CallID:      req.ToolName + ":" + req.ToolEventID,
		RunID:       req.RunID,
		SessionID:   req.SessionID,
		ToolEventID: req.ToolEventID,
		Name:        req.ToolName,
		Args:        []byte(req.ArgsJSON),
		AccessMode:  req.AccessMode,
	})
	if invokeErr != nil {
		status := model.ToolError
		if errors.Is(invokeErr, toolapi.ErrDenied) {
			status = model.ToolDenied
		}
		if errors.Is(invokeErr, context.DeadlineExceeded) {
			status = model.ToolTimeout
		}
		return failure(status, invokeErr.Error()), nil
	}
	if res == nil {
		return failure(model.ToolError, "工具返回了空结果"), nil
	}
	if res.IsError {
		return failure(model.ToolError, res.Content), nil
	}
	// 工具输出统一封装成**合法 JSON**：model.ToolEvent.ResultJSON 是 json.RawMessage，
	// 直接塞纯文本会让事件序列化失败（曾导致 /tool-events 返回空响应体）。
	payload := map[string]any{"content": res.Content}
	if len(res.Meta) > 0 {
		payload["meta"] = res.Meta
	}
	if res.CostTokens > 0 {
		payload["cost_tokens"] = res.CostTokens
	}
	raw, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return failure(model.ToolError, "工具结果序列化失败: "+marshalErr.Error()), nil
	}
	return agent.ToolRunResult{
		Status:      model.ToolOK,
		ResultJSON:  string(raw),
		ResultBlob:  res.BlobRef,
		Executor:    model.ExecutorServer,
		SourceTrust: model.TrustTrusted,
	}, nil
}

// PromptRefs 返回注入用的工具引用（tier 由 prompt 自行分层处理）。
func (r *Runner) PromptRefs() []prompt.ToolRef {
	specs := r.reg.Specs()
	out := make([]prompt.ToolRef, 0, len(specs))
	for _, s := range specs {
		out = append(out, prompt.ToolRef{
			Name:        s.Name,
			Description: s.Description,
			InputSchema: s.SchemaJSON,
			Tier:        toPromptTier(s.Tier),
		})
	}
	return out
}

// Injection 返回本轮注入视图与预算明细（供端点与启动日志）。
func (r *Runner) Injection() loader.Injection {
	return loader.Build(r.reg.Specs(), r.limits)
}

// Specs 返回全部工具规格（tier 已应用覆盖）。
func (r *Runner) Specs() []toolapi.Spec { return r.reg.Specs() }

// SetOverrides 更新全局 tier 覆盖（持久化由 service 层负责）。
func (r *Runner) SetOverrides(over map[string]toolapi.Tier) { r.reg.SetOverrides(over) }

// Risk 实现 agent.ToolCatalog：把契约风险级翻译为引擎的 model.Risk（取值一致）。
func (r *Runner) Risk(toolName string) (model.Risk, bool) {
	rk, ok := r.reg.Risk(toolName)
	if !ok {
		return "", false
	}
	return model.Risk(rk), true
}

// Snapshot 返回注册表快照。
func (r *Runner) Snapshot() registry.Snapshot { return r.reg.Snapshot() }

// Router 返回执行面路由器。
func (r *Runner) Router() *router.Router { return r.router }

// toPromptTier 单点映射：toolapi.Tier → prompt.Tier（避免两份常量漂移）。
func toPromptTier(t toolapi.Tier) prompt.Tier {
	switch t {
	case toolapi.TierCore:
		return prompt.TierCore
	case toolapi.TierOnDemand:
		return prompt.TierOnDemand
	default:
		return prompt.TierDeferred
	}
}

// failure 构造失败结果（本地执行面的失败，故 server + trusted）。
func failure(status model.ToolEventStatus, msg string) agent.ToolRunResult {
	return agent.ToolRunResult{
		Status:      status,
		Error:       msg,
		Executor:    model.ExecutorServer,
		SourceTrust: model.TrustTrusted,
	}
}
