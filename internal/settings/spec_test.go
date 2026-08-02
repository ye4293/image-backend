package settings

import "testing"

func TestSpecsCoverExactlyTheIntendedKeys(t *testing.T) {
	// 白名单是这个包的安全边界：不在表里的 key 一律拒绝写入。多一项等于把一个
	// 没人校验的配置暴露给后台，少一项等于该项永远改不了。
	want := map[string]bool{
		"ezlinkaiBaseUrl":   false,
		"fluxApiKey":        true,
		"r2Endpoint":        false,
		"r2AccessKeyId":     true,
		"r2SecretAccessKey": true,
		"r2Bucket":          false,
		"r2PublicBaseUrl":   false,
		"appBaseUrl":        false,
	}
	if len(Specs) != len(want) {
		t.Fatalf("白名单有 %d 项，期望 %d 项", len(Specs), len(want))
	}
	for _, s := range Specs {
		secret, ok := want[s.Key]
		if !ok {
			t.Errorf("白名单里有意料之外的 key: %s", s.Key)
			continue
		}
		if s.Secret != secret {
			t.Errorf("%s 的 Secret 应当是 %v", s.Key, secret)
		}
	}
}

func TestLookupRejectsUnknownKey(t *testing.T) {
	// 静默接受未知 key 会让打错字的保存"成功"而什么都没生效。
	if _, ok := Lookup("nope"); ok {
		t.Error("未知 key 必须查不到")
	}
	if _, ok := Lookup("r2Bucket"); !ok {
		t.Error("已知 key 应当查得到")
	}
}

func TestValidateRejectsBadR2PublicBaseURL(t *testing.T) {
	// 与 config.ValidateStorage 同一套规则——那三种填法都会让每张图 401 或变成
	// 相对路径，而上传全部"成功"。
	for _, bad := range []string{
		"https://acct.r2.cloudflarestorage.com",
		"https://acct.r2.cloudflarestorage.com/",
		"https://acct.r2.cloudflarestorage.com/images",
		"img.example.com",
		"//img.example.com",
		"ftp://img.example.com",
	} {
		if err := Validate("r2PublicBaseUrl", bad); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
}

func TestValidateAllowsGoodR2PublicBaseURL(t *testing.T) {
	for _, good := range []string{
		"https://img.example.com",
		"https://img.example.com/",
		"https://pub-abc123.r2.dev",
		"", // 空表示清空，合法
	} {
		if err := Validate("r2PublicBaseUrl", good); err != nil {
			t.Errorf("%q 应当被接受，得到 %v", good, err)
		}
	}
}

func TestValidateRejectsNonHTTPAppBaseURL(t *testing.T) {
	for _, bad := range []string{"app.example.com", "ftp://x"} {
		if err := Validate("appBaseUrl", bad); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
	if err := Validate("appBaseUrl", "https://app.example.com"); err != nil {
		t.Errorf("合法值被拒: %v", err)
	}
}

func TestValidateUnknownKeyIsAnError(t *testing.T) {
	if err := Validate("nope", "x"); err == nil {
		t.Error("未知 key 的校验必须报错，而不是默认放行")
	}
}
