package api

import (
	"net/http"

	"harness-go/internal/apperr"
	"harness-go/internal/service"
)

// marketUnavailableErr 构造「外部市场子系统未接入」错误。
func marketUnavailableErr() error {
	return apperr.New(apperr.CodeUnavailable, "外部市场子系统未接入（装配缺失）")
}

// ============ 外部市场（远端 registry） ============
//
// 语义澄清（前端必须先明确说明这一点）：
//   本市场是**外部来源**，制品由远端 registry 提供，服务端负责
//   「拉索引 → 校验 sha256 → 验签 → 落盘安装」。
//   与 /api/v1/packages（本地 zip 安装）是两条独立链路：
//   packages 只装服务端本地已有文件，本市场会**主动从远端下载**。
//
// 因此 GET /api/v1/market/registry 永远先回 status 再回 items：
//   未配置/已禁用/拉取失败 都必须让界面显示**原因**，绝不返回空列表冒充「市场为空」。

// getMarketRegistry 拉取外部市场索引（TTL 缓存；未配置时如实返回 status/reason）。
//
//	GET /api/v1/market/registry
func (h *handlers) getMarketRegistry(w http.ResponseWriter, r *http.Request) {
	if h.d.Market == nil {
		writeError(w, r, marketUnavailableErr())
		return
	}
	reg, err := h.d.Market.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reg)
}

// installMarketItemReq 市场安装请求体。
type installMarketItemReq struct {
	// ID 制品 id（必填）：与 registry 索引中的 id 一致
	ID string `json:"id"`
	// Kind 可选校验：package / skill（为空则不校验，便于前端简化）
	Kind string `json:"kind"`
}

// installMarketItem 下载并安装市场制品（sha256 完整性 + ed25519 验签双校验）。
//
//	POST /api/v1/market/install
func (h *handlers) installMarketItem(w http.ResponseWriter, r *http.Request) {
	if h.d.Market == nil {
		writeError(w, r, marketUnavailableErr())
		return
	}
	var req installMarketItemReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.ID == "" {
		writeError(w, r, invalidArgErr("id 不能为空"))
		return
	}
	item, err := h.d.Market.Install(r.Context(), req.ID, req.Kind)
	if err != nil {
		writeError(w, r, marketInstallErr(err))
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

// marketInstallErr 把市场错误翻译为应用错误码。
//
// 为什么单独翻译：市场是**外部不可信输入**，失败原因必须能被前端展示成
// 「配置问题 / 网络问题 / 校验拒绝 / 冲突」，而不是统一 500。
func marketInstallErr(err error) error {
	if me, ok := service.AsMarketError(err); ok {
		switch me.Kind {
		case "unconfigured", "disabled":
			return apperr.New(apperr.CodeUnavailable, "%s", me.Msg)
		case "not_found":
			return apperr.New(apperr.CodeNotFound, "%s", me.Msg)
		case "conflict":
			return apperr.New(apperr.CodeConflict, "%s", me.Msg)
		case "network", "bad_index", "digest_mismatch", "invalid":
			return apperr.New(apperr.CodeInvalidArgument, "%s", me.Msg)
		}
	}
	return err
}
