package settings

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Spec 描述一个可在后台修改的配置项。
type Spec struct {
	Key string
	// Secret 为 true 时值在库里加密存放，且**永不**通过 API 回传明文。
	Secret bool
	// EnvVar 首次启动播种时从哪个环境变量取（见设计文档 §2.4）。
	EnvVar string
}

// Specs 是白名单，也是这个包的安全边界。
//
// **管理接口只接受这里列出的 key。** 静默接受未知 key 会让一次打错字的保存看
// 起来成功、实际什么都没生效——而配置类的静默失效最难排查。
var Specs = []Spec{
	{Key: "ezlinkaiBaseUrl", EnvVar: "EZLINKAI_BASE_URL"},
	{Key: "fluxApiKey", Secret: true, EnvVar: "FLUX_API_KEY"},
	{Key: "r2Endpoint", EnvVar: "R2_ENDPOINT"},
	{Key: "r2AccessKeyId", Secret: true, EnvVar: "R2_ACCESS_KEY_ID"},
	{Key: "r2SecretAccessKey", Secret: true, EnvVar: "R2_SECRET_ACCESS_KEY"},
	{Key: "r2Bucket", EnvVar: "R2_BUCKET"},
	{Key: "r2PublicBaseUrl", EnvVar: "R2_PUBLIC_BASE_URL"},
	{Key: "appBaseUrl", EnvVar: "APP_BASE_URL"},
	// signupBonusCredits 新用户注册时自动赠送的月度次数。**空或 0 = 不赠送。**
	//
	// 放在这里而不是环境变量，是为了让调转化漏斗不必发版——这是个会被反复微调的
	// 运营参数。刻意**不给 EnvVar**：它没有需要从旧部署迁移的历史值，默认就该是
	// 不赠送，让"开始送钱"成为一次显式的后台操作而不是某次部署的副作用。
	//
	// ⚠️ 开启前请想清楚防滥用：注册接口没有邮箱验证也没有验证码，赠送额度会把
	//    "刷注册"从纯 CPU 消耗变成有经济收益的滥用——每一次刷都在花上游的真钱。
	//    而**应用内的按 IP 限流在本项目架构下基本无效**（浏览器经 BFF 服务端转发，
	//    ClientIP 是转发方而非真实用户，详见 internal/config 的 RateLimitRPS 注释）。
	//    真正能拦住"直接打 api 域名刷注册"的是 nginx 的 limit_req
	//    （deploy/nginx.conf.example），开赠送之前请确认那一道配上了。
	{Key: "signupBonusCredits"},
}

// maxSignupBonusCredits 后台允许填的上限。
//
// 设上限而不是只拦负数：这一项直接决定送出去多少真金白银，而输入框里多打一个 0
// 是最常见的手误。1000 次已经远超任何合理的体验额度（Starter 档一个月才 200 次）。
const maxSignupBonusCredits = 1000

func Lookup(key string) (Spec, bool) {
	for _, s := range Specs {
		if s.Key == key {
			return s, true
		}
	}
	return Spec{}, false
}

// Validate 在**写入之前**校验。
//
// 这是主防线：它在坏数据产生之前拦住。启动期的同类校验只降级为告警（见设计
// 文档 §2.5），因为那时拒绝启动等于让一次误操作把服务打死。
func Validate(key, value string) error {
	if _, ok := Lookup(key); !ok {
		return fmt.Errorf("未知配置项 %q", key)
	}
	// 空值一律合法，表示清空该项（secret 清空即退化成未配置）。
	if value == "" {
		return nil
	}
	switch key {
	case "r2PublicBaseUrl":
		u, err := url.Parse(value)
		if err != nil {
			return fmt.Errorf("r2PublicBaseUrl 解析失败：%w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf(
				"r2PublicBaseUrl 是 %q，必须以 http:// 或 https:// 开头——"+
					"否则拼出来的图片地址会被浏览器当成相对路径", value)
		}
		// 用后缀匹配而不是和 r2Endpoint 比字符串：带末尾斜杠、带路径、换个
		// account id 的粘贴都坏得一模一样，字符串相等一个都拦不住。
		// *.r2.dev 是 R2 正经的公开域名，不能拦。
		if strings.HasSuffix(u.Hostname(), ".r2.cloudflarestorage.com") {
			return fmt.Errorf(
				"r2PublicBaseUrl 是 %q，那是 S3 API 域名、不允许匿名读——"+
					"上传会成功但每张图在浏览器里都是 401；请填绑在桶上的自定义域"+
					"或 *.r2.dev 公开域名", value)
		}
	case "appBaseUrl", "r2Endpoint", "ezlinkaiBaseUrl":
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%s 是 %q，必须是完整的 http(s) URL", key, value)
		}
	case "signupBonusCredits":
		// 这里是主防线：拦在坏数据产生之前。启动期的同类校验只降级为告警
		// （见文件头注释），所以放过一个 100000 之后，服务会带着它照常运行、
		// 一边给每个注册的人送 100000 次额度。
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("signupBonusCredits 是 %q，必须是整数（0 表示不赠送）", value)
		}
		if n < 0 {
			return fmt.Errorf("signupBonusCredits 不能为负（得到 %d）", n)
		}
		if n > maxSignupBonusCredits {
			return fmt.Errorf(
				"signupBonusCredits 是 %d，超过上限 %d——这一项直接决定送出去多少真钱，"+
					"多打一个 0 是最常见的手误；确实需要更高请改代码里的 maxSignupBonusCredits",
				n, maxSignupBonusCredits)
		}
	}
	return nil
}
