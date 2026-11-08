package observ

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotationWriter 按大小轮转的 io.Writer（供 slog 使用）。
//
// 为什么自实现而不引第三方：slog 的 io.Writer 接口极窄（一个 Write），
// 为这点功能引入 lumberjack 会破坏本项目「单二进制、零额外直接依赖」的约束。
// 实现约 100 行，且行为完全可控。

// RotationOptions 轮转参数。
type RotationOptions struct {
	Path    string // 主文件路径（如 data/app.log）
	MaxMB   int    // 单文件上限（MB），超过即轮转
	MaxKeep int    // 保留份数（app.log.1 .. app.log.N）
}

// RotationWriter 并发安全的按大小轮转 writer。
//
// 文件命名：app.log（当前）→ app.log.1（次新）→ ... → app.log.N（最旧）。
// 轮转时：删除 app.log.N，其余依次后移，当前文件重命名为 app.log.1。
type RotationWriter struct {
	mu   sync.Mutex
	opt  RotationOptions
	file *os.File
	size int64
	// maxBytes 轮转阈值（字节）
	maxBytes int64
}

// NewRotationWriter 打开（或创建）主文件并返回 writer。
func NewRotationWriter(opt RotationOptions) (*RotationWriter, error) {
	if opt.Path == "" {
		return nil, fmt.Errorf("observ: 日志路径不能为空")
	}
	if opt.MaxMB <= 0 {
		opt.MaxMB = 32
	}
	if opt.MaxKeep <= 0 {
		opt.MaxKeep = 5
	}
	if err := os.MkdirAll(filepath.Dir(opt.Path), 0o755); err != nil {
		return nil, fmt.Errorf("observ: 创建日志目录失败: %w", err)
	}
	f, err := os.OpenFile(opt.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("observ: 打开日志文件 %s 失败: %w", opt.Path, err)
	}
	var size int64
	if fi, sErr := f.Stat(); sErr == nil {
		size = fi.Size()
	}
	return &RotationWriter{
		opt:      opt,
		file:     f,
		size:     size,
		maxBytes: int64(opt.MaxMB) << 20,
	}, nil
}

// Write 实现 io.Writer。写入前检查大小，超阈值先轮转。
func (w *RotationWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		// 已被 Close：丢弃写入而不是报错（关闭期间的尾部日志不值得让调用方崩）
		return len(p), nil
	}
	// 先判断「写完后是否会超阈值」：单条日志可能很大，按累计值判断更准确
	if w.size+int64(len(p)) > w.maxBytes && w.size > 0 {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked 执行轮转。调用方必须持锁。
func (w *RotationWriter) rotateLocked() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("observ: 关闭待轮转日志失败: %w", err)
	}
	// 删除最旧一份，其余依次后移
	oldest := fmt.Sprintf("%s.%d", w.opt.Path, w.opt.MaxKeep)
	_ = os.Remove(oldest) // 不存在不是错误
	for i := w.opt.MaxKeep - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.opt.Path, i)
		dst := fmt.Sprintf("%s.%d", w.opt.Path, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
	if err := os.Rename(w.opt.Path, w.opt.Path+".1"); err != nil {
		return fmt.Errorf("observ: 轮转日志失败: %w", err)
	}
	f, err := os.OpenFile(w.opt.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("observ: 重建日志文件失败: %w", err)
	}
	w.file = f
	w.size = 0
	return nil
}

// Close 关闭当前文件（幂等）。
func (w *RotationWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// CurrentSize 返回当前文件大小（测试与运维观测用）。
func (w *RotationWriter) CurrentSize() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}
