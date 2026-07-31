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

	// R2Endpoint Cloudflare R2 的 S3 兼容 endpoint，形如
	// https://<account_id>.r2.cloudflarestorage.com
	//
	// **存完整 endpoint 而不是 account id**：这样测试能把它指向
	// httptest.Server 或本地 minio，不必为了跑测试去连真的 R2。
	R2Endpoint        string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string
	// R2PublicBaseURL 绑在桶上的自定义域，形如 https://img.example.com。
	// 最终写进 generations.image_url 的 URL 由它拼出来。
	//
	// **不能用 R2Endpoint 代替**：S3 endpoint 不允许匿名读，拿它拼出来的 URL
	// 每一个都会 401。ValidateStorage 会拦这个误配。
	R2PublicBaseURL string
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

		R2Endpoint:        getEnv("R2_ENDPOINT", ""),
		R2AccessKeyID:     getEnv("R2_ACCESS_KEY_ID", ""),
		R2SecretAccessKey: getEnv("R2_SECRET_ACCESS_KEY", ""),
		R2Bucket:          getEnv("R2_BUCKET", ""),
		R2PublicBaseURL:   getEnv("R2_PUBLIC_BASE_URL", ""),
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

// StorageEnabled 图片转存是否可用。
//
// **五项必须齐全。** 半套配置比没配置更危险：它会让转存走到一半才失败，而那个
// 失败点只在生产才第一次出现。缺任何一项都退化成 NoopStorage——与 FluxAPIKey
// 为空退化成 stub、StripeSecretKey 为空禁用计费是同一个约定。
func (c *Config) StorageEnabled() bool {
	return c.R2Endpoint != "" && c.R2AccessKeyID != "" && c.R2SecretAccessKey != "" &&
		c.R2Bucket != "" && c.R2PublicBaseURL != ""
}

// ValidateStorage 启动时的误配拦截。
//
// 只拦一种组合：有凭证、没公开域名。这个组合**不报错，只静默产出坏数据**——
// 上传全部成功、库里 stored=true、而每张图在浏览器里 401，因为 URL 是拿不允许
// 匿名读的 S3 endpoint 拼出来的。等发现时已经攒了一批 URL 全错的记录，而它们
// 指向的对象是好的，得写脚本回头改。
//
// 完全未配置是合法的本地开发状态，不拦。
func (c *Config) ValidateStorage() error {
	hasCreds := c.R2Endpoint != "" || c.R2AccessKeyID != "" ||
		c.R2SecretAccessKey != "" || c.R2Bucket != ""
	if hasCreds && c.R2PublicBaseURL == "" {
		return fmt.Errorf(
			"检测到 R2 凭证但 R2_PUBLIC_BASE_URL 为空——上传会成功但每张图的 URL 都不可匿名访问；" +
				"请填绑在桶上的自定义域，如 https://img.example.com")
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
