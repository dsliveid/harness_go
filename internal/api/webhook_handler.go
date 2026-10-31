package api

// Webhook 事件订阅管理（P1 / 需求 §4.4）：CRUD + 联通测试。
// secret 只写不读（API 永不回显明文/密文，只回 has_secret）。

import (
	"net/http"

	"harness-go/internal/apperr"
	"harness-go/internal/service"
)

// webhookView 订阅视图（secret 脱敏）。
type webhookView struct {
	ID        string   `json:"id"`
	URL       string   `json:"url"`
	HasSecret bool     `json:"has_secret"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
	CreatedAt int64    `json:"created_at"`
}

// listWebhooks 订阅列表。
//
//	GET /api/v1/webhooks
func (h *handlers) listWebhooks(w http.ResponseWriter, r *http.Request) {
	if h.d.Webhooks == nil {
		writeError(w, r, webhookUnavailableErr())
		return
	}
	items, err := h.d.Webhooks.ListView(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// createWebhook 新增订阅。
//
//	POST /api/v1/webhooks
func (h *handlers) createWebhook(w http.ResponseWriter, r *http.Request) {
	if h.d.Webhooks == nil {
		writeError(w, r, webhookUnavailableErr())
		return
	}
	var in service.WebhookCreateInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, r, err)
		return
	}
	v, err := h.d.Webhooks.Create(r.Context(), in)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// deleteWebhook 删除订阅。
//
//	DELETE /api/v1/webhooks/{id}
func (h *handlers) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	if h.d.Webhooks == nil {
		writeError(w, r, webhookUnavailableErr())
		return
	}
	if err := h.d.Webhooks.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusNoContent, map[string]any{})
}

// setWebhookEnabled 启用/停用。
//
//	POST /api/v1/webhooks/{id}/enable | /disable
func (h *handlers) setWebhookEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.d.Webhooks == nil {
			writeError(w, r, webhookUnavailableErr())
			return
		}
		if err := h.d.Webhooks.SetEnabled(r.Context(), r.PathValue("id"), enabled); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// testWebhook 联通测试：向该订阅同步投递一条 webhook.test 事件（不重试）。
//
//	POST /api/v1/webhooks/{id}/test
func (h *handlers) testWebhook(w http.ResponseWriter, r *http.Request) {
	if h.d.Webhooks == nil {
		writeError(w, r, webhookUnavailableErr())
		return
	}
	status, detail, err := h.d.Webhooks.TestFire(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	ok := status >= 200 && status < 300
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "ok": ok, "error": detail})
}

func webhookUnavailableErr() error {
	return apperr.New(apperr.CodeUnavailable, "Webhook 子系统未接线")
}
