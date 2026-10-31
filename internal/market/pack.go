package market

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"harness-go/pkg/toolapi"
)

// BuildPackage 把「manifest + 本地二进制」组装为 pkg.zip 字节（发布方工具）。
//
// 用途：私有 registry 发布、测试 fixture（如把 cmd/fake-mcp 打包为本地工具包，
// 验证安装 → 子进程握手 → 调用 → 卸载的完整链路）。布局与 Parse 的期望严格互逆：
//
//	manifest.json / DIGESTS（覆盖 manifest + bin）/ SIGNATURE（priv 非 nil 时写入）
//	bin/<goos>_<goarch>/<binaryBase>
//
// priv 为 nil 时产出未签名包（用于验证「装未签名包被拒」的出口标准）。
func BuildPackage(m toolapi.Manifest, binPath string, priv ed25519.PrivateKey) ([]byte, error) {
	bin, err := os.ReadFile(binPath)
	if err != nil {
		return nil, fmt.Errorf("market: 读取二进制失败: %w", err)
	}
	if m.Kind != toolapi.KindPackage {
		return nil, fmt.Errorf("market: 打包的 manifest.kind 必须为 %q", toolapi.KindPackage)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("market: manifest 非法: %w", err)
	}
	manifestBytes, err := jsonMarshalIndent(m)
	if err != nil {
		return nil, fmt.Errorf("market: 序列化 manifest 失败: %w", err)
	}

	// 稳定命名：bin/<goos>_<goarch>/<源文件名>（.exe 后缀在 windows 目标上自动补齐）
	binBase := filepath.Base(binPath)
	if runtime.GOOS == "windows" && !hasSuffixFold(binBase, ".exe") {
		binBase += ".exe"
	}
	binName := BinaryDir + "/" + runtime.GOOS + "_" + runtime.GOARCH + "/" + binBase

	files := map[string][]byte{
		ManifestFile: manifestBytes,
		binName:      bin,
	}
	digests := BuildDigests(files)
	files[DigestsFile] = digests
	if priv != nil {
		files[SignatureFile] = []byte(Sign(priv, digests))
	}

	// 组 zip（条目按名稳定排序，保证同内容同包字节可复现）
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(files[n]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func hasSuffixFold(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	tail := s[len(s)-len(suffix):]
	for i := 0; i < len(suffix); i++ {
		a, b := lowerByte(tail[i]), lowerByte(suffix[i])
		if a != b {
			return false
		}
	}
	return true
}

func lowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}

// jsonMarshalIndent 缩进序列化（manifest 落包人类可读，便于审计）。
func jsonMarshalIndent(v any) ([]byte, error) {
	return jsonIndent(v)
}
