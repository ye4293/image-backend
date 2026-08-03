package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"image-backend/internal/config"
)

// withRateLimit 打开限流。默认的测试配置（零值）刻意关闭限流——否则每个测试函数里
// 注册/登录超过 burst 次就会以「注册返回 429」的形式失败，而那在一个测注册的测试里
// 是极具误导性的报错。所以要考察限流的测试必须自己显式打开。
func withRateLimit(rps float64, burst int) func(*config.Config) {
	return func(c *config.Config) {
		c.RateLimitRPS = rps
		c.RateLimitBurst = burst
	}
}

// postAuthFrom 从指定的源地址发一次注册请求。addr 形如 "1.2.3.4:1111"。
//
// httptest.NewRequest 默认把 RemoteAddr 设成 192.0.2.1:1234，所有请求同源，
// 考察"不同 IP 互不影响"就必须自己改它。
func postAuthFrom(r *gin.Engine, addr, email string, headers map[string]string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"email":%q,"password":"secret12345"}`, email)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = addr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRateLimitBlocksBurstFromSameIP(t *testing.T) {
	// burst=3 且 rps 极低，第 4 次必须被拒。
	r := setupRouter(t, withRateLimit(0.01, 3))

	for i := 1; i <= 3; i++ {
		w := postAuthFrom(r, "203.0.113.7:1111", fmt.Sprintf("rl%d@example.com", i), nil)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次请求就被限流了，burst=3 应当放过前 3 次", i)
		}
	}

	w := postAuthFrom(r, "203.0.113.7:1111", "rl4@example.com", nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 4 次应当 429（burst 已耗尽），得到 %d；body=%s", w.Code, w.Body.String())
	}
	// Retry-After 必须存在且非 0：回 0 会让客户端立刻重试，等于没限流。
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("429 必须带非 0 的 Retry-After，得到 %q", ra)
	}
}

func TestRateLimitIsPerIP(t *testing.T) {
	// 一个 IP 打满不该影响别人。搞错这一点的表现是"一个人刷号导致全站注册不了"，
	// 比不限流更糟。
	r := setupRouter(t, withRateLimit(0.01, 2))

	for i := 1; i <= 3; i++ {
		postAuthFrom(r, "203.0.113.10:1111", fmt.Sprintf("a%d@example.com", i), nil)
	}
	// 上面第 3 次已被拒，确认这个 IP 确实满了
	if w := postAuthFrom(r, "203.0.113.10:1111", "a9@example.com", nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("前提不成立：该 IP 应当已被限流，得到 %d", w.Code)
	}

	w := postAuthFrom(r, "198.51.100.20:2222", "b1@example.com", nil)
	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("另一个 IP 不该受影响，却拿到 429；body=%s", w.Body.String())
	}
}

func TestRateLimitIgnoresForwardedForFromUntrustedSource(t *testing.T) {
	// **这条是限流唯一的绕过方式，也是整组测试里最重要的一条。**
	//
	// 若 gin 信任了请求来源的 X-Forwarded-For，攻击者每次换一个伪造的头就换一个新桶，
	// 限流形同不存在。测试里 RemoteAddr 是 203.0.113.x（公网地址，不在 TRUSTED_PROXIES
	// 的默认网段里），所以 gin 必须忽略这个头、按 RemoteAddr 计数。
	r := setupRouter(t, withRateLimit(0.01, 2))

	const from = "203.0.113.30:1111"
	for i := 1; i <= 2; i++ {
		postAuthFrom(r, from, fmt.Sprintf("ff%d@example.com", i), nil)
	}

	// 桶已耗尽。换一个伪造的 X-Forwarded-For 再打——必须仍然 429。
	w := postAuthFrom(r, from, "ff9@example.com", map[string]string{
		"X-Forwarded-For": "1.2.3.4",
	})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("伪造 X-Forwarded-For 换到了新桶，限流被绕过了（得到 %d）。"+
			"这说明 router.go 里的 SetTrustedProxies 没生效——注意 gin.New() 的默认值是"+
			"信任所有代理（[\"0.0.0.0/0\",\"::/0\"]），漏调这个函数就等于把限流敞开",
			w.Code)
	}
}

func TestJSONOnlyRejectsNonJSONContentType(t *testing.T) {
	// gin 的 ShouldBindJSON 完全不看 Content-Type，所以跨域页面可以用 text/plain
	// 发 JSON 体——那属于 CORS 简单请求、不触发预检，因此绕过 CORS 白名单。
	// 而 /auth/register 是有副作用的（建用户、烧 bcrypt CPU、发赠送额度）。
	r := setupRouter(t)

	for _, ct := range []string{
		"text/plain",
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"", // 完全不带
	} {
		body := `{"email":"ct@example.com","password":"secret12345"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type=%q 应当被拒（415），得到 %d；body=%s", ct, w.Code, w.Body.String())
		}
	}
}

func TestJSONOnlyAcceptsCharsetParameter(t *testing.T) {
	// 防过度拦截：合法的 Content-Type 允许带参数，浏览器与各种客户端都会发
	// "application/json; charset=utf-8"。用字符串相等比较会把它误拒，
	// 而那个故障表现是"所有注册都 415"——比放过 text/plain 严重得多。
	r := setupRouter(t)

	body := `{"email":"charset@example.com","password":"secret12345"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("带 charset 参数的 application/json 必须放行，却被拒了；body=%s", w.Body.String())
	}
}

func TestRateLimitDisabledByDefaultInTests(t *testing.T) {
	// 钉住"零值 Config = 关闭限流"这个约定。它是所有既有测试能反复注册登录的前提，
	// 若哪天改成"零值 = 最严格"，几十个无关测试会一起变红，而报错都指向注册接口。
	if (&config.Config{}).RateLimitEnabled() {
		t.Fatal("零值 Config 必须视为关闭限流")
	}
	// 只配一半同样算关闭：burst=0 会让桶永远取不出令牌，把所有请求都拒掉。
	if (&config.Config{RateLimitRPS: 1}).RateLimitEnabled() {
		t.Error("只有 rps 没有 burst 时必须视为关闭——burst=0 会拒掉所有请求")
	}
	if (&config.Config{RateLimitBurst: 5}).RateLimitEnabled() {
		t.Error("只有 burst 没有 rps 时必须视为关闭")
	}
}
