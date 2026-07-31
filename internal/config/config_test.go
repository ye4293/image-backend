package config

import (
	"strings"
	"testing"
)

func TestValidateStripeRejectsLiveKeyWithLocalhost(t *testing.T) {
	cfg := &Config{StripeSecretKey: "sk_live_x", AppBaseURL: "http://localhost:3000"}
	if err := cfg.ValidateStripe(); err == nil {
		t.Fatal("live key 配 localhost 必须拒绝启动：真实扣款后会跳到用户打不开的地址")
	}
}

func TestValidateStripeAllowsTestKeyWithLocalhost(t *testing.T) {
	cfg := &Config{StripeSecretKey: "sk_test_x", AppBaseURL: "http://localhost:3000"}
	if err := cfg.ValidateStripe(); err != nil {
		t.Fatalf("本地开发的常规组合不该被拒：%v", err)
	}
}

func TestValidateStripeAllowsEmptyKey(t *testing.T) {
	cfg := &Config{}
	if err := cfg.ValidateStripe(); err != nil {
		t.Fatalf("未配置 Stripe 时应放行（计费功能禁用，其余功能照常）：%v", err)
	}
}

func TestBillingEnabledRequiresBothSecrets(t *testing.T) {
	if (&Config{StripeSecretKey: "sk_test_x"}).BillingEnabled() {
		t.Error("只有 secret key、没有 webhook secret 时不算启用——收得到钱但发不出额度，比整个关掉更糟")
	}
	if !(&Config{StripeSecretKey: "sk_test_x", StripeWebhookSecret: "whsec_x"}).BillingEnabled() {
		t.Error("两个都有时应当启用")
	}
}

func TestStorageEnabledRequiresAllFive(t *testing.T) {
	full := func() *Config {
		return &Config{
			R2Endpoint:        "https://acct.r2.cloudflarestorage.com",
			R2AccessKeyID:     "ak",
			R2SecretAccessKey: "sk",
			R2Bucket:          "images",
			R2PublicBaseURL:   "https://img.example.com",
		}
	}
	if !full().StorageEnabled() {
		t.Fatal("五项齐全应当算已配置")
	}

	// 少任何一项都必须退化成未配置。半套配置比没配置更危险：它会让上传走到
	// 一半才失败，而失败点在生产才第一次出现。
	blanks := map[string]func(*Config){
		"R2_ENDPOINT":          func(c *Config) { c.R2Endpoint = "" },
		"R2_ACCESS_KEY_ID":     func(c *Config) { c.R2AccessKeyID = "" },
		"R2_SECRET_ACCESS_KEY": func(c *Config) { c.R2SecretAccessKey = "" },
		"R2_BUCKET":            func(c *Config) { c.R2Bucket = "" },
		"R2_PUBLIC_BASE_URL":   func(c *Config) { c.R2PublicBaseURL = "" },
	}
	for name, blank := range blanks {
		c := full()
		blank(c)
		if c.StorageEnabled() {
			t.Errorf("缺 %s 时不该算已配置", name)
		}
	}
}

func TestValidateStorageRejectsCredentialsWithoutPublicURL(t *testing.T) {
	// 这个组合不报错，只静默产出坏数据：少了公开域名就只能拿 S3 endpoint 拼 URL，
	// 而那个地址不允许匿名读——上传全部成功、stored=true、每张图在浏览器里 401。
	c := &Config{
		R2Endpoint:        "https://acct.r2.cloudflarestorage.com",
		R2AccessKeyID:     "ak",
		R2SecretAccessKey: "sk",
		R2Bucket:          "images",
	}
	err := c.ValidateStorage()
	if err == nil {
		t.Fatal("配了凭证但没配 R2_PUBLIC_BASE_URL 必须拒绝启动")
	}
	if !strings.Contains(err.Error(), "R2_PUBLIC_BASE_URL") {
		t.Errorf("错误信息要点名缺的是哪个变量，得到：%v", err)
	}
}

func TestValidateStorageAllowsFullyUnconfigured(t *testing.T) {
	// 完全没配 R2 是合法的本地开发状态，不能拦。
	if err := (&Config{}).ValidateStorage(); err != nil {
		t.Errorf("完全未配置不该报错：%v", err)
	}
}

func TestValidateStorageAllowsFullConfig(t *testing.T) {
	c := &Config{
		R2Endpoint:        "https://acct.r2.cloudflarestorage.com",
		R2AccessKeyID:     "ak",
		R2SecretAccessKey: "sk",
		R2Bucket:          "images",
		R2PublicBaseURL:   "https://img.example.com",
	}
	if err := c.ValidateStorage(); err != nil {
		t.Errorf("五项齐全不该报错：%v", err)
	}
}

// withPublicURL 五项齐全、只有公开域名可变的配置，用于单独考察 R2_PUBLIC_BASE_URL 的校验。
func withPublicURL(publicURL string) *Config {
	return &Config{
		R2Endpoint:        "https://acct.r2.cloudflarestorage.com",
		R2AccessKeyID:     "ak",
		R2SecretAccessKey: "sk",
		R2Bucket:          "images",
		R2PublicBaseURL:   publicURL,
	}
}

func TestValidateStorageRejectsS3EndpointAsPublicURL(t *testing.T) {
	// 把 S3 endpoint 粘进 R2_PUBLIC_BASE_URL 是最容易犯的错：两个变量长得像，
	// 而且配错之后上传全部成功、stored=true，只有浏览器里每张图 401。
	//
	// 后三个用例与 R2Endpoint **字符串并不相等**——若把校验写成 == R2Endpoint，
	// 它们会全部漏过，而它们坏得和第一个完全一样。
	cases := []string{
		"https://acct.r2.cloudflarestorage.com",
		"https://acct.r2.cloudflarestorage.com/",
		"https://acct.r2.cloudflarestorage.com/images",
		"https://other-acct.r2.cloudflarestorage.com",
	}
	for _, publicURL := range cases {
		err := withPublicURL(publicURL).ValidateStorage()
		if err == nil {
			t.Errorf("%q 是不允许匿名读的 S3 API 域名，必须拒绝启动", publicURL)
			continue
		}
		if !strings.Contains(err.Error(), "R2_PUBLIC_BASE_URL") {
			t.Errorf("%q 的错误信息要点名变量名，得到：%v", publicURL, err)
		}
	}
}

func TestValidateStorageRejectsPublicURLWithoutScheme(t *testing.T) {
	// 少了 scheme 的话拼出来的 "img.example.com/g/x.png" 会被浏览器当成**相对路径**，
	// 在每个页面上都指向不同的错地址。
	for _, publicURL := range []string{"img.example.com", "//img.example.com", "ftp://img.example.com"} {
		err := withPublicURL(publicURL).ValidateStorage()
		if err == nil {
			t.Errorf("%q 缺少 http/https scheme，必须拒绝启动", publicURL)
			continue
		}
		if !strings.Contains(err.Error(), "R2_PUBLIC_BASE_URL") {
			t.Errorf("%q 的错误信息要点名变量名，得到：%v", publicURL, err)
		}
	}
}

func TestValidateStorageAllowsLegitimatePublicDomains(t *testing.T) {
	// 防过度拦截。r2.dev 是 R2 官方的公开访问域名，与 r2.cloudflarestorage.com
	// 不是一回事，不能一起拒掉；末尾斜杠由下游 NewR2Storage 去掉，校验不该管。
	for _, publicURL := range []string{
		"https://img.example.com",
		"https://img.example.com/",
		"https://pub-abc123.r2.dev",
		"http://localhost:9000/images",
	} {
		if err := withPublicURL(publicURL).ValidateStorage(); err != nil {
			t.Errorf("%q 是合法的公开域名，不该报错：%v", publicURL, err)
		}
	}
}
