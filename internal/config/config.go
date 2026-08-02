package config

import (
	"errors"
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

	// ConfigEncryptionKey 加密 app_settings 里 secret 项的主密钥（base64 的 32 字节）。
	//
	// 这一项**不能**搬进后台——它正是用来解开后台里那些 secret 的。
	// 生成：openssl rand -base64 32
	ConfigEncryptionKey string

	// CORSAllowedOrigins 允许跨域直连本后端的前端来源，逗号分隔。
	//
	// 形如 "https://moloom.ai,https://*-myteam.vercel.app"。留空表示**不发任何
	// CORS 头**——同源部署（nginx 同时服务前端与 /api）或前端走服务端代理时，
	// 那才是正确状态，所以空值不算配置错误。
	//
	// **刻意留在环境变量、不进后台设置页。** 它是安全边界：做成管理员可改，等于
	// 给后台一个把 origin 改成 * 的开关。而且它要在启动期硬拦截，库里的值只能
	// 降级成告警（见 internal/settings/runtime.go 对"拒绝启动会让服务死锁"的说明）。
	//
	// 也**不复用已在库里的 appBaseUrl**：那一项是"拼 Stripe 跳转用的前端地址"，
	// 单值；CORS 要多值（生产域名 + preview 域名）。
	CORSAllowedOrigins string
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

		ConfigEncryptionKey: getEnv("CONFIG_ENCRYPTION_KEY", ""),

		CORSAllowedOrigins: getEnv("CORS_ALLOWED_ORIGINS", ""),
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
// 拦三种组合，它们的共同点是**都不报错，只静默产出坏数据**——上传全部成功、
// 库里 stored=true、而每张图在浏览器里打不开。等发现时已经攒了一批 URL 全错的
// 记录，而它们指向的对象是好的，得写脚本回头改：
//
//   - 有凭证、没公开域名：URL 只能拿不允许匿名读的 S3 endpoint 拼，每张图 401。
//   - 公开域名填了 S3 endpoint：同上，且这是最容易犯的错——两个变量长得像。
//   - 公开域名少了 scheme：拼出来的地址被浏览器当成相对路径。
//
// 完全未配置是合法的本地开发状态，不拦。
func (c *Config) ValidateStorage() error {
	hasCreds := c.R2Endpoint != "" || c.R2AccessKeyID != "" ||
		c.R2SecretAccessKey != "" || c.R2Bucket != ""
	if hasCreds && c.R2PublicBaseURL == "" {
		return errors.New(
			"检测到 R2 凭证但 R2_PUBLIC_BASE_URL 为空——上传会成功但每张图的 URL 都不可匿名访问；" +
				"请填绑在桶上的自定义域，如 https://img.example.com")
	}
	if c.R2PublicBaseURL == "" {
		return nil
	}
	u, err := url.Parse(c.R2PublicBaseURL)
	if err != nil {
		return fmt.Errorf("R2_PUBLIC_BASE_URL 解析失败：%w", err)
	}
	// 少了 scheme 时拼出来的 "img.example.com/g/x.png" 会被浏览器当成**相对路径**，
	// 于是每个页面上的图都指向各自不同的错地址——比 404 更难认出来。
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf(
			"R2_PUBLIC_BASE_URL 是 %q，必须以 http:// 或 https:// 开头——"+
				"否则拼出来的图片地址会被浏览器当成相对路径", c.R2PublicBaseURL)
	}
	// 用后缀匹配而不是和 R2Endpoint 比字符串：带末尾斜杠、带路径、换个 account id
	// 的粘贴都坏得一模一样，字符串相等一个都拦不住。
	// 只认 r2.cloudflarestorage.com（S3 API 域名）；*.r2.dev 是 R2 正经的公开域名，不能拦。
	if strings.HasSuffix(u.Hostname(), ".r2.cloudflarestorage.com") {
		return fmt.Errorf(
			"R2_PUBLIC_BASE_URL 是 %q，那是 S3 API 域名、不允许匿名读——"+
				"上传会成功但每张图在浏览器里都是 401；请填绑在桶上的自定义域或 *.r2.dev 公开域名",
			c.R2PublicBaseURL)
	}
	return nil
}

// AllowedOrigins 把逗号分隔的 CORS_ALLOWED_ORIGINS 切成列表（去空白、丢空项）。
//
// 返回 nil 表示未配置，此时中间件完全不介入。
func (c *Config) AllowedOrigins() []string {
	var out []string
	for part := range strings.SplitSeq(c.CORSAllowedOrigins, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ValidateCORS 启动时的误配拦截。
//
// 拦的这几种写法与 ValidateStorage 拦的三种同类：**都不报错，只静默产出坏结果**。
// 而 CORS 的坏结果格外难查——写错一项的表现是线上所有浏览器请求被拦掉，可 curl
// 测后端**全部正常**，服务端日志里也看不出是来源列表写错了。所以这里宁可拒绝
// 启动：值只可能由一次部署引入，会被立刻发现。
//
// 完全未配置是合法状态（同源部署、或前端走服务端代理），不拦。
func (c *Config) ValidateCORS() error {
	for _, origin := range c.AllowedOrigins() {
		if err := validateOrigin(origin); err != nil {
			return fmt.Errorf("CORS_ALLOWED_ORIGINS 中的 %q 不合法：%w", origin, err)
		}
	}
	return nil
}

// validateOrigin 校验单个来源项。合法形状只有 scheme://host[:port]，**不支持通配符**。
//
// 为什么不支持通配符（这是一次审查后的刻意回退）：先前的实现允许
// https://*-myteam.vercel.app 这类后缀通配，用来覆盖 Vercel 每次部署都变的 preview
// 域名。代码审查用真实函数跑出四类绕过，根因是两条：
//
//   - 后缀匹配无法可靠地锚定在 DNS 标签边界上。https://*moloom.ai（比正确写法
//     少一个点）会放行攻击者注册的 evilmoloom.ai，而它能通过任何"形状"校验。
//   - "后缀必须含点号"这条约束会被通配符自带的那个分隔点满足，于是 https://*.com
//     一路通过，等于放行整个 TLD——与被 Fatal 拦掉的裸 * 功能等价。
//
// 再加一层黑名单（当时加的多租户平台域名列表）只是在打补丁：黑名单天生不完备，
// 而且它自己也出过大小写绕过。通配符能提供的唯一价值是 preview 域名，代价是把
// 一个安全边界建立在字符串后缀上，不成比例。
//
// 真要让 preview 直连后端，正解是用 Vercel 的 Preview Deployment Suffix 把预览放到
// 自有域名下（*.preview.moloom.ai），那时再加**标签锚定**的通配才是安全的。
//
// 校验用 net/url 而不是手写字符串处理，与同文件的 ValidateStorage 保持一致：
// url.Parse 免费带来端口合法性、userinfo、控制字符这些边界，手写的版本全都漏掉了。
func validateOrigin(origin string) error {
	// 单独拦 *，因为它是这里唯一**不会**以"匹配不上"的形式暴露的错误写法：
	// 本项目认证走 Authorization 头而非 cookie，浏览器允许 * 与该头共存，
	// 于是它会安静地工作——同时把 API 对全网任何站点敞开。
	if origin == "*" {
		return errors.New("不接受通配一切的 *（它能正常工作，只是把 API 对全网敞开）；请显式列举来源")
	}
	if strings.Contains(origin, "*") {
		return errors.New("不支持通配符；后缀通配无法可靠锚定在域名标签边界上（https://*moloom.ai 会放行 evilmoloom.ai），" +
			"请逐个列举来源。Vercel preview 若需直连后端，请用 Preview Deployment Suffix 把预览放到自有域名下")
	}
	u, err := url.Parse(origin)
	if err != nil {
		// url.Parse 会在这里报出非法端口（:abc）、缺右括号的 IPv6、host 里的空格
		// 与控制字符——这些手写的字符串校验全都放过了，而它们每一个都是永远匹配不上。
		return fmt.Errorf("解析失败：%w", err)
	}
	if u.Scheme == "" {
		return errors.New("缺少 http:// 或 https:// 前缀；Origin 请求头永远带 scheme，所以这一项永远匹配不上任何请求")
	}
	// url.Parse 已经把 scheme 规范化成小写，所以 HTTPS://moloom.ai 会被接受——
	// 它在运行时本来就能正常匹配，先前那版大小写敏感的检查会让它拒绝启动。
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme 是 %q，只支持 http 或 https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("scheme 后面没有域名")
	}
	if u.User != nil {
		return errors.New("不能带用户名密码；Origin 请求头只有 scheme+host+port，带了就永远匹配不上")
	}
	// Origin 请求头只有 scheme + host + port，永远不带尾斜杠、路径或查询串。
	// 多写了这些的项同样是永远匹配不上——而尾斜杠是最容易顺手加上的一个。
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("不能带尾斜杠、路径或查询串；Origin 请求头只有 scheme+host+port，带了就永远匹配不上")
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
