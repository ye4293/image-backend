package config

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
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

	// TrustedProxies 哪些来源的 X-Forwarded-For 可以被信任，逗号分隔的 CIDR 或 IP。
	//
	// 它决定 c.ClientIP() 的取值，而按 IP 限流的正确性完全建立在那上面。两种配错
	// 方式的后果都很坏，且方向相反：
	//
	//   - 留空 → gin 信任所有代理 → 任何人加一个 X-Forwarded-For 头就换一个新的
	//     限流桶，限流形同不存在。
	//   - 配得太窄 → gin 不信任真实来源、忽略 X-Forwarded-For → **所有请求的
	//     ClientIP 都是同一个上游地址**，全站用户共享一个桶，几个人同时注册就集体
	//     429。
	//
	// 本项目的部署拓扑是「宿主机 nginx → 127.0.0.1:5000 → docker 端口映射 → 容器」，
	// 容器内看到的源 IP 是 **docker 网桥网关**（通常 172.17.0.1），不是回环。所以
	// 默认值同时包含回环与 docker 默认网段。直接跑二进制（无容器）时回环那条生效。
	//
	// ⚠️ 这个误配在本地开发完全复现不出来——本地不过容器，ClientIP 天然正确。
	//    生产上线后必须实际打一次超限，确认日志里的 ClientIP 是公网地址。
	TrustedProxies string

	// RateLimitRPS / RateLimitBurst 是 /auth/* 的按 IP 限流参数。
	//
	// **0 表示关闭限流。** 这个约定让测试可以直接用零值 Config 构造路由而不被限流
	// 干扰——否则每个测试函数里注册/登录超过 burst 次就会以「注册返回 429」的形式
	// 失败，而那在一个测注册的测试里是极具误导性的报错。
	//
	// 生产路径走 Load()，那里给了非零默认值，所以"忘记配置"不会导致无保护。真要
	// 关掉必须显式把环境变量设成 0，main.go 会为此打一条告警。
	RateLimitRPS   float64
	RateLimitBurst int
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

		// 默认覆盖回环与 docker 默认网桥网段（172.16.0.0/12 含 172.17.x）。
		// 见字段注释：这两者对应"直接跑二进制"与"跑在容器里"两种部署。
		TrustedProxies: getEnv("TRUSTED_PROXIES", "127.0.0.1/32,::1/128,172.16.0.0/12"),

		// 稳态 0.2 次/秒（5 秒一个令牌）、突发 10 次。真人注册或重试打不到这个量，
		// 脚本刷号会立刻撞上。留出 10 次突发是因为「填错密码连试几次」是常见的。
		RateLimitRPS:   getEnvFloat("RATE_LIMIT_RPS", 0.2),
		RateLimitBurst: getEnvInt("RATE_LIMIT_BURST", 10),
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

// AllowedProxies 把逗号分隔的 TRUSTED_PROXIES 切成列表（去空白、丢空项）。
//
// 返回 nil 表示未配置。接线处（internal/server/router.go）会把它当成"谁都不信任"，
// 那正好也是 gin 对 nil 的处理——**但危险的是根本不调 SetTrustedProxies**：
// gin.New() 的默认值是信任所有代理，那才是限流被 X-Forwarded-For 绕过的路径。
func (c *Config) AllowedProxies() []string {
	var out []string
	for part := range strings.SplitSeq(c.TrustedProxies, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ValidateTrustedProxies 启动时的误配拦截。
//
// 每一项必须是合法的 IP 或 CIDR。写错一个字符的后果不是报错，而是 gin 在
// SetTrustedProxies 时返回 error——若调用方忽略了那个 error，就退回"信任所有代理"，
// 限流被一个请求头绕过，而日志里什么都看不出来。
func (c *Config) ValidateTrustedProxies() error {
	for _, p := range c.AllowedProxies() {
		if strings.Contains(p, "/") {
			if _, _, err := net.ParseCIDR(p); err != nil {
				return fmt.Errorf("TRUSTED_PROXIES 中的 %q 不是合法 CIDR：%w", p, err)
			}
			continue
		}
		if net.ParseIP(p) == nil {
			return fmt.Errorf("TRUSTED_PROXIES 中的 %q 既不是 IP 也不是 CIDR", p)
		}
	}
	return nil
}

// RateLimitEnabled 两个参数都为正才算启用。
//
// 只配一半（例如 rps>0 但 burst=0）是没有意义的组合：burst=0 意味着桶永远取不出
// 令牌，会把所有请求都拒掉。所以要求两者同时为正，否则整体视为关闭并由 main.go 告警
// ——把 API 全拒和不设限流之间，静默选中前者是最坏的结果。
func (c *Config) RateLimitEnabled() bool {
	return c.RateLimitRPS > 0 && c.RateLimitBurst > 0
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvFloat / getEnvInt 解析失败时**回退到默认值并告警**，而不是取零值。
//
// 取零值是这里最坏的选择：限流参数的零值等于关闭限流，于是把 RATE_LIMIT_RPS 写成
// "0.2 " 带个空格这种小错，会静默地把防护整个关掉。告警 + 用默认值意味着最坏情况
// 是"没按你写的那个数生效"，而不是"没有防护"。
func getEnvFloat(key string, fallback float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		log.Printf("config: %s=%q 不是合法数字，回退到默认值 %v", key, raw, fallback)
		return fallback
	}
	return v
}

func getEnvInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		log.Printf("config: %s=%q 不是合法整数，回退到默认值 %d", key, raw, fallback)
		return fallback
	}
	return v
}
