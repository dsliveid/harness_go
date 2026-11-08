package agent

import (
	"context"
	"encoding/json"
	"time"

	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/observ"
	"harness-go/internal/prompt"
)

// 本文件承载**审计日志埋点**（agent → TraceSink）。
//
// 设计原则（务必遵守，否则会拖慢主链路）：
//  1. 所有函数在 e.deps.Traces == nil 时立即返回，零开销；
//  2. 组装记录 → 调 Sink（Sink 实现自带非阻塞队列 + 满即丢弃）；
//  3. 本文件绝不返回错误：日志失败永远不能影响 Run。

// now 统一时间源（Deps.Now 可注入，未注入回退系统时钟）。
func (e *Engine) now() int64 {
	if e.deps.Now != nil {
		return e.deps.Now()
	}
	return time.Now().UnixMilli()
}

// SetTraceSink 后置注入审计日志端口。
//
// 为什么需要后置注入而不是构造参数：审计日志库族（logvault）依赖 settings 服务，
// 而 settings 服务又需要在引擎之后构建（避免循环依赖）。此方法只在启动装配期调用一次
// （服务开始对外之前），因此不需要额外加锁 —— 但**禁止在 Run 运行期间热替换**。
func (e *Engine) SetTraceSink(sink TraceSink) {
	e.deps.Traces = sink
	if e.deps.TraceID == nil {
		e.deps.TraceID = func(ctx context.Context) string {
			return observ.TraceFrom(ctx)
		}
	}
}

// traceID 取当前请求的 trace_id（未注入时返回空串）。
func (e *Engine) traceID(ctx context.Context) string {
	if e.deps.TraceID == nil {
		return ""
	}
	return e.deps.TraceID(ctx)
}

// marshalForTrace 把 ChatRequest 序列化为审计用 JSON。
//
// 注意：**此处不做脱敏**，脱敏统一在 logvault 写入侧执行（唯一入口，避免遗漏）。
// 序列化失败返回空串 —— 宁可少记报文，也不能因日志报错影响 Run。
func marshalForTrace(req llm.ChatRequest) string {
	raw, err := json.Marshal(req)
	if err != nil {
		return ""
	}
	return string(raw)
}

// recordLLMTrace 投递一次模型调用的审计记录。
//
// 调用点：streamOnce 的每条返回路径（ok / error / cancelled）都必须调用，
// 否则失败调用的报文会永久缺失 —— 而排障恰恰最需要失败现场。
func (e *Engine) recordLLMTrace(ctx context.Context, req RunRequest, provider llm.Provider,
	res prompt.BuildResult, startedAt, firstTokenAt int64, payload string, out streamOutcome,
	tokens int64, status, errCode, errMsg string) {
	if e.deps.Traces == nil {
		return
	}
	endedAt := e.now()
	var ttft int64
	if firstTokenAt > 0 && firstTokenAt >= startedAt {
		ttft = firstTokenAt - startedAt
	}
	duration := endedAt - startedAt
	if duration < 0 {
		duration = 0
	}
	var tps float64
	if duration > 0 && out.completionTokens > 0 {
		tps = float64(out.completionTokens) * 1000 / float64(duration)
	}
	modelName := res.Request.Model
	providerName := ""
	if provider != nil {
		providerName = provider.Name()
	}
	e.deps.Traces.RecordLLM(LLMTrace{
		CreatedAt:        startedAt,
		SessionID:        req.SessionID,
		RunID:            req.RunID,
		MessageID:        out.messageID,
		TraceID:          e.traceID(ctx),
		Model:            modelName,
		Provider:         providerName,
		Endpoint:         "chat/completions",
		Payload:          payload,
		PromptTokens:     out.promptTokens,
		CompletionTokens: out.completionTokens,
		CachedTokens:     out.cachedTokens,
		IsEstimated:      out.estimated,
		TTFTMs:           ttft,
		DurationMs:       duration,
		TokensPerSec:     tps,
		Status:           status,
		ErrorCode:        errCode,
		ErrorMessage:     errMsg,
	})
}

// recordToolTrace 投递一次工具调用的审计记录（会话库 tool_events 的查询副本）。
//
// 为什么需要副本：会话库按 run_id 查（业务回放），但「过去一小时哪些 write 工具失败了」
// 这类跨会话审计查询在「一会话一库」下要遍历 N 个库，不可行。
// 天库副本是可丢弃的，丢失只影响审计查询，不影响业务。
func (e *Engine) recordToolTrace(ctx context.Context, req RunRequest, ev *model.ToolEvent,
	resultJSON, resultBlob, errMsg string, exec model.ToolEventExec, status model.ToolEventStatus) {
	if e.deps.Traces == nil || ev == nil {
		return
	}
	endedAt := e.now()
	var duration int64
	if ev.StartedAt != nil {
		duration = endedAt - *ev.StartedAt
		if duration < 0 {
			duration = 0
		}
	}
	// 结果 >16KB 时只留摘要（与会话库 result_json 的截断策略一致），
	// 正文在会话库 blob 里，审计副本不需要重复占用。
	result := resultJSON
	if len(result) > 16<<10 {
		result = result[:16<<10]
	}
	e.deps.Traces.RecordToolCall(ToolTrace{
		CreatedAt:   endedAt,
		SessionID:   req.SessionID,
		RunID:       req.RunID,
		ToolEventID: ev.ID,
		ToolName:    ev.ToolName,
		Namespace:   ev.Namespace,
		ParamsJSON:  string(ev.ArgsJSON),
		ResultJSON:  result,
		Status:      string(status),
		Risk:        string(ev.Risk),
		Error:       errMsg,
		DurationMs:  duration,
	})
}
