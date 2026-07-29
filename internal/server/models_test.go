package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"image-backend/internal/model"
)

func TestListModelsReturnsSeededFlux(t *testing.T) {
	r, db := setupRouterWithDB(t)

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
	// 与库里的实际值比，不写死 credits：它是运营可调的产品参数（PATCH /admin/models/:id），
	// 写死会让一次正常的调价把这条"接口是否正确返回 seed 内容"的测试搞红——真正的
	// 信号会被这类噪音埋掉。
	var seeded model.ImageModel
	if err := db.Where("id = ?", "flux-2-max").First(&seeded).Error; err != nil {
		t.Fatalf("读 seed 模型: %v", err)
	}
	if m.ID != seeded.ID || m.Name != seeded.DisplayName || m.Credits != seeded.Credits {
		t.Fatalf("模型内容错误: got %+v, want id=%s name=%s credits=%d",
			m, seeded.ID, seeded.DisplayName, seeded.Credits)
	}
}
