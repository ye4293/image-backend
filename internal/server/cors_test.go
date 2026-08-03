package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"image-backend/internal/config"
)

const testOrigin = "https://moloom.ai"

func corsRouter(t *testing.T, origins string) *gin.Engine {
	t.Helper()
	return setupRouter(t, func(c *config.Config) { c.CORSAllowedOrigins = origins })
}

// doOptions 发一次预检。现有 helper 都不发 OPTIONS，也不检查响应头。
func doOptions(r *gin.Engine, path, origin, reqMethod string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodOptions, path, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", reqMethod)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doGet(r *gin.Engine, path, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	r := corsRouter(t, testOrigin)
	w := doOptions(r, "/api/v1/generations", testOrigin, http.MethodPost)

	// 预检必须由中间件短路。仓库里没有 OPTIONS 路由，落到路由层就是 404，
	// 而 404 不带 CORS 头——浏览器于是拦掉真正的那个请求。
	if w.Code != http.StatusNoContent {
		t.Fatalf("预检应当 204（由中间件短路），得到 %d；落到路由层会是 404 且不带 CORS 头", w.Code)
	}
	want := map[string]string{
		"Access-Control-Allow-Origin":  testOrigin,
		"Access-Control-Allow-Methods": "GET, POST, PATCH, OPTIONS",
		"Access-Control-Allow-Headers": "authorization,content-type", // 回显请求声明的头
		"Access-Control-Max-Age":       "600",
	}
	for h, v := range want {
		if got := w.Header().Get(h); got != v {
			t.Errorf("%s：期望 %q，得到 %q", h, v, got)
		}
	}
}

func TestCORSPreflightEchoesRequestedHeaders(t *testing.T) {
	// 硬编码 Allow-Headers 的话，前端加任何自定义头（本项目要支持四语，locale 头
	// 是迟早的事；追踪 SDK 也会自动挂 sentry-trace/baggage）都会让**全部** API
	// 调用失败，而预检照样回 204、服务端零日志、curl 全通，且 Max-Age 把这个
	// "看起来成功"的预检缓存 10 分钟。
	r := corsRouter(t, testOrigin)
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/generations", nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,x-locale,sentry-trace")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "authorization,content-type,x-locale,sentry-trace" {
		t.Errorf("应当回显浏览器声明要发的头，得到 %q", got)
	}
}

func TestCORSPreflightRejectsUnknownOrigin(t *testing.T) {
	r := corsRouter(t, testOrigin)
	w := doOptions(r, "/api/v1/generations", "https://evil.com", http.MethodPost)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("白名单外的 origin 不该拿到 Allow-Origin，得到 %q", got)
	}
	// 必须钉住状态码：若这条 abort 被去掉，预检会落到 NoRoute 回 404，而本次改动
	// 的全部动机就是"日志里只有一串 404 OPTIONS 给不出任何信号"。只断言头缺失的话，
	// 那个回退一个测试都抓不到。
	if w.Code != http.StatusForbidden {
		t.Errorf("白名单外的预检应当 403（明确拒绝），得到 %d；404 意味着又退回了无信号的状态", w.Code)
	}
}

func TestCORSNonPreflightFromUnknownOriginIsServed(t *testing.T) {
	// 刻意放行：CORS 只保护"浏览器读响应"，在这里 abort 没有安全收益（攻击者用
	// curl 一样能发），只会让不带白名单的调试请求以一个莫名其妙的 403 失败。
	//
	// 这条断言是防"顺手加固"的：把中间件那处 c.Next() 改成 abort(403) 看起来更安全，
	// 而在同源部署（白名单为空）下浏览器对所有非 GET 请求都会带 Origin，于是整个
	// API 会对真实用户返回 403，而 curl（不带 Origin）测起来一切正常。
	r := corsRouter(t, testOrigin)
	w := doGet(r, "/api/v1/health", "https://evil.com")

	if w.Code != http.StatusOK {
		t.Fatalf("白名单外的非预检请求应当照常处理（200），得到 %d；body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("但不该带 Allow-Origin，得到 %q", got)
	}
}

func TestCORSVaryOnEveryResponseIncludingNoOrigin(t *testing.T) {
	// 会被共享缓存投毒的方向恰恰是"无 Origin"那一侧：/models 与 /plans 是公开可缓存
	// 端点，探活/预热/curl 会先存下一份不带 Allow-Origin 的响应；若那份响应没有
	// Vary，缓存就会把它喂给随后带白名单内 Origin 的浏览器请求，浏览器因缺头拦掉。
	// 只在有 CDN/缓存层时出现，本地和现有 nginx 示例都测不出来。
	r := corsRouter(t, testOrigin)
	for _, origin := range []string{testOrigin, "https://evil.com", ""} {
		w := doGet(r, "/api/v1/models", origin)
		if got := w.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Origin=%q 的响应也必须带 Vary: Origin（否则缓存层会跨 origin 复用），得到 %q", origin, got)
		}
	}
}

func TestCORSAllowedOriginIsEchoedByteExact(t *testing.T) {
	// 匹配大小写不敏感，但回显必须是**客户端发来的原字节**：浏览器对
	// Access-Control-Allow-Origin 做逐字节比较，回一个规范化后的值会被判为不匹配。
	// 若哪天把小写化提到 GetHeader 那一步，这条会失败。
	r := corsRouter(t, testOrigin)
	const mixed = "https://MOLOOM.ai"
	w := doGet(r, "/api/v1/health", mixed)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != mixed {
		t.Errorf("应当原样回显客户端发来的 origin %q，得到 %q", mixed, got)
	}
}

func TestCORSSecondWhitelistEntryWorks(t *testing.T) {
	// 多项配置时，非第一项也必须生效。只验第一项的话，一个"只看 allowed[0]"的
	// 实现错误能完全溜过去。
	r := corsRouter(t, "https://other.example.com,"+testOrigin+",http://localhost:3000")
	for _, origin := range []string{"https://other.example.com", testOrigin, "http://localhost:3000"} {
		w := doOptions(r, "/api/v1/generations", origin, http.MethodPost)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("%q 在白名单里，应当放行，Allow-Origin 得到 %q", origin, got)
		}
	}
}

func TestCORSRejectsLookalikeOrigins(t *testing.T) {
	// 精确匹配不该被这些形状骗到。上一版实现支持后缀通配，
	// https://evil.com/-myteam.vercel.app 之类能直接绕进来（审查实测），
	// 通配符因此被整个移除——这组用例钉住"别再把后缀匹配加回来"。
	r := corsRouter(t, testOrigin)
	for _, origin := range []string{
		"https://evil.moloom.ai",             // 子域不自动继承
		"https://moloom.ai.evil.com",         // 后缀出现在中间
		"https://evilmoloom.ai",              // 未锚定标签边界
		"https://moloom.ai:8443",             // 端口不同即不同 origin
		"http://moloom.ai",                   // scheme 不同即不同 origin
		"https://evil.com/moloom.ai",         // 路径夹带
		"https://evil.com@moloom.ai",         // userinfo 夹带
		"https://evil.com#https://moloom.ai", // fragment 夹带
		"null",                               // sandboxed iframe / file://
	} {
		w := doGet(r, "/api/v1/health", origin)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%q 必须被拒，却拿到了 Allow-Origin %q", origin, got)
		}
	}
}

func TestCORSDisabledWhenUnconfigured(t *testing.T) {
	// 空配置是同源部署（或前端走服务端代理）的正确状态，不是缺失。此时响应必须与
	// 没有本中间件时**逐字节一致**——多一个 Vary 都会让 CDN 的缓存键被无谓地拆分。
	r := corsRouter(t, "")
	w := doGet(r, "/api/v1/health", testOrigin)

	if w.Code != http.StatusOK {
		t.Fatalf("未配置白名单时请求应当照常处理，得到 %d", w.Code)
	}
	for _, h := range []string{"Access-Control-Allow-Origin", "Vary"} {
		if got := w.Header().Get(h); got != "" {
			t.Errorf("未配置白名单时不该出现 %s，得到 %q", h, got)
		}
	}
}

func TestCORSNoOriginHeaderGetsNoAllowOrigin(t *testing.T) {
	// 服务端到服务端的调用没有 Origin：Stripe webhook、健康检查、curl。
	// 它们不该拿到 Allow-Origin。（Vary 会有，理由见
	// TestCORSVaryOnEveryResponseIncludingNoOrigin。）
	r := corsRouter(t, testOrigin)
	w := doGet(r, "/api/v1/health", "")

	if w.Code != http.StatusOK {
		t.Fatalf("无 Origin 的请求应当照常处理，得到 %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("无 Origin 时不该出现 Allow-Origin，得到 %q", got)
	}
}

func TestCORSNeverSendsAllowCredentials(t *testing.T) {
	// 认证走 Authorization 头、不用 cookie，所以这个头没有用处。
	// 而开了它就必须永远回显具体 origin、不能用 *，并且将来某天若有人改成 cookie
	// 认证，凭据会立刻暴露给白名单里的每一项。
	r := corsRouter(t, testOrigin)
	w := doOptions(r, "/api/v1/generations", testOrigin, http.MethodPost)
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("不该发 Access-Control-Allow-Credentials，得到 %q", got)
	}
}

func TestCORSPreflightDoesNotBypassAuth(t *testing.T) {
	// 预检回 204 是"允许浏览器发那个请求"，不是"那个请求免认证"。
	r := corsRouter(t, testOrigin)
	w := doGet(r, "/api/v1/me", testOrigin)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("带 Origin 但无 token 的请求仍须 401，得到 %d；body=%s", w.Code, w.Body.String())
	}
	// 401 也要带 CORS 头，否则浏览器读不到状态码，前端只能显示"网络错误"而非"请登录"。
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != testOrigin {
		t.Errorf("错误响应也要带 Allow-Origin，否则前端读不到 401，得到 %q", got)
	}
}

func TestCORSTrailingSlashStillCarriesCORSHeaders(t *testing.T) {
	// gin 的 RedirectTrailingSlash 是唯一一条全局中间件跑不到的路径（它直接
	// redirectTrailingSlash 并 return，从不 c.Next()）。开着它的话，前端 base URL
	// 多一个尾斜杠时：预检正常回 204 → 浏览器放行真实请求 → 真实请求拿到不带 CORS
	// 头的 301 → 被浏览器拦掉，而 Logger 也被跳过所以服务端日志一行都没有，
	// curl -L 则完全正常。router.go 因此关掉了它。
	r := corsRouter(t, testOrigin)
	w := doGet(r, "/api/v1/models/", testOrigin)

	if w.Code == http.StatusMovedPermanently || w.Code == http.StatusTemporaryRedirect {
		t.Fatalf("尾斜杠不该走重定向（那条路径绕过所有中间件、不带 CORS 头、且日志里毫无痕迹），得到 %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != testOrigin {
		t.Errorf("尾斜杠路径的响应也必须带 Allow-Origin，否则浏览器读不到这个 404，得到 %q", got)
	}
}

func TestCORSAllowMethodsCoversAllRoutes(t *testing.T) {
	// corsAllowMethods 是人工从路由表抄来的常量。加一条 DELETE 路由却忘了改它的话，
	// 预检照样回 204、服务端日志一切正常，但浏览器发现方法不在 Allow 列表里就不发
	// 真实请求，而 Max-Age 还把这个"看起来成功"的预检缓存 10 分钟。
	r := corsRouter(t, testOrigin)
	advertised := strings.Split("GET, POST, PATCH, OPTIONS", ", ")
	for _, route := range r.Routes() {
		if !slices.Contains(advertised, route.Method) {
			t.Errorf("路由 %s %s 的方法不在 CORS 预检公布的 Allow-Methods 里——"+
				"浏览器会拒发这个请求，而预检仍回 204、服务端毫无信号；"+
				"请同步 internal/middleware/cors.go 的 corsAllowMethods",
				route.Method, route.Path)
		}
	}
}
