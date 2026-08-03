package middleware

import (
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// JSONOnly 拒收带请求体但 Content-Type 不是 application/json 的请求。
//
// 这不是洁癖，而是把一个具体的绕过口堵上：gin 的 ShouldBindJSON 直接走 JSON 解析、
// **完全不看 Content-Type**（与 ShouldBind 不同）。于是跨域页面可以用
// `Content-Type: text/plain` 发一个 JSON 体，那属于 CORS 的"简单请求"、**不触发预检**，
// 因此绕过了 CORS 白名单——而 /auth/register 是有副作用的（建用户、烧 bcrypt CPU、
// 将来还要发赠送额度）。
//
// 要求 application/json 之后，跨域调用必须带自定义 Content-Type，那就必须先过预检，
// 而预检会被 internal/middleware/cors.go 的白名单拦下。这条对策在 cors.go 的注释里
// 已经写明，这里是它的实现。
//
// 只管有请求体的方法。GET/HEAD/OPTIONS 不带体，要求它们声明 Content-Type 没有意义
// （浏览器也不会发）。
func JSONOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodDelete:
			c.Next()
			return
		}

		ct := c.GetHeader("Content-Type")
		// 用 mime.ParseMediaType 而不是字符串比较：合法的头可以带参数，
		// 例如 "application/json; charset=utf-8"，直接 == 会把它误拒。
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || !strings.EqualFold(mediaType, "application/json") {
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType,
				gin.H{"code": 41500, "message": "content-type must be application/json"})
			return
		}
		c.Next()
	}
}
