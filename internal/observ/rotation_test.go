package observ

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 轮转是「日志无上限增长」的唯一止损手段，必须验证：
//  1. 超过阈值真的切文件；
//  2. 保留份数不超过 MaxKeep（否则磁盘还是会被吃满）；
//  3. 最新内容留在主文件，历史按 .1 → .N 顺序归档。
func TestRotationWriterRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// 阈值 1MB（RotationOptions 以 MB 为单位，取 1 便于构造超限写入）
	w, err := NewRotationWriter(RotationOptions{Path: path, MaxMB: 1, MaxKeep: 3})
	if err != nil {
		t.Fatalf("创建轮转 writer 失败: %v", err)
	}
	defer func() { _ = w.Close() }()

	// 每行约 1KB，写 6000 行 ≈ 6MB → 至少轮转 5 次（保证 .1/.2/.3 都被填满）
	line := make([]byte, 1024)
	for i := range line {
		line[i] = 'a'
	}
	for i := 0; i < 6000; i++ {
		if _, err := fmt.Fprintf(w, "%04d %s\n", i, line); err != nil {
			t.Fatalf("写入失败（第 %d 行）: %v", i, err)
		}
	}

	// 主文件必须存在且不超过阈值太多（允许单条超阈值的一条余量）
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("主文件不存在: %v", err)
	}
	if fi.Size() > 2<<20 {
		t.Fatalf("主文件 %d 字节，超过阈值过多（轮转未生效）", fi.Size())
	}

	// 必须产生 .1
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("期望存在 app.log.1，得到 %v", err)
	}

	// 保留份数不得超过 MaxKeep：.4 必须不存在（MaxKeep=3）
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf("app.log.4 不应存在（MaxKeep=3），err=%v", err)
	}
	if _, err := os.Stat(path + ".3"); err != nil {
		t.Fatalf("期望存在 app.log.3（MaxKeep=3）: %v", err)
	}
}

// Close 之后写入必须安全丢弃而不是 panic（关闭期间的尾部日志不值得让调用方崩）。
func TestRotationWriterWriteAfterClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := NewRotationWriter(RotationOptions{Path: path, MaxMB: 1, MaxKeep: 2})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	n, wErr := w.Write([]byte("after close\n"))
	if wErr != nil {
		t.Fatalf("关闭后写入不应报错，得到 %v", wErr)
	}
	if n != len("after close\n") {
		t.Fatalf("关闭后写入应返回 len(p)，得到 %d", n)
	}
	// 幂等关闭
	if err := w.Close(); err != nil {
		t.Fatalf("重复关闭应幂等，得到 %v", err)
	}
}

// 保证阈值/保留份数的默认值兜底有效（<=0 时回落 32MB / 5 份）。
func TestRotationWriterDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := NewRotationWriter(RotationOptions{Path: path, MaxMB: 0, MaxKeep: 0})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	defer func() { _ = w.Close() }()
	if w.maxBytes != 32<<20 {
		t.Fatalf("MaxMB<=0 应回落 32MB，得到 %d", w.maxBytes)
	}
	if w.opt.MaxKeep != 5 {
		t.Fatalf("MaxKeep<=0 应回落 5 份，得到 %d", w.opt.MaxKeep)
	}
}
