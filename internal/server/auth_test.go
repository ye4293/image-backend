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

	// 邮箱大小写归一化：混合大小写注册后响应应为全小写
	w = postJSON(r, "/api/v1/auth/register", `{"email":"Mixed@Test.com","password":"secret123"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d, body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["email"] != "mixed@test.com" {
		t.Fatalf("expected normalized email mixed@test.com, got %v", resp["email"])
	}

	// 归一化后再用全小写注册应视为重复 → 409
	w = postJSON(r, "/api/v1/auth/register", `{"email":"mixed@test.com","password":"secret123"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestLogin(t *testing.T) {
	r := setupRouter(t)
	postJSON(r, "/api/v1/auth/register", `{"email":"login@test.com","password":"secret123"}`)

	w := postJSON(r, "/api/v1/auth/login", `{"email":"login@test.com","password":"secret123"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if token, _ := resp["token"].(string); token == "" {
		t.Fatal("expected non-empty token")
	}

	// 密码错误 → 401
	w = postJSON(r, "/api/v1/auth/login", `{"email":"login@test.com","password":"wrongpass"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// 不存在的用户 → 401
	w = postJSON(r, "/api/v1/auth/login", `{"email":"nobody@test.com","password":"secret123"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
