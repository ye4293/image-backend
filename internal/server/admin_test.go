package server

import (
	"encoding/json"
	"fmt"
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

func TestAdminGrantNegativeIsRejectedAsBadRequest(t *testing.T) {
	r, db := setupRouterWithDB(t)
	adminToken := registerAndLogin(t, r, "admin-neg@example.com", "secret12345")
	registerAndLogin(t, r, "target-neg@example.com", "secret12345")
	if err := db.Model(&model.User{}).Where("email = ?", "admin-neg@example.com").
		Update("role", "admin").Error; err != nil {
		t.Fatalf("提权: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credits",
		strings.NewReader(`{"email":"target-neg@example.com","monthly":-100,"addon":0}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 负数是**参数错误**，必须是 400；而数据库故障必须是 500。此前两者都被当成
	// 400 并原样回传 err.Error()，既泄露内部信息，又让运维以为是调用方写错了参数。
	if w.Code != http.StatusBadRequest {
		t.Fatalf("负数发放应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "40000") {
		t.Fatalf("应当返回 40000: body=%s", w.Body.String())
	}
}

func TestAdminGrantRecordsWhichAdmin(t *testing.T) {
	r, db := setupRouterWithDB(t)
	adminToken := registerAndLogin(t, r, "admin-audit@example.com", "secret12345")
	registerAndLogin(t, r, "target-audit@example.com", "secret12345")
	if err := db.Model(&model.User{}).Where("email = ?", "admin-audit@example.com").
		Update("role", "admin").Error; err != nil {
		t.Fatalf("提权: %v", err)
	}
	var admin model.User
	if err := db.Where("email = ?", "admin-audit@example.com").First(&admin).Error; err != nil {
		t.Fatalf("查 admin: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credits",
		strings.NewReader(`{"email":"target-audit@example.com","monthly":5,"addon":0}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("发放应当成功: got %d; body=%s", w.Code, w.Body.String())
	}

	// 流水必须能回答"谁发的"，否则等真有钱之后就是审计缺口。
	var tx model.CreditTransaction
	if err := db.Where("type = ?", model.TxAdminGrant).First(&tx).Error; err != nil {
		t.Fatalf("缺少发放流水: %v", err)
	}
	want := fmt.Sprintf("admin grant by user #%d", admin.ID)
	if tx.Note != want {
		t.Fatalf("流水未记录操作者: got %q, want %q", tx.Note, want)
	}
}
