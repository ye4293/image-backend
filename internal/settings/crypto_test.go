package settings

import (
	"encoding/base64"
	"strings"
	"testing"
)

// testKey 是一把合法的 32 字节密钥（base64）。
func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := ParseKey(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	k := testKey(t)
	ct, err := Encrypt(k, "sk_live_secret_value")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if strings.Contains(ct, "sk_live_secret_value") {
		t.Fatal("密文里出现了明文——加密没有真的发生")
	}
	pt, err := Decrypt(k, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if pt != "sk_live_secret_value" {
		t.Errorf("往返后不一致: got %q", pt)
	}
}

func TestEncryptUsesRandomNonce(t *testing.T) {
	// 同一明文两次加密必须得到不同密文。nonce 固定的话，相同的 key 在库里长得
	// 一模一样，攻击者能据此判断两个环境是否用了同一把上游 key。
	k := testKey(t)
	a, _ := Encrypt(k, "same")
	b, _ := Encrypt(k, "same")
	if a == b {
		t.Fatal("两次加密得到相同密文——nonce 没有随机化")
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	// GCM 的完整性保证：改一个字节就应当解不开，而不是解出乱码。
	k := testKey(t)
	ct, _ := Encrypt(k, "value")
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatalf("解 base64: %v", err)
	}
	raw[len(raw)-1] ^= 0x01
	if _, err := Decrypt(k, base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("密文被改动后必须解密失败")
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	ct, _ := Encrypt(testKey(t), "value")
	other, err := ParseKey(base64.StdEncoding.EncodeToString([]byte("ffffffffffffffffffffffffffffffff")))
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	if _, err := Decrypt(other, ct); err == nil {
		t.Fatal("换一把密钥必须解密失败")
	}
}

func TestParseKeyRejectsWrongLength(t *testing.T) {
	// 16 字节是合法 AES 密钥长度，但我们固定用 AES-256，混进来会让不同环境
	// 用不同强度而无人察觉。
	for _, bad := range []string{
		base64.StdEncoding.EncodeToString([]byte("short")),
		base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), // 16 字节
		"这不是 base64",
		"",
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("非法密钥 %q 应当被拒绝", bad)
		}
	}
}

func TestMaskKeepsTailAndHidesBody(t *testing.T) {
	got := Mask("sk_live_abcdefgh1234")
	if strings.Contains(got, "abcdefgh") {
		t.Errorf("掩码里泄露了中间部分: %q", got)
	}
	if !strings.HasSuffix(got, "1234") {
		t.Errorf("掩码要保留末四位以便辨认是哪一把: %q", got)
	}
}

func TestMaskShortValueRevealsNothing(t *testing.T) {
	// 短值不能"保留末四位"，否则等于把整个值露出来。
	for _, s := range []string{"", "a", "abcd", "abcde"} {
		got := Mask(s)
		if s != "" && strings.Contains(got, s) {
			t.Errorf("短值 %q 的掩码泄露了原值: %q", s, got)
		}
	}
}
