package api

// OpenAI 兼容端点（P1 / 需求 §4.3 / F9.2）：/v1/models + /v1/chat/completions。
//
// 鉴权沿用全局 Bearer API Token 中间件（/v1/* 不在公开路径白名单）。
// 响应错误用 OpenAI 形状（{"error":{message,type,code}}），不混用平台错误包裹。

import (
	"encoding/json"
	"net/http"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/service"
)

// listOpenAIModels 模型清单。
//
//	GET /v1/models
func (h *handlers) listOpenAIModels(w http.ResponseWriter, r *http.Request) {
	if h.d.OpenAI == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "openai_not_ready", "OpenAI 兼容端点未接线")
		return
	}
	models, err := h.d.OpenAI.Models(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

// openaiChatReq 请求体（含流式选项）。
type openaiChatReq struct {
	service.OpenAIChatRequest
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

// createOpenAIChatCompletion 对话补全（stream:true 时为 OpenAI 形状 SSE）。
//
//	POST /v1/chat/completions
func (h *handlers) createOpenAIChatCompletion(w http.ResponseWriter, r *http.Request) {
	if h.d.OpenAI == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "openai_not_ready", "OpenAI 兼容端点未接线")
		return
	}
	var req openaiChatReq
	if err := decodeJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}
	sessionID := r.Header.Get("X-Session-ID")

	if !req.Stream {
		resp, err := h.d.OpenAI.Chat(r.Context(), req.OpenAIChatRequest, sessionID)
		if err != nil {
			writeOpenAIServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// —— SSE（OpenAI chunk 形状） ——
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "当前连接不支持流式")
		return
	}

	id := "chatcmpl-stream"
	model := req.Model
	created := time.Now().Unix()
	sendChunk := func(delta map[string]any, finishReason any) error {
		choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}
		frame := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created,
			"model": model, "choices": []any{choice},
		}
		b, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		if _, wErr := w.Write([]byte("data: " + string(b) + "\n\n")); wErr != nil {
			return wErr
		}
		flusher.Flush()
		return nil
	}

	// role 帧（OpenAI 客户端约定首帧带 role）
	if err := sendChunk(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
		return
	}

	err := h.d.OpenAI.ChatStream(r.Context(), req.OpenAIChatRequest, sessionID,
		func(item service.OpenAIStreamItem) error {
			switch {
			case item.Err != nil:
				return item.Err // 交给外层错误帧
			case item.Delta != "":
				return sendChunk(map[string]any{"content": item.Delta}, nil)
			case item.FinishReason != "":
				if req.StreamOptions != nil && req.StreamOptions.IncludeUsage && item.Usage != nil {
					// usage 帧（choices 为空数组——OpenAI 约定）
					b, _ := json.Marshal(map[string]any{
						"id": id, "object": "chat.completion.chunk", "created": created,
						"model": model, "choices": []any{},
						"usage": item.Usage,
					})
					if _, wErr := w.Write([]byte("data: " + string(b) + "\n\n")); wErr != nil {
						return wErr
					}
					flusher.Flush()
				}
				return sendChunk(map[string]any{}, item.FinishReason)
			}
			return nil
		})
	if err != nil {
		// 流中错误：输出 OpenAI error 帧（尽力而为——客户端可能已断开）
		ae := apperr.From(err)
		msg := err.Error()
		code := "internal_error"
		if ae != nil {
			msg = ae.Message
			code = string(ae.Code)
		}
		b, mErr := json.Marshal(map[string]any{
			"error": map[string]any{"message": msg, "type": "server_error", "code": code},
		})
		if mErr == nil {
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// writeOpenAIServiceError 把 service 层错误映射为 OpenAI 错误形状。
func writeOpenAIServiceError(w http.ResponseWriter, err error) {
	ae := apperr.From(err)
	if ae == nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	status := ae.Status()
	errType := "server_error"
	if status == http.StatusBadRequest || status == http.StatusNotFound {
		errType = "invalid_request_error"
	}
	writeOpenAIError(w, status, errType, ae.Message)
}

// writeOpenAIError OpenAI 错误形状。
func writeOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": errType, "code": errType},
	})
}
