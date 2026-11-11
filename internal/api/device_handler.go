package api

import (
	"net/http"
	"strconv"
	"strings"
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
//	GET    /api/v1/devices             设备列表（默认只含未吊销；?include_revoked=true 含审计）
//	DELETE /api/v1/devices/{did}       吊销设备（令牌立即失效 + 断连）
//	POST   /api/v1/devices/unpair      端侧自我解约（凭 device_token 吊销自己，免用户令牌）

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
	view, err := h.d.Devices.IssuePairCode(r.Context(), UserID(r.Context()), req.Name, model.DeviceKind(req.Kind))
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
//
// 免二次登录：配对码本身记录了**授权用户**（签发时必须登录，见 createPairCode），
// 故配对成功后可一并签发该用户的会话令牌，端侧拿到即可直达已登录态，无需再输口令。
//
// 降级铁律（三条必须同时成立才下发，禁止半截响应）：
//   - 配对码携带了 user_id（历史码为空串 → 不下发）；
//   - Auth 已装配（h.d.Auth != nil）；
//   - IssueToken + Me 均成功。
//
// 任一不满足时**省略** user_token/user/session_ttl_s 三字段，配对本身仍算成功 ——
// 此时配对码已被一次性消费，若因签发失败而回滚，用户只能重配，代价远大于多登一次。
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

	// 基础响应：与旧契约逐字节兼容（旧客户端忽略追加字段即维持原行为）。
	body := map[string]any{
		"device_id":        res.DeviceID,
		"device_token":     res.DeviceToken,
		"ws_path":          res.WSPath,
		"protocol_version": res.ProtocolVersion,
	}
	if extra := h.issuePairSessionUser(r, res); extra != nil {
		for k, v := range extra {
			body[k] = v
		}
	}
	writeJSON(w, http.StatusCreated, body)
}

// issueSessionPayload 为指定用户签发一份网页会话，返回响应片段（失败返 nil）。
//
// 配对与设备自续会话两处共用，避免「IssueToken + Me + TTL」这套逻辑写两遍后悄悄漂移
// —— 尤其是 scope 与 kind 这两个安全参数，两处取值必须永远一致。
//
// 安全性（三条，改动前必须先读）：
//   - 签发的是**与表单登录同源**的网页会话（kind=web_session），而非 api/admin 令牌；
//   - scope 恒为 write（最小权限，不授 admin），高危操作仍由审批闸门二次把关；
//   - 失败只记 Warn 且**绝不打印令牌明文**（error 文案由 service 层保证不含凭据）。
//
// 返回 nil 表示降级（Auth 未装配 / 签发失败 / 读账号失败），调用方据此省略字段，
// **绝不**把 nil 当成致命错误回滚已经成功的业务动作。
func (h *handlers) issueSessionPayload(r *http.Request, userID, deviceID, scene string) map[string]any {
	userID = strings.TrimSpace(userID)
	if userID == "" || h.d.Auth == nil {
		// 无归属身份 / Auth 未装配：合法降级。
		return nil
	}
	token, _, err := h.d.Auth.IssueToken(r.Context(), userID, model.TokenKindWebSession, "write")
	if err != nil {
		h.d.Log.Warn(scene+"：签发用户会话失败（已降级）",
			"device_id", deviceID, "user_id", userID, "err", err)
		return nil
	}
	user, err := h.d.Auth.Me(r.Context(), userID)
	if err != nil {
		h.d.Log.Warn(scene+"：读取账号失败（已降级）",
			"device_id", deviceID, "user_id", userID, "err", err)
		return nil
	}
	return map[string]any{
		"user_token":    token,
		"user":          toAuthUser(user),
		"session_ttl_s": int(h.d.AuthConfig.SessionTTL.Seconds()),
	}
}

// issuePairSessionUser 为配对成功的端侧顺带签发用户会话（失败一律降级为 nil）。
//
// 授权强度的来源仍是配对码本身（一次性 + TTL 120s + 失败限流 6 次/分钟/IP），
// 本函数不扩大攻击面：拿到码即已等同于拿到接入权，此处只是补齐「操作者是谁」。
func (h *handlers) issuePairSessionUser(r *http.Request, res *service.PairResult) map[string]any {
	return h.issueSessionPayload(r, res.OwnerUserID, res.DeviceID, "配对后")
}

// deviceSession 凭设备令牌换取一份新用户会话（设备与账号持久绑定的续期入口）。
//
//	POST /api/v1/devices/session
//
// 为什么需要它：user_token 会过期（session_ttl_h，默认 168h），过期后端侧只剩
// device_token，而设备令牌通不过用户令牌中间件。没有这个端点，「绑定即免登录」
// 只能撑 7 天，之后用户被迫重新输密码 —— 而设备其实还在，只是服务端不知道它属于谁。
//
// 鉴权（三条铁律）：
//   - 本端点位于 publicPaths（端侧没有用户令牌），故**自行**完成鉴权：
//     只认 `Authorization: Bearer <device_token>`。
//   - **明确拒绝 Cookie 通道**：extractToken 会回退读 hg_sess，那是 WebView 的
//     用户会话。若允许它，WebView 就能拿用户会话换取"刷新"，等于绕开设备身份、
//     也让吊销设备失效 —— 必须用 bearerToken 而非 extractToken。
//   - 设备被吊销 / 令牌无效 → Authenticate 统一 401（不区分原因，防枚举）。
func (h *handlers) deviceSession(w http.ResponseWriter, r *http.Request) {
	if h.d.Devices == nil {
		writeError(w, r, apperr.New(apperr.CodeDeviceDisabled, "设备功能未配置"))
		return
	}
	// 刻意用 bearerToken 而不是 extractToken：后者会回退到 hg_sess Cookie，
	// 而本端点要证明的是"这台机器"而非"这个浏览器会话"。
	token := bearerToken(r)
	if token == "" {
		writeError(w, r, apperr.New(apperr.CodeUnauthorized, "缺少设备令牌"))
		return
	}
	grant, err := h.d.Devices.IssueSessionForDevice(r.Context(), token)
	if err != nil {
		writeError(w, r, err)
		return
	}
	payload := h.issueSessionPayload(r, grant.UserID, grant.DeviceID, "设备自续会话")
	if payload == nil {
		// 归属已知但签发失败（Auth 未装配 / 存储故障）：这是真实的服务端问题，
		// 不能像配对那样静默降级 —— 端侧此刻没有别的凭据可用。
		writeError(w, r, apperr.New(apperr.CodeInternal, "无法为设备签发会话，请稍后重试"))
		return
	}
	h.d.Log.Info("设备凭令牌自续用户会话", "device_id", grant.DeviceID, "user_id", grant.UserID)
	writeJSON(w, http.StatusOK, payload)
}

// unpairDevice 端侧自我解约：凭 device_token 吊销**自己**（免用户令牌）。
//
// # 为什么必须是独立端点（而不是复用 DELETE /devices/{did}）
//
// 那个端点走全局用户令牌中间件，而端侧点「解除本机绑定」时手里只有
// device_token —— user_token 可能已过期 7 天（正是设备持久绑定要解决的问题）。
// 于是端侧解绑**结构上不可达**，只能清本地凭据，服务端记录原封不动，
// 这就是「客户端取消绑定后服务端没取消」的根因。
//
// # 鉴权口径（与 deviceSession 严格一致）
//
//   - 只认 `Authorization: Bearer <device_token>`，用 bearerToken(r) 而**不用**
//     extractToken(r)：后者会回退读 hg_sess Cookie，那就能用 WebView 的用户会话
//     "替别人解绑"，既绕开设备身份也让吊销语义失效。
//   - 目标设备由令牌哈希反查，**请求体不接受任何 deviceID** → 物理上无法删别人。
//
// # 幂等
//
// 令牌无效 / 设备不存在 / 已吊销 → 一律 204。端侧解绑的目标状态是
// 「这台设备不再绑定」，已经达成的事实不该报错，否则客户端会卡在
// 「解绑失败」而不敢清本地凭据。只有令牌缺失才是 401（调用方编程错误）。
func (h *handlers) unpairDevice(w http.ResponseWriter, r *http.Request) {
	if h.d.Devices == nil {
		writeError(w, r, apperr.New(apperr.CodeDeviceDisabled, "设备功能未配置"))
		return
	}
	token := bearerToken(r)
	if token == "" {
		writeError(w, r, apperr.New(apperr.CodeUnauthorized, "缺少设备令牌"))
		return
	}
	if err := h.d.Devices.RevokeSelf(r.Context(), token); err != nil {
		writeError(w, r, err)
		return
	}
	h.d.Log.Info("端侧自我解约成功（设备令牌已吊销）")
	w.WriteHeader(http.StatusNoContent)
}

// listDevices 设备列表。
//
// 默认只返回**未吊销**设备（= 当前仍处于绑定关系的设备）。管理端「解除绑定」后
// 设备即从列表消失，这是用户对"取消绑定"的直觉预期。
// 审计/运维需要看历史吊销记录时传 `?include_revoked=true`。
func (h *handlers) listDevices(w http.ResponseWriter, r *http.Request) {
	if h.d.Devices == nil {
		writeError(w, r, apperr.New(apperr.CodeDeviceDisabled, "设备功能未配置"))
		return
	}
	// 参数容错：非法值（如 include_revoked=abc）按 false 处理而不是 400 ——
	// 这是个纯展示开关，没必要因为它让整个列表打不开。
	includeRevoked, _ := strconv.ParseBool(strings.TrimSpace(r.URL.Query().Get("include_revoked")))
	list, err := h.d.Devices.List(r.Context(), includeRevoked)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":           list,
		"include_revoked": includeRevoked,
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
