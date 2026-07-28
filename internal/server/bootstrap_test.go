package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"image-backend/internal/config"
	"image-backend/internal/database"
)

// 全新环境里的顺序是"先起服务，再注册"，所以注册时也要认 BOOTSTRAP_ADMIN_EMAIL。
// 只靠启动时提权的话，配了该变量的操作者得注册完再重启一次；而 dev 模式每次启动
// 都是新的临时 SQLite，那一重启会把刚注册的账号一起丢掉，管理员永远拿不到。
func TestBootstrapAdminEmailPromotesOnRegister(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{JWTSecret: "test-secret", BootstrapAdminEmail: "Boss@Example.com"}
	r := NewRouter(db, cfg)

	// 大小写不该影响判定：注册时邮箱被 ToLower，配置里可能是大写。
	token := registerAndLogin(t, r, "boss@example.com", "secret12345")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if out["role"] != "admin" {
		t.Fatalf("配置的邮箱注册后应当是 admin: %+v", out)
	}
}

// 其他邮箱注册照旧是普通用户——否则这就是个人人可用的提权后门。
func TestBootstrapAdminEmailDoesNotPromoteOthers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{JWTSecret: "test-secret", BootstrapAdminEmail: "boss@example.com"}
	r := NewRouter(db, cfg)

	token := registerAndLogin(t, r, "someone-else@example.com", "secret12345")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["role"] != "user" {
		t.Fatalf("其他邮箱不该被提权: %+v", out)
	}
}
