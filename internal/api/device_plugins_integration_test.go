package api

import (
	"net/http"
	"testing"
)

// pairTestDevice 走一次「配对码 → 配对」拿到设备 ID，供插件清单用例复用。
//
// 刻意申报 capabilities 含 `fs.read`：用例后面要断言「PUT 插件清单不会抹掉协议能力」，
// 若这里不申报，那条断言会形同虚设（空集恒成立）。
func pairTestDevice(t *testing.T, baseURL, token string) string {
	t.Helper()
	var code struct {
		Code string `json:"code"`
	}
	doJSON(t, http.MethodPost, baseURL+"/api/v1/devices/pair-codes", token,
		map[string]any{"name": "插件用例设备", "kind": "desktop"}, http.StatusCreated, &code)
	if code.Code == "" {
		t.Fatal("配对码签发失败")
	}
	var paired struct {
		DeviceID string `json:"device_id"`
	}
	doJSON(t, http.MethodPost, baseURL+"/api/v1/devices/pair", "",
		map[string]any{
			"code":         code.Code,
			"name":         "插件用例设备",
			"kind":         "desktop",
			"platform":     "windows",
			"capabilities": []map[string]string{{"name": "fs.read", "version": "1.0"}},
		}, http.StatusCreated, &paired)
	if paired.DeviceID == "" {
		t.Fatal("配对未返回 device_id")
	}
	return paired.DeviceID
}

// TestDevicePluginsEndpoints 客户端插件安装清单端到端（GET / PUT /api/v1/devices/{did}/plugins）。
//
// 这是插件市场「切到桌面端后仍显示仅启停」的修复出口验证。断言要点：
//   - PUT 整份覆盖后 GET 能读回（持久化在服务端，不是前端内存态）；
//   - 卸载 = 传入不含该项的新清单，同样能读回（幂等）；
//   - **协议能力不被抹掉**（fs.read 在配对时申报，PUT 之后必须还在）——
//     这条是复用 capabilities 表的安全红线，抹掉会让桌面端目录列举能力协商失败；
//   - 设备不存在返回 404，空 keys 合法（= 清空清单）。
func TestDevicePluginsEndpoints(t *testing.T) {
	srv, token := newTestServer(t)
	deviceID := pairTestDevice(t, srv.URL, token)
	// ① 初始为空（配对时申报的是 fs.read，不是插件项）
	var got struct {
		Keys []string `json:"keys"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/devices/"+deviceID+"/plugins", token, nil, http.StatusOK, &got)
	if len(got.Keys) != 0 {
		t.Fatalf("新配对设备的插件清单应为空，实际 %v", got.Keys)
	}

	// ② 安装两个工具（裸 key）
	var put struct {
		Keys []string `json:"keys"`
	}
	doJSON(t, http.MethodPut, srv.URL+"/api/v1/devices/"+deviceID+"/plugins", token,
		map[string]any{"keys": []string{"read_file", "list_dir"}}, http.StatusOK, &put)
	if len(put.Keys) != 2 || put.Keys[0] != "read_file" || put.Keys[1] != "list_dir" {
		t.Fatalf("PUT 回写应为服务端落库的裸 key，实际 %v", put.Keys)
	}

	// ③ 读回（关键：跨请求持久化，不是内存态）
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/devices/"+deviceID+"/plugins", token, nil, http.StatusOK, &got)
	if len(got.Keys) != 2 {
		t.Fatalf("插件清单应已持久化为 2 项，实际 %v", got.Keys)
	}

	// ④ 协议能力未被抹掉：fs.read 是配对时申报的，PUT 只应动 plugin: 命名空间。
	//    用设备列表端点核验（它回传 capabilities 全量）。
	var list struct {
		Items []struct {
			ID           string `json:"id"`
			Capabilities []struct {
				Name string `json:"name"`
			} `json:"capabilities"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/devices", token, nil, http.StatusOK, &list)
	var found bool
	for _, d := range list.Items {
		if d.ID != deviceID {
			continue
		}
		found = true
		var hasFsRead, hasPlugin bool
		for _, c := range d.Capabilities {
			if c.Name == "fs.read" {
				hasFsRead = true
			}
			if c.Name == "plugin:read_file" {
				hasPlugin = true
			}
		}
		if !hasFsRead {
			t.Fatalf("协议能力 fs.read 被插件清单覆写抹掉了（会让目录列举能力协商失败）：%+v", d.Capabilities)
		}
		if !hasPlugin {
			t.Fatalf("插件项未被写入 plugin: 命名空间：%+v", d.Capabilities)
		}
	}
	if !found {
		t.Fatalf("设备 %s 未出现在列表中", deviceID)
	}

	// ⑤ 卸载一个（整份覆盖：只留 list_dir）
	doJSON(t, http.MethodPut, srv.URL+"/api/v1/devices/"+deviceID+"/plugins", token,
		map[string]any{"keys": []string{"list_dir"}}, http.StatusOK, &put)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/devices/"+deviceID+"/plugins", token, nil, http.StatusOK, &got)
	if len(got.Keys) != 1 || got.Keys[0] != "list_dir" {
		t.Fatalf("卸载后应只剩 list_dir，实际 %v", got.Keys)
	}

	// ⑥ 清空清单是合法操作（不是 400）
	doJSON(t, http.MethodPut, srv.URL+"/api/v1/devices/"+deviceID+"/plugins", token,
		map[string]any{"keys": []string{}}, http.StatusOK, &put)
	if len(put.Keys) != 0 {
		t.Fatalf("清空后回写应为空，实际 %v", put.Keys)
	}

	// ⑦ 不存在的设备 → 404
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/devices/does-not-exist/plugins", token, nil, http.StatusNotFound, nil)
}
