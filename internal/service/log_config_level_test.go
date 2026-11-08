package service

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"harness-go/internal/observ"
	"harness-go/internal/store"
)

// newLevelTestService 构造带真实 settings 表与 LevelVar 的日志配置用例。
func newLevelTestService(t *testing.T, defaultLevel string) (*LogConfigService, *slog.LevelVar) {
	t.Helper()
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })

	logger, lv := observ.NewLoggerWithLevel(defaultLevel, "text", io.Discard)
	_ = logger
	svc := NewLogConfigService(LogConfigOptions{
		Settings:     NewSettingsService(main),
		DefaultMode:  "preview",
		DefaultLevel: defaultLevel,
		LevelVar:     lv,
	})
	return svc, lv
}

// TestLogLevelHotReload 验证日志级别热调的核心语义：
//   - 合法值写入后 LevelVar 立即生效（无需重启），且 Level() 回读与写入一致（小写口径）
//   - 非法值必须被拒绝，且**库与运行时都不被污染**（不能静默兜底成 info）
//   - ApplyStoredLevel 能把上次保存的值恢复到新进程的 LevelVar（重启恢复）
func TestLogLevelHotReload(t *testing.T) {
	ctx := context.Background()
	svc, lv := newLevelTestService(t, "warn")

	// 初始：LevelVar 由 config 默认值初始化
	if got := svc.Level(ctx); got != "warn" {
		t.Fatalf("初始级别应为 warn，得到 %q", got)
	}
	if lv.Level() != slog.LevelWarn {
		t.Fatalf("初始 LevelVar 应为 WARN，得到 %v", lv.Level())
	}

	// ① 合法写入 info：运行时立刻生效
	if _, err := svc.SetLevel(ctx, "info"); err != nil {
		t.Fatalf("写入 info 失败: %v", err)
	}
	if lv.Level() != slog.LevelInfo {
		t.Fatalf("热调未生效：LevelVar 仍为 %v", lv.Level())
	}
	if got := svc.Level(ctx); got != "info" {
		t.Fatalf("Level() 应回读 info，得到 %q", got)
	}

	// ② 非法值：必须报错，且运行时与持久值都不变
	if _, err := svc.SetLevel(ctx, "verbose"); err == nil {
		t.Fatal("非法级别 verbose 应被拒绝")
	}
	if lv.Level() != slog.LevelInfo {
		t.Fatalf("非法写入污染了运行时级别: %v", lv.Level())
	}
	if got := svc.Level(ctx); got != "info" {
		t.Fatalf("非法写入污染了持久值，Level()=%q", got)
	}

	// ③ 重启恢复：新进程用 config 默认 warn 起，ApplyStoredLevel 应恢复为库里的 info
	svc2, lv2 := newLevelTestServiceWithStore(t, svc, "warn")
	_ = svc2
	if applied := svc2.ApplyStoredLevel(ctx); applied != "info" {
		t.Fatalf("重启恢复应得到 info，得到 %q", applied)
	}
	if lv2.Level() != slog.LevelInfo {
		t.Fatalf("重启恢复未作用于 LevelVar: %v", lv2.Level())
	}
}

// newLevelTestServiceWithStore 复用已有 svc 的库，构造第二个用例（模拟重启后的新进程）。
//
// 关键：新进程的 LevelVar 用 config 默认值初始化，必须靠 ApplyStoredLevel 才能恢复用户选择。
func newLevelTestServiceWithStore(t *testing.T, prev *LogConfigService, defaultLevel string) (*LogConfigService, *slog.LevelVar) {
	t.Helper()
	_, lv := observ.NewLoggerWithLevel(defaultLevel, "text", io.Discard)
	svc := NewLogConfigService(LogConfigOptions{
		Settings:     prev.settings,
		DefaultMode:  "preview",
		DefaultLevel: defaultLevel,
		LevelVar:     lv,
	})
	return svc, lv
}
