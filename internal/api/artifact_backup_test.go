package api

// 制品下载（R4）与数据导出/备份/恢复（R2）的传输层测试。
//
// 覆盖：正常路径 + 非法 id（含路径穿越）/不存在 → 400/404 + 空数据边界 + owner 权限。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/auth"
	"harness-go/internal/model"
	"harness-go/internal/observ"
	"harness-go/internal/service"
	"harness-go/internal/snapshot"
	"harness-go/internal/store"
)

// dataTestEnv 数据域（制品/导出/备份）测试装配体。
type dataTestEnv struct {
	srv      *httptest.Server
	token    string
	dir      string
	main     *store.MainStore
	sessions *store.SessionStore
	blobs    *snapshot.GlobalBlobs
	authSvc  *auth.Service
}

// newDataTestEnv 组装最小链路（store → auth → service → api），不启动引擎与设备域。
func newDataTestEnv(t *testing.T) *dataTestEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	sessions, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 4})
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	blobs, err := snapshot.NewGlobalBlobs(dir)
	if err != nil {
		t.Fatalf("初始化快照 CAS 失败: %v", err)
	}
	authSvc := auth.NewService(auth.Options{Main: main})
	if _, _, err := authSvc.EnsureBootstrapUser(ctx, "admin", "test-password-123"); err != nil {
		t.Fatalf("创建 owner 账号失败: %v", err)
	}
	plain, _, err := authSvc.EnsureBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}

	logger := observ.NewLogger("error", "text", io.Discard)
	router := NewRouter(Deps{
		Version:   "test",
		Log:       logger,
		Artifacts: service.NewArtifactService(service.ArtifactServiceOptions{Blobs: blobs}),
		Bundles: service.NewBundleService(service.BundleServiceOptions{
			Main: main, Sessions: sessions, DataDir: dir, WorkspaceRoot: t.TempDir(),
		}),
		Backups:     service.NewBackupService(service.BackupServiceOptions{DataDir: dir}),
		Auth:        authSvc,
		VerifyToken: authSvc.VerifyToken,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(func() {
		srv.Close()
		_ = sessions.Close()
		_ = main.Close()
	})
	return &dataTestEnv{srv: srv, token: plain, dir: dir, main: main, sessions: sessions, blobs: blobs, authSvc: authSvc}
}

// do 发起一次带令牌的请求，返回 (状态码, 响应头, body)。
func (e *dataTestEnv) do(t *testing.T, method, path, contentType string, body []byte, token string) (int, http.Header, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

// errCode 从统一错误体解析错误码。
func errCode(t *testing.T, raw []byte) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("错误体不是 JSON: %s", raw)
	}
	return body.Error.Code
}

// ---------- R4 制品下载 ----------

// TestDownloadArtifactOK 正常路径：CAS 内容按 sha256 寻址下载，带正确头。
func TestDownloadArtifactOK(t *testing.T) {
	env := newDataTestEnv(t)
	content := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("p", 32))
	hash, size, err := env.blobs.Put(context.Background(), content)
	if err != nil {
		t.Fatalf("写入 CAS 失败: %v", err)
	}

	status, hdr, body := env.do(t, http.MethodGet, "/api/v1/artifacts/"+hash, "", nil, env.token)
	if status != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", status, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type 应为 image/png，得到 %q", ct)
	}
	cd := hdr.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, ".png") || !strings.Contains(cd, hash[:16]) {
		t.Fatalf("Content-Disposition 异常: %q", cd)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("响应体与 CAS 内容不一致: %d/%d 字节", len(body), size)
	}
}

// TestDownloadArtifactEmptyContent 空数据边界：0 字节制品仍应 200 且响应体为空。
func TestDownloadArtifactEmptyContent(t *testing.T) {
	env := newDataTestEnv(t)
	hash, _, err := env.blobs.Put(context.Background(), []byte{})
	if err != nil {
		t.Fatalf("写入空内容失败: %v", err)
	}
	status, _, body := env.do(t, http.MethodGet, "/api/v1/artifacts/"+hash, "", nil, env.token)
	if status != http.StatusOK {
		t.Fatalf("空内容制品期望 200，得到 %d: %s", status, body)
	}
	if len(body) != 0 {
		t.Fatalf("空内容制品响应体应为空，得到 %d 字节", len(body))
	}
}

// TestDownloadArtifactInvalidID 非法 id（非 64 位 sha256 十六进制）→ 400。
func TestDownloadArtifactInvalidID(t *testing.T) {
	env := newDataTestEnv(t)
	for _, id := range []string{
		"not-a-hash",
		strings.Repeat("g", 64),                  // 非法十六进制字符
		strings.Repeat("a", 63),                  // 长度不足
		strings.Repeat("a", 65),                  // 超长
		"../../main.db",                          // 路径穿越
		"..%2F..%2Fmain.db",                      // URL 编码穿越
		strings.ToUpper(strings.Repeat("a", 64)), // 会被 lower 归一 → 合法格式（走 404 分支）
	} {
		status, _, body := env.do(t, http.MethodGet, "/api/v1/artifacts/"+id, "", nil, env.token)
		wantStatus := http.StatusBadRequest
		wantCode := string(apperr.CodeInvalidArgument)
		if strings.HasPrefix(id, "../../") || strings.Contains(id, "%2F") {
			// Go ServeMux 对含 ".." 的路径会先做路径清洗（301）或 404，均不放行到 handler
			if status != http.StatusBadRequest && status != http.StatusNotFound && status != http.StatusMovedPermanently {
				t.Fatalf("路径穿越 id %q 期望 400/404/301，得到 %d: %s", id, status, body)
			}
			continue
		}
		if id == strings.ToUpper(strings.Repeat("a", 64)) {
			wantStatus = http.StatusNotFound // 归一后格式合法但 CAS 无内容
			wantCode = string(apperr.CodeNotFound)
		}
		if status != wantStatus {
			t.Fatalf("id %q 期望 %d，得到 %d: %s", id, wantStatus, status, body)
		}
		if got := errCode(t, body); got != wantCode {
			t.Fatalf("id %q 期望错误码 %s，得到 %s", id, wantCode, got)
		}
	}
}

// TestDownloadArtifactNotFound 合法格式但 CAS 无内容 → 404。
func TestDownloadArtifactNotFound(t *testing.T) {
	env := newDataTestEnv(t)
	sum := sha256.Sum256([]byte("从未写入的内容"))
	missing := hex.EncodeToString(sum[:])
	status, _, body := env.do(t, http.MethodGet, "/api/v1/artifacts/"+missing, "", nil, env.token)
	if status != http.StatusNotFound {
		t.Fatalf("期望 404，得到 %d: %s", status, body)
	}
	if got := errCode(t, body); got != string(apperr.CodeNotFound) {
		t.Fatalf("期望 not_found，得到 %s", got)
	}
}

// TestDownloadArtifactRequiresToken 全局鉴权：匿名访问 → 401。
func TestDownloadArtifactRequiresToken(t *testing.T) {
	env := newDataTestEnv(t)
	status, _, body := env.do(t, http.MethodGet, "/api/v1/artifacts/"+strings.Repeat("a", 64), "", nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("匿名访问期望 401，得到 %d: %s", status, body)
	}
}

// TestArtifactServiceRejectsTraversal 服务层直接校验路径穿越（不依赖 mux 路径清洗）。
func TestArtifactServiceRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	blobs, err := snapshot.NewGlobalBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewArtifactService(service.ArtifactServiceOptions{Blobs: blobs})
	for _, id := range []string{
		"", ".", "..", "../main.db", `..\main.db`, "/etc/passwd",
		"C:/windows/win.ini", strings.Repeat("a", 64) + "/../../x",
	} {
		_, _, err := svc.Open(context.Background(), id)
		if err == nil {
			t.Fatalf("非法 id %q 应被拒绝", id)
		}
		ae := apperr.From(err)
		if ae == nil || ae.Code != apperr.CodeInvalidArgument {
			t.Fatalf("id %q 期望 invalid_argument，得到 %v", id, err)
		}
	}
}

// TestArtifactServiceUnavailable 未接线（Blobs 与 DataDir 均为空）→ unavailable。
func TestArtifactServiceUnavailable(t *testing.T) {
	svc := service.NewArtifactService(service.ArtifactServiceOptions{})
	_, _, err := svc.Open(context.Background(), strings.Repeat("a", 64))
	ae := apperr.From(err)
	if ae == nil || ae.Code != apperr.CodeUnavailable {
		t.Fatalf("期望 unavailable，得到 %v", err)
	}
}

// ---------- R2 会话导出 ----------

// seedSession 建会话 + 1 条消息 + 1 个 Run + 1 条工具事件（导出内容的三要素）。
func (e *dataTestEnv) seedSession(t *testing.T, sid string) {
	t.Helper()
	ctx := context.Background()
	sess := &model.Session{
		ID: sid, Title: "导出用例", CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := e.main.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	sdb, err := e.sessions.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("取会话库失败: %v", err)
	}
	if err := sdb.Runs().Create(ctx, &model.Run{ID: "run-1", Status: model.RunCompleted}); err != nil {
		t.Fatalf("建 Run 失败: %v", err)
	}
	if err := sdb.Messages().Append(ctx, &model.Message{
		ID: "m-1", RunID: "run-1", Role: model.RoleUser, Content: "你好",
	}); err != nil {
		t.Fatalf("追加消息失败: %v", err)
	}
	if err := sdb.ToolEvents().Insert(ctx, &model.ToolEvent{
		ID: "te-1", RunID: "run-1", ToolName: "write_file", Status: model.ToolOK,
		ArgsJSON: []byte(`{"path":"a.txt"}`),
	}); err != nil {
		t.Fatalf("插入工具事件失败: %v", err)
	}
}

// TestExportSessionZip 正常路径：200 + application/zip，包内含消息/tool_events/清单。
func TestExportSessionZip(t *testing.T) {
	env := newDataTestEnv(t)
	env.seedSession(t, "sess-export-1")

	status, hdr, body := env.do(t, http.MethodGet,
		"/api/v1/sessions/sess-export-1/export?format=zip", "", nil, env.token)
	if status != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", status, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type 应为 application/zip，得到 %q", ct)
	}
	if cd := hdr.Get("Content-Disposition"); !strings.Contains(cd, "attachment") ||
		!strings.Contains(cd, "sess-export-1") {
		t.Fatalf("Content-Disposition 异常: %q", cd)
	}
	names := zipEntryNames(t, body)
	for _, want := range []string{"manifest.json", "messages.jsonl", "tool_events.jsonl", "runs.json"} {
		if !names[want] {
			t.Fatalf("导出包缺少 %s，实际条目 %v", want, keys(names))
		}
	}
}

// TestExportSessionFormatRejected 非 zip 格式 → 400。
func TestExportSessionFormatRejected(t *testing.T) {
	env := newDataTestEnv(t)
	env.seedSession(t, "sess-export-2")
	status, _, body := env.do(t, http.MethodGet,
		"/api/v1/sessions/sess-export-2/export?format=tar", "", nil, env.token)
	if status != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d: %s", status, body)
	}
	if got := errCode(t, body); got != string(apperr.CodeInvalidArgument) {
		t.Fatalf("期望 invalid_argument，得到 %s", got)
	}
}

// TestExportSessionNotFound 未知会话 → 404。
func TestExportSessionNotFound(t *testing.T) {
	env := newDataTestEnv(t)
	status, _, body := env.do(t, http.MethodGet,
		"/api/v1/sessions/sess-does-not-exist/export?format=zip", "", nil, env.token)
	if status != http.StatusNotFound {
		t.Fatalf("期望 404，得到 %d: %s", status, body)
	}
}

// ---------- R2 全量备份 / 恢复 ----------

// TestBackupDataZip owner 备份：200 + zip，含 main.db 与 backup-manifest.json。
func TestBackupDataZip(t *testing.T) {
	env := newDataTestEnv(t)
	status, hdr, body := env.do(t, http.MethodGet, "/api/v1/data/backup", "", nil, env.token)
	if status != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", status, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type 应为 application/zip，得到 %q", ct)
	}
	names := zipEntryNames(t, body)
	if !names["main.db"] || !names[service.BackupManifestName] {
		t.Fatalf("备份包缺关键条目，实际 %v", keys(names))
	}
	// -wal / -shm / tmp/** 必须被跳过（文件级快照的显式取舍）
	for name := range names {
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") ||
			strings.HasPrefix(name, "tmp/") {
			t.Fatalf("备份包不应包含条目 %s", name)
		}
	}
	// 备份产物本身不得进包
	for name := range names {
		if strings.HasSuffix(name, ".part") {
			t.Fatalf("备份包不应包含临时文件 %s", name)
		}
	}
}

// TestBackupRequiresOwner 非 owner 账号 → 403（backup 与 restore 同策略）。
func TestBackupRequiresOwner(t *testing.T) {
	env := newDataTestEnv(t)
	ctx := context.Background()
	hash, err := auth.HashPassword("member-password-123", auth.Params{})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.main.Users().Create(ctx, &model.User{
		ID: "u-member", Username: "member", PasswordHash: hash, DisplayName: "member",
		Role: "member", CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("创建普通账号失败: %v", err)
	}
	plain, _, err := env.authSvc.IssueToken(ctx, "u-member", model.TokenKindAPI, "read")
	if err != nil {
		t.Fatalf("签发普通账号令牌失败: %v", err)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/data/backup"},
		{http.MethodPost, "/api/v1/data/restore"},
	} {
		status, _, body := env.do(t, tc.method, tc.path, "application/zip", []byte("x"), plain)
		if status != http.StatusForbidden {
			t.Fatalf("%s %s 非 owner 期望 403，得到 %d: %s", tc.method, tc.path, status, body)
		}
		if got := errCode(t, body); got != string(apperr.CodeForbidden) {
			t.Fatalf("%s 期望 forbidden，得到 %s", tc.path, got)
		}
	}
}

// TestRestoreRoundTrip 备份 → 恢复：200 + 暂存目录生成，且生产 data/ 内容不被覆盖。
func TestRestoreRoundTrip(t *testing.T) {
	env := newDataTestEnv(t)
	_, _, zipBody := env.do(t, http.MethodGet, "/api/v1/data/backup", "", nil, env.token)
	if len(zipBody) == 0 {
		t.Fatal("备份响应体为空")
	}
	before := fileHash(t, filepath.Join(env.dir, "main.db"))

	status, _, body := env.do(t, http.MethodPost, "/api/v1/data/restore",
		"application/zip", zipBody, env.token)
	if status != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", status, body)
	}
	var res service.RestoreResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("恢复响应不是 JSON: %s", body)
	}
	if res.StagingPath == "" || !res.RequiresRestart || res.Entries == 0 {
		t.Fatalf("恢复结果异常: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(res.StagingPath, "main.db")); err != nil {
		t.Fatalf("暂存目录缺少 main.db: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(res.StagingPath), "restore-staging-") {
		t.Fatalf("暂存目录名不符: %s", res.StagingPath)
	}
	// 生产数据必须原样（仅校验 + 暂存，不覆盖）
	if after := fileHash(t, filepath.Join(env.dir, "main.db")); after != before {
		t.Fatal("恢复不得修改运行中的 data/main.db")
	}
}

// TestRestoreInvalidZip 非法内容（非 zip / 空 / 缺清单 / 路径穿越）→ 400 且不落暂存。
func TestRestoreInvalidZip(t *testing.T) {
	env := newDataTestEnv(t)
	cases := map[string][]byte{
		"非 zip 字节":   []byte("this is not a zip file"),
		"空体":         nil,
		"zip 缺清单":    buildZip(t, map[string]string{"main.db": "x"}),
		"zip 缺 main": buildZip(t, map[string]string{"backup-manifest.json": `{"schema":1}`}),
		"清单 schema 越界": buildZip(t, map[string]string{
			"backup-manifest.json": `{"schema":99}`, "main.db": "x",
		}),
		"路径穿越条目": buildZip(t, map[string]string{
			"backup-manifest.json": `{"schema":1}`, "main.db": "x", "../evil.txt": "pwn",
		}),
	}
	for name, payload := range cases {
		stagingBefore := countStaging(t, env.dir)
		status, _, body := env.do(t, http.MethodPost, "/api/v1/data/restore",
			"application/zip", payload, env.token)
		if status != http.StatusBadRequest {
			t.Fatalf("%s 期望 400，得到 %d: %s", name, status, body)
		}
		if got := errCode(t, body); got != string(apperr.CodeInvalidArgument) {
			t.Fatalf("%s 期望 invalid_argument，得到 %s", name, got)
		}
		if after := countStaging(t, env.dir); after != stagingBefore {
			t.Fatalf("%s 失败时不应留下暂存目录（%d → %d）", name, stagingBefore, after)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(env.dir), "evil.txt")); err == nil {
			t.Fatalf("%s 穿越条目写出了 data 目录之外的文件", name)
		}
	}
}

// TestRestoreRejectsNonZipContentType 显式 JSON 请求体 → 400（避免误用）。
func TestRestoreRejectsNonZipContentType(t *testing.T) {
	env := newDataTestEnv(t)
	status, _, body := env.do(t, http.MethodPost, "/api/v1/data/restore",
		"application/json", []byte(`{"zip_path":"x"}`), env.token)
	if status != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d: %s", status, body)
	}
}

// TestRestoreRejectsMultipart 回归锁：前端曾误用 FormData(multipart) 上传备份包，
// 而后端只接受**原始 zip 字节流**（application/zip / application/octet-stream）。
// 该组合必然 400 —— 此测试锁死该边界，防止前端再次退回 multipart 却无人察觉。
func TestRestoreRejectsMultipart(t *testing.T) {
	env := newDataTestEnv(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "backup.zip")
	if err != nil {
		t.Fatalf("构造 multipart 失败: %v", err)
	}
	if _, err = fw.Write([]byte("PK\x03\x04fake")); err != nil {
		t.Fatalf("写入 multipart 内容失败: %v", err)
	}
	if err = mw.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}

	status, _, body := env.do(t, http.MethodPost, "/api/v1/data/restore",
		mw.FormDataContentType(), buf.Bytes(), env.token)
	if status != http.StatusBadRequest {
		t.Fatalf("multipart 上传必须被拒（后端只收原始 zip 字节流），期望 400，得到 %d: %s", status, body)
	}
	// 断言错误文案明确指出正确用法，便于前端开发者自查
	if !strings.Contains(string(body), "application/zip") {
		t.Fatalf("错误文案应提示正确的 Content-Type，实际: %s", body)
	}
}

// ---------- 测试辅助 ----------

func zipEntryNames(t *testing.T, raw []byte) map[string]bool {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("响应不是合法 zip: %v", err)
	}
	out := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		out[f.Name] = true
	}
	return out
}

func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("创建 zip 条目 %s 失败: %v", name, err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatalf("写入 zip 条目 %s 失败: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	return buf.Bytes()
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func countStaging(t *testing.T, dir string) int {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	n := 0
	for _, it := range items {
		if it.IsDir() && strings.HasPrefix(it.Name(), "restore-staging-") {
			n++
		}
	}
	return n
}
