package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"image-backend/internal/config"
	"image-backend/internal/model"
)

// withFakeStripe 让路由构造出一个**非 nil** 的 billing.Client。
//
// 用假的 sk_test_ 密钥是安全的：客户端只在真正 Create 时才发请求，构造本身不
// 联网。本文件里的测试都在到达 Stripe 之前就返回了（未知档位、禁用档位、
// Price 未播种、没有 customer），所以全程无网络调用。
func withFakeStripe(cfg *config.Config) {
	cfg.StripeSecretKey = "sk_test_fake_not_used"
	cfg.AppBaseURL = "http://localhost:3000"
}

func postAuthed(r *gin.Engine, token, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestSubscribeRequiresAuth：未登录不得下单。
func TestSubscribeRequiresAuth(t *testing.T) {
	r := setupRouter(t, withFakeStripe)

	w := postAuthed(r, "", "/api/v1/billing/subscribe", `{"planId":"pro"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应当 401，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestSubscribeWhenBillingUnconfiguredReturns503：本地没配 Stripe 是正常状态，
// 不能 500，要给出明确文案。
func TestSubscribeWhenBillingUnconfiguredReturns503(t *testing.T) {
	r := setupRouter(t) // 默认配置不含 Stripe 密钥 → billing.New 返回 nil
	token := registerAndLogin(t, r, "billing-off@example.com", "secret12345")

	w := postAuthed(r, token, "/api/v1/billing/subscribe", `{"planId":"pro"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置计费应当 503，得到 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("文案应当说明计费未配置，得到 %s", w.Body.String())
	}
}

func TestSubscribeRejectsUnknownPlan(t *testing.T) {
	r := setupRouter(t, withFakeStripe)
	token := registerAndLogin(t, r, "sub-unknown@example.com", "secret12345")

	w := postAuthed(r, token, "/api/v1/billing/subscribe", `{"planId":"does-not-exist"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知档位应当 400，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestSubscribeRejectsDisabledPlan：运营下架某档后，前端缓存的旧页面仍可能发来
// 请求——即使 Price ID 已播种，也不能下单。
func TestSubscribeRejectsDisabledPlan(t *testing.T) {
	r, db := setupRouterWithDB(t, withFakeStripe)
	token := registerAndLogin(t, r, "sub-disabled@example.com", "secret12345")
	if err := db.Model(&model.Plan{}).Where("id = ?", "pro").
		Updates(map[string]any{"enabled": false, "stripe_price_id": "price_seeded"}).Error; err != nil {
		t.Fatalf("下架 pro: %v", err)
	}

	w := postAuthed(r, token, "/api/v1/billing/subscribe", `{"planId":"pro"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("禁用档位应当 400，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestSubscribeWhenPriceNotSeededReturns503：Price 未播种是我们的运维状态问题，
// 不是用户参数错误。回 400 会让前端显示"套餐无效"，误导排查方向。
func TestSubscribeWhenPriceNotSeededReturns503(t *testing.T) {
	r, db := setupRouterWithDB(t, withFakeStripe)
	token := registerAndLogin(t, r, "sub-unseeded@example.com", "secret12345")
	// 确认前置：播种命令没跑过，stripe_price_id 为空。
	var plan model.Plan
	if err := db.Where("id = ?", "pro").First(&plan).Error; err != nil {
		t.Fatalf("查 pro: %v", err)
	}
	if plan.StripePriceID != "" {
		t.Fatalf("夹具前提失效：pro 的 stripe_price_id 应为空，得到 %q", plan.StripePriceID)
	}

	w := postAuthed(r, token, "/api/v1/billing/subscribe", `{"planId":"pro"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Price 未播种应当 503，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestPortalWithoutCustomerReturns400：没进过结账流程的用户，Portal 里什么都
// 没有，跳过去只会看到空页面——所以直接 400，不去造一个 customer。
func TestPortalWithoutCustomerReturns400(t *testing.T) {
	r, db := setupRouterWithDB(t, withFakeStripe)
	token := registerAndLogin(t, r, "portal-nocust@example.com", "secret12345")
	var user model.User
	if err := db.Where("email = ?", "portal-nocust@example.com").First(&user).Error; err != nil {
		t.Fatalf("查用户: %v", err)
	}
	if user.StripeCustomerID != nil {
		t.Fatalf("夹具前提失效：新用户不该有 stripe_customer_id，得到 %v", *user.StripeCustomerID)
	}

	w := postAuthed(r, token, "/api/v1/billing/portal", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("无 customer 应当 400，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestPortalWhenBillingUnconfiguredReturns503：与 Subscribe 一致，不能 500。
func TestPortalWhenBillingUnconfiguredReturns503(t *testing.T) {
	r := setupRouter(t)
	token := registerAndLogin(t, r, "portal-off@example.com", "secret12345")

	w := postAuthed(r, token, "/api/v1/billing/portal", `{}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置计费应当 503，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestPortalRequiresAuth：未登录不得进 Portal。
func TestPortalRequiresAuth(t *testing.T) {
	r := setupRouter(t, withFakeStripe)

	w := postAuthed(r, "", "/api/v1/billing/portal", `{}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应当 401，得到 %d：%s", w.Code, w.Body.String())
	}
}
