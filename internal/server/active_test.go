package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"image-backend/internal/model"
)

func TestBannedUserIsRejected(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "banned@example.com", "secret12345")

	if err := db.Model(&model.User{}).Where("email = ?", "banned@example.com").
		Update("status", "banned").Error; err != nil {
		t.Fatalf("封禁: %v", err)
	}

	// 封禁必须**立即**生效，不能等 JWT 过期（7 天）。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("被封禁用户应当 403: got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestActiveUserPassesThrough(t *testing.T) {
	r := setupRouter(t)
	token := registerAndLogin(t, r, "active@example.com", "secret12345")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("正常用户应当放行: got %d; body=%s", w.Code, w.Body.String())
	}
}
