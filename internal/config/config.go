package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// DevDefaultJWTSecret 是 dev 模式的默认 JWT 密钥，非 dev 模式启动时会拒绝使用该值。
const DevDefaultJWTSecret = "dev-secret-change-me"

type Config struct {
	Port        string
	DatabaseURL string
	JWTSecret   string
	// EZLinkAIBaseURL 上游网关地址。可覆盖是为了让测试指向 httptest.Server。
	EZLinkAIBaseURL string
	// FluxAPIKey 为空时使用 stub adapter（见 internal/generation/stub.go）。
	FluxAPIKey string
	// BootstrapAdminEmail 启动时把该邮箱的用户提权为管理员（见 internal/bootstrap）。
	// 留空（默认）则完全不动。它**不创建用户**，只在用户已存在时改 role。
	BootstrapAdminEmail string
	// StripeSecretKey 为空时计费功能整体禁用，相关接口返回明确的"未配置"错误
	// 而不是 500——让没配 Stripe 的本地开发仍能跑其余功能。
	StripeSecretKey string
	// StripeWebhookSecret 由 `stripe listen` 或 Dashboard 的 endpoint 提供。
	// **本地与生产是两个不同的值**（按 endpoint 生成），混用的表现是验签一直失败。
	StripeWebhookSecret string
	// AppBaseURL 前端地址，用于拼 Checkout 的 success_url / cancel_url。
	AppBaseURL string
}

func Load() *Config {
	return &Config{
		Port:            getEnv("PORT", "8080"),
		DatabaseURL:     getEnv("DATABASE_URL", ""),
		JWTSecret:       getEnv("JWT_SECRET", DevDefaultJWTSecret),
		EZLinkAIBaseURL: getEnv("EZLINKAI_BASE_URL", "https://api.ezlinkai.com"),
		FluxAPIKey:      getEnv("FLUX_API_KEY", ""),

		BootstrapAdminEmail: getEnv("BOOTSTRAP_ADMIN_EMAIL", ""),

		StripeSecretKey:     getEnv("STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret: getEnv("STRIPE_WEBHOOK_SECRET", ""),
		AppBaseURL:          getEnv("APP_BASE_URL", "http://localhost:3000"),
	}
}

// BillingEnabled 计费功能是否可用。
//
// **两个 secret 都要有。** 只有 secret key 意味着能创建 Checkout、收得到钱，
// 却因为没有 webhook secret 而无法发放额度——用户付了钱拿不到东西，
// 这比整个功能关掉严重得多。
func (c *Config) BillingEnabled() bool {
	return c.StripeSecretKey != "" && c.StripeWebhookSecret != ""
}

// ValidateStripe 启动时的误配拦截。
func (c *Config) ValidateStripe() error {
	if c.StripeSecretKey == "" {
		return nil
	}
	if strings.HasPrefix(c.StripeSecretKey, "sk_live_") {
		u, err := url.Parse(c.AppBaseURL)
		if err != nil {
			return fmt.Errorf("APP_BASE_URL 解析失败：%w", err)
		}
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "" {
			// 这个组合几乎必然是误配，而后果是真实扣款后跳到用户打不开的地址。
			return fmt.Errorf(
				"检测到 live 模式密钥但 APP_BASE_URL 是 %q——真实扣款后用户会跳到打不开的地址；"+
					"本地开发请用 sk_test_ 开头的密钥", c.AppBaseURL)
		}
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
