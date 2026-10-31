package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
