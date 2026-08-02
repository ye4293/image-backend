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

func TestAllowedOriginsSplitsAndTrims(t *testing.T) {
	c := &Config{CORSAllowedOrigins: " https://moloom.ai , http://localhost:3000 ,, "}
	got := c.AllowedOrigins()
	want := []string{"https://moloom.ai", "http://localhost:3000"}
	if len(got) != len(want) {
		t.Fatalf("期望 %d 项，得到 %d 项：%q", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项：期望 %q，得到 %q", i, want[i], got[i])
		}
	}
	// 未配置必须是空列表而不是含一个空串的列表：空串会被中间件当成一个来源去比，
	// 而没有 Origin 头的请求（Stripe webhook）拿到的正是空串。
	if n := len((&Config{}).AllowedOrigins()); n != 0 {
		t.Errorf("未配置时应当返回空列表，得到 %d 项", n)
	}
}

func TestValidateCORSRejectsWildcardAll(t *testing.T) {
	// 这一项与本文件其他"拒绝"用例的性质不同：它**不会**以匹配不上的形式暴露。
	// 认证走 Authorization 头而非 cookie，浏览器允许 * 与该头共存，于是 * 会
	// 安静地正常工作，同时把 API 对全网任何站点敞开。
	err := (&Config{CORSAllowedOrigins: "*"}).ValidateCORS()
	if err == nil {
		t.Fatal("* 必须拒绝启动：它能正常工作，只是把 API 对全网敞开")
	}
	if !strings.Contains(err.Error(), "CORS_ALLOWED_ORIGINS") {
		t.Errorf("错误信息要点名是哪个变量，得到：%v", err)
	}
}

func TestValidateCORSRejectsAnyWildcard(t *testing.T) {
	// 通配符曾被支持（用来覆盖 Vercel preview 每次部署都变的域名），代码审查用真实
	// 函数跑出四类绕过后整个移除。这组用例钉住"别再加回来"，并列出当时的绕过：
	//   · https://*moloom.ai        放行了 evilmoloom.ai（后缀未锚定标签边界）
	//   · https://*.com             放行了整个 TLD（"后缀须含点号"被通配符自带的点满足）
	//   · https://*.Vercel.App      大小写绕过了当时的多租户域名黑名单
	//   · https://*-myteam.vercel.app 配合 path 夹带放行了 evil.com/-myteam.vercel.app
	for _, pattern := range []string{
		"https://*.vercel.app",
		"https://*-myteam.vercel.app",
		"https://*.Vercel.App",
		"https://*.com",
		"https://*moloom.ai",
		"https://*.moloom.ai",
		"https://moloom.ai,https://*.vercel.app",
	} {
		err := (&Config{CORSAllowedOrigins: pattern}).ValidateCORS()
		if err == nil {
			t.Errorf("%q 必须拒绝启动：通配符已移除，后缀匹配无法可靠锚定在域名标签边界上", pattern)
			continue
		}
		if !strings.Contains(err.Error(), "CORS_ALLOWED_ORIGINS") {
			t.Errorf("%q 的错误信息要点名变量名，得到：%v", pattern, err)
		}
	}
}

func TestValidateCORSRejectsSilentlyUnmatchableForms(t *testing.T) {
	// 这些写法的共同点：进程照常启动、日志无异常，但那一项**永远匹配不上任何
	// 请求**——因为 Origin 请求头只有 scheme+host+port。表现是线上浏览器全挂而
	// curl 全通，没有任何信号指向来源列表。
	//
	// 后四项是改用 net/url 之后才拦住的；先前手写 strings.Cut 的版本全部放过。
	cases := map[string]string{
		"缺 scheme":      "moloom.ai",
		"协议相对":          "//moloom.ai",
		"尾斜杠":           "https://moloom.ai/",
		"带路径":           "https://moloom.ai/api",
		"带查询串":          "https://moloom.ai?x=1",
		"非 http scheme": "ftp://moloom.ai",
		"带 userinfo":    "https://user@moloom.ai",
		"非法端口":          "https://moloom.ai:abc",
		"IPv6 缺右括号":     "http://[::1:3000",
		"host 含空格":      "https://molo om.ai",
	}
	for name, pattern := range cases {
		err := (&Config{CORSAllowedOrigins: pattern}).ValidateCORS()
		if err == nil {
			t.Errorf("%s（%q）必须拒绝启动：它永远匹配不上任何请求", name, pattern)
			continue
		}
		if !strings.Contains(err.Error(), "CORS_ALLOWED_ORIGINS") {
			t.Errorf("%q 的错误信息要点名变量名，得到：%v", pattern, err)
		}
	}
}

func TestValidateCORSAllowsLegitimateForms(t *testing.T) {
	// 防过度拦截。本地开发要带端口、生产是裸域名、IPv6 与自定义端口也都合法；
	// 完全未配置是同源部署的正确状态，也不能拦。
	//
	// HTTPS://moloom.ai 必须放行：url.Parse 会把 scheme 规范化成小写，而运行时
	// 匹配也是大小写不敏感的，所以它本来就能正常工作。先前那版大小写敏感的检查
	// 会让这个能用的值拒绝启动——拦下一个本来正确的配置比放过一个错的更糟。
	for _, origins := range []string{
		"",
		"https://moloom.ai",
		"HTTPS://moloom.ai",
		"http://localhost:3000",
		"http://127.0.0.1:3000",
		"http://[::1]:3000",
		"https://app.moloom.ai",
		"https://moloom.ai,http://localhost:3000,https://app.moloom.ai",
	} {
		if err := (&Config{CORSAllowedOrigins: origins}).ValidateCORS(); err != nil {
			t.Errorf("%q 是合法配置，不该报错：%v", origins, err)
		}
	}
}
