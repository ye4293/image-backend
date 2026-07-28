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
)

// setupRouterWithDB 同时返回 db，供需要直接操作数据（发放次数、提权）的测试使用。
func setupRouterWithDB(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{JWTSecret: "test-secret"}
	return NewRouter(db, cfg), db
}

func setupRouter(t *testing.T) *gin.Engine {
	t.Helper()
	r, _ := setupRouterWithDB(t)
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

func TestHealth(t *testing.T) {
	r := setupRouter(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}
