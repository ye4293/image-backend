package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/config"
	"image-backend/internal/database"
	"image-backend/internal/generation"
)

// setupRouterWithDB 同时返回 db，供需要直接操作数据（发放次数、提权）的测试使用。
//
// opts 可以改写默认配置（例如给 StripeSecretKey 塞一个假 test key，让计费
// handler 走到"已配置"分支）。默认配置**不含** Stripe 密钥，所以既有测试的
// 行为不变。
func setupRouterWithDB(t *testing.T, opts ...func(*config.Config)) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{JWTSecret: "test-secret"}
	for _, opt := range opts {
		opt(cfg)
	}
	return NewRouter(db, cfg), db
}

func setupRouter(t *testing.T, opts ...func(*config.Config)) *gin.Engine {
	t.Helper()
	r, _ := setupRouterWithDB(t, opts...)
	return r
}

// registerAndLogin 注册并登录，返回 JWT。原先这段逻辑内联在 me_test.go 里。
func registerAndLogin(t *testing.T, r *gin.Engine, email, password string) string {
	t.Helper()
	body := `{"email":"` + email + `","password":"` + password + `"}`
	postJSON(r, "/api/v1/auth/register", body)
	w := postJSON(r, "/api/v1/auth/login", body)
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析登录响应: %v; body=%s", err, w.Body.String())
	}
	token, _ := resp["token"].(string)
	if token == "" {
		t.Fatalf("登录未返回 token: %s", w.Body.String())
	}
	return token
}

func TestBuildAdaptersWrapsEveryProviderInStoringAdapter(t *testing.T) {
	// 转存是靠 BuildAdapters 包这一层实现的，而 stub 返回相对路径、装饰器对它
	// 直接跳过——所以"有没有包"在任何行为断言里都看不出来。这条按类型直接钉住，
	// 顺带保证以后新增的 provider 也不会漏包。
	reg := BuildAdapters(&config.Config{})
	if len(reg) == 0 {
		t.Fatal("registry 是空的")
	}
	for name, a := range reg {
		if _, ok := a.(*generation.StoringAdapter); !ok {
			t.Errorf("provider %q 没有被 StoringAdapter 包住——转存会整个静默失效，而所有行为测试照样绿", name)
		}
	}
}

func TestHealth(t *testing.T) {
	r := setupRouter(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}
