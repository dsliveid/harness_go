// Package secret 提供落盘密文列的对称加密（AES-256-GCM）。
//
// 用途：`providers.api_key_enc` 等敏感列只存密文，密钥保存在 `data/secret.key`（0600），
// 不随数据库导出而泄漏。存储格式：nonce(12B) || ciphertext||tag。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// KeySize AES-256 密钥长度。
const KeySize = 32

// Cipher 对称加解密器（并发安全：AEAD 无内部状态）。
type Cipher struct {
	aead cipher.AEAD
}

// LoadOrCreate 从 path 读取密钥；文件不存在时生成并写入（0600）。
//
// 密钥文件损坏/长度不符一律返回错误（**不静默重建**，否则历史密文全部不可解）。
func LoadOrCreate(path string) (*Cipher, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(raw) != KeySize {
			return nil, fmt.Errorf("secret: 密钥文件长度非法（%d，应为 %d）", len(raw), KeySize)
		}
	case errors.Is(err, os.ErrNotExist):
		raw = make([]byte, KeySize)
		if _, rErr := io.ReadFull(rand.Reader, raw); rErr != nil {
			return nil, fmt.Errorf("secret: 生成密钥失败: %w", rErr)
		}
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
			return nil, fmt.Errorf("secret: 创建密钥目录失败: %w", mkErr)
		}
		if wErr := os.WriteFile(path, raw, 0o600); wErr != nil {
			return nil, fmt.Errorf("secret: 写入密钥文件失败: %w", wErr)
		}
	default:
		return nil, fmt.Errorf("secret: 读取密钥文件失败: %w", err)
	}
	return New(raw)
}

// New 用给定密钥构造加解密器。
func New(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secret: 密钥长度必须为 %d 字节", KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: 初始化 AES 失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: 初始化 GCM 失败: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt 加密明文；空明文返回 nil（表示「未设置」）。
func (c *Cipher) Encrypt(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secret: 生成 nonce 失败: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plain, nil), nil
}

// Decrypt 解密密文；空密文返回空明文。
func (c *Cipher) Decrypt(enc []byte) ([]byte, error) {
	if len(enc) == 0 {
		return nil, nil
	}
	n := c.aead.NonceSize()
	if len(enc) < n {
		return nil, errors.New("secret: 密文长度非法")
	}
	out, err := c.aead.Open(nil, enc[:n], enc[n:], nil)
	if err != nil {
		return nil, fmt.Errorf("secret: 解密失败（密钥不匹配或数据损坏）: %w", err)
	}
	return out, nil
}
