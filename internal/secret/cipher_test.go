package secret

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadOrCreatePersistsKey 校验密钥文件首次生成后可复用（重启后历史密文仍可解）。
func TestLoadOrCreatePersistsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "secret.key")

	first, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate(first) error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("密钥文件未生成: %v", err)
	}
	if info.Size() != KeySize {
		t.Fatalf("密钥长度应为 %d，实际 %d", KeySize, info.Size())
	}

	enc, err := first.Encrypt([]byte("sk-plain-secret"))
	if err != nil {
		t.Fatalf("Encrypt error: %v", err)
	}

	second, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate(second) error: %v", err)
	}
	got, err := second.Decrypt(enc)
	if err != nil {
		t.Fatalf("复用密钥解密失败: %v", err)
	}
	if string(got) != "sk-plain-secret" {
		t.Fatalf("解密结果不一致: %q", string(got))
	}
}

// TestLoadOrCreateRejectsCorruptedKeyFile 校验密钥文件损坏时不静默重建（否则历史密文全废）。
func TestLoadOrCreateRejectsCorruptedKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatalf("准备损坏密钥文件失败: %v", err)
	}

	if _, err := LoadOrCreate(path); err == nil {
		t.Fatal("损坏的密钥文件应返回错误，实际为 nil")
	} else if !strings.Contains(err.Error(), "长度非法") {
		t.Fatalf("错误信息应包含「长度非法」，实际: %v", err)
	}

	// 文件必须保持原样（未被覆盖重建）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if string(raw) != "short" {
		t.Fatalf("损坏的密钥文件被覆盖重建: %q", string(raw))
	}
}

// TestNewRejectsInvalidKeySize 校验密钥长度校验。
func TestNewRejectsInvalidKeySize(t *testing.T) {
	if _, err := New(make([]byte, KeySize-1)); err == nil {
		t.Fatal("长度非法的密钥应返回错误")
	}
	if _, err := New(make([]byte, KeySize)); err != nil {
		t.Fatalf("合法长度密钥构造失败: %v", err)
	}
}

// TestRoundTripAndNonceUniqueness 校验加解密往返、空值语义与随机 nonce。
func TestRoundTripAndNonceUniqueness(t *testing.T) {
	c, err := New(bytes.Repeat([]byte{0x2a}, KeySize))
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	if enc, err := c.Encrypt(nil); err != nil || enc != nil {
		t.Fatalf("空明文应返回 (nil, nil)，实际 (%v, %v)", enc, err)
	}
	if out, err := c.Decrypt(nil); err != nil || out != nil {
		t.Fatalf("空密文应返回 (nil, nil)，实际 (%v, %v)", out, err)
	}

	plain := []byte("https://api.openai.com/v1|sk-abc123")
	e1, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt error: %v", err)
	}
	e2, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt error: %v", err)
	}
	if bytes.Equal(e1, e2) {
		t.Fatal("同一明文两次加密结果相同，nonce 未随机化")
	}
	if bytes.Contains(e1, plain) {
		t.Fatal("密文中出现明文")
	}
	out, err := c.Decrypt(e1)
	if err != nil {
		t.Fatalf("Decrypt error: %v", err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("往返结果不一致: %q", string(out))
	}
}

// TestDecryptRejectsTamperedAndWrongKey 校验篡改密文/错误密钥必须报错而非返回脏数据。
func TestDecryptRejectsTamperedAndWrongKey(t *testing.T) {
	c, err := New(bytes.Repeat([]byte{0x11}, KeySize))
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	enc, err := c.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt error: %v", err)
	}

	tampered := append([]byte(nil), enc...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatal("篡改后的密文应解密失败")
	}
	if _, err := c.Decrypt(enc[:3]); err == nil {
		t.Fatal("长度不足的密文应解密失败")
	}

	other, err := New(bytes.Repeat([]byte{0x22}, KeySize))
	if err != nil {
		t.Fatalf("New(other) error: %v", err)
	}
	if _, err := other.Decrypt(enc); err == nil {
		t.Fatal("错误密钥应解密失败")
	}
}
