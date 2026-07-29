package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	stripe "github.com/stripe/stripe-go/v86"
	"gorm.io/gorm"

	"image-backend/internal/billing"
	"image-backend/internal/config"
	"image-backend/internal/credit"
	"image-backend/internal/database"
	"image-backend/internal/model"
)

// 本文件测的是 Task 8：五个事件的业务处理。签名/幂等/版本校验的测试在
// stripe_webhook_test.go，其中的 signPayload / postWebhook / countStripeEvents
// 在这里复用。

// fakeSubscriptionFetcher 是 billing.SubscriptionFetcher 的假实现。
//
// **拉取订阅必须可注入**：invoice.paid 的业务规则（按 Price 反查档位、交叉校验
// user_id）全都依赖"订阅当前长什么样"，而那份数据来自一次 API 调用。真的去请求
// api.stripe.com 会让这些测试变慢、要密钥、且随网络抖动变红——于是它们迟早会被
// 跳过，而这几条恰恰是最不能失守的规则。
type fakeSubscriptionFetcher struct {
	sub    *stripe.Subscription
	err    error
	calls  int
	lastID string
}

func (f *fakeSubscriptionFetcher) FetchSubscription(_ context.Context, id string) (*stripe.Subscription, error) {
	f.calls++
	f.lastID = id
	if f.err != nil {
		return nil, f.err
	}
	return f.sub, nil
}

// fakeSubscription 造一个 v86 形状的订阅：周期与 Price 都挂在 **Items.Data[0]**
// 上，不在订阅顶层（v86 把它们下移到了 item 上）。
func fakeSubscription(subID, customerID, priceID, status string, cancelAtPeriodEnd bool, meta map[string]string) *stripe.Subscription {
	now := time.Now()
	return &stripe.Subscription{
		ID:                subID,
		Customer:          &stripe.Customer{ID: customerID},
		Status:            stripe.SubscriptionStatus(status),
		CancelAtPeriodEnd: cancelAtPeriodEnd,
		Metadata:          meta,
		Items: &stripe.SubscriptionItemList{
			Data: []*stripe.SubscriptionItem{{
				ID:                 "si_test",
				CurrentPeriodStart: now.Unix(),
				CurrentPeriodEnd:   now.Add(30 * 24 * time.Hour).Unix(),
				Price:              &stripe.Price{ID: priceID},
			}},
		},
	}
}

// setupWebhookRouter 建一个带 webhook 密钥、并注入假订阅拉取器的路由。
func setupWebhookRouter(t *testing.T, subs billing.SubscriptionFetcher) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{JWTSecret: "test-secret", StripeWebhookSecret: testWebhookSecret}
	return NewRouter(db, cfg, WithSubscriptionFetcher(subs)), db
}

// customEventPayload 造一个带任意 data.object 的事件载荷。
//
// api_version 必须是 SDK 的版本，否则会被 Handle 的版本校验挡在 500，测试根本
// 走不到业务逻辑（而失败信息看起来像是业务出错）。
func customEventPayload(id, evType, objJSON string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"object":"event","api_version":%q,"type":%q,"data":{"object":%s}}`,
		id, stripe.APIVersion, evType, objJSON))
}

// invoiceObjectJSON 造 v86 形状的发票：订阅 id 与 metadata 都在
// **parent.subscription_details** 下，不在顶层。
func invoiceObjectJSON(invID, customerID, subID, metadataJSON string) string {
	return fmt.Sprintf(`{"id":%q,"object":"invoice","customer":%q,"status":"paid",`+
		`"amount_paid":2990,"billing_reason":"subscription_create",`+
		`"parent":{"type":"subscription_details","subscription_details":`+
		`{"subscription":%q,"metadata":%s}}}`, invID, customerID, subID, metadataJSON)
}

// subscriptionObjectJSON 造 v86 形状的订阅 JSON（用于 subscription.updated/deleted）。
func subscriptionObjectJSON(subID, customerID, priceID, status string, cancelAtPeriodEnd bool, userID uint) string {
	now := time.Now()
	return fmt.Sprintf(`{"id":%q,"object":"subscription","customer":%q,"status":%q,`+
		`"cancel_at_period_end":%t,"metadata":{"user_id":"%d"},`+
		`"items":{"object":"list","data":[{"id":"si_test","object":"subscription_item",`+
		`"current_period_start":%d,"current_period_end":%d,"price":{"id":%q,"object":"price"}}]}}`,
		subID, customerID, status, cancelAtPeriodEnd, userID,
		now.Unix(), now.Add(30*24*time.Hour).Unix(), priceID)
}

func createUserID(t *testing.T, r *gin.Engine, db *gorm.DB, email string) uint {
	t.Helper()
	registerAndLogin(t, r, email, "secret12345")
	var u model.User
	if err := db.Where("email = ?", email).First(&u).Error; err != nil {
		t.Fatalf("查用户 %s: %v", email, err)
	}
	return u.ID
}

// seedPriceID 把某档位的 stripe_price_id 设成给定值（模拟播种命令跑过）。
func seedPriceID(t *testing.T, db *gorm.DB, planID, priceID string) {
	t.Helper()
	if err := db.Model(&model.Plan{}).Where("id = ?", planID).
		Update("stripe_price_id", priceID).Error; err != nil {
		t.Fatalf("回填 %s 的 price id: %v", planID, err)
	}
}

func mustBalance(t *testing.T, db *gorm.DB, userID uint) model.CreditAccount {
	t.Helper()
	acct, err := credit.Balance(db, userID)
	if err != nil {
		t.Fatalf("读余额: %v", err)
	}
	return acct
}

func countCreditTx(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.CreditTransaction{}).Count(&n).Error; err != nil {
		t.Fatalf("统计 credit_transactions: %v", err)
	}
	return n
}

func mustSubscription(t *testing.T, db *gorm.DB, userID uint) model.Subscription {
	t.Helper()
	var sub model.Subscription
	if err := db.Where("user_id = ?", userID).First(&sub).Error; err != nil {
		t.Fatalf("查订阅行: %v", err)
	}
	return sub
}

func seedSubscriptionRow(t *testing.T, db *gorm.DB, userID uint, planID, subID, status string) {
	t.Helper()
	if err := db.Create(&model.Subscription{
		UserID: userID, PlanID: planID, StripeSubscriptionID: subID, Status: status,
		CurrentPeriodStart: time.Now().Add(-24 * time.Hour),
		CurrentPeriodEnd:   time.Now().Add(6 * 24 * time.Hour),
	}).Error; err != nil {
		t.Fatalf("夹具订阅行: %v", err)
	}
}

func deliverEvent(t *testing.T, r *gin.Engine, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	return postWebhook(r, payload, signPayload(t, payload, testWebhookSecret))
}

// TestInvoicePaidGrantsAndResetsMonthly：invoice.paid 是**唯一**的发放入口，
// 且是"设置"而不是"累加"。
//
// 用户已有月度 50 / 加量包 30，收到 pro 档的 invoice.paid → 月度设为 800
// （不是 850），加量包仍是 30。
func TestInvoicePaidGrantsAndResetsMonthly(t *testing.T) {
	fetcher := &fakeSubscriptionFetcher{}
	r, db := setupWebhookRouter(t, fetcher)
	uid := createUserID(t, r, db, "inv-paid@example.com")
	seedPriceID(t, db, "pro", "price_pro")
	if err := credit.Grant(db, uid, 50, 30, "夹具"); err != nil {
		t.Fatalf("夹具发放: %v", err)
	}
	fetcher.sub = fakeSubscription("sub_paid_1", "cus_paid_1", "price_pro", "active", false,
		map[string]string{"user_id": fmt.Sprint(uid)})

	payload := customEventPayload("evt_inv_paid_1", "invoice.paid",
		invoiceObjectJSON("in_1", "cus_paid_1", "sub_paid_1",
			fmt.Sprintf(`{"user_id":"%d","plan_id":"pro"}`, uid)))
	w := deliverEvent(t, r, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", w.Code, w.Body.String())
	}
	if fetcher.calls != 1 || fetcher.lastID != "sub_paid_1" {
		t.Errorf("应当拉取一次订阅 sub_paid_1，实际 calls=%d id=%q", fetcher.calls, fetcher.lastID)
	}

	acct := mustBalance(t, db, uid)
	if acct.MonthlyCredits != 800 {
		t.Errorf("月度应被**设置**为 800（不是累加成 850），得到 %d", acct.MonthlyCredits)
	}
	if acct.AddonCredits != 30 {
		t.Errorf("加量包不该被动，期望 30，得到 %d", acct.AddonCredits)
	}

	sub := mustSubscription(t, db, uid)
	if sub.PlanID != "pro" || sub.Status != "active" || sub.StripeSubscriptionID != "sub_paid_1" {
		t.Errorf("subscriptions 行不对：plan=%s status=%s subID=%s",
			sub.PlanID, sub.Status, sub.StripeSubscriptionID)
	}
	if sub.CurrentPeriodEnd.IsZero() {
		t.Error("周期结束时间应当从 items.data[0].current_period_end 同步，得到零值")
	}
}

// TestInvoicePaidResolvesPlanByPriceIDNotMetadata 这是最容易写错、也最难在真实
// 使用中发现的一条。
//
// metadata 是我们**下单时请求的**，Price 是用户**实际被计费的**。用户在 Billing
// Portal 里升档时 Stripe 换 Price 但**不改 metadata**——照 metadata 发就是"付了
// Pro 的钱、拿到 Starter 的次数"，而这条路径在只测新订阅时永远撞不到。
func TestInvoicePaidResolvesPlanByPriceIDNotMetadata(t *testing.T) {
	fetcher := &fakeSubscriptionFetcher{}
	r, db := setupWebhookRouter(t, fetcher)
	uid := createUserID(t, r, db, "inv-upgrade@example.com")
	seedPriceID(t, db, "starter", "price_starter")
	seedPriceID(t, db, "pro", "price_pro")
	// 订阅实际计费的是 pro 的 Price，但 metadata 还留着首次下单时的 starter。
	fetcher.sub = fakeSubscription("sub_up_1", "cus_up_1", "price_pro", "active", false,
		map[string]string{"user_id": fmt.Sprint(uid), "plan_id": "starter"})

	payload := customEventPayload("evt_inv_up_1", "invoice.paid",
		invoiceObjectJSON("in_2", "cus_up_1", "sub_up_1",
			fmt.Sprintf(`{"user_id":"%d","plan_id":"starter"}`, uid)))
	if w := deliverEvent(t, r, payload); w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", w.Code, w.Body.String())
	}

	if acct := mustBalance(t, db, uid); acct.MonthlyCredits != 800 {
		t.Errorf("应按**实际计费的 Price**（pro=800）发放，而不是 metadata 的 starter=200，得到 %d",
			acct.MonthlyCredits)
	}
	if sub := mustSubscription(t, db, uid); sub.PlanID != "pro" {
		t.Errorf("subscriptions.plan_id 也应按 Price 反查为 pro，得到 %s", sub.PlanID)
	}
}

// TestInvoicePaidWithUnknownPriceGrantsNothing：Price 在 plans 里查不到
// （Dashboard 手工建的订阅）→ 不发放、记日志、200。
//
// 宁可漏发等人工处理，也不要瞎猜一个档位。
func TestInvoicePaidWithUnknownPriceGrantsNothing(t *testing.T) {
	fetcher := &fakeSubscriptionFetcher{}
	r, db := setupWebhookRouter(t, fetcher)
	uid := createUserID(t, r, db, "inv-unknown@example.com")
	seedPriceID(t, db, "pro", "price_pro")
	fetcher.sub = fakeSubscription("sub_unk_1", "cus_unk_1", "price_made_in_dashboard", "active", false,
		map[string]string{"user_id": fmt.Sprint(uid)})

	payload := customEventPayload("evt_inv_unk_1", "invoice.paid",
		invoiceObjectJSON("in_3", "cus_unk_1", "sub_unk_1",
			fmt.Sprintf(`{"user_id":"%d","plan_id":"pro"}`, uid)))
	if w := deliverEvent(t, r, payload); w.Code != http.StatusOK {
		t.Fatalf("未知 Price 不该让 Stripe 白重投，期望 200，得到 %d：%s", w.Code, w.Body.String())
	}
	if acct := mustBalance(t, db, uid); acct.MonthlyCredits != 0 {
		t.Errorf("未知 Price 不得发放，得到月度 %d", acct.MonthlyCredits)
	}
	if n := countCreditTx(t, db); n != 0 {
		t.Errorf("未知 Price 不得产生流水，得到 %d 行", n)
	}
}

// TestInvoicePaidRejectsMismatchedUserIDs：metadata 的 user_id 与
// stripe_customer_id 反查到的用户不一致 → 报错告警，**不发放**。
//
// 不要猜哪个对——数据已经串了，猜都可能把 A 的额度发给 B。
func TestInvoicePaidRejectsMismatchedUserIDs(t *testing.T) {
	fetcher := &fakeSubscriptionFetcher{}
	r, db := setupWebhookRouter(t, fetcher)
	uidA := createUserID(t, r, db, "inv-a@example.com")
	uidB := createUserID(t, r, db, "inv-b@example.com")
	seedPriceID(t, db, "pro", "price_pro")
	// customer 绑在 B 上，metadata 却说是 A。
	const cus = "cus_crossed"
	if err := db.Model(&model.User{}).Where("id = ?", uidB).
		Update("stripe_customer_id", cus).Error; err != nil {
		t.Fatalf("绑 customer: %v", err)
	}
	fetcher.sub = fakeSubscription("sub_cross_1", cus, "price_pro", "active", false, nil)

	payload := customEventPayload("evt_inv_cross_1", "invoice.paid",
		invoiceObjectJSON("in_4", cus, "sub_cross_1",
			fmt.Sprintf(`{"user_id":"%d","plan_id":"pro"}`, uidA)))
	w := deliverEvent(t, r, payload)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("user_id 串了必须失败告警（500），得到 %d：%s", w.Code, w.Body.String())
	}
	if acct := mustBalance(t, db, uidA); acct.MonthlyCredits != 0 {
		t.Errorf("不得给 metadata 里的 A 发放，得到 %d", acct.MonthlyCredits)
	}
	if acct := mustBalance(t, db, uidB); acct.MonthlyCredits != 0 {
		t.Errorf("不得给 customer 对应的 B 发放，得到 %d", acct.MonthlyCredits)
	}
	if n := countStripeEvents(t, db); n != 0 {
		t.Errorf("业务失败时整个事务应当回滚（含幂等记录），否则重投会被当成已处理丢弃；实际 %d 行", n)
	}
}

// TestCheckoutCompletedBindsCustomerButGrantsNothing 这是整个里程碑最要紧的一条。
//
// checkout.session.completed 只写 users.stripe_customer_id。它和 invoice.paid
// 都发额度的话就是**双倍到账**，而这个 bug 只在真实付款时才暴露。
//
// 另一重要性：不回填 customer id，用户首次订阅后**进不了 Billing Portal**。
func TestCheckoutCompletedBindsCustomerButGrantsNothing(t *testing.T) {
	fetcher := &fakeSubscriptionFetcher{}
	r, db := setupWebhookRouter(t, fetcher)
	uid := createUserID(t, r, db, "checkout@example.com")
	seedPriceID(t, db, "pro", "price_pro")

	obj := fmt.Sprintf(`{"id":"cs_1","object":"checkout.session","mode":"subscription",`+
		`"customer":"cus_checkout_1","client_reference_id":"%d","subscription":"sub_ck_1",`+
		`"status":"complete","payment_status":"paid"}`, uid)
	payload := customEventPayload("evt_ck_1", "checkout.session.completed", obj)
	if w := deliverEvent(t, r, payload); w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", w.Code, w.Body.String())
	}

	var u model.User
	if err := db.First(&u, uid).Error; err != nil {
		t.Fatalf("查用户: %v", err)
	}
	if u.StripeCustomerID == nil || *u.StripeCustomerID != "cus_checkout_1" {
		t.Errorf("必须回填 stripe_customer_id（否则进不了 Billing Portal），得到 %v", u.StripeCustomerID)
	}
	if n := countCreditTx(t, db); n != 0 {
		t.Errorf("checkout.session.completed **绝不能**发额度——和 invoice.paid 都发就是双倍到账；"+
			"实际产生了 %d 条流水", n)
	}
	if acct := mustBalance(t, db, uid); acct.MonthlyCredits != 0 || acct.AddonCredits != 0 {
		t.Errorf("checkout 不得改变余额，得到 monthly=%d addon=%d", acct.MonthlyCredits, acct.AddonCredits)
	}
	if fetcher.calls != 0 {
		t.Errorf("checkout 分支不需要拉取订阅，实际调用 %d 次", fetcher.calls)
	}
}

// TestPaymentFailedSetsPastDueButKeepsCredits：status → past_due，次数**不变**。
//
// 用户可能只是卡过期，几天内会补款；清零会让他在补款前完全不能用。
func TestPaymentFailedSetsPastDueButKeepsCredits(t *testing.T) {
	fetcher := &fakeSubscriptionFetcher{}
	r, db := setupWebhookRouter(t, fetcher)
	uid := createUserID(t, r, db, "pay-failed@example.com")
	seedPriceID(t, db, "pro", "price_pro")
	if err := credit.Grant(db, uid, 800, 30, "夹具"); err != nil {
		t.Fatalf("夹具发放: %v", err)
	}
	seedSubscriptionRow(t, db, uid, "pro", "sub_pf_1", "active")

	payload := customEventPayload("evt_pf_1", "invoice.payment_failed",
		invoiceObjectJSON("in_5", "cus_pf_1", "sub_pf_1",
			fmt.Sprintf(`{"user_id":"%d","plan_id":"pro"}`, uid)))
	if w := deliverEvent(t, r, payload); w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", w.Code, w.Body.String())
	}

	if sub := mustSubscription(t, db, uid); sub.Status != "past_due" {
		t.Errorf("status 应改为 past_due，得到 %s", sub.Status)
	}
	acct := mustBalance(t, db, uid)
	if acct.MonthlyCredits != 800 || acct.AddonCredits != 30 {
		t.Errorf("扣款失败不得动次数（用户可能只是卡过期，几天内会补款），得到 monthly=%d addon=%d",
			acct.MonthlyCredits, acct.AddonCredits)
	}
}

// TestSubscriptionDeletedZerosMonthlyKeepsAddon：月度清零、**加量包保留**。
//
// 加量包是用户单独花钱买的，退订不能没收。
func TestSubscriptionDeletedZerosMonthlyKeepsAddon(t *testing.T) {
	fetcher := &fakeSubscriptionFetcher{}
	r, db := setupWebhookRouter(t, fetcher)
	uid := createUserID(t, r, db, "sub-deleted@example.com")
	seedPriceID(t, db, "pro", "price_pro")
	if err := credit.Grant(db, uid, 800, 30, "夹具"); err != nil {
		t.Fatalf("夹具发放: %v", err)
	}
	seedSubscriptionRow(t, db, uid, "pro", "sub_del_1", "active")

	payload := customEventPayload("evt_del_1", "customer.subscription.deleted",
		subscriptionObjectJSON("sub_del_1", "cus_del_1", "price_pro", "canceled", false, uid))
	if w := deliverEvent(t, r, payload); w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", w.Code, w.Body.String())
	}

	if sub := mustSubscription(t, db, uid); sub.Status != "canceled" {
		t.Errorf("status 应改为 canceled，得到 %s", sub.Status)
	}
	acct := mustBalance(t, db, uid)
	if acct.MonthlyCredits != 0 {
		t.Errorf("月度应清零，得到 %d", acct.MonthlyCredits)
	}
	if acct.AddonCredits != 30 {
		t.Errorf("加量包是单独付费买的，退订不得没收，期望 30，得到 %d", acct.AddonCredits)
	}
}

// TestSubscriptionUpdatedDoesNotTouchCredits：只同步 plan_id / status /
// cancel_at_period_end / 周期，额度变化交给随之而来的 invoice.paid。
//
// 事件顺序不保证：这个事件可能先于 invoice.paid 到达，所以它自己动额度会打乱账。
func TestSubscriptionUpdatedDoesNotTouchCredits(t *testing.T) {
	fetcher := &fakeSubscriptionFetcher{}
	r, db := setupWebhookRouter(t, fetcher)
	uid := createUserID(t, r, db, "sub-updated@example.com")
	seedPriceID(t, db, "starter", "price_starter")
	seedPriceID(t, db, "pro", "price_pro")
	if err := credit.Grant(db, uid, 200, 30, "夹具"); err != nil {
		t.Fatalf("夹具发放: %v", err)
	}
	seedSubscriptionRow(t, db, uid, "starter", "sub_upd_1", "active")
	txBefore := countCreditTx(t, db)

	// 用户在 Portal 里升到 pro 并勾了"到期不再续费"。
	payload := customEventPayload("evt_upd_1", "customer.subscription.updated",
		subscriptionObjectJSON("sub_upd_1", "cus_upd_1", "price_pro", "active", true, uid))
	if w := deliverEvent(t, r, payload); w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", w.Code, w.Body.String())
	}

	sub := mustSubscription(t, db, uid)
	if sub.PlanID != "pro" {
		t.Errorf("plan_id 应按 Price 反查同步为 pro，得到 %s", sub.PlanID)
	}
	if !sub.CancelAtPeriodEnd {
		t.Error("cancel_at_period_end 应同步为 true")
	}
	acct := mustBalance(t, db, uid)
	if acct.MonthlyCredits != 200 || acct.AddonCredits != 30 {
		t.Errorf("subscription.updated 不得动次数（交给随后的 invoice.paid），得到 monthly=%d addon=%d",
			acct.MonthlyCredits, acct.AddonCredits)
	}
	if n := countCreditTx(t, db); n != txBefore {
		t.Errorf("不得产生新流水，之前 %d 行，现在 %d 行", txBefore, n)
	}
}

// TestSubscriptionUpdatedArrivingBeforeInvoicePaidStillWorks：事件顺序不保证。
//
// customer.subscription.updated 可能**先于** invoice.paid 到达（首次订阅时两者
// 几乎同时发出），那时我们还没有任何 subscriptions 行。这个 handler 必须能独立
// 建出行来，不能依赖"invoice.paid 已经跑过"。
func TestSubscriptionUpdatedArrivingBeforeInvoicePaidStillWorks(t *testing.T) {
	fetcher := &fakeSubscriptionFetcher{}
	r, db := setupWebhookRouter(t, fetcher)
	uid := createUserID(t, r, db, "sub-first@example.com")
	seedPriceID(t, db, "pro", "price_pro")

	payload := customEventPayload("evt_upd_first", "customer.subscription.updated",
		subscriptionObjectJSON("sub_first_1", "cus_first_1", "price_pro", "active", false, uid))
	if w := deliverEvent(t, r, payload); w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", w.Code, w.Body.String())
	}

	sub := mustSubscription(t, db, uid)
	if sub.PlanID != "pro" || sub.Status != "active" || sub.StripeSubscriptionID != "sub_first_1" {
		t.Errorf("应当凭 metadata 里的 user_id 建出订阅行：plan=%s status=%s subID=%s",
			sub.PlanID, sub.Status, sub.StripeSubscriptionID)
	}
	if n := countCreditTx(t, db); n != 0 {
		t.Errorf("仍然不得发额度，得到 %d 条流水", n)
	}
}
