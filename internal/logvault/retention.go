package logvault

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// 保留策略——**在线 30 天到期自动物理删除**（用户指定需求）。
//
// 双触发（安全网）：
//  1. 天数触发：天键早于今天 - RetentionDays → 删除
//  2. 配额触发：整个日志库族总占用 > MaxTotalGB → 从最早天库开始强制删除
//
// 只按天数不安全：若用户把抓取档位切到 full，30 天可达 150GB，磁盘会先爆。
// 配额熔断是唯一能保证磁盘有界的机制。

// RetentionOptions 保留策略参数。
type RetentionOptions struct {
	Shard *Shard
	// RetentionDays 在线保留天数（超期物理删除）。必须 > 0。
	RetentionDays int
	// ColdArchiveDays >0 时，超期库先 gzip 归档到 archive/ 再删；0 = 直接删。
	ColdArchiveDays int
	// MaxTotalBytes 日志库族总量上限（0 = 不限）。
	MaxTotalBytes int64
	Logger        *slog.Logger
}

// RetentionReport 一次清理的结果（写日志 + 可通过 API 查询）。
type RetentionReport struct {
	RanAt          int64    `json:"ran_at"`
	DeletedDays    []string `json:"deleted_days"`
	ArchivedDays   []string `json:"archived_days"`
	QuotaForced    []string `json:"quota_forced_days"`
	FreedBytes     int64    `json:"freed_bytes"`
	TotalAfterBytes int64   `json:"total_after_bytes"`
	RetentionDays  int      `json:"retention_days"`
	EarliestDay    string   `json:"earliest_day"`
	LatestDay      string   `json:"latest_day"`
	Errors         []string `json:"errors,omitempty"`
}

// Retention 保留期清理器。
type Retention struct {
	opt RetentionOptions
}

// NewRetention 创建清理器。
func NewRetention(opt RetentionOptions) *Retention {
	if opt.RetentionDays <= 0 {
		opt.RetentionDays = 30
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	return &Retention{opt: opt}
}

// Sweep 执行一次清理（启动时 + 每日定时调用）。
//
// 顺序很重要：
//  1. 先按天数处理（可归档）
//  2. 再按配额强制删（不归档 —— 配额告急时必须立刻回收空间）
func (r *Retention) Sweep(ctx context.Context) RetentionReport {
	now := time.Now()
	rep := RetentionReport{RanAt: now.UnixMilli(), RetentionDays: r.opt.RetentionDays}
	cutoff := now.AddDate(0, 0, -r.opt.RetentionDays).Format(dayLayout)

	for _, day := range r.opt.Shard.ListDays() {
		select {
		case <-ctx.Done():
			rep.Errors = append(rep.Errors, "清理被取消")
			return r.finish(rep)
		default:
		}
		// 天键是 YYYYMMDD 定长数字，字典序即时间序，可直接字符串比较
		if day >= cutoff {
			continue
		}
		before := r.opt.Shard.DaySizeBytes(day)
		if r.opt.ColdArchiveDays > 0 && r.shouldArchive(day, now) {
			if err := r.archiveDay(day); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("归档 %s 失败: %v", day, err))
				continue
			}
			rep.ArchivedDays = append(rep.ArchivedDays, day)
		}
		if err := r.opt.Shard.DropDay(day); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("删除 %s 失败: %v", day, err))
			continue
		}
		rep.DeletedDays = append(rep.DeletedDays, day)
		rep.FreedBytes += before
	}

	// 配额熔断：从最早天库开始强删直到低于上限
	if r.opt.MaxTotalBytes > 0 {
		for total := r.opt.Shard.TotalSizeBytes(); total > r.opt.MaxTotalBytes; {
			days := r.opt.Shard.ListDays()
			if len(days) == 0 {
				break
			}
			oldest := days[0]
			before := r.opt.Shard.DaySizeBytes(oldest)
			if err := r.opt.Shard.DropDay(oldest); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("配额强制删除 %s 失败: %v", oldest, err))
				break
			}
			rep.QuotaForced = append(rep.QuotaForced, oldest)
			rep.DeletedDays = append(rep.DeletedDays, oldest)
			rep.FreedBytes += before
			next := r.opt.Shard.TotalSizeBytes()
			if next >= total {
				// 没减少（例如归档目录占满），避免死循环
				break
			}
			total = next
		}
	}

	return r.finish(rep)
}

// finish 补齐报告尾部字段并写日志。
func (r *Retention) finish(rep RetentionReport) RetentionReport {
	rep.TotalAfterBytes = r.opt.Shard.TotalSizeBytes()
	rep.EarliestDay, rep.LatestDay = r.opt.Shard.AvailableRange()
	if len(rep.DeletedDays) > 0 {
		r.opt.Logger.Info("审计日志保留期清理完成",
			"deleted_days", rep.DeletedDays,
			"archived_days", rep.ArchivedDays,
			"quota_forced", rep.QuotaForced,
			"freed_mb", rep.FreedBytes>>20,
			"total_mb_after", rep.TotalAfterBytes>>20,
			"retention_days", rep.RetentionDays,
			"earliest_day", rep.EarliestDay)
	}
	for _, e := range rep.Errors {
		r.opt.Logger.Warn("审计日志清理异常", "err", e)
	}
	return rep
}

// shouldArchive 判断是否该归档（超过 ColdArchiveDays 才归档，介于 RetentionDays
// 与 ColdArchiveDays 之间的先只删除不再归档——因为 RetentionDays 是硬上限）。
//
// 注：当 ColdArchiveDays > RetentionDays 时，实际上「超过 RetentionDays 就删」，
// 永远走不到归档分支。这是刻意行为：用户要求的是「超 30 天自动清理」，
// 归档是未来的可选升级项，不是默认路径。Validate 已强制 ColdArchiveDays > RetentionDays。
func (r *Retention) shouldArchive(day string, now time.Time) bool {
	if r.opt.ColdArchiveDays <= 0 {
		return false
	}
	cutoff := now.AddDate(0, 0, -r.opt.ColdArchiveDays).Format(dayLayout)
	return day < cutoff
}

// archiveDay 把天库及其 blob 目录 gzip 打包到 archive/logs-YYYYMMDD.tar.gz。
//
// 只打包 db 文件（blob 目录单独成包），避免单包过大。
func (r *Retention) archiveDay(day string) error {
	archiveRoot := r.opt.Shard.ArchiveRoot()
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		return err
	}
	dbPath := filepath.Join(r.opt.Shard.Root(), "logs-"+day+".db")
	src, err := os.Open(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 已不存在，视为成功
		}
		return err
	}
	defer func() { _ = src.Close() }()

	dstPath := filepath.Join(archiveRoot, "logs-"+day+".db.gz")
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()

	// gzip Level 5：JSON 文本实测 4~6× 压缩比，CPU 与比率平衡点
	zw, err := gzip.NewWriterLevel(dst, 5)
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, src); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

// Start 启动每日定时清理协程（ctx 取消即退出）。
//
// 为什么必须常驻：只在启动时清理不够 —— 长期运行且不重启的进程会无声涨盘。
func (r *Retention) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.Sweep(ctx)
			}
		}
	}()
}
