package service

import (
	"harness-go/internal/agent"
	"harness-go/internal/logvault"
)

// traceSinkAdapter 把 agent.TraceSink 适配到 logvault.Writer。
//
// 为什么需要适配层：
//   - agent 包铁律是「不 import store / logvault」，只依赖自己的窄接口；
//   - logvault 不感知 agent 的 RunRequest 等概念。
//
// 两侧结构体字段一一对应，本适配器只做类型搬运，**不做任何阻塞操作**：
// logvault.Writer.RecordLLM/RecordToolCall 内部是非阻塞有界队列投递。
type traceSinkAdapter struct {
	writer *logvault.Writer
}

// RecordLLM 投递一次模型调用记录（非阻塞）。
func (a traceSinkAdapter) RecordLLM(t agent.LLMTrace) {
	if a.writer == nil {
		return
	}
	a.writer.RecordLLM(logvault.LLMRecord{
		CreatedAt:        t.CreatedAt,
		SessionID:        t.SessionID,
		RunID:            t.RunID,
		MessageID:        t.MessageID,
		TraceID:          t.TraceID,
		Model:            t.Model,
		Provider:         t.Provider,
		Endpoint:         t.Endpoint,
		Payload:          t.Payload,
		PromptTokens:     t.PromptTokens,
		CompletionTokens: t.CompletionTokens,
		CachedTokens:     t.CachedTokens,
		IsEstimated:      t.IsEstimated,
		TTFTMs:           t.TTFTMs,
		DurationMs:       t.DurationMs,
		TokensPerSec:     t.TokensPerSec,
		Status:           t.Status,
		ErrorCode:        t.ErrorCode,
		ErrorMessage:     t.ErrorMessage,
	})
}

// RecordToolCall 投递一次工具调用副本（非阻塞，可丢弃）。
func (a traceSinkAdapter) RecordToolCall(t agent.ToolTrace) {
	if a.writer == nil {
		return
	}
	a.writer.RecordToolCall(logvault.ToolCallRecord{
		CreatedAt:   t.CreatedAt,
		SessionID:   t.SessionID,
		RunID:       t.RunID,
		ToolEventID: t.ToolEventID,
		ToolName:    t.ToolName,
		Namespace:   t.Namespace,
		ParamsJSON:  t.ParamsJSON,
		ResultJSON:  t.ResultJSON,
		Status:      t.Status,
		Risk:        t.Risk,
		Error:       t.Error,
		DurationMs:  t.DurationMs,
	})
}

// NewTraceSink 构造审计端口适配器（供 main 装配使用）。
func NewTraceSink(writer *logvault.Writer) agent.TraceSink {
	return traceSinkAdapter{writer: writer}
}
