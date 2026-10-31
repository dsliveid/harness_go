package market

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"io"
	"testing"
)

// readZip 把 zip 字节读为 map（测试辅助）。
func readZip(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("读 zip 失败: %v", err)
	}
	out := make(map[string][]byte)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", f.Name, err)
		}
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f.Name, err)
		}
		out[f.Name] = content
	}
	return out
}

// mutateZipFile 重组 zip 并允许测试按名替换内容（篡改测试用）。
func mutateZipFile(t *testing.T, raw []byte, fn func(name string, content []byte) []byte) []byte {
	t.Helper()
	files := readZip(t, raw)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(fn(name, data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildEvilZip 构造含指定文件名的恶意包（路径穿越测试用）。
func buildEvilZip(t *testing.T, evilName string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(evilName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// toHex 编码十六进制。
func toHex(b []byte) string { return hex.EncodeToString(b) }
