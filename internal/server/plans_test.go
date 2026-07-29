package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"image-backend/internal/model"
)

func TestGetPlansReturnsEnabledOnlyAndHidesPriceID(t *testing.T) {
	r, db := setupRouterWithDB(t)
	if err := db.Model(&model.Plan{}).Where("id = ?", "max").Update("enabled", false).Error; err != nil {
		t.Fatalf("下架 max: %v", err)
	}
	if err := db.Model(&model.Plan{}).Where("id = ?", "pro").Update("stripe_price_id", "price_secret").Error; err != nil {
		t.Fatalf("写入 pro 的 Price ID: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/plans", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `"max"`) {
		t.Error("禁用的档位不该出现在公开列表里")
	}
	if strings.Contains(body, "price_secret") {
		t.Error("stripe_price_id 是服务端细节，不该出现在响应里")
	}
	if !strings.Contains(body, "starter") || !strings.Contains(body, "pro") {
		t.Errorf("启用的档位应当返回，得到 %s", body)
	}

	var parsed struct {
		Plans []struct {
			ID             string `json:"id"`
			DisplayName    string `json:"displayName"`
			PriceUSDCents  int    `json:"priceUsdCents"`
			MonthlyCredits int    `json:"monthlyCredits"`
		} `json:"plans"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, body)
	}
	if len(parsed.Plans) != 2 {
		t.Fatalf("档位数量: got %d, want 2; body=%s", len(parsed.Plans), body)
	}
	// sort_order 升序：starter(10) 在 pro(20) 之前。
	if parsed.Plans[0].ID != "starter" || parsed.Plans[1].ID != "pro" {
		t.Fatalf("顺序应按 sortOrder 升序: %+v", parsed.Plans)
	}
	if parsed.Plans[0].DisplayName != "Starter" || parsed.Plans[0].PriceUSDCents != 990 || parsed.Plans[0].MonthlyCredits != 200 {
		t.Fatalf("starter 内容错误: %+v", parsed.Plans[0])
	}
}

// TestGetPlansIsPublic：不带 token 也应当 200——定价页在未登录时就要能看。
func TestGetPlansIsPublic(t *testing.T) {
	r := setupRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/plans", nil)
	// 故意不设置 Authorization 头。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("未登录访问 /plans 应当 200，得到 %d：%s", w.Code, w.Body.String())
	}
}
