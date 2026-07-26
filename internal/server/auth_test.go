package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func postJSON(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestRegister(t *testing.T) {
	r := setupRouter(t)

	w := postJSON(r, "/api/v1/auth/register", `{"email":"user@test.com","password":"secret123"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["email"] != "user@test.com" {
		t.Fatalf("unexpected email: %v", resp["email"])
	}

	// 重复邮箱 → 409
	w = postJSON(r, "/api/v1/auth/register", `{"email":"user@test.com","password":"secret123"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}

	// 密码过短 → 400
	w = postJSON(r, "/api/v1/auth/register", `{"email":"x@test.com","password":"short"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
