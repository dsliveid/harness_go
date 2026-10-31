package api

import (
	"net/http"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/service"
)

// ============ 设备（阶段 C-2：配对与设备注册） ============
//
// 端点清单（与 api/openapi.yaml 设备段一一对应）：
//
//	POST   /api/v1/devices/pair-codes  生成 6 位配对码（TTL 120s，管理端）
//	POST   /api/v1/devices/pair        端侧凭码配对换取 device_token（免登录，凭码即凭据）
//	GET    /api/v1/devices             设备列表（含离线/能力项）
//	DELETE /api/v1/devices/{did}       吊销设备（令牌立即失效 + 断连）

type createPairCodeReq struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// createPairCode 生成配对码。
func (h *handlers) createPairCode(w http.ResponseWriter, r *http.Request) {
	if h.d.Devices == nil {
		writeError(w, r, apperr.New(apperr.CodeDeviceDisabled, "设备功能未配置"))
		return
	}
	var req createPairCodeReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	view, err := h.d.Devices.IssuePairCode(r.Context(), req.Name, model.DeviceKind(req.Kind))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// capabilityInput 端侧能力上报项（只接受 name/version，禁止客户端指定 device_id）。
type capabilityInput struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type pairDeviceReq struct {
	Code         string            `json:"code"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Platform     string            `json:"platform"`
	AppVersion   string            `json:"app_version"`
	Capabilities []capabilityInput `json:"capabilities"`
}

// pairDevice 端侧凭码配对。成功返回 device_token 明文（仅此一次）。
func (h *handlers) pairDevice(w http.ResponseWriter, r *http.Request) {
	if h.d.Devices == nil {
		writeError(w, r, apperr.New(apperr.CodeDeviceDisabled, "设备功能未配置"))
		return
	}
	var req pairDeviceReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	caps := make([]model.Capability, 0, len(req.Capabilities))
	for _, c := range req.Capabilities {
		caps = append(caps, model.Capability{Name: c.Name, Version: c.Version})
	}
	res, err := h.d.Devices.Pair(r.Context(), clientIP(r), service.PairRequest{
		Code:         req.Code,
		Name:         req.Name,
		Kind:         model.DeviceKind(req.Kind),
		Platform:     req.Platform,
		AppVersion:   req.AppVersion,
		Capabilities: caps,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// listDevices 设备列表。
func (h *handlers) listDevices(w http.ResponseWriter, r *http.Request) {
	if h.d.Devices == nil {
		writeError(w, r, apperr.New(apperr.CodeDeviceDisabled, "设备功能未配置"))
		return
	}
	list, err := h.d.Devices.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":           list,
		"pair_ttl_s":      int(h.d.Devices.PairTTL().Seconds()),
		"heartbeat_s":     int(h.d.Devices.Heartbeat().Seconds()),
		"offline_after_s": int(h.d.Devices.OfflineAfter().Seconds()),
	})
}

// revokeDevice 吊销设备。
func (h *handlers) revokeDevice(w http.ResponseWriter, r *http.Request) {
	if h.d.Devices == nil {
		writeError(w, r, apperr.New(apperr.CodeDeviceDisabled, "设备功能未配置"))
		return
	}
	if err := h.d.Devices.Revoke(r.Context(), r.PathValue("did")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getSessionLease 查询会话租约（C-6：写者归属 / epoch / 是否有效）。
//
// 返回口径：**无租约不是 404**——语义是「当前无跨端写者，服务端本地可写」，
// 以 active=true / epoch=0 / holder="" 表示，前端可直接展示而无需分支。
func (h *handlers) getSessionLease(w http.ResponseWriter, r *http.Request) {
	if h.d.Devices == nil {
		writeError(w, r, apperr.New(apperr.CodeDeviceDisabled, "设备功能未配置"))
		return
	}
	view, err := h.d.Devices.SessionLease(r.Context(), r.PathValue("sid"), time.Now().UnixMilli())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// ============ 设备长连接（阶段 C-3：WS 网关） ============

// deviceWS 设备长连接升级入口（端 → 服出站 WS）。
//
// 鉴权**在升级前**由网关内部完成（device_token → devices.token_hash + 未吊销），
// 失败直接 401 JSON，绝不返回 101。故本路径置于 publicPaths（用户令牌中间件放行，
// 由设备令牌独立鉴权 —— 端侧没有用户账号）。
//
// 令牌经 query `?token=` 传入（WS 无法自定义请求头），access log 的 query 白名单
// 不含 token，故 **token 永不落日志**（observ.RedactQuery）。
func (h *handlers) deviceWS(w http.ResponseWriter, r *http.Request) {
	if h.d.Devices == nil || !h.d.Devices.Enabled() || h.d.DeviceWS == nil {
		writeError(w, r, apperr.New(apperr.CodeDeviceDisabled, "设备功能未启用"))
		return
	}
	h.d.DeviceWS.ServeHTTP(w, r)
}
