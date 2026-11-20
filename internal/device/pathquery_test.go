package device

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"harness-go/internal/apperr"
)

// ============ 目录列举 RPC（L1）回归测试 ============
//
// 这是「客户端目录选择」的唯一执行通道，因此每条铁律都必须被断言锁住：
//   A. 强制指定 deviceID，绝不改选其他在线设备；
//   B. 设备不可用时明确报错，**绝不**回落服务端本地列举；
//   C. 不落库、不产生 tool_event（本包不 import store/engine 即为结构性保证）；
//   D. 3 秒超时；
//   E. 端侧拒绝如实透出，不降级成空列表。

// pathQueryConn 满足 Connection 的最小桩，记录写出的帧。
//
// 刻意不复用 gateway_test.go 里的 fakeConn：那个桩带有网关专属行为（如自动触发
// OnOffline），复用会让本文件的断言依赖无关逻辑，失败时难以定位。
type pathQueryConn struct {
	id   string
	sent []Envelope
	err  error
}

func (c *pathQueryConn) DeviceID() string { return c.id }
func (c *pathQueryConn) Send(env Envelope) error {
	if c.err != nil {
		return c.err
	}
	c.sent = append(c.sent, env)
	return nil
}
func (c *pathQueryConn) CloseWith(string, string) {}
func (c *pathQueryConn) LastSeen() time.Time      { return time.Now() }
func (c *pathQueryConn) RemoteAddr() string       { return "test" }

// newPathQueryEnv 造一个带假连接的 Hub。
func newPathQueryEnv(t *testing.T, deviceID string) (*PathQuery, *pathQueryConn, *Hub) {
	t.Helper()
	h := NewHub(HubOptions{})
	c := &pathQueryConn{id: deviceID}
	h.Register(c)
	return NewPathQuery(h, nil), c, h
}

// ① 设备在线 → 必须写出 fs.list 帧，且 req_id 与 payload 形状正确。
func TestPathQuerySendsFsListFrame(t *testing.T) {
	q, c, _ := newPathQueryEnv(t, "dev-1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 应答由测试线程投递（模拟端侧）
		// 先等帧写出一小会儿再由外部 Deliver（见下）
	}()
	_ = done

	// 用 goroutine 触发 List，同时在主线程等待帧写出后投递结果。
	resCh := make(chan *FsListResultPayload, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := q.List(context.Background(), "dev-1", `D:\Work`)
		resCh <- res
		errCh <- err
	}()

	// 等到帧出现
	var env Envelope
	for i := 0; i < 200; i++ {
		if len(c.sent) > 0 {
			env = c.sent[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if env.Type != MsgFsList {
		t.Fatalf("应下发 fs.list 帧，实际 %q", env.Type)
	}
	var p FsListPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("帧解码失败: %v", err)
	}
	if p.Path != `D:\Work` {
		t.Fatalf("path 应透传，得到 %q", p.Path)
	}

	parent := `D:\`
	q.Deliver(env.ID, FsListResultPayload{
		Path: `D:\Work`, Parent: &parent, Separator: `\`,
		Dirs: []FsDirEntry{{Name: "Proj", Path: `D:\Work\Proj`}},
	})

	res := <-resCh
	if err := <-errCh; err != nil {
		t.Fatalf("正常应答不应报错: %v", err)
	}
	if res == nil || len(res.Dirs) != 1 || res.Dirs[0].Name != "Proj" {
		t.Fatalf("应返回端侧目录项，得到 %+v", res)
	}
}

// ② 设备不在线 → fs.host_offline，且**不能**退化为本地列举。
func TestPathQueryOfflineDeviceFails(t *testing.T) {
	q, _, _ := newPathQueryEnv(t, "dev-1")

	_, err := q.List(context.Background(), "dev-other", `D:\`)
	if err == nil {
		t.Fatal("设备不在线必须报错")
	}
	if code := errCode(err); code != apperr.CodeFSHostOffline {
		t.Fatalf("应为 fs.host_offline，得到 %s", code)
	}
}

// ③ 空 deviceID → 明确拒绝（铁律 A：不许"挑一台在线设备"）。
func TestPathQueryEmptyDeviceRejected(t *testing.T) {
	q, c, _ := newPathQueryEnv(t, "dev-1")

	_, err := q.List(context.Background(), "   ", `D:\`)
	if err == nil {
		t.Fatal("空 deviceID 必须报错")
	}
	if code := errCode(err); code != apperr.CodeFSHostUnsupported {
		t.Fatalf("应为 fs.host_unsupported，得到 %s", code)
	}
	if len(c.sent) != 0 {
		t.Fatal("空 deviceID 时不得向任何设备写帧（禁止改选设备）")
	}
}

// ④ 端侧不回应 → 3 秒超时（fs.host_timeout）。
//
// 为了不让测试真等 3 秒，这里用带 deadline 的 ctx 提前触发 ctx.Done 分支；
// 两条分支都映射到同一错误码，语义一致。
func TestPathQueryTimeout(t *testing.T) {
	q, _, _ := newPathQueryEnv(t, "dev-1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := q.List(ctx, "dev-1", `D:\`)
	if err == nil {
		t.Fatal("无应答必须超时报错")
	}
	if code := errCode(err); code != apperr.CodeFSHostTimeout {
		t.Fatalf("应为 fs.host_timeout，得到 %s", code)
	}
	if q.Pending() != 0 {
		t.Fatalf("超时后必须清理等待者，实际残留 %d", q.Pending())
	}
}

// ⑤ 端侧回报 path_denied / path_not_found → 如实映射，**不降级成空列表**。
func TestPathQueryTranslatesDeviceErrors(t *testing.T) {
	cases := []struct {
		name string
		code string
		want apperr.Code
	}{
		{"越界", "path_denied", apperr.CodeFSPathDenied},
		{"不存在", "path_not_found", apperr.CodeFSPathNotFound},
		{"未知码保守按拒绝", "some_future_code", apperr.CodeFSPathDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, c, _ := newPathQueryEnv(t, "dev-1")
			errCh := make(chan error, 1)
			go func() {
				_, err := q.List(context.Background(), "dev-1", `D:\x`)
				errCh <- err
			}()
			var env Envelope
			for i := 0; i < 200; i++ {
				if len(c.sent) > 0 {
					env = c.sent[0]
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			q.Deliver(env.ID, FsListResultPayload{Code: tc.code, Error: "拒绝了"})
			err := <-errCh
			if err == nil {
				t.Fatal("端侧拒绝必须转成错误，不得返回空列表")
			}
			if code := errCode(err); code != tc.want {
				t.Fatalf("应为 %s，得到 %s", tc.want, code)
			}
		})
	}
}

// ⑥ 端侧文案含 % 时不得被 Sprintf 解析坏（真实发生过的一类缺陷）。
func TestPathQueryMessageWithPercentIsLiteral(t *testing.T) {
	q, c, _ := newPathQueryEnv(t, "dev-1")
	errCh := make(chan error, 1)
	go func() {
		_, err := q.List(context.Background(), "dev-1", `D:\x`)
		errCh <- err
	}()
	var env Envelope
	for i := 0; i < 200; i++ {
		if len(c.sent) > 0 {
			env = c.sent[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	q.Deliver(env.ID, FsListResultPayload{Code: "path_denied", Error: "已用 50% 配额"})
	err := <-errCh
	if err == nil {
		t.Fatal("应报错")
	}
	if got := err.Error(); !contains(got, "50%") {
		t.Fatalf("含 %% 的端侧文案必须原样透出，实际 %q", got)
	}
}

// ⑦ 迟到/重复应答不得 panic，也不得重复投递。
func TestPathQueryLateResponseIsDropped(t *testing.T) {
	q, _, _ := newPathQueryEnv(t, "dev-1")
	if q.Deliver("no-such-req", FsListResultPayload{}) {
		t.Fatal("无等待者的应答应被静默丢弃")
	}
}

// ⑧ hub 为 nil → 明确报错，绝不 panic（设备域未启用的部署形态）。
func TestPathQueryNilHubIsSafe(t *testing.T) {
	q := NewPathQuery(nil, nil)
	if _, err := q.List(context.Background(), "dev-1", ""); err == nil {
		t.Fatal("未启用设备域时必须报错")
	}
	var nilQ *PathQuery
	if _, err := nilQ.List(context.Background(), "dev-1", ""); err == nil {
		t.Fatal("nil RPC 必须报错")
	}
}

// ⑨ 未定位（path 为空）→ with_roots=true，让端侧只回根。
func TestPathQueryEmptyPathRequestsRoots(t *testing.T) {
	q, c, _ := newPathQueryEnv(t, "dev-1")
	go func() { _, _ = q.List(context.Background(), "dev-1", "") }()

	var env Envelope
	for i := 0; i < 200; i++ {
		if len(c.sent) > 0 {
			env = c.sent[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var p FsListPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if !p.WithRoots {
		t.Fatal("未定位时必须请求 roots")
	}
	q.Deliver(env.ID, FsListResultPayload{Separator: `/`})
}

// ⑩ 转仓契约 fixture：internal/device/testdata/fs_list.json 必须能解码为本包帧类型。
//
// 🚨 **双仓同步约定**：testdata/fs_list.json 与 testdata/fs_list_result.json 是
// harness_go 与 harness_desktop（crates/hd-protocol/fixtures/）**共用的契约样本**，
// 由服务端 Go 与桌面端 Rust 各自解码同一份字节。任何一侧改动必须双仓同步，
// 否则两端会出现「自己都能过测试但互相听不懂」的分叉（这正是 fs.list 事故的土壤）。
// 服务端侧只允许用 Copy-Item 逐字节复制，**禁止**在此手工编辑。
func TestFsListFixtureContract(t *testing.T) {
	raw := readFixture(t, "fs_list.json")

	var env Envelope
	if err := decodeJSONStrict(raw, &env); err != nil {
		t.Fatalf("fs_list.json 必须能解码为 Envelope: %v", err)
	}
	if env.Type != MsgFsList {
		t.Fatalf("type 应为 %q，得到 %q", MsgFsList, env.Type)
	}
	if !env.Type.Valid() {
		t.Fatal("fs.list 必须是已知帧类型（否则端侧会归为 Unknown 静默丢弃）")
	}
	if env.Type.IsTaskFrame() {
		t.Fatal("fs.list 不得属于任务帧族（否则会走 dispatcher 产生 tool_event）")
	}
	if env.ID != "fsq-1704067200000000000-1" {
		t.Fatalf("id 应逐字保留，得到 %q", env.ID)
	}
	if env.V != 1 {
		t.Fatalf("v 应为 1，得到 %d", env.V)
	}
	var p FsListPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("payload 解码失败: %v", err)
	}
	if p.Path != "" {
		t.Fatalf("fixture 为「未定位」请求，path 应为空串，得到 %q", p.Path)
	}
	if !p.WithRoots {
		t.Fatal("未定位请求必须 with_roots=true")
	}
}

// ⑪ 转仓契约 fixture：fs.list.result 关键字段必须逐字对齐（字段名拼错即在此爆红）。
func TestFsListResultFixtureContract(t *testing.T) {
	raw := readFixture(t, "fs_list_result.json")

	var env Envelope
	if err := decodeJSONStrict(raw, &env); err != nil {
		t.Fatalf("fs_list_result.json 必须能解码为 Envelope: %v", err)
	}
	if env.Type != MsgFsListResult {
		t.Fatalf("type 应为 %q，得到 %q", MsgFsListResult, env.Type)
	}
	var p FsListResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("payload 解码失败: %v", err)
	}
	if p.Path != `C:\Users\me\work` {
		t.Fatalf("path 应逐字对齐，得到 %q", p.Path)
	}
	if p.Parent == nil || *p.Parent != `C:\Users\me` {
		t.Fatalf("parent 应逐字对齐，得到 %v", p.Parent)
	}
	if p.Separator != `\` {
		t.Fatalf("separator 应为反斜杠，得到 %q", p.Separator)
	}
	if len(p.Roots) != 2 || p.Roots[0].Name != "work" || p.Roots[1].Path != `C:\Users\me\tmp` {
		t.Fatalf("roots 应逐字对齐，得到 %+v", p.Roots)
	}
	if len(p.Dirs) != 2 || p.Dirs[0].Name != "Proj" || p.Dirs[1].Path != `C:\Users\me\work\Docs` {
		t.Fatalf("dirs 应逐字对齐，得到 %+v", p.Dirs)
	}
	if p.Truncated {
		t.Fatal("truncated 应为 false")
	}
	if p.Error != "" || p.Code != "" {
		t.Fatalf("成功样本不得携带 error/code，得到 %q/%q", p.Error, p.Code)
	}
}

// ⑫ 端侧回带 error + code 的应答帧（走真实 JSON 解码路径）→ 必须如实映射，
// **不得**降级成空列表（否则用户会以为那台机器上这个目录是空的）。
func TestPathQueryDeviceErrorFrameMapping(t *testing.T) {
	cases := []struct {
		name string
		body string
		want apperr.Code
	}{
		{
			name: "path_denied",
			body: `{"v":1,"type":"fs.list.result","id":"r1","payload":{"path":"","parent":null,` +
				`"separator":"\\","dirs":[],"error":"该路径不在白名单内","code":"path_denied"}}`,
			want: apperr.CodeFSPathDenied,
		},
		{
			name: "path_not_found",
			body: `{"v":1,"type":"fs.list.result","id":"r2","payload":{"path":"","parent":null,` +
				`"separator":"\\","dirs":[],"error":"目录不存在","code":"path_not_found"}}`,
			want: apperr.CodeFSPathNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var env Envelope
			if err := decodeJSONStrict([]byte(tc.body), &env); err != nil {
				t.Fatalf("帧解码失败: %v", err)
			}
			if env.Type != MsgFsListResult {
				t.Fatalf("type 应为 fs.list.result，得到 %q", env.Type)
			}
			var payload FsListResultPayload
			if err := env.Decode(&payload); err != nil {
				t.Fatalf("payload 解码失败: %v", err)
			}

			q, c, _ := newPathQueryEnv(t, "dev-1")
			errCh := make(chan error, 1)
			go func() {
				_, err := q.List(context.Background(), "dev-1", `D:\x`)
				errCh <- err
			}()
			// 等请求帧写出，再用 fixture 同形的应答投递（reqID 必须配对）。
			for i := 0; i < 200 && len(c.sent) == 0; i++ {
				time.Sleep(5 * time.Millisecond)
			}
			q.Deliver(c.sent[0].ID, payload)
			err := <-errCh
			if err == nil {
				t.Fatal("端侧拒绝必须转成错误，不得返回空列表")
			}
			if code := errCode(err); code != tc.want {
				t.Fatalf("应为 %s，得到 %s", tc.want, code)
			}
		})
	}
}

// readFixture 读取 testdata 下的契约样本（缺失即失败，绝不静默跳过）。
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取契约样本 %s 失败（双仓同步约定要求该文件必须存在）: %v", name, err)
	}
	return raw
}

// decodeJSONStrict 严格 JSON 解码（禁止未知字段，字段名漂移即报错）。
func decodeJSONStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("样本中存在多余 JSON 值")
	}
	return nil
}

// errCode 从错误中取 apperr 码。
func errCode(err error) apperr.Code {
	if ae, ok := err.(*apperr.Error); ok {
		return ae.Code
	}
	return ""
}

// contains 轻量包含判断（避免为测试引入 strings 依赖歧义）。
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
