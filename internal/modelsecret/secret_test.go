package modelsecret

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	box, err := New("unit-test-master-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const plaintext = "sk-test-abcdef123456"
	ciphertext, err := box.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// 密文不得包含明文：这是整个设计的底线。
	if strings.Contains(ciphertext, plaintext) {
		t.Fatalf("ciphertext contains the plaintext: %s", ciphertext)
	}
	got, err := box.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("round trip mismatch: got %q want %q", got, plaintext)
	}
}

// 同一明文两次加密必须得到不同密文（随机 nonce）。
// 若这条不成立，旁观者能通过密文相等判断"两个企业用了同一个 Key"。
func TestEncryptIsNonDeterministic(t *testing.T) {
	box, _ := New("unit-test-master-key")
	first, err := box.Encrypt("sk-same")
	if err != nil {
		t.Fatalf("Encrypt first: %v", err)
	}
	second, err := box.Encrypt("sk-same")
	if err != nil {
		t.Fatalf("Encrypt second: %v", err)
	}
	if first == second {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext")
	}
}

func TestNewWithoutMasterKeyFailsClosed(t *testing.T) {
	if _, err := New(""); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
	if _, err := New("   "); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured for whitespace key, got %v", err)
	}
}

// 密文被改一个字符就必须解不开：AEAD 的认证意义就在这里。
func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	box, _ := New("unit-test-master-key")
	ciphertext, err := box.Encrypt("sk-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	flipped := flipSealedByte(ciphertext)
	if flipped == ciphertext {
		t.Skip("could not construct a distinct ciphertext")
	}
	if _, err := box.Decrypt(flipped); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("expected ErrInvalidCiphertext, got %v", err)
	}
}

// 换了主密钥后旧密文必须明确解不开，而不是返回空字符串——
// 后者会让"配过 Key"静默变成"没有 Key"。
func TestDecryptRejectsWrongMasterKey(t *testing.T) {
	box, _ := New("master-key-one")
	ciphertext, err := box.Encrypt("sk-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	other, _ := New("master-key-two")
	if _, err := other.Decrypt(ciphertext); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("expected ErrInvalidCiphertext across master keys, got %v", err)
	}
}

func TestDecryptRejectsForeignInput(t *testing.T) {
	box, _ := New("unit-test-master-key")
	for _, input := range []string{"", "plain-text", "v1:not-base64!!", "v2:abcd"} {
		if _, err := box.Decrypt(input); !errors.Is(err, ErrInvalidCiphertext) {
			t.Fatalf("Decrypt(%q) expected ErrInvalidCiphertext, got %v", input, err)
		}
	}
}

func TestHintNeverExposesTheWholeKey(t *testing.T) {
	const long = "sk-proj-0123456789abcdefXYZ"
	hint := Hint(long)
	if hint == "" {
		t.Fatal("empty hint for a non-empty key")
	}
	if strings.Contains(hint, long) {
		t.Fatalf("hint contains the whole key: %s", hint)
	}
	// 必须保留可区分的前缀，否则多个 Key 之间无法辨认。
	if !strings.Contains(hint, "sk-") {
		t.Fatalf("hint lost the distinguishing prefix: %s", hint)
	}
	// 短 Key 只给长度：显示几乎整个 Key 等于泄露。
	if got := Hint("abc"); got != "已保存（3 字符）" {
		t.Fatalf("short key hint = %q", got)
	}
	if Hint("  ") != "" {
		t.Fatal("whitespace key should produce an empty hint")
	}
}

// flipSealedByte 篡改密文主体里的一个字节后重新编码。
//
// 刻意不改 base64 的最后一个字符：RawStdEncoding 的末尾字符只携带少量有效位，
// 改动它常常解码出完全相同的字节，那样测的是"没改动"而不是"改动被识别"。
func flipSealedByte(value string) string {
	trimmed := strings.TrimPrefix(value, ciphertextPrefix)
	raw, err := base64.RawStdEncoding.DecodeString(trimmed)
	if err != nil || len(raw) < 2 {
		return value
	}
	// 取中间位置的一个字节取反：既避开 nonce 头部，也确保落在密文/标签区。
	index := len(raw) / 2
	raw[index] ^= 0xff
	return ciphertextPrefix + base64.RawStdEncoding.EncodeToString(raw)
}
