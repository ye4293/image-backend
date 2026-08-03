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
		// signupBonusCredits 非 secret：它是个数字，加密存放只会让后台读不回来
		// 自己填过什么，而它本身不是凭据。
		"signupBonusCredits": false,
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

func TestValidateSignupBonusCredits(t *testing.T) {
	// 这一项直接决定给每个新注册用户送出去多少真金白银，所以写入时就要拦死。
	// 主防线在这里而不是启动期：启动期的同类校验只降级为告警（见 runtime.go 的
	// Validate 注释），放过一个 100000 之后服务会带着它照常运行、一边送钱。
	for _, bad := range []string{"-1", "abc", "1.5", "1e3", "100000", " 5"} {
		if err := Validate("signupBonusCredits", bad); err == nil {
			t.Errorf("signupBonusCredits=%q 必须被拒", bad)
		}
	}
	// 防过度拦截：0（不赠送）、空串（清空）与合理值都必须放行。
	for _, ok := range []string{"", "0", "5", "10", "1000"} {
		if err := Validate("signupBonusCredits", ok); err != nil {
			t.Errorf("signupBonusCredits=%q 是合法值，不该报错：%v", ok, err)
		}
	}
}

func TestParseBonusFallsBackToZero(t *testing.T) {
	// 解析失败一律退化成 0（不赠送），而不是让 Reload 报错——Reload 失败会让整个
	// 配置停留在旧版本，于是同一次保存里其他项（比如刚换的上游 key）也一起不生效。
	//
	// 上限在这里也兜一道：Validate 是主防线，但手工改库能绕过它，而这一项对应真钱。
	for _, raw := range []string{"", "abc", "-1", "1.5", "999999"} {
		if got := parseBonus(raw); got != 0 {
			t.Errorf("parseBonus(%q) 应当是 0，得到 %d", raw, got)
		}
	}
	if got := parseBonus("8"); got != 8 {
		t.Errorf("parseBonus(\"8\") 应当是 8，得到 %d", got)
	}
	// 带空白的合法数字应当被接受：env 或后台输入框里带一个尾随空格是常见的，
	// 而"因为一个空格就静默不送额度"是个很难查的表现。
	if got := parseBonus(" 6 "); got != 6 {
		t.Errorf("parseBonus(\" 6 \") 应当是 6，得到 %d", got)
	}
}
