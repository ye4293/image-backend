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
	// 与库里的实际值比，不写死价格与次数：两者都是运营可调的产品参数，写死会让一次
	// 正常调价把这条"接口是否正确投影 plans 行"的测试搞红。
	var seeded model.Plan
	if err := db.Where("id = ?", "starter").First(&seeded).Error; err != nil {
		t.Fatalf("读 seed 档位: %v", err)
	}
	got := parsed.Plans[0]
	if got.DisplayName != seeded.DisplayName ||
		got.PriceUSDCents != seeded.PriceUSDCents ||
		got.MonthlyCredits != seeded.MonthlyCredits {
		t.Fatalf("starter 内容错误: got %+v, want name=%s price=%d credits=%d",
			got, seeded.DisplayName, seeded.PriceUSDCents, seeded.MonthlyCredits)
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
