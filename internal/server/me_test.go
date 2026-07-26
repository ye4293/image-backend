package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMe(t *testing.T) {
	r := setupRouter(t)
	postJSON(r, "/api/v1/auth/register", `{"email":"me@test.com","password":"secret123"}`)
	w := postJSON(r, "/api/v1/auth/login", `{"email":"me@test.com","password":"secret123"}`)
	var loginResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &loginResp); err != nil {
		t.Fatal(err)
	}
	token, _ := loginResp["token"].(string)

	// 无 token → 401
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Code)
	}

	// 带 token → 200
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	var meResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &meResp); err != nil {
		t.Fatal(err)
	}
	if meResp["email"] != "me@test.com" {
		t.Fatalf("unexpected email: %v", meResp["email"])
	}
}
