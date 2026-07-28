package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListModelsReturnsSeededFlux(t *testing.T) {
	r := setupRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Models []struct {
			ID                   string `json:"id"`
			Name                 string `json:"name"`
			Credits              int    `json:"credits"`
			SupportsImageToImage bool   `json:"supportsImageToImage"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, w.Body.String())
	}
	if len(body.Models) != 1 {
		t.Fatalf("模型数量: got %d, want 1", len(body.Models))
	}
	m := body.Models[0]
	if m.ID != "flux-2-max" || m.Name != "Flux 2 Max" || m.Credits != 1 {
		t.Fatalf("模型内容错误: %+v", m)
	}
}
