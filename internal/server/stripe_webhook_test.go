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

// TestWebhookOldAPIVersionIsNotReportedAsBadSignature 版本不匹配必须与验签失败
// **区分开**，且不能被静默放过。
//
// 这是一条真实会踩的路：Stripe 账号的默认 API 版本常年停在建号时那一版（本项目
// 实测是 2023-10-16），而 `stripe listen` 默认就按账号默认版本转发。若把版本错误
// 报成 "invalid signature"，排查会奔向完全错误的方向——签名明明是好的。
//
// 也不能当成"未知事件"回 200：旧版本的载荷是不同的结构（2023-10-16 的发票把
// subscription 放在顶层，本代码读的是 parent.subscription_details），硬解会得到
// 一堆 nil，表现为"确认收到了付款但额度没发"——静默丢钱。
func TestWebhookOldAPIVersionIsNotReportedAsBadSignature(t *testing.T) {
	const secret = "whsec_test"
	r, db := setupRouterWithDB(t, func(c *config.Config) { c.StripeWebhookSecret = secret })

	// 签名是**正确**的，只有 api_version 是旧的。
	payload := []byte(`{"id":"evt_oldver","object":"event","api_version":"2023-10-16",` +
		`"type":"invoice.paid","data":{"object":{}}}`)
	w := postWebhook(r, payload, signPayload(t, payload, secret))

	if w.Code == http.StatusBadRequest {
		t.Errorf("版本不匹配被报成了 400/invalid signature，会把排查引向错误方向；body=%s", w.Body.String())
	}
	if w.Code == http.StatusOK {
		t.Errorf("版本不匹配不能回 200——那等于确认收到付款却永不发额度（静默丢钱）；body=%s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("期望 500（让失败挂在 Stripe 失败列表里等人处理），得到 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "version") {
		t.Errorf("响应里应当指明是版本问题，得到 %s", w.Body.String())
	}
	// 不能留幂等记录：留了的话，将来修好版本配置再重放这个事件会被当成"已处理"丢弃。
	if n := countStripeEvents(t, db); n != 0 {
		t.Errorf("版本不匹配不得写入 stripe_events（否则修好配置后无法重放），实际 %d 行", n)
	}
}

// TestWebhookAcceptsCurrentAPIVersion 正向对照：版本对得上时照常处理。
//
// 没有这条，上一条测试有可能因为"所有事件都被拒"而假绿。
func TestWebhookAcceptsCurrentAPIVersion(t *testing.T) {
	const secret = "whsec_test"
	r, db := setupRouterWithDB(t, func(c *config.Config) { c.StripeWebhookSecret = secret })

	payload := eventPayload("evt_curver", "invoice.paid")
	w := postWebhook(r, payload, signPayload(t, payload, secret))
	if w.Code != http.StatusOK {
		t.Fatalf("当前版本的事件应当 200，得到 %d：%s", w.Code, w.Body.String())
	}
	if n := countStripeEvents(t, db); n != 1 {
		t.Errorf("成功处理应当留下 1 行幂等记录，实际 %d 行", n)
	}
}

// TestWebhookAcceptsSameReleaseTrainDifferentDate 同一发布列车、不同日期必须放行。
//
// **这条是生产阻塞级的。** Dashboard 创建 webhook endpoint 时能选的 API 版本里
// 没有一项等于 SDK 钉的那一串（SDK 是 2026-06-24.dahlia，下拉给的是
// 2026-07-29.dahlia「最新版本」、2026-06-24.preview、2024-11-20.acacia，以及账号
// 建号时的老版本）。原先那道 `ev.APIVersion != stripe.APIVersion` 的精确相等检查
// 会把四个选项**全部**拒掉：每笔真实付款回 500、Stripe 无限重投、额度永远发不出，
// 而用户已经被扣款。
//
// 按发布列车比对是 Stripe 自己的兼容语义（同列车内载荷结构稳定，stripe-go 的
// isCompatibleAPIVersion 也只比列车名），不是为了让测试变绿而放松标准。
func TestWebhookAcceptsSameReleaseTrainDifferentDate(t *testing.T) {
	const secret = "whsec_test"
	r, db := setupRouterWithDB(t, func(c *config.Config) { c.StripeWebhookSecret = secret })

	// 故意用一个与 SDK **日期不同**但列车相同的版本串，模拟 Dashboard 里那个
	// "最新版本"。日期写死成未来值，避免哪天 SDK 升级后这条测试悄悄退化成"与当前
	// 版本完全相等"——那样它就不再考察列车比对了。
	futureSameTrain := "2099-01-01." + stripe.APIMajorVersion
	if futureSameTrain == stripe.APIVersion {
		t.Fatalf("构造的版本串不该等于 SDK 版本 %q，否则这条测试考察不到列车比对", stripe.APIVersion)
	}
	payload := []byte(fmt.Sprintf(
		`{"id":"evt_sametrain","object":"event","api_version":%q,"type":"invoice.paid","data":{"object":{}}}`,
		futureSameTrain))

	w := postWebhook(r, payload, signPayload(t, payload, secret))
	if w.Code != http.StatusOK {
		t.Fatalf("同一发布列车（%s）的事件必须放行，得到 %d：%s\n"+
			"若这里失败，Dashboard 里能选的每一个 API 版本都会被拒——用户付款后拿不到额度",
			futureSameTrain, w.Code, w.Body.String())
	}
	if n := countStripeEvents(t, db); n != 1 {
		t.Errorf("成功处理应当留下 1 行幂等记录，实际 %d 行", n)
	}
}

// TestWebhookRejectsOtherReleaseTrains 别的发布列车仍然要拦住。
//
// 放宽成"按列车比对"之后，最容易顺手放过的就是这些：它们都带列车后缀、长得很像
// 合法值，但载荷结构与 dahlia 不同，硬解会得到一堆 nil，表现为"事件收到了但什么
// 都没发生"——比直接报错难查得多。
func TestWebhookRejectsOtherReleaseTrains(t *testing.T) {
	const secret = "whsec_test"
	for _, ver := range []string{
		"2024-11-20.acacia",  // 更老的列车
		"2026-06-24.preview", // 预览列车，与 dahlia 不是一回事
		"2025-01-01.basil",
		"2023-10-16", // 没有列车后缀的老版本
	} {
		r, db := setupRouterWithDB(t, func(c *config.Config) { c.StripeWebhookSecret = secret })
		payload := []byte(fmt.Sprintf(
			`{"id":"evt_x","object":"event","api_version":%q,"type":"invoice.paid","data":{"object":{}}}`, ver))
		w := postWebhook(r, payload, signPayload(t, payload, secret))

		if w.Code != http.StatusInternalServerError {
			t.Errorf("%q 属于别的发布列车，必须回 500 而不是 %d——载荷结构不同，"+
				"放过去会静默地什么都不做；body=%s", ver, w.Code, w.Body.String())
		}
		// 同前：不能留幂等记录，否则修好配置后重放会被当成"已处理"丢弃。
		if n := countStripeEvents(t, db); n != 0 {
			t.Errorf("%q 被拒时不得写入 stripe_events，实际 %d 行", ver, n)
		}
	}
}
