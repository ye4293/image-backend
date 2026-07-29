package config

import "testing"

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
