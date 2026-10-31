// Package market 实现本地工具包（pkg.zip）的格式、签名校验与安装机制（阶段 E）。
//
// 包格式（docs/插件体系设计.md §3.5 替代落地方案）：
//
//	manifest.json   — toolapi.Manifest（kind=package，Tools 声明工具集，唯一一套格式）
//	DIGESTS         — "SHA256" 首行 + "<相对路径>:<hex>" 逐行（覆盖 manifest + 全部 bin 文件）
//	SIGNATURE       — ed25519 签名（hex，对 DIGESTS 文件原始字节）；缺省 = 未签名
//	bin/<goos>_<goarch>/<binary>[.exe] — 多平台可执行文件
//
// 信任分级（builtin / signed / unsigned）：
//   - builtin：编译期注册，不经本包；
//   - signed：SIGNATURE 对任一受信公钥校验通过；
//   - unsigned：无签名（默认安装被拒，须显式 allow_unsigned）；
//   - **签名存在但校验失败 → 一律硬拒**（防篡改优先于策略放行）。
//
// 工具包与 MCP Server 在实现上是同一套代码路径（子进程 + stdio JSON-RPC，
// 复用 internal/mcp 的 stdio 传输），只差「包从哪来」——本包只负责
// 「格式校验 + 签名 + 落盘」，连接与注册由 service 层经 internal/mcp 完成。
package market

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"harness-go/pkg/toolapi"
)

// Trust 信任级别。
type Trust string

const (
	// TrustSigned 签名校验通过。
	TrustSigned Trust = "signed"
	// TrustUnsigned 未签名（默认安装被拒，仅显式 allow_unsigned 放行）。
	TrustUnsigned Trust = "unsigned"
	// TrustBuiltin 编译期内置（不经本包，枚举占位保持三级完整）。
	TrustBuiltin Trust = "builtin"
)

// 包内固定文件名。
const (
	ManifestFile  = "manifest.json"
	DigestsFile   = "DIGESTS"
	SignatureFile = "SIGNATURE"
	// BinaryDir 二进制目录前缀（bin/<goos>_<goarch>/）。
	BinaryDir = "bin"
)

// 包格式错误（安装被拒的机器可判据）。
var (
	ErrUnsigned     = errors.New("market: 包未签名（信任策略拒绝安装，可配置 market.allow_unsigned 显式放行）")
	ErrBadSignature = errors.New("market: 包签名校验失败（内容可能被篡改，一律拒绝）")
	ErrBadDigest    = errors.New("market: 包内容与 DIGESTS 不一致（完整性校验失败）")
	ErrNoBinary     = errors.New("market: 包内没有当前平台的可执行文件")
)

// Package 打开的工具包（内存态，未落盘）。
type Package struct {
	Manifest toolapi.Manifest
	Trust    Trust
	// Digests 原始 DIGESTS 文件字节（签名对象，调试可展示）。
	Digests []byte
	// signatureHex SIGNATURE 内容（signed 时非空）。
	signatureHex string
	// files 包内全部文件（相对路径 → 内容）。
	files map[string][]byte
}

// Open 读取并校验工具包：清单解析 → 完整性（DIGESTS）→ 签名（ed25519）。
//
// trustedKeys 为空时任何签名都无法通过 → 带签名包一律硬拒（无法证明来源）；
// 调用方须显式配置受信公钥才能安装签名包。
func Open(zipPath string, trustedKeys []ed25519.PublicKey) (*Package, error) {
	raw, err := os.ReadFile(zipPath)
	if err != nil {
		return nil, fmt.Errorf("market: 读取包失败: %w", err)
	}
	return Parse(raw, trustedKeys)
}

// Parse 从字节解析工具包（Open 的核心，便于测试注入）。
func Parse(raw []byte, trustedKeys []ed25519.PublicKey) (*Package, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("market: 非法 zip: %w", err)
	}
	p := &Package{files: make(map[string][]byte)}
	for _, f := range zr.File {
		name := path.Clean(strings.TrimPrefix(filepath.ToSlash(f.Name), "./"))
		if strings.HasPrefix(name, "../") || strings.Contains(name, "/../") || path.IsAbs(name) {
			return nil, fmt.Errorf("market: 包内路径越界: %q（疑似路径穿越，拒绝）", f.Name)
		}
		rc, oerr := f.Open()
		if oerr != nil {
			return nil, fmt.Errorf("market: 读取包内文件 %s 失败: %w", name, oerr)
		}
		content, rerr := io.ReadAll(io.LimitReader(rc, 512<<20))
		_ = rc.Close()
		if rerr != nil {
			return nil, fmt.Errorf("market: 读取包内文件 %s 失败: %w", name, rerr)
		}
		p.files[name] = content
	}

	// ① manifest
	mb, ok := p.files[ManifestFile]
	if !ok {
		return nil, fmt.Errorf("market: 包缺少 %s", ManifestFile)
	}
	if err := jsonUnmarshalStrict(mb, &p.Manifest); err != nil {
		return nil, fmt.Errorf("market: manifest 解析失败: %w", err)
	}
	if p.Manifest.Kind != toolapi.KindPackage {
		return nil, fmt.Errorf("market: manifest.kind 必须为 %q，得到 %q", toolapi.KindPackage, p.Manifest.Kind)
	}
	if err := p.Manifest.Validate(); err != nil {
		return nil, fmt.Errorf("market: manifest 非法: %w", err)
	}

	// ② 当前平台二进制存在性
	if _, _, err := p.Binary(); err != nil {
		return nil, err
	}

	// ③ 完整性 + 签名（签名存在但无效 → 硬拒；无签名 → unsigned 交策略决定）
	sigBytes, hasSig := p.files[SignatureFile]
	if hasSig && len(bytes.TrimSpace(sigBytes)) > 0 {
		dg, hasDigests := p.files[DigestsFile]
		if !hasDigests {
			return nil, fmt.Errorf("market: 有 %s 但缺少 %s", SignatureFile, DigestsFile)
		}
		sig, derr := hex.DecodeString(strings.TrimSpace(string(sigBytes)))
		if derr != nil {
			return nil, fmt.Errorf("%w: 签名不是合法 hex", ErrBadSignature)
		}
		if !verifyAnyKey(trustedKeys, dg, sig) {
			return nil, ErrBadSignature
		}
		if verr := p.verifyDigests(dg); verr != nil {
			return nil, verr
		}
		p.Trust = TrustSigned
		p.Digests = dg
		p.signatureHex = strings.TrimSpace(string(sigBytes))
		return p, nil
	}
	// 无签名：仍校验 DIGESTS（如果作者提供了），未提供则跳过（完整性由安装时解压路径保证）
	if dg, ok := p.files[DigestsFile]; ok {
		if verr := p.verifyDigests(dg); verr != nil {
			return nil, verr
		}
		p.Digests = dg
	}
	p.Trust = TrustUnsigned
	return p, nil
}

// verifyDigests 逐行核对 DIGESTS 与实际内容（防清单内文件被换）。
func (p *Package) verifyDigests(dg []byte) error {
	lines := strings.Split(strings.TrimSpace(string(dg)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "SHA256" {
		return fmt.Errorf("market: %s 格式非法（首行应为 SHA256）", DigestsFile)
	}
	for _, ln := range lines[1:] {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		parts := strings.SplitN(ln, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("market: %s 行格式非法: %q", DigestsFile, ln)
		}
		name, want := parts[0], parts[1]
		content, ok := p.files[name]
		if !ok {
			return fmt.Errorf("%w: %s 在包内缺失", ErrBadDigest, name)
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != want {
			return fmt.Errorf("%w: %s 内容不匹配", ErrBadDigest, name)
		}
	}
	return nil
}

// Binary 返回当前平台可执行文件（相对路径 + 内容）。
// 约定：bin/<goos>_<goarch>/ 下唯一文件即目标；多文件时由 manifest.Meta["binary"] 指名。
func (p *Package) Binary() (rel string, content []byte, err error) {
	dir := BinaryDir + "/" + runtime.GOOS + "_" + runtime.GOARCH
	var candidates []string
	for name := range p.files {
		if strings.HasPrefix(name, dir+"/") && !strings.HasSuffix(name, "/") {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return "", nil, fmt.Errorf("%w（缺 %s）", ErrNoBinary, dir)
	}
	if len(candidates) == 1 {
		return candidates[0], p.files[candidates[0]], nil
	}
	if want, ok := p.Manifest.Meta["binary"].(string); ok {
		rel = path.Join(dir, path.Base(want))
		if c, ok := p.files[rel]; ok {
			return rel, c, nil
		}
	}
	return "", nil, fmt.Errorf("%w: %s 下有 %d 个文件且 manifest.Meta.binary 未指名", ErrNoBinary, dir, len(candidates))
}

// Extract 落盘到 <dest>/<id>/<version>/（安装目录约定），返回该目录。
//
// 幂等：已存在同版本目录先清理（半安装残留不叠加）。
func (p *Package) Extract(dest string) (string, error) {
	root := filepath.Join(dest, p.Manifest.ID, p.Manifest.Version)
	if err := os.RemoveAll(root); err != nil {
		return "", fmt.Errorf("market: 清理安装目录失败: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("market: 创建安装目录失败: %w", err)
	}
	for name, content := range p.files {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", fmt.Errorf("market: 创建目录失败: %w", err)
		}
		if err := os.WriteFile(target, content, 0o755); err != nil {
			return "", fmt.Errorf("market: 写入 %s 失败: %w", name, err)
		}
	}
	return root, nil
}

// SignatureHex 返回签名（hex；未签名为空）。
func (p *Package) SignatureHex() string { return p.signatureHex }

// verifyAnyKey 任一受信公钥校验通过即 signed。
func verifyAnyKey(keys []ed25519.PublicKey, msg, sig []byte) bool {
	if len(keys) == 0 || len(sig) != ed25519.SignatureSize {
		return false
	}
	for _, k := range keys {
		if ed25519.Verify(k, msg, sig) {
			return true
		}
	}
	return false
}

// ParsePublicKeys 解析 hex 公钥列表（配置项形态），非法键报错（禁静默跳过）。
func ParsePublicKeys(hexKeys []string) ([]ed25519.PublicKey, error) {
	out := make([]ed25519.PublicKey, 0, len(hexKeys))
	for i, h := range hexKeys {
		b, err := hex.DecodeString(strings.TrimSpace(h))
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("market: 受信公钥 #%d 非法（需 64 位 hex ed25519 公钥）", i)
		}
		out = append(out, ed25519.PublicKey(b))
	}
	return out, nil
}

// GenerateSigningKeypair 生成签名密钥对（发布方工具；返回 hex 公钥/私钥）。
func GenerateSigningKeypair() (pubHex, privHex string, err error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", err
	}
	return hex.EncodeToString(pub), hex.EncodeToString(priv), nil
}

// BuildDigests 为调用方（发布方工具/测试）构造 DIGESTS 内容。
func BuildDigests(files map[string][]byte) []byte {
	var b strings.Builder
	b.WriteString("SHA256\n")
	// 稳定排序：manifest 优先，其余按名升序
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sortStrings(names)
	for _, n := range names {
		sum := sha256.Sum256(files[n])
		fmt.Fprintf(&b, "%s:%s\n", n, hex.EncodeToString(sum[:]))
	}
	return []byte(b.String())
}

// Sign 用私钥对 DIGESTS 签名（发布方工具/测试）。
func Sign(priv ed25519.PrivateKey, digests []byte) string {
	return hex.EncodeToString(ed25519.Sign(priv, digests))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// jsonUnmarshalStrict 严格 JSON 解析（未知字段报错，防 manifest 拼写错误静默失效）。
func jsonUnmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("market: manifest 含多余的 JSON 内容")
	}
	return nil
}
