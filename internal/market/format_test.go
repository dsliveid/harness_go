package market

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness-go/pkg/toolapi"
)

// mkManifest 构造合法包清单。
func mkManifest() toolapi.Manifest {
	return toolapi.Manifest{
		ID:              "demo-pkg",
		Name:            "演示包",
		Version:         "1.0.0",
		Kind:            toolapi.KindPackage,
		Description:     "fixture 包",
		DefaultTier:     toolapi.TierDeferred,
		ProtocolVersion: toolapi.ProtocolVersion,
		Tools: []toolapi.Spec{
			{Name: "echo", Description: "回显", Tier: toolapi.TierDeferred, Risk: toolapi.RiskReadonly},
		},
	}
}

// mkZip 用真实二进制内容打包（内容无所谓，只验证格式与签名链路）。
func mkZip(t *testing.T, m toolapi.Manifest, priv ed25519.PrivateKey) []byte {
	t.Helper()
	bin := []byte("#!/bin/sh\necho fake\n")
	if priv != nil {
		// 用签名私钥造一个确定性前缀，便于篡改测试
		bin = append([]byte("FAKE-BIN-"), bin...)
	}
	raw, err := BuildPackage(m, writeBin(t, bin), priv)
	if err != nil {
		t.Fatalf("BuildPackage 失败: %v", err)
	}
	return raw
}

func writeBin(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-bin.exe")
	if err := os.WriteFile(p, content, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func keypair(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// TestParseSignedRoundtrip 签名包完整链路：打包 → 解析 → signed。
func TestParseSignedRoundtrip(t *testing.T) {
	priv := keypair(t)
	raw := mkZip(t, mkManifest(), priv)
	p, err := Parse(raw, []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatalf("解析签名包失败: %v", err)
	}
	if p.Trust != TrustSigned {
		t.Fatalf("应为 signed，得到 %v", p.Trust)
	}
	if p.Manifest.ID != "demo-pkg" || len(p.Manifest.Tools) != 1 {
		t.Fatalf("manifest 还原错误: %+v", p.Manifest)
	}
	if p.SignatureHex() == "" {
		t.Fatalf("signed 包必须带签名")
	}
}

// TestParseUnsigned 无签名包 → unsigned（策略层决定拒装）。
func TestParseUnsigned(t *testing.T) {
	raw := mkZip(t, mkManifest(), nil)
	p, err := Parse(raw, nil)
	if err != nil {
		t.Fatalf("解析未签名包失败: %v", err)
	}
	if p.Trust != TrustUnsigned {
		t.Fatalf("应为 unsigned，得到 %v", p.Trust)
	}
}

// TestParseBadSignatureRejected 签名存在但校验失败 → 一律硬拒（防篡改优先于策略）。
func TestParseBadSignatureRejected(t *testing.T) {
	priv := keypair(t)
	other := keypair(t)
	raw := mkZip(t, mkManifest(), priv)
	if _, err := Parse(raw, []ed25519.PublicKey{other.Public().(ed25519.PublicKey)}); err == nil {
		t.Fatalf("非受信密钥的签名必须被拒")
	}
	// 受信密钥为空 → 签名包无法证明来源，同样硬拒
	if _, err := Parse(raw, nil); err == nil {
		t.Fatalf("无受信公钥时签名包必须被拒")
	}
}

// TestParseTamperedContentRejected 包内容被换 → 完整性失败硬拒。
func TestParseTamperedContentRejected(t *testing.T) {
	priv := keypair(t)
	raw := mkZip(t, mkManifest(), priv)
	// 篡改 zip 内二进制：重新组织 zip（DIGESTS 未更新 → 校验必失败）
	p, err := Parse(raw, []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatalf("原包应合法: %v", err)
	}
	_ = p
	// 重打包：内容微调但不重算 DIGESTS —— 直接改 DIGESTS 对应文件内容
	tampered := mutateZipFile(t, raw, func(name string, content []byte) []byte {
		if strings.Contains(name, "bin/") {
			return append(content, 'X')
		}
		return content
	})
	if _, err := Parse(tampered, []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)}); err == nil {
		t.Fatalf("篡改后的包必须被拒")
	}
}

// TestParseRejectsPathTraversal 路径穿越文件名必须被拒。
func TestParseRejectsPathTraversal(t *testing.T) {
	priv := keypair(t)
	_ = priv
	traversal := buildEvilZip(t, "../evil.txt")
	if _, err := Parse(traversal, []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)}); err == nil {
		t.Fatalf("路径穿越必须被拒")
	}
}

// TestExtract 落盘布局与幂等。
func TestExtract(t *testing.T) {
	priv := keypair(t)
	raw := mkZip(t, mkManifest(), priv)
	p, err := Parse(raw, []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	root, err := p.Extract(dest)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ManifestFile)); err != nil {
		t.Fatalf("manifest 未落盘")
	}
	// 幂等：重复解压同目录不报错
	if _, err := p.Extract(dest); err != nil {
		t.Fatalf("重复解压应幂等: %v", err)
	}
}

// TestParsePublicKeys 非法公钥显式报错（禁静默跳过）。
func TestParsePublicKeys(t *testing.T) {
	if _, err := ParsePublicKeys([]string{"zz-not-hex"}); err == nil {
		t.Fatalf("非法 hex 必须报错")
	}
	if _, err := ParsePublicKeys([]string{"aabb"}); err == nil {
		t.Fatalf("长度不对必须报错")
	}
	priv := keypair(t)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("公钥类型断言失败")
	}
	keys, err := ParsePublicKeys([]string{pubHex(pub)})
	if err != nil || len(keys) != 1 {
		t.Fatalf("合法公钥应通过: %v", err)
	}
}

func pubHex(k ed25519.PublicKey) string {
	return toHex(k)
}

// TestBinarySelection 平台二进制选择（唯一文件）。
func TestBinarySelection(t *testing.T) {
	priv := keypair(t)
	raw := mkZip(t, mkManifest(), priv)
	p, err := Parse(raw, []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	rel, content, err := p.Binary()
	if err != nil {
		t.Fatalf("选择二进制失败: %v", err)
	}
	if !strings.Contains(rel, "bin/") || len(content) == 0 {
		t.Fatalf("二进制选择错误: %q", rel)
	}
	// manifest 往返一致
	var back toolapi.Manifest
	if err := json.Unmarshal([]byte(readZip(t, raw)[ManifestFile]), &back); err != nil {
		t.Fatalf("manifest JSON 非法: %v", err)
	}
	if back.ID != "demo-pkg" {
		t.Fatalf("manifest 内容错误: %+v", back)
	}
}
