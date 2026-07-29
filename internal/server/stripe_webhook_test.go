package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	stripe "github.com/stripe/stripe-go/v86"
	"gorm.io/gorm"

	"image-backend/internal/config"
	"image-backend/internal/model"
)

const testWebhookSecret = "whsec_test_only_not_a_real_secret"

// withWebhookSecret 注入 webhook 签名密钥。**不需要真实的 whsec_**：验签只是
// HMAC，测试自己用同一个字符串签名即可，全程不联网。
func withWebhookSecret(cfg *config.Config) {
	cfg.StripeWebhookSecret = testWebhookSecret
}

// signPayload 构造 Stripe 的签名头：t=<unix>,v1=<hex hmac-sha256("<t>.<payload>")>。
func signPayload(t *testing.T, payload []byte, secret string) string {
	t.Helper()
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", ts, payload)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// eventPayload 造一个形状合法的事件载荷。
//
// api_version 必须和 SDK 的 stripe.APIVersion 属于同一个 release train，否则
// webhook.ConstructEvent 会以"版本不兼容"报错——那样测试就分不清失败是因为
// 验签还是因为版本，会把签名回归测试变成永假。
func eventPayload(id, evType string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%q,"object":"event","api_version":%q,"type":%q,"data":{"object":{}}}`,
		id, stripe.APIVersion, evType))
}

// postWebhook 用给定的签名头投递一次 webhook。sig 为空表示完全不带头。
func postWebhook(r *gin.Engine, payload []byte, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/stripe", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("Stripe-Signature", sig)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func countStripeEvents(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.StripeEvent{}).Count(&n).Error; err != nil {
		t.Fatalf("统计 stripe_events: %v", err)
	}
	return n
}

// TestWebhookRejectsBadSignature：用错误的密钥签名 → 400，且**不得**在
// stripe_events 里留下记录。
//
// 留了记录就等于把伪造事件的 id 占掉，真事件到达时会被当成重复而丢弃。
func TestWebhookRejectsBadSignature(t *testing.T) {
	r, db := setupRouterWithDB(t, withWebhookSecret)
	payload := eventPayload("evt_forged_1", "invoice.paid")

	w := postWebhook(r, payload, signPayload(t, payload, "whsec_attacker_key"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("错误密钥签名应当 400，得到 %d：%s", w.Code, w.Body.String())
	}
	if n := countStripeEvents(t, db); n != 0 {
		t.Errorf("验签失败不得留下幂等记录，stripe_events 有 %d 行", n)
	}
}

// TestWebhookRejectsMissingSignature：无 Stripe-Signature 头 → 400。
func TestWebhookRejectsMissingSignature(t *testing.T) {
	r, db := setupRouterWithDB(t, withWebhookSecret)
	payload := eventPayload("evt_unsigned_1", "invoice.paid")

	w := postWebhook(r, payload, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺签名头应当 400，得到 %d：%s", w.Code, w.Body.String())
	}
	if n := countStripeEvents(t, db); n != 0 {
		t.Errorf("缺签名头不得留下幂等记录，stripe_events 有 %d 行", n)
	}
}

// TestWebhookIsIdempotent：同一 event id 投递两次 → 都 200，但 stripe_events
// 只有一行，且第二次走的是"重复"分支（没有再跑一遍业务处理）。
func TestWebhookIsIdempotent(t *testing.T) {
	r, db := setupRouterWithDB(t, withWebhookSecret)
	payload := eventPayload("evt_dup_1", "customer.subscription.updated")
	sig := signPayload(t, payload, testWebhookSecret)

	first := postWebhook(r, payload, sig)
	if first.Code != http.StatusOK {
		t.Fatalf("首次投递应当 200，得到 %d：%s", first.Code, first.Body.String())
	}
	second := postWebhook(r, payload, sig)
	if second.Code != http.StatusOK {
		t.Fatalf("重复投递应当 200，得到 %d：%s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), `"duplicate":true`) {
		t.Errorf("重复投递应当标记 duplicate，得到 %s", second.Body.String())
	}
	if strings.Contains(first.Body.String(), `"duplicate":true`) {
		t.Errorf("首次投递不该标记 duplicate，得到 %s", first.Body.String())
	}
	if n := countStripeEvents(t, db); n != 1 {
		t.Errorf("同一 event id 只应留一行，stripe_events 有 %d 行", n)
	}
}

// TestWebhookReturns200ForUnhandledType：我们不关心的事件类型 → 200。
//
// 返回 5xx 会让 Stripe 一直重投我们根本不处理的事件。
func TestWebhookReturns200ForUnhandledType(t *testing.T) {
	r, _ := setupRouterWithDB(t, withWebhookSecret)
	payload := eventPayload("evt_unhandled_1", "payment_intent.created")

	w := postWebhook(r, payload, signPayload(t, payload, testWebhookSecret))
	if w.Code != http.StatusOK {
		t.Fatalf("未处理的事件类型应当 200，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestWebhookDoesNotRequireAuth：不带 cookie/Authorization → 不是 401。
//
// Stripe 不带我们的凭证，安全性完全由验签保证。若这条路由被挂到认证中间件下，
// 线上所有 webhook 都会 401 被丢弃。
func TestWebhookDoesNotRequireAuth(t *testing.T) {
	r, _ := setupRouterWithDB(t, withWebhookSecret)
	payload := eventPayload("evt_noauth_1", "customer.subscription.updated")

	w := postWebhook(r, payload, signPayload(t, payload, testWebhookSecret))
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("webhook 不得要求认证，得到 401：%s", w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("合法签名应当 200，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestWebhookWhenSecretUnconfiguredReturns503：没配 STRIPE_WEBHOOK_SECRET 时
// 不能拿空密钥去验签（空密钥下攻击者能自己算出合法签名），直接 503。
func TestWebhookWhenSecretUnconfiguredReturns503(t *testing.T) {
	r, db := setupRouterWithDB(t) // 默认配置不含 webhook 密钥
	payload := eventPayload("evt_nosecret_1", "invoice.paid")

	w := postWebhook(r, payload, signPayload(t, payload, ""))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置 webhook 密钥应当 503，得到 %d：%s", w.Code, w.Body.String())
	}
	if n := countStripeEvents(t, db); n != 0 {
		t.Errorf("未配置时不得留下幂等记录，stripe_events 有 %d 行", n)
	}
}
