package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"image-backend/internal/model"
)

func TestAdminGrantRequiresAdminRole(t *testing.T) {
	r := setupRouter(t)
	token := registerAndLogin(t, r, "plain@example.com", "secret12345")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credits",
		strings.NewReader(`{"email":"plain@example.com","monthly":10,"addon":0}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("普通用户应当 403: got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestAdminGrantAddsCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	adminToken := registerAndLogin(t, r, "admin@example.com", "secret12345")
	registerAndLogin(t, r, "target@example.com", "secret12345")

	// 提权：注册接口不会创建 admin，只能直接改库
	if err := db.Model(&model.User{}).Where("email = ?", "admin@example.com").
		Update("role", "admin").Error; err != nil {
		t.Fatalf("提权: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credits",
		strings.NewReader(`{"email":"target@example.com","monthly":50,"addon":10}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Credits struct {
			Monthly int `json:"monthly"`
			Addon   int `json:"addon"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v; body=%s", err, w.Body.String())
	}
	if body.Credits.Monthly != 50 || body.Credits.Addon != 10 {
		t.Fatalf("发放后余额: got %d/%d, want 50/10", body.Credits.Monthly, body.Credits.Addon)
	}
}

func TestAdminGrantUnknownEmailReturns404(t *testing.T) {
	r, db := setupRouterWithDB(t)
	adminToken := registerAndLogin(t, r, "admin2@example.com", "secret12345")
	if err := db.Model(&model.User{}).Where("email = ?", "admin2@example.com").
		Update("role", "admin").Error; err != nil {
		t.Fatalf("提权: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credits",
		strings.NewReader(`{"email":"nobody@example.com","monthly":10,"addon":0}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("未知邮箱应当 404: got %d; body=%s", w.Code, w.Body.String())
	}
}
