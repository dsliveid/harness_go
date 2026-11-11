package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/service"
)

// ============ 审批（B-3） ============

type decideApprovalReq struct {
	Decision string `json:"decision"` // allow_once | allow_session | deny
	Reason   string `json:"reason"`
	// DeviceID 决策端设备 ID（阶段 C-7 跨端仲裁）：端侧 Runner 决策时填自身 device_id；
	// 浏览器/管理端留空表示服务端本地执行面。strict_approval_device=true 时用于校验。
	DeviceID string `json:"device_id"`
}

// decideApproval 提交审批决策：POST /api/v1/sessions/{sid}/runs/{rid}/approvals/{aid}/decide
//
// device_id 也可通过 query `?device_id=` 传入（端侧 WS 通道复用同一端点时更自然）；
// body 优先（显式声明强于隐式 query）。
func (h *handlers) decideApproval(w http.ResponseWriter, r *http.Request) {
	var req decideApprovalReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	deviceID := strings.TrimSpace(req.DeviceID)
	if deviceID == "" {
		deviceID = strings.TrimSpace(r.URL.Query().Get("device_id"))
	}
	// 硬约束 ③（协议 §6.2）：WebView 兑换出的降级会话不得做审批决策。
	//
	// 理由：WebView 里没有生物识别、没有设备绑定上下文，让它在手机上点「允许」
	// 等于把高危操作的最后一道人审搬到最不可信的界面上。移动端的产品定位是
	// 「只感知审批状态，不参与决策」（协议 §8 三端职责矩阵）。
	//
	// 校验方式：直接看当前请求令牌的 kind。不走中间件注入，是为了不改
	// TokenVerifier 签名（那会波及全部既有调用点），在唯一需要限制的入口校验更内聚。
	//
	// ============ 【有意取舍】这条校验是 fail-open 的，为什么可以接受 ============
	//
	// 现象：currentTokenKind 在「token 为空 / Auth 未装配 / TokenKind 查库报错」时一律返回空串，
	// 空串 != tokenKindWebview → 本校验放行。也就是说**存储抖动时 WebView 会话可以走完审批决策**。
	// 这不是漏写，是刻意的 fail-open：
	//
	// ① 为什么选 fail-open 而非 fail-closed：
	//   - 若改成「查不到 kind 就 403」，那么 DB 抖动 / 连接池耗尽 / TokenVerifier 未装配的**任何**抖动，
	//     都会让**桌面端与浏览器的正常用户**也无法审批。审批是 Run 的阻塞点（Gate.Await 卡在等待），
	//     一次抖动会把所有进行中的 Run 全部挂到超时（超时一律按 Deny 落库，见 Gate.Await），
	//     把「短暂存储故障」放大成「大批任务被误拒」，这是比越权更常见、更痛的故障。
	//   - 而 fail-open 的最坏后果被另一条边兜住：WebView 降级会话即便走到这里，也仍然受
	//     DecideApprovalFromDevice 的跨端仲裁（strict_approval_device=true 时 danger 风险审批
	//     只能由发起执行端设备确认）与 Gate 的待决项校验约束。真正的高危闸门不在本函数。
	//
	// ② 这个取舍在什么信任边界下才安全（三条前提，缺一条就得改成 fail-closed）：
	//   - 前提 a：服务端与存储同属一个信任域。TokenKind 查的是本机主库，其抖动是**基础设施故障**，
	//     不是攻击者可控的输入；外部攻击者无法诱导它返回 error。
	//   - 前提 b：WebView 降级是**纵深防御的一层，不是唯一一层**。strict_approval_device=true 时
	//     Gate.CheckDevice 仍会按 RequiredDeviceID 拒绝非执行端设备；因此即便本层 fail-open 漏过，
	//     高危审批依然被设备绑定挡住。若哪天 Gate 侧的设备仲裁被移除或默认关闭且无人开启 strict，
	//     本层就会变成**唯一**防线，必须立刻改为 fail-closed。
	//   - 前提 c：降级标记由服务端令牌签发时写入（tokenKindWebview 只在兑换 ticket 时落到令牌上），
	//     不来自请求体/请求头等客户端可控字段。一旦有人改成读 body/query 里的 kind，攻击者直接伪造绕过。
	//
	// ③ 什么时候会真正变成风险：① TokenKind 的错误来源变成攻击者可诱导的（例如改成远程校验、
	// 或错误信息可被外部触发）；② strict_approval_device 长期保持默认 false（只审计不阻断）
	// 且部署在多用户/公网场景——此时 WebView 会话的越权审批将无人兜底；
	// ③ currentTokenKind 被改成「忽略 error 但返回部分结果」这类更宽松的形态。
	// 改动本节任一条，都必须把本函数从 fail-open 切换为 fail-closed（并在 Gate 侧保留审计日志）。
	if h.currentTokenKind(r) == tokenKindWebview {
		writeError(w, r, apperr.New(apperr.CodeForbidden,
			"WebView 会话不能审批，请在电脑端确认"))
		return
	}
	err := h.d.Runs.DecideApprovalFromDevice(r.Context(),
		r.PathValue("sid"), r.PathValue("rid"), r.PathValue("aid"),
		req.Decision, req.Reason, deviceID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"approval_id":       r.PathValue("aid"),
		"decision":          req.Decision,
		"status":            "accepted",
		"decided_by_device": deviceID,
	})
}

// ============ 厂商（B-4） ============

type providerReq struct {
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key"`
	Models  []string `json:"models"`
	Enabled *bool    `json:"enabled"`
}

func (req providerReq) toModel() model.Provider {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return model.Provider{
		Name:    req.Name,
		BaseURL: req.BaseURL,
		APIKey:  req.APIKey,
		Models:  req.Models,
		Enabled: enabled,
	}
}

func (h *handlers) listProviders(w http.ResponseWriter, r *http.Request) {
	items, err := h.d.Providers.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *handlers) createProvider(w http.ResponseWriter, r *http.Request) {
	var req providerReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	p, err := h.d.Providers.Create(r.Context(), req.toModel())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *handlers) updateProvider(w http.ResponseWriter, r *http.Request) {
	var req providerReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	p, err := h.d.Providers.Update(r.Context(), r.PathValue("pid"), req.toModel())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *handlers) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Providers.Delete(r.Context(), r.PathValue("pid")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// providerTestReq 连通测试入参（均可选）：
//   - model 留空用厂商首个模型；
//   - base_url / api_key / models 携带前端草稿里「尚未保存」的当前值，实现所见即所测
//     （厂商未落库时 base_url 必填，否则 404；api_key 仅本次测试使用，不落库）。
type providerTestReq struct {
	Model   string   `json:"model"`
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key"`
	Models  []string `json:"models"`
}

func (h *handlers) testProvider(w http.ResponseWriter, r *http.Request) {
	var req providerTestReq
	// 允许空 body（前端不给 model 时用厂商首个模型）
	if err := decodeJSON(r, &req); err != nil {
		// 只有「请求体为空/格式不完整」刻意忽略（Body 为 EOF），其余照常报错
		if !isBodyEmpty(err) {
			writeError(w, r, err)
			return
		}
		req = providerTestReq{}
	}
	res, err := h.d.Providers.Test(r.Context(), r.PathValue("pid"), service.ProviderTestInput{
		Model:   req.Model,
		BaseURL: req.BaseURL,
		APIKey:  req.APIKey,
		Models:  req.Models,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// listProviderModels 拉取上游真实模型清单：POST /api/v1/providers/{pid}/models
//
// 支持草稿覆盖（与 test 一致）：厂商尚未保存时传 base_url/api_key 即可直接拉取，
// 仅本次请求使用、不落库。失败时按上游原因返回可读错误（4xx/5xx），**不做本地目录兜底**。
func (h *handlers) listProviderModels(w http.ResponseWriter, r *http.Request) {
	var req providerTestReq
	// 允许空 body（已保存厂商直接读库内配置）
	if err := decodeJSON(r, &req); err != nil {
		if !isBodyEmpty(err) {
			writeError(w, r, err)
			return
		}
		req = providerTestReq{}
	}
	// 兼容 query 传参（旧 GET 调用 / 命令行调试）
	if v := strings.TrimSpace(r.URL.Query().Get("base_url")); v != "" && req.BaseURL == "" {
		req.BaseURL = v
	}
	if v := strings.TrimSpace(r.URL.Query().Get("api_key")); v != "" && req.APIKey == "" {
		req.APIKey = v
	}
	res, err := h.d.Providers.ListModels(r.Context(), r.PathValue("pid"), service.ProviderModelsInput{
		BaseURL: req.BaseURL,
		APIKey:  req.APIKey,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

// revealProviderKey 读取厂商密钥明文（眼睛图标显示 / 复制）：GET /api/v1/providers/{pid}/secret
//
// 明文敏感：响应禁止任何缓存，并明确 no-referrer，避免被中间层/截图链路留存。
func (h *handlers) revealProviderKey(w http.ResponseWriter, r *http.Request) {
	res, err := h.d.Providers.RevealAPIKey(r.Context(), r.PathValue("pid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusOK, res)
}

// ============ 配置（B-4） ============

func (h *handlers) getSettings(w http.ResponseWriter, r *http.Request) {
	items, err := h.d.Settings.Get(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *handlers) patchSettings(w http.ResponseWriter, r *http.Request) {
	var patch map[string]json.RawMessage
	if err := decodeJSON(r, &patch); err != nil {
		writeError(w, r, err)
		return
	}
	items, err := h.d.Settings.Patch(r.Context(), patch)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ============ 统计与日志（B-4） ============
func (h *handlers) stats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.d.Maintenance.Stats(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// 注：旧 logs handler 已迁移至 log_handler.go —— 新增 from/to/traceId/keyword/offset
// 与保留策略元信息，并保持响应向后兼容（items[{level,text}]）。

// isBodyEmpty 判断解析失败是否仅仅因为请求体为空（EOF）。
//
// 用于「可选 body」端点（如厂商连通测试）：空 body 用默认值，格式错误仍报 400。
func isBodyEmpty(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
