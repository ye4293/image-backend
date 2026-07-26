package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"image-backend/internal/config"
	"image-backend/internal/database"
)

func setupRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{JWTSecret: "test-secret"}
	return NewRouter(db, cfg)
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
