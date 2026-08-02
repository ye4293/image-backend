package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// corsAllowMethods 本后端实际用到的动作。router.go 里只有 GET / POST / PATCH，
// 多列（PUT、DELETE）没有收益，只会让预检响应描述一个不存在的接口面。
//
// 新增其他动作的路由时**必须同步改这里**，否则浏览器会拒发真实请求，而预检照样
// 回 204、服务端日志一切正常。TestCORSAllowMethodsCoversAllRoutes 钉住了这个同步。
const corsAllowMethods = "GET, POST, PATCH, OPTIONS"

// corsFallbackAllowHeaders 预检没声明 Access-Control-Request-Headers 时的兜底值。
// 正常情况下走回显（见下），这个常量只用于手工构造的、不带该头的预检。
const corsFallbackAllowHeaders = "Authorization, Content-Type"

// corsMaxAgeSeconds 预检结果的缓存秒数。
//
// 不能省：token 走 Authorization 头，那是"非简单请求头"，于是浏览器在**每个** API
// 请求前都要先发一次 OPTIONS。没有这个头的话每次生成都要多付一个往返。
const corsMaxAgeSeconds = "600"

// CORS 按白名单回 CORS 响应头，并短路 OPTIONS 预检。
//
// 为什么需要它：前端在 Vercel、后端在自己的服务器，是两个 origin。而 token 走
// Authorization 头（见 auth.go），那是"非简单请求头"，于是浏览器在每个 API 请求
// 之前都会先发一次 OPTIONS 预检。本仓库没有任何 OPTIONS 路由，gin 的
// HandleMethodNotAllowed 也是关的，所以预检会落到 NoRoute——全局中间件在那条路径上
// 会跑（gin 的 Use 会重建 allNoRoute），所以这里短路得住。
//
// 那个失败特别难查：**curl 测后端全部正常，浏览器里全部失败**，而反代日志里只有
// 一串被拒的 OPTIONS——没有任何信号指向"缺 CORS"。
//
// **只支持精确匹配，不支持通配符。** 原因见 config.validateOrigin 的注释：后缀通配
// 无法可靠地锚定在 DNS 标签边界上，代码审查在上一版实现里跑出了四类绕过。
//
// **刻意不发 Access-Control-Allow-Credentials。** 认证靠 Authorization 头，不用
// cookie。开了它就必须永远回显具体 origin、不能用 *，而且将来某天若有人改成
// cookie 认证，凭据会立刻暴露给白名单里的每一项。
//
// allowed 为空时返回一个纯粹的直通中间件，**连 Vary 都不加**：同源部署（nginx 同时
// 服务前端与 /api）或前端走服务端代理时那是正确状态，此时响应应当与没有本中间件
// 时逐字节一致。
func CORS(allowed []string) gin.HandlerFunc {
	// 白名单在启动时就固定了，所以在这里预处理一次，而不是每请求重复小写化。
	// 用 map 也让精确匹配从线性扫描变成 O(1)。
	exact := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		exact[strings.ToLower(o)] = struct{}{}
	}
	if len(exact) == 0 {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		// **无条件**加 Vary，包括没有 Origin 头的请求。
		//
		// 会被共享缓存投毒的方向恰恰是"无 Origin"那一侧：/models 与 /plans 是公开
		// 可缓存端点，探活/预热/curl（无 Origin）会先存下一份不带 Allow-Origin 的
		// 响应；若那份响应没有 Vary，缓存就会把它喂给随后带白名单内 Origin 的浏览器
		// 请求，浏览器因缺头拦掉。只在有 CDN/缓存层时出现，本地测不出来。
		c.Writer.Header().Add("Vary", "Origin")

		origin := c.GetHeader("Origin")
		if origin == "" {
			// 服务端到服务端的调用：Stripe webhook、健康检查、curl。
			c.Next()
			return
		}

		if _, ok := exact[strings.ToLower(origin)]; !ok {
			if c.Request.Method == http.MethodOptions {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			// 非预检请求照常放行，只是不加 CORS 头。
			//
			// 这一条只对**已认证**端点成立地充分：认证走 Authorization 头、没有任何
			// cookie，所以跨域页面没有环境凭据，带 Authorization 又必然触发预检、
			// 预检在上面被拒，请求根本发不出去——POST /generations 扣不到费。
			//
			// 未认证的副作用端点（POST /auth/register）不受此保护：ShouldBindJSON
			// 不看 Content-Type，攻击者页面可用 text/plain 发 JSON 而不触发预检。
			// 但拦 Origin 也解决不了那个（僵尸网络根本不带 Origin），正确的手段是
			// 限流 + 拒收非 application/json，**不是**在这里改成 abort。
			c.Next()
			return
		}

		// 回显**具体** origin，绝不回 *。
		c.Writer.Header().Set("Access-Control-Allow-Origin", origin)

		if c.Request.Method == http.MethodOptions {
			c.Writer.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
			// 回显浏览器声明要发的头，而不是回一个硬编码列表。
			//
			// 硬编码 "Authorization, Content-Type" 的话，前端将来加任何自定义头
			// （i18n 的 locale 头、追踪 SDK 自动挂的 sentry-trace/baggage）都会让
			// **全部** API 调用失败，而预检照样回 204、服务端零日志、curl 全通，
			// 且 Max-Age 还把这个"看起来成功"的预检缓存 10 分钟。
			//
			// 回显在这里是安全的：来源已经过精确白名单，且不发 Allow-Credentials，
			// 所以跨域页面拿不到任何环境凭据——放宽头列表不增加它能做的事。
			if reqHeaders := c.GetHeader("Access-Control-Request-Headers"); reqHeaders != "" {
				c.Writer.Header().Set("Access-Control-Allow-Headers", reqHeaders)
			} else {
				c.Writer.Header().Set("Access-Control-Allow-Headers", corsFallbackAllowHeaders)
			}
			c.Writer.Header().Set("Access-Control-Max-Age", corsMaxAgeSeconds)
			// 预检必须在这里就结束：它走不到任何业务路由（仓库里没有 OPTIONS
			// 路由），继续下去就是 404。
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
