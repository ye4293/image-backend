// Package settings 管理运营可在后台修改的配置项。
//
// secret 类配置在库里以密文存放：明文写库意味着它们会出现在 pg_dump 备份、
// 任何有库读权限的人手里、被拖库时，以及排查问题时随手 SELECT * 的终端历史里。
package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// keyLen 固定 32 字节（AES-256）。
//
// 不接受 16/24：AES-128 也是合法的，但允许多种长度会让不同环境在无人察觉的
// 情况下用上不同强度。
const keyLen = 32

// ParseKey 解析 base64 编码的主密钥。
func ParseKey(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, errors.New("CONFIG_ENCRYPTION_KEY 为空")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("CONFIG_ENCRYPTION_KEY 不是合法 base64: %w", err)
	}
	if len(raw) != keyLen {
		return nil, fmt.Errorf(
			"CONFIG_ENCRYPTION_KEY 解码后是 %d 字节，必须是 %d 字节（AES-256）；"+
				"生成：openssl rand -base64 32", len(raw), keyLen)
	}
	return raw, nil
}

// Encrypt 返回 base64(nonce || ciphertext)。
func Encrypt(key []byte, plaintext string) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成 nonce: %w", err)
	}
	// nonce 前置拼在密文前：GCM 解密需要它，而它不必保密。**每次加密都要新的
	// nonce**——固定 nonce 会让同一明文产生同一密文，于是能从库里看出两个环境
	// 是否用了同一把上游 key。
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func Decrypt(key []byte, encoded string) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("密文长度不足，无法取出 nonce")
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	// GCM 会一并校验完整性：密文被改过一个字节就在这里失败，而不是解出乱码。
	pt, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥不对或密文被改动）: %w", err)
	}
	return string(pt), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("密钥必须是 %d 字节", keyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("构造 AES: %w", err)
	}
	return cipher.NewGCM(block)
}

// Mask 把 secret 变成可以安全回传给前端的形式。
//
// 保留末四位是为了让管理员能辨认"是不是我刚填的那一把"，而不必把值读出来。
// **短值一律全遮**：不到 8 位时"保留末四位"等于把大半个值露出去。
func Mask(s string) string {
	const keep = 4
	if len(s) < keep*2 {
		if s == "" {
			return ""
		}
		return strings.Repeat("•", 8)
	}
	return strings.Repeat("•", 8) + s[len(s)-keep:]
}
