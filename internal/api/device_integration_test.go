package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"harness-go/internal/model"
)

// TestDeviceEndpoints 设备端点端到端：配对码 → 配对（免登录）→ 列表 → 吊销。
//
// 断言要点（阶段 C-2 出口）：
//   - 配对码 TTL 回显 120s
//   - 配对端点无需用户令牌（publicPaths），返回 device_token 明文仅此一次
//   - 列表不泄露令牌摘要
//   - 吊销后二次吊销返回 404
//   - 复用同一配对码返回统一错误码 device.pair_code_invalid（防枚举）
func TestDeviceEndpoints(t *testing.T) {
	srv, token := newTestServer(t)

	// ① 生成配对码（需登录）
	var code struct {
		Code      string `json:"code"`
		ExpiresAt int64  `json:"expires_at"`
		TTLSec    int    `json:"ttl_s"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair-codes", token,
		map[string]any{"name": "办公桌面", "kind": "desktop"}, http.StatusCreated, &code)
	if len(code.Code) != 6 || code.TTLSec != 120 || code.ExpiresAt == 0 {
		t.Fatalf("配对码响应错误（应为 6 位 / TTL 120s）: %+v", code)
	}

	// ② 端侧配对（免登录：不带 Authorization）
	var paired struct {
		DeviceID        string `json:"device_id"`
		DeviceToken     string `json:"device_token"`
		WSPath          string `json:"ws_path"`
		ProtocolVersion int    `json:"protocol_version"`

		// 免二次登录（本轮新增）：配对码携带授权用户，故一并下发用户会话。
		UserToken   string `json:"user_token"`
		SessionTTLS int    `json:"session_ttl_s"`
		User        struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair", "",
		map[string]any{
			"code":         code.Code,
			"name":         "我的笔记本",
			"kind":         "desktop",
			"platform":     "windows",
			"capabilities": []map[string]string{{"name": "fs.read", "version": "1.0"}},
		}, http.StatusCreated, &paired)
	if paired.DeviceID == "" || !strings.HasPrefix(paired.DeviceToken, "hgd_") {
		t.Fatalf("配对响应缺少 device_id / device_token: %+v", paired)
	}
	if paired.WSPath == "" || paired.ProtocolVersion == 0 {
		t.Fatalf("配对响应缺少 ws_path / protocol_version: %+v", paired)
	}
	// 关键断言：配对码由已登录账号签发 → 响应必须带用户会话（三字段同生共死）。
	if !strings.HasPrefix(paired.UserToken, "hgt_") {
		t.Fatalf("配对响应应包含 user_token（端侧免二次登录），得到 %q", paired.UserToken)
	}
	if paired.User.ID == "" || paired.SessionTTLS <= 0 {
		t.Fatalf("user_token 必须与 user / session_ttl_s 同时下发，得到 %+v", paired)
	}
	// 该会话必须真的可用：直接拿它访问需鉴权的 /auth/me。
	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", paired.UserToken, nil, http.StatusOK, &me)
	if me.ID == "" || me.ID != paired.User.ID {
		t.Fatalf("配对下发的 user_token 应能访问 /auth/me 且身份一致：me=%+v pair=%+v", me, paired.User)
	}
	// 双令牌边界不得被破坏：device_token 依然不能访问用户接口。
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", paired.DeviceToken, nil, http.StatusUnauthorized, nil)

	// ③ 同一码二次配对 → 401 同一错误码（防枚举）
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair", "",
		map[string]any{"code": code.Code}, http.StatusUnauthorized, &errBody)
	if errBody.Error.Code != "device.pair_code_invalid" {
		t.Fatalf("二次配对错误码应为 device.pair_code_invalid，得到 %q", errBody.Error.Code)
	}

	// ④ 不存在的码 → 同一状态与错误码
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair", "",
		map[string]any{"code": "000007"}, http.StatusUnauthorized, &errBody)
	if errBody.Error.Code != "device.pair_code_invalid" {
		t.Fatalf("不存在码的错误码应与已消费一致，得到 %q", errBody.Error.Code)
	}

	// ⑤ 列表：不泄露令牌摘要，包含能力项
	var list struct {
		Items []struct {
			ID               string `json:"id"`
			Name             string `json:"name"`
			Status           string `json:"status"`
			TokenHash        string `json:"-"`
			CapabilitiesHash string `json:"capabilities_hash"`
			Capabilities     []struct {
				Name string `json:"name"`
			} `json:"capabilities"`
		} `json:"items"`
		PairTTLS int `json:"pair_ttl_s"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/devices", token, nil, http.StatusOK, &list)
	if len(list.Items) != 1 {
		t.Fatalf("应有 1 台设备，得到 %d", len(list.Items))
	}
	if list.PairTTLS != 120 {
		t.Fatalf("列表应回显 pair_ttl_s=120，得到 %d", list.PairTTLS)
	}
	got := list.Items[0]
	if got.ID != paired.DeviceID || got.Name != "我的笔记本" {
		t.Fatalf("列表设备信息不一致: %+v", got)
	}
	if got.CapabilitiesHash == "" || len(got.Capabilities) != 1 {
		t.Fatalf("能力项未落库: %+v", got)
	}

	// 原始响应体不得出现令牌摘要字段
	rawResp, err := http.Get(srv.URL + "/api/v1/devices")
	if err != nil {
		t.Fatal(err)
	}
	defer rawResp.Body.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(rawResp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw["items"]), "token_hash") || strings.Contains(string(raw["items"]), paired.DeviceToken) {
		t.Fatal("设备列表响应体绝不允许包含 token_hash 或令牌明文")
	}

	// ⑥ 吊销 → 204；二次吊销 → 404 a device.not_found
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/devices/"+paired.DeviceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("吊销应返回 204，得到 %d", resp.StatusCode)
	}
	resp.Body.Close()

	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/devices/"+paired.DeviceID, token, nil,
		http.StatusNotFound, &errBody)
	if errBody.Error.Code != "device.not_found" {
		t.Fatalf("二次吊销错误码应为 device.not_found，得到 %q", errBody.Error.Code)
	}

	// ⑦ 解绑即消失：吊销后默认列表不再包含该设备（用户报告的「取消绑定后没消失」）
	afterRevoke := listDevicesRaw(t, srv.URL, token, "")
	if len(afterRevoke.Items) != 0 {
		t.Fatalf("吊销后默认设备列表应为空（解绑即消失），得到 %d 条", len(afterRevoke.Items))
	}
	// 审计视角仍可查（include_revoked=true）
	audit := listDevicesRaw(t, srv.URL, token, "?include_revoked=true")
	if len(audit.Items) != 1 || audit.Items[0].ID != paired.DeviceID {
		t.Fatalf("审计视角应含 1 条已吊销设备，得到 %+v", audit.Items)
	}
	if !audit.IncludeRevoked {
		t.Fatal("响应应回显 include_revoked=true")
	}
}

// listDevicesRaw 拉取设备列表（query 可为空串）。
func listDevicesRaw(t *testing.T, base, token, query string) DeviceListBody {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/devices"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("设备列表应 200，得到 %d", resp.StatusCode)
	}
	var body DeviceListBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// DeviceListBody 列表响应体的最小投影（含 include_revoked 回显）。
type DeviceListBody struct {
	Items []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"items"`
	IncludeRevoked bool `json:"include_revoked"`
}

// TestDeviceUnpairSelf 端侧自我解约端点：凭 device_token 吊销自己（免用户令牌）。
//
// 这是「客户端取消绑定后服务端必须一并取消」的服务端一半。覆盖五条边界：
//  ① 凭 device_token 调用 → 204，服务端立即从默认列表消失
//  ② 设备令牌随即失效（WS 鉴权 401，防"解绑后仍能连"）
//  ③ 同一令牌重复调用 → 204（幂等，不因"已经解绑"而失败）
//  ④ 无令牌 → 401（不是"已解绑"，是调用方错误）
//  ⑤ 用**用户令牌**调用 → 204 但什么都不吊销（bearerToken 只认设备令牌，
//     而用户令牌反查不到设备 → 幂等未命中），绝不误伤任意设备
func TestDeviceUnpairSelf(t *testing.T) {
	srv, userToken, _ := newTestServerWithDevices(t)

	// —— 准备两台设备，验证按令牌解绑绝不会误伤别人
	pair := func(name string) (deviceID, deviceToken string) {
		var code struct {
			Code string `json:"code"`
		}
		doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair-codes", userToken,
			map[string]any{"name": name, "kind": "desktop"}, http.StatusCreated, &code)
		var paired struct {
			DeviceID    string `json:"device_id"`
			DeviceToken string `json:"device_token"`
		}
		doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair", "",
			map[string]any{"code": code.Code, "name": name}, http.StatusCreated, &paired)
		return paired.DeviceID, paired.DeviceToken
	}
	selfID, selfToken := pair("要解绑的机器")
	otherID, _ := pair("别人的机器")

	if got := listDevicesRaw(t, srv.URL, userToken, ""); len(got.Items) != 2 {
		t.Fatalf("解绑前应有 2 台设备，得到 %d", len(got.Items))
	}

	// ④ 无令牌 → 401（先测，避免影响后续状态）
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/unpair", "", nil, http.StatusUnauthorized, nil)

	// ⑤ 用户令牌调用：204（幂等未命中）且**不吊销任何设备**
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/unpair", userToken, nil, http.StatusNoContent, nil)
	if got := listDevicesRaw(t, srv.URL, userToken, ""); len(got.Items) != 2 {
		t.Fatalf("用户令牌不应吊销任何设备（只认设备令牌），仍有 %d 台", len(got.Items))
	}

	// ① 凭设备令牌解绑自己 → 204，且从默认列表消失、别人的仍在
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/unpair", selfToken, nil, http.StatusNoContent, nil)
	visible := listDevicesRaw(t, srv.URL, userToken, "")
	if len(visible.Items) != 1 || visible.Items[0].ID != otherID {
		t.Fatalf("解绑后默认列表应只剩另一台设备，得到 %+v", visible.Items)
	}
	audit := listDevicesRaw(t, srv.URL, userToken, "?include_revoked=true")
	if len(audit.Items) != 2 {
		t.Fatalf("审计视角应仍能看到 2 台（含已解绑），得到 %d", len(audit.Items))
	}

	// ② 设备令牌即刻失效：不能再续会话（吊销 = 解绑的全部意义）
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/session", selfToken, nil,
		http.StatusUnauthorized, &errBody)

	// ③ 幂等：同一令牌重复解绑 → 204（不因"已解绑"报错，客户端才敢放心清本地凭据）
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/unpair", selfToken, nil, http.StatusNoContent, nil)

	// 确认 selfID 确实已是吊销态（而非被物理删除或状态错乱）
	audit2 := listDevicesRaw(t, srv.URL, userToken, "?include_revoked=true")
	var found bool
	for _, it := range audit2.Items {
		if it.ID == selfID {
			found = true
			if it.Status != "revoked" {
				t.Fatalf("解绑后状态应为 revoked，得到 %q", it.Status)
			}
		}
	}
	if !found {
		t.Fatal("审计视角应仍保留已解绑设备记录（revoked_at 是审计事实）")
	}
}

// TestDeviceEndpointsRequireAuth 管理端点仍需鉴权；配对端点免鉴权。
func TestDeviceEndpointsRequireAuth(t *testing.T) {
	srv, _ := newTestServer(t)

	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/devices", "", nil, http.StatusUnauthorized, &errBody)
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair-codes", "", map[string]any{}, http.StatusUnauthorized, &errBody)

	// 配对端点不带令牌可达（错误码是配对码无效，不是缺少令牌）
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair", "",
		map[string]any{"code": "123456"}, http.StatusUnauthorized, &errBody)
	if errBody.Error.Code != "device.pair_code_invalid" {
		t.Fatalf("配对端点应免鉴权并以配对码错误响应，得到 %q", errBody.Error.Code)
	}
}

// TestDeviceSessionRenew 设备凭令牌自续用户会话（设备与账号持久绑定的核心链路）。
//
// 覆盖五条硬边界，任一条破了都意味着「绑定即免登录」会在某个场景下静默失效：
//  ① 设备令牌能换到会话，且身份 = 配对授权者（主链路）
//  ② 拿换来的 **user_token** 反过来调本端点 → 401（只有设备令牌可以换）
//  ③ 设备被吊销后 → 401（吊销即刻失效，不留后门）
//  ④ 归属未知的历史设备 → 409 device.owner_unknown（可解释降级，不签发给任何账号）
//  ⑤ device_token 依旧不能直接访问 /auth/me（双令牌边界不被合并）
func TestDeviceSessionRenew(t *testing.T) {
	srv, userToken, deviceSvc := newTestServerWithDevices(t)

	// —— 准备：签发配对码 → 配对（拿到 device_token 与配对下发的 user_token）
	var code struct {
		Code string `json:"code"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair-codes", userToken,
		map[string]any{"name": "续期测试机", "kind": "desktop"}, http.StatusCreated, &code)

	var paired struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
		UserToken   string `json:"user_token"`
		User        struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair", "",
		map[string]any{
			"code": code.Code, "name": "续期测试机", "kind": "desktop", "platform": "windows",
			// 能力项可为空：本用例只关心会话续期，不涉及执行面能力协商
			"capabilities": []map[string]string{},
		}, http.StatusCreated, &paired)
	if paired.DeviceToken == "" || paired.User.ID == "" {
		t.Fatalf("前置配对未拿到设备令牌或授权用户: %+v", paired)
	}

	// —— ① 设备令牌换会话：身份必须等于配对时的授权者
	var renewed struct {
		UserToken   string `json:"user_token"`
		SessionTTLS int    `json:"session_ttl_s"`
		User        struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/session", paired.DeviceToken,
		nil, http.StatusOK, &renewed)
	if !strings.HasPrefix(renewed.UserToken, "hgt_") {
		t.Fatalf("自续会话应返回 hgt_ 前缀的 user_token，得到 %q", renewed.UserToken)
	}
	if renewed.User.ID != paired.User.ID {
		t.Fatalf("自续会话的身份必须是设备归属账号：得到 %q，期望 %q",
			renewed.User.ID, paired.User.ID)
	}
	if renewed.SessionTTLS <= 0 {
		t.Fatalf("自续会话应下发 session_ttl_s，得到 %d", renewed.SessionTTLS)
	}
	// 换来的会话必须真能用（只判非空会放过「签了但不可用」这类假绿）。
	var me struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", renewed.UserToken, nil, http.StatusOK, &me)
	if me.ID != paired.User.ID {
		t.Fatalf("换来的 user_token 应能访问 /auth/me 且身份一致：me=%q expected=%q",
			me.ID, paired.User.ID)
	}

	// —— ② 用 user_token 反调本端点：必须 401（只有设备令牌可以换）
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/session", renewed.UserToken,
		nil, http.StatusUnauthorized, nil)

	// —— ⑤ device_token 不能直接访问用户接口（双令牌边界）
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", paired.DeviceToken,
		nil, http.StatusUnauthorized, nil)

	// —— ③ 吊销设备后，原设备令牌换会话必须 401
	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/devices/"+paired.DeviceID, userToken,
		nil, http.StatusNoContent, nil)
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/session", paired.DeviceToken,
		nil, http.StatusUnauthorized, nil)

	// —— ④ 归属未知的历史设备：令牌有效但不能签发会话
	//
	// 构造方式：用 service 直接签发一张**不带授权者**的配对码（userID 传空串），
	// 兑换出的设备 user_id 为空 —— 这正是 0011 迁移后存量设备的真实形态。
	// 不走 HTTP 签发是因为 createPairCode 现在必然带上 UserID（那正是新行为）。
	ownless, err := deviceSvc.IssuePairCode(context.Background(), "", "老设备", model.DeviceKindDesktop)
	if err != nil {
		t.Fatalf("签发无归属配对码失败: %v", err)
	}
	var legacy struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
		UserToken   string `json:"user_token"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair", "",
		map[string]any{
			"code": ownless.Code, "name": "老设备", "kind": "desktop", "platform": "windows",
			"capabilities": []map[string]string{},
		}, http.StatusCreated, &legacy)
	if legacy.DeviceToken == "" {
		t.Fatalf("历史设备应配对成功（令牌有效才轮得到归属判定）: %+v", legacy)
	}
	// 无授权者 ⇒ 配对时也不该下发 user_token（同一降级口径）。
	if legacy.UserToken != "" {
		t.Fatalf("无授权者的配对不应下发 user_token，得到 %q", legacy.UserToken)
	}
	var ownerErr struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/session", legacy.DeviceToken,
		nil, http.StatusConflict, &ownerErr)
	if ownerErr.Error.Code != "device.owner_unknown" {
		t.Fatalf("归属未知设备应返回 device.owner_unknown，得到 %q", ownerErr.Error.Code)
	}
}
