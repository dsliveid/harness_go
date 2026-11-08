package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness-go/internal/config"
	"harness-go/internal/logvault"
	"harness-go/internal/store"
)

// writeLogFile 写一份日志文件（自动补行尾换行）。
func writeLogFile(t *testing.T, path string, lines []string) {
	t.Helper()
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("写日志文件失败: %v", err)
	}
}

// TestParseLevelStructuredField 结构化字段优先，正文里出现 ERROR 一词不得误判。
func TestParseLevelStructuredField(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			name: "text 格式取 level 字段",
			line: `time=2026-11-08T10:00:00.000+08:00 level=WARN msg="工具返回 ERROR 文本"`,
			want: "warn",
		},
		{
			name: "json 格式取 level 字段",
			line: `{"time":"2026-11-08T10:00:00.000+08:00","level":"DEBUG","msg":"x"}`,
			want: "debug",
		},
		{
			name: "正文含 ERROR 但级别是 info",
			line: `time=2026-11-08T10:00:00.000+08:00 level=INFO msg="grep 命中 ERROR"`,
			want: "info",
		},
		{
			name: "json 正文含 error 但级别是 info",
			line: `{"level":"INFO","msg":"stdout: Error: boom"}`,
			want: "info",
		},
		{
			name: "无结构化字段时回退子串匹配",
			line: `panic: something ERROR happened`,
			want: "error",
		},
		{
			name: "完全无线索默认 info",
			line: `plain line with nothing`,
			want: "info",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseLevel(c.line); got != c.want {
				t.Fatalf("parseLevel = %q, want %q", got, c.want)
			}
		})
	}
}

// TestLogQueryReverseEarlyStop 倒序读取：返回最新行、顺序为从新到旧。
func TestLogQueryReverseEarlyStop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf(`time=2026-11-08T10:00:%02d.000+08:00 level=INFO msg="line-%03d"`, i%60, i))
	}
	writeLogFile(t, path, lines)

	svc := NewLogService(path, 500)
	res, err := svc.Query(context.Background(), LogQuery{Limit: 5})
	if err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	if len(res.Items) != 5 {
		t.Fatalf("items = %d, want 5", len(res.Items))
	}
	// 最新一行必须在最前（line-100），整体从新到旧。
	if !strings.Contains(res.Items[0].Text, "line-100") {
		t.Fatalf("首条应为最新行 line-100, got %q", res.Items[0].Text)
	}
	if !strings.Contains(res.Items[4].Text, "line-096") {
		t.Fatalf("末条应为 line-096, got %q", res.Items[4].Text)
	}
	if !res.HasMore {
		t.Fatal("还有更早的行，HasMore 应为 true")
	}
	// 早停生效：只扫了很少的行，而不是全文件 100 行。
	if res.Total > 10 {
		t.Fatalf("倒序早停失效: Total=%d（应为个位数）", res.Total)
	}
}

// TestLogQueryFilters 覆盖 level / keyword / traceId / 时间范围 / offset。
func TestLogQueryFilters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeLogFile(t, path, []string{
		`time=2026-11-08T10:00:00.000+08:00 level=INFO msg="boot" trace_id=aaa111`,
		`time=2026-11-08T10:00:10.000+08:00 level=ERROR msg="boom" trace_id=bbb222`,
		`time=2026-11-08T10:00:20.000+08:00 level=WARN msg="slow" trace_id=aaa111`,
		`{"time":"2026-11-08T10:00:30.000+08:00","level":"ERROR","msg":"json boom","trace_id":"ccc333"}`,
	})
	svc := NewLogService(path, 500)
	ctx := context.Background()

	t.Run("level", func(t *testing.T) {
		res, err := svc.Query(ctx, LogQuery{Level: "error"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Items) != 2 {
			t.Fatalf("error 行数 = %d, want 2", len(res.Items))
		}
		if !strings.Contains(res.Items[0].Text, "json boom") {
			t.Fatalf("首条应为最新的 json boom, got %q", res.Items[0].Text)
		}
	})

	t.Run("traceId", func(t *testing.T) {
		res, err := svc.Query(ctx, LogQuery{TraceID: "aaa111"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Items) != 2 {
			t.Fatalf("traceId 命中 = %d, want 2", len(res.Items))
		}
	})

	t.Run("keyword 大小写不敏感", func(t *testing.T) {
		res, err := svc.Query(ctx, LogQuery{Keyword: "BOOM"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Items) != 2 {
			t.Fatalf("keyword 命中 = %d, want 2", len(res.Items))
		}
	})

	t.Run("时间范围", func(t *testing.T) {
		// 只取 10:00:15 ~ 10:00:25，应命中 WARN 那一行。
		from := mustMillis(t, "2026-11-08T10:00:15.000+08:00")
		to := mustMillis(t, "2026-11-08T10:00:25.000+08:00")
		res, err := svc.Query(ctx, LogQuery{From: from, To: to})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Items) != 1 || !strings.Contains(res.Items[0].Text, "slow") {
			t.Fatalf("时间范围命中异常: %+v", res.Items)
		}
	})

	t.Run("offset 翻页不重叠", func(t *testing.T) {
		p1, err := svc.Query(ctx, LogQuery{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		p2, err := svc.Query(ctx, LogQuery{Limit: 2, Offset: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(p1.Items) != 2 || len(p2.Items) != 2 {
			t.Fatalf("分页条数异常: p1=%d p2=%d", len(p1.Items), len(p2.Items))
		}
		if p1.Items[0].Text == p2.Items[0].Text {
			t.Fatal("offset 翻页返回了重复行")
		}
		if !strings.Contains(p1.Items[0].Text, "json boom") || !strings.Contains(p2.Items[0].Text, "boom") {
			t.Fatalf("分页顺序异常: p1[0]=%q p2[0]=%q", p1.Items[0].Text, p2.Items[0].Text)
		}
	})

	t.Run("时间解析失败的行在设置范围时被排除", func(t *testing.T) {
		writeLogFile(t, path, []string{
			`time=2026-11-08T10:00:00.000+08:00 level=INFO msg="has time"`,
			`panic: 没有时间戳的行`,
		})
		from := mustMillis(t, "2026-11-01T00:00:00.000+08:00")
		res, err := svc.Query(ctx, LogQuery{From: from})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Items) != 1 || !strings.Contains(res.Items[0].Text, "has time") {
			t.Fatalf("无时间戳行未被排除: %+v", res.Items)
		}
	})
}

// TestLogQueryRotationFiles 轮转文件 app.log.1 也要被读到（新文件不够时继续往前）。
func TestLogQueryRotationFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeLogFile(t, path, []string{`time=2026-11-08T12:00:00.000+08:00 level=INFO msg="newest"`})
	writeLogFile(t, path+".1", []string{`time=2026-11-08T11:00:00.000+08:00 level=INFO msg="older-1"`})
	writeLogFile(t, path+".2", []string{`time=2026-11-08T10:00:00.000+08:00 level=INFO msg="older-2"`})

	svc := NewLogService(path, 500)
	res, err := svc.Query(context.Background(), LogQuery{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 3 {
		t.Fatalf("跨轮转文件条数 = %d, want 3", len(res.Items))
	}
	want := []string{"newest", "older-1", "older-2"}
	for i, w := range want {
		if !strings.Contains(res.Items[i].Text, w) {
			t.Fatalf("第 %d 条应为 %s, got %q", i, w, res.Items[i].Text)
		}
	}
	// 三个文件全部读尽，无更旧文件 → HasMore 必须为 false（否则最后一页仍显示「下一页」）。
	if res.HasMore {
		t.Fatal("轮转文件已全部读尽，HasMore 应为 false")
	}

	// 只取 2 条时仍可能有下一页（older-2 未读）。
	res2, err := svc.Query(context.Background(), LogQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.HasMore {
		t.Fatal("还有 older-2 未读，HasMore 应为 true")
	}
}

// TestLogQueryNoTrailingNewlineAndCRLF 边界：末行无换行符、CRLF 行尾不得被拼错或带入 \r。
func TestLogQueryNoTrailingNewlineAndCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// 手工写入：前两行 CRLF，最后一行没有换行符。
	content := "time=2026-11-08T10:00:00.000+08:00 level=INFO msg=\"first\"\r\n" +
		"time=2026-11-08T10:00:01.000+08:00 level=INFO msg=\"second\"\r\n" +
		"time=2026-11-08T10:00:02.000+08:00 level=INFO msg=\"third-no-newline\""
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewLogService(path, 500)
	res, err := svc.Query(context.Background(), LogQuery{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 3 {
		t.Fatalf("items = %d, want 3: %+v", len(res.Items), res.Items)
	}
	wantTexts := []string{"third-no-newline", "second", "first"}
	for i, want := range wantTexts {
		if !strings.Contains(res.Items[i].Text, want) {
			t.Fatalf("第 %d 条应为 %s, got %q", i, want, res.Items[i].Text)
		}
		if strings.HasSuffix(res.Items[i].Text, "\r") {
			t.Fatalf("第 %d 条残留 \\r: %q", i, res.Items[i].Text)
		}
	}
	// 全部读尽，没有更早的行 → HasMore 必须为 false。
	if res.HasMore {
		t.Fatal("读尽全部行后 HasMore 应为 false")
	}
}

// TestLogQueryEmptyPath 未接入日志时返回空列表而非报错。
func TestLogQueryEmptyPath(t *testing.T) {
	svc := NewLogService("", 100)
	res, err := svc.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatalf("空路径不应报错: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("空路径 items = %d, want 0", len(res.Items))
	}
	// 旧 Tail 签名仍需可用（兼容 provider_handler 旧端点）。
	lines, err := svc.Tail(10, "")
	if err != nil || len(lines) != 0 {
		t.Fatalf("Tail 兼容失败: lines=%d err=%v", len(lines), err)
	}
}

// mustMillis 解析 RFC3339 为 Unix 毫秒（测试辅助）。
func mustMillis(t *testing.T, s string) int64 {
	t.Helper()
	ms, ok := parseLogTime("time=" + s)
	if !ok {
		t.Fatalf("解析时间失败: %s", s)
	}
	return ms
}

// TestLogConfigServiceSnapshotMode 档位读写：默认值、非法值 400、Refresh 生效。
func TestLogConfigServiceSnapshotMode(t *testing.T) {
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })

	settings := NewSettingsService(main)
	provider := logvault.NewModeProvider(config.SnapshotPreview)
	svc := NewLogConfigService(LogConfigOptions{
		Settings:             settings,
		Modes:                provider,
		DefaultMode:          config.SnapshotPreview,
		DefaultRetentionDays: 30,
	})

	// 键缺失 → config 兜底值。
	if got := svc.SnapshotMode(ctx); got != config.SnapshotPreview {
		t.Fatalf("缺失时应回落 preview, got %q", got)
	}

	// 合法写入 → 立即对写入路径生效。
	got, err := svc.SetSnapshotMode(ctx, config.SnapshotFull)
	if err != nil {
		t.Fatalf("SetSnapshotMode 失败: %v", err)
	}
	if got != config.SnapshotFull {
		t.Fatalf("返回生效值 = %q, want full", got)
	}
	if provider.SnapshotMode() != logvault.ModeFull {
		t.Fatalf("ModeProvider 未刷新: %q", provider.SnapshotMode())
	}
	if m := svc.SnapshotMode(ctx); m != config.SnapshotFull {
		t.Fatalf("回读档位 = %q, want full", m)
	}

	// 非法值必须 400，且不静默兜底、不污染已存值。
	if _, err := svc.SetSnapshotMode(ctx, "FULL"); err == nil {
		t.Fatal("非法档位应返回错误")
	}
	if m := svc.SnapshotMode(ctx); m != config.SnapshotFull {
		t.Fatalf("非法写入不应改动已存值, got %q", m)
	}
	// Patch 路径也要拦（白名单 + 值域校验双保险）。
	bad := map[string]json.RawMessage{"log.snapshot_mode": json.RawMessage(`"FULL"`)}
	if _, err := settings.Patch(ctx, bad); err == nil {
		t.Fatal("Patch 非法档位应返回错误")
	}
	good := map[string]json.RawMessage{"log.snapshot_mode": json.RawMessage(`"off"`)}
	if _, err := settings.Patch(ctx, good); err != nil {
		t.Fatalf("Patch 合法档位失败: %v", err)
	}
	if provider.SnapshotMode() != logvault.ModeFull {
		t.Fatal("Patch 不应绕过用例层直接改 ModeProvider（需由用例层 Set 才刷新）")
	}
}

// TestLogConfigServiceRetentionDays 保留天数读写与区间校验。
func TestLogConfigServiceRetentionDays(t *testing.T) {
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })

	settings := NewSettingsService(main)
	svc := NewLogConfigService(LogConfigOptions{
		Settings:             settings,
		DefaultMode:          config.SnapshotPreview,
		DefaultRetentionDays: 30,
	})

	if got := svc.RetentionDays(ctx); got != 30 {
		t.Fatalf("缺失时应回落 30, got %d", got)
	}
	if got, err := svc.SetRetentionDays(ctx, 7); err != nil || got != 7 {
		t.Fatalf("SetRetentionDays = %d, %v", got, err)
	}
	if got := svc.RetentionDays(ctx); got != 7 {
		t.Fatalf("回读 = %d, want 7", got)
	}
	for _, bad := range []int{0, -1, 366, 1000} {
		if _, err := svc.SetRetentionDays(ctx, bad); err == nil {
			t.Fatalf("天数 %d 应被拒绝", bad)
		}
	}
	if got := svc.RetentionDays(ctx); got != 7 {
		t.Fatalf("非法写入不应改动已存值, got %d", got)
	}
}
