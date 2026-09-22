// Package modelsecret 保管企业自配模型接口的 API Key。
//
// 设计约束（与仓库其他安全边界一致）：
//
//   - Key 永不返回给调用方。落盘的是密文，接口只回一个用于展示的尾缀提示。
//   - 主密钥只来自环境变量，不落库、不写日志、不出现在任何响应里。
//   - 未配置主密钥时**整体失败关闭**：不加密、不保存、不返回明文，
//     而不是退化成明文存储——那会让"已加密"变成一句假话。
//   - 密文带版本前缀，便于将来轮换算法时识别旧数据。
package modelsecret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// 密文前缀。带版本号是为了轮换：将来换成 AEAD 之外的算法时，
// 旧数据能被明确识别成"无法解密"，而不是被当成损坏数据静默丢弃。
const ciphertextPrefix = "v1:"

var (
	// ErrNotConfigured 未配置主密钥。调用方必须把它当成"功能不可用"，
	// 而不是"保存一个空 Key"。
	ErrNotConfigured = errors.New("模型密钥主密钥未配置")
	// ErrInvalidCiphertext 密文无法解析或认证失败（被篡改、密钥不匹配、格式错误）。
	ErrInvalidCiphertext = errors.New("模型密钥密文无效")
)

// Box 用主密钥封装 API Key。
type Box struct {
	aead cipher.AEAD
}

// New 用主密钥构造封装器。masterKey 为空时返回 ErrNotConfigured。
//
// 主密钥经 SHA-256 归一到 32 字节：允许运维填写任意长度的随机串，
// 不必手工数够 32 字节（数错会静默降低强度，而归一是确定的）。
func New(masterKey string) (*Box, error) {
	trimmed := strings.TrimSpace(masterKey)
	if trimmed == "" {
		return nil, ErrNotConfigured
	}
	digest := sha256.Sum256([]byte(trimmed))
	block, err := aes.NewCipher(digest[:])
	if err != nil {
		return nil, fmt.Errorf("初始化密钥封装失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 AEAD 失败: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Encrypt 返回可安全落盘的密文。每次调用使用新的 nonce，
// 因此同一明文两次加密得到不同密文——这是 AEAD 的正常行为，
// 也意味着不能用密文相等来判断"Key 是否变化"。
func (b *Box) Encrypt(plaintext string) (string, error) {
	if b == nil || b.aead == nil {
		return "", ErrNotConfigured
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成 nonce 失败: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return ciphertextPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt 还原明文。输入不是本包产生的密文时返回 ErrInvalidCiphertext，
// 调用方必须把它当作"密钥不可用"处理，不能退化成空字符串继续调用模型。
func (b *Box) Decrypt(ciphertext string) (string, error) {
	if b == nil || b.aead == nil {
		return "", ErrNotConfigured
	}
	if !strings.HasPrefix(ciphertext, ciphertextPrefix) {
		return "", ErrInvalidCiphertext
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(ciphertext, ciphertextPrefix))
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	if len(raw) < b.aead.NonceSize() {
		return "", ErrInvalidCiphertext
	}
	nonce, sealed := raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():]
	plaintext, err := b.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	return string(plaintext), nil
}

// Hint 从明文 Key 生成一个**不泄露内容**的展示提示。
//
// 只保留首尾各几个字符：足够运维在多个 Key 之间区分，又不足以重放。
// 过短的 Key 只显示长度，避免"几乎整个 Key 都显示出来了"。
func Hint(plaintext string) string {
	trimmed := strings.TrimSpace(plaintext)
	if trimmed == "" {
		return ""
	}
	runes := []rune(trimmed)
	if len(runes) <= 8 {
		return fmt.Sprintf("已保存（%d 字符）", len(runes))
	}
	head := string(runes[:3])
	tail := string(runes[len(runes)-4:])
	return fmt.Sprintf("已保存 %s…%s", head, tail)
}
