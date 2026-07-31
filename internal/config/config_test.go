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
