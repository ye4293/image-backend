package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"image-backend/internal/credit"
	"image-backend/internal/model"
)

// 本文件是五个 Stripe 事件的业务处理。三条不变量，改动前请先读：
//
//  1. **invoice.paid 是发放额度的唯一入口。** 首次订阅与每月续费都触发它。
//     checkout.session.completed 也发的话就是双倍到账，而那个 bug 只在真实付款
//     时才暴露（测试环境里不会有人真的刷卡）。
//  2. **发多少次数按订阅当前的 Price ID 反查 plans，不按 metadata 的 plan_id。**
//     metadata 是我们下单时请求的，Price 是用户实际被计费的。用户在 Billing
//     Portal 里升档时 Stripe 换 Price 但**不改 metadata**。
//  3. **事件顺序不保证。** customer.subscription.updated 可能先于 invoice.paid
//     到达，所以每个 handler 必须独立正确、不依赖别的 handler 跑过——
//     subscriptions 行一律用 upsert。
//
// 所有 handler 都在调用方（webhook handler）的事务内运行：返回 error 会连幂等
// 记录一起回滚，Stripe 随后重投；返回 nil 表示"已处理完毕，不必重投"。
//
// v86 的字段路径与网上多数示例不同，且 inv.Parent / inv.Customer / sub.Items
// 都是指针：**判空**。这里 panic 掉的是 webhook handler，后果是 Stripe 无限重投
// 同一个事件。

// ErrCustomerConflict 同一个 Stripe customer 指向了两个不同的本地用户。
//
// 这属于数据已经串了，不能猜——猜都可能把 A 的钱记到 B 头上。
var ErrCustomerConflict = errors.New("stripe customer 与本地用户对应关系冲突")

// HandleInvoicePaid 是**唯一**的额度发放入口。
func HandleInvoicePaid(ctx context.Context, tx *gorm.DB, subs SubscriptionFetcher, ev stripe.Event) error {
	var inv stripe.Invoice
	if err := json.Unmarshal(ev.Data.Raw, &inv); err != nil {
		return fmt.Errorf("解析 invoice 失败: %w", err)
	}

	// 非订阅发票（一次性收款，例如将来的加量包）不在本流程里发放。返回 nil 而不是
	// 错误：这是正常业务，让 Stripe 重投毫无意义。
	if inv.Parent == nil || inv.Parent.SubscriptionDetails == nil {
		log.Printf("[stripe] invoice.paid %s 不是订阅发票（parent=%v），跳过", ev.ID, parentType(inv.Parent))
		return nil
	}
	sd := inv.Parent.SubscriptionDetails
	subID := ""
	if sd.Subscription != nil {
		subID = sd.Subscription.ID
	}
	if subID == "" {
		log.Printf("[stripe] invoice.paid %s 的 parent.subscription_details 里没有订阅 id，跳过", ev.ID)
		return nil
	}

	if subs == nil {
		// 配了 webhook secret 却没配 secret key。回错误让失败挂在 Stripe 的失败列表
		// 里：静默返回 200 等于确认收到付款却永不发放额度。
		return fmt.Errorf("%w：无法拉取订阅 %s", ErrNotConfigured, subID)
	}
	sub, err := subs.FetchSubscription(ctx, subID)
	if err != nil {
		// 上游故障是**应该**重投的情况。
		return fmt.Errorf("拉取订阅 %s 失败: %w", subID, err)
	}
	if sub == nil || sub.Items == nil || len(sub.Items.Data) == 0 {
		return fmt.Errorf("订阅 %s 没有 items，无法判断档位（v86 的 Price 与周期都挂在 items.data[0] 上）", subID)
	}
	item := sub.Items.Data[0]
	priceID := ""
	if item.Price != nil {
		priceID = item.Price.ID
	}

	// **按 Price 反查，不按 metadata。** 见文件头不变量 2。
	plan, err := planByPriceID(tx, priceID)
	if err != nil {
		return err
	}
	if plan == nil {
		// Dashboard 手工建的订阅会走到这里。宁可漏发等人工处理，也不要瞎猜一个档位
		// ——猜错就是给钱少的人发多，且没人会发现。返回 nil：重投也还是查不到。
		log.Printf("[stripe] invoice.paid %s：Price %q 在 plans 里查不到，**未发放任何额度**（订阅 %s）。"+
			"若这是有效订阅，请把该 Price 回填到 plans.stripe_price_id 后手工补发", ev.ID, priceID, subID)
		return nil
	}

	userID, err := resolveUserID(tx, sd.Metadata["user_id"], customerID(inv.Customer, sub.Customer))
	if err != nil {
		return err
	}

	if err := upsertSubscription(tx, model.Subscription{
		UserID:               userID,
		PlanID:               plan.ID,
		StripeSubscriptionID: sub.ID,
		Status:               string(sub.Status),
		CurrentPeriodStart:   unixTime(item.CurrentPeriodStart),
		CurrentPeriodEnd:     unixTime(item.CurrentPeriodEnd),
		CancelAtPeriodEnd:    sub.CancelAtPeriodEnd,
	}); err != nil {
		return err
	}

	err = credit.ResetMonthly(tx, userID, plan.MonthlyCredits, ev.ID, "订阅发放："+plan.ID)
	if errors.Is(err, credit.ErrAlreadyGranted) {
		// stripe_events 已经保证了一次，走到这里说明有人删过那行让 Stripe 重投。
		// 兜底幂等生效了，不是错误。
		log.Printf("[stripe] invoice.paid %s 的额度已发放过（兜底幂等命中），跳过发放", ev.ID)
		return nil
	}
	if err != nil {
		return err
	}
	log.Printf("[stripe] invoice.paid %s：用户 %d 月度次数设为 %d（档位 %s，Price %s，订阅 %s）",
		ev.ID, userID, plan.MonthlyCredits, plan.ID, priceID, sub.ID)
	return nil
}

// HandleCheckoutCompleted **只**回填 users.stripe_customer_id，绝不发额度。
//
// 两件事都在这里做就是双倍到账（invoice.paid 紧随其后也会到），而这个 bug 只在
// 真实付款时暴露。
//
// 回填本身也不是可选的：没有 customer id，用户首次订阅后就**进不了 Billing
// Portal**（换卡、取消、看发票全都在那里）。
func HandleCheckoutCompleted(tx *gorm.DB, ev stripe.Event) error {
	var sess stripe.CheckoutSession
	if err := json.Unmarshal(ev.Data.Raw, &sess); err != nil {
		return fmt.Errorf("解析 checkout.session 失败: %w", err)
	}
	cus := ""
	if sess.Customer != nil {
		cus = sess.Customer.ID
	}
	if cus == "" {
		log.Printf("[stripe] checkout.session.completed %s 没有 customer，无可回填", ev.ID)
		return nil
	}
	// ClientReferenceID 由 CreateCheckoutSession 写入。不用 session metadata：
	// 我们的 metadata 挂在 subscription_data 上（invoice.paid 要用）。
	userID, err := parseUserID(sess.ClientReferenceID)
	if err != nil {
		return fmt.Errorf("checkout.session %s 的 client_reference_id 非法: %w", ev.ID, err)
	}
	if userID == 0 {
		// 不是我们后端创建的会话（例如 Payment Link）。无从判断归属，返回 nil。
		log.Printf("[stripe] checkout.session.completed %s 没有 client_reference_id，无法确定用户，跳过绑定", ev.ID)
		return nil
	}

	// stripe_customer_id 上有唯一索引：先看这个 customer 是否已被别人占了。直接
	// UPDATE 撞唯一键会让整个事务失败并被 Stripe 无限重投，且报错读不出原因。
	var owner model.User
	err = tx.Where("stripe_customer_id = ?", cus).First(&owner).Error
	switch {
	case err == nil && owner.ID == userID:
		return nil // 已绑定，幂等
	case err == nil:
		return fmt.Errorf("%w：customer %s 已绑定用户 %d，事件 %s 却声称属于用户 %d",
			ErrCustomerConflict, cus, owner.ID, ev.ID, userID)
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return err
	}

	res := tx.Model(&model.User{}).Where("id = ?", userID).Update("stripe_customer_id", cus)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 用户被删了或 id 是编造的。回错误告警：这条链路断了的表现是用户进不了
		// Portal，而那时已经没有线索可查。
		return fmt.Errorf("checkout.session %s 指向的用户 %d 不存在，无法回填 customer %s", ev.ID, userID, cus)
	}
	log.Printf("[stripe] checkout.session.completed %s：用户 %d 绑定 customer %s（**未**发放额度，等 invoice.paid）",
		ev.ID, userID, cus)
	return nil
}

// HandlePaymentFailed 把订阅状态改成 past_due，**不动次数**。
//
// 用户可能只是卡过期，几天内会补款（Stripe 自己会重试几次）。清零会让他在补款前
// 完全不能用，而绝大多数扣款失败最后都补上了。
func HandlePaymentFailed(tx *gorm.DB, ev stripe.Event) error {
	var inv stripe.Invoice
	if err := json.Unmarshal(ev.Data.Raw, &inv); err != nil {
		return fmt.Errorf("解析 invoice 失败: %w", err)
	}
	if inv.Parent == nil || inv.Parent.SubscriptionDetails == nil ||
		inv.Parent.SubscriptionDetails.Subscription == nil {
		log.Printf("[stripe] invoice.payment_failed %s 不是订阅发票，跳过", ev.ID)
		return nil
	}
	subID := inv.Parent.SubscriptionDetails.Subscription.ID
	if subID == "" {
		return nil
	}
	res := tx.Model(&model.Subscription{}).
		Where("stripe_subscription_id = ?", subID).
		Update("status", "past_due")
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 我们没有这条订阅（Dashboard 手工建的，或事件顺序导致行还没建出来）。
		// 重投也不会变，返回 nil。
		log.Printf("[stripe] invoice.payment_failed %s：本地没有订阅 %s 的记录，未改状态", ev.ID, subID)
		return nil
	}
	log.Printf("[stripe] invoice.payment_failed %s：订阅 %s 置为 past_due（次数**未**变动）", ev.ID, subID)
	return nil
}

// HandleSubscriptionUpdated 同步档位 / 状态 / 续费开关 / 周期，**不动次数**。
//
// 额度变化交给随之而来的 invoice.paid：升档时 Stripe 会开一张补差价的发票，
// 那张发票付掉才算真的升级成功。在这里就发额度，等于用户点了升级但没付成也拿到
// 高档次数。
func HandleSubscriptionUpdated(tx *gorm.DB, ev stripe.Event) error {
	sub, ok, err := parseSubscription(ev)
	if err != nil || !ok {
		return err
	}

	// 先找本地行：它同时给出 user_id 和"档位查不到时的兜底值"。
	existing, found, err := subscriptionByStripeID(tx, sub.ID)
	if err != nil {
		return err
	}

	planID := ""
	if sub.Items != nil && len(sub.Items.Data) > 0 && sub.Items.Data[0].Price != nil {
		plan, err := planByPriceID(tx, sub.Items.Data[0].Price.ID)
		if err != nil {
			return err
		}
		if plan != nil {
			planID = plan.ID
		}
	}
	if planID == "" {
		if !found {
			log.Printf("[stripe] %s %s：Price 查不到对应档位且本地无记录，跳过同步（订阅 %s）",
				ev.Type, ev.ID, sub.ID)
			return nil
		}
		// 查不到就**保留**原档位，不要写空——plan_id 是 not null，写空会让
		// /me 显示一个不存在的档位。
		planID = existing.PlanID
	}

	userID := existing.UserID
	if !found {
		// 事件顺序不保证：subscription.updated 可能先于 invoice.paid 到达，那时
		// 还没有任何本地行。凭 metadata / customer 自己建出来。
		userID, err = resolveUserID(tx, sub.Metadata["user_id"], customerID(sub.Customer, nil))
		if err != nil {
			// 认不出归属就不写。返回 nil 而不是错误：重投同样认不出。
			log.Printf("[stripe] %s %s：无法确定订阅 %s 归属（%v），跳过同步", ev.Type, ev.ID, sub.ID, err)
			return nil
		}
	}

	row := model.Subscription{
		UserID:               userID,
		PlanID:               planID,
		StripeSubscriptionID: sub.ID,
		Status:               string(sub.Status),
		CancelAtPeriodEnd:    sub.CancelAtPeriodEnd,
		CurrentPeriodStart:   existing.CurrentPeriodStart,
		CurrentPeriodEnd:     existing.CurrentPeriodEnd,
	}
	if sub.Items != nil && len(sub.Items.Data) > 0 {
		row.CurrentPeriodStart = unixTime(sub.Items.Data[0].CurrentPeriodStart)
		row.CurrentPeriodEnd = unixTime(sub.Items.Data[0].CurrentPeriodEnd)
	}
	if err := upsertSubscription(tx, row); err != nil {
		return err
	}
	log.Printf("[stripe] %s %s：用户 %d 订阅同步为 plan=%s status=%s cancelAtPeriodEnd=%t（次数未变动）",
		ev.Type, ev.ID, userID, row.PlanID, row.Status, row.CancelAtPeriodEnd)
	return nil
}

// HandleSubscriptionDeleted 状态置 canceled，月度次数清零，**加量包保留**。
//
// 加量包是用户单独花钱买的，退订不能没收。ResetMonthly 本来就只改月度。
func HandleSubscriptionDeleted(tx *gorm.DB, ev stripe.Event) error {
	sub, ok, err := parseSubscription(ev)
	if err != nil || !ok {
		return err
	}
	existing, found, err := subscriptionByStripeID(tx, sub.ID)
	if err != nil {
		return err
	}
	userID := existing.UserID
	if !found {
		userID, err = resolveUserID(tx, sub.Metadata["user_id"], customerID(sub.Customer, nil))
		if err != nil {
			// 本地没有这条订阅、也认不出归属：没有什么可清零的。
			log.Printf("[stripe] customer.subscription.deleted %s：本地无订阅 %s 且无法确定归属（%v），跳过",
				ev.ID, sub.ID, err)
			return nil
		}
	} else {
		if err := tx.Model(&model.Subscription{}).
			Where("stripe_subscription_id = ?", sub.ID).
			Updates(map[string]any{"status": "canceled", "cancel_at_period_end": false}).Error; err != nil {
			return err
		}
	}

	err = credit.ResetMonthly(tx, userID, 0, ev.ID, "订阅取消：月度清零（加量包保留）")
	if errors.Is(err, credit.ErrAlreadyGranted) {
		log.Printf("[stripe] customer.subscription.deleted %s 已处理过（兜底幂等命中）", ev.ID)
		return nil
	}
	if err != nil {
		return err
	}
	log.Printf("[stripe] customer.subscription.deleted %s：用户 %d 月度清零，加量包保留", ev.ID, userID)
	return nil
}

// --- 内部辅助 -------------------------------------------------------------

// parseSubscription 解析订阅类事件的载荷。ok=false 表示载荷里没有可用的订阅
// （例如构造出来的空对象），调用方应当直接返回 nil。
func parseSubscription(ev stripe.Event) (stripe.Subscription, bool, error) {
	var sub stripe.Subscription
	if err := json.Unmarshal(ev.Data.Raw, &sub); err != nil {
		return sub, false, fmt.Errorf("解析 subscription 失败: %w", err)
	}
	if sub.ID == "" {
		log.Printf("[stripe] %s %s 的载荷里没有订阅 id，跳过", ev.Type, ev.ID)
		return sub, false, nil
	}
	return sub, true, nil
}

// planByPriceID 按 Stripe Price 反查档位。查不到返回 (nil, nil)。
//
// **不过滤 enabled**：运营下架某档后，已经在付费的老用户仍然每月被扣款，必须
// 继续拿到次数。enabled 只管"能不能新下单"。
func planByPriceID(tx *gorm.DB, priceID string) (*model.Plan, error) {
	if priceID == "" {
		return nil, nil
	}
	var plan model.Plan
	err := tx.Where("stripe_price_id = ?", priceID).First(&plan).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

func subscriptionByStripeID(tx *gorm.DB, subID string) (model.Subscription, bool, error) {
	var row model.Subscription
	err := tx.Where("stripe_subscription_id = ?", subID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.Subscription{}, false, nil
	}
	if err != nil {
		return model.Subscription{}, false, err
	}
	return row, true, nil
}

// upsertSubscription 按 user_id 覆盖写订阅行。
//
// 用 upsert 而不是"先查再决定 insert/update"：事件顺序不保证，任何 handler 都
// 可能是第一个见到这条订阅的，而并发的两个事件各查到"不存在"就会双插。
//
// 显式列出要更新的列而不是 UpdateAll：那会把 created_at 一起覆盖掉。
func upsertSubscription(tx *gorm.DB, row model.Subscription) error {
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"plan_id", "stripe_subscription_id", "status",
			"current_period_start", "current_period_end", "cancel_at_period_end", "updated_at",
		}),
	}).Create(&row).Error
}

// resolveUserID 确定这笔订阅属于哪个本地用户。
//
// 两个来源交叉校验：metadata 里的 user_id（我们下单时写的）与 stripe_customer_id
// 反查到的用户。**两者都存在且不一致就报错**，不猜——数据已经串了，猜哪个都可能
// 把 A 的额度发给 B，而那种错误发生后极难还原。
func resolveUserID(tx *gorm.DB, metaUserID, cus string) (uint, error) {
	fromMeta, err := parseUserID(metaUserID)
	if err != nil {
		return 0, fmt.Errorf("metadata 里的 user_id 非法: %w", err)
	}

	var fromCustomer uint
	if cus != "" {
		var u model.User
		err := tx.Where("stripe_customer_id = ?", cus).First(&u).Error
		if err == nil {
			fromCustomer = u.ID
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, err
		}
	}

	switch {
	case fromMeta != 0 && fromCustomer != 0 && fromMeta != fromCustomer:
		return 0, fmt.Errorf("%w：metadata 说是用户 %d，customer %s 却绑在用户 %d 上——"+
			"数据已串，拒绝发放以免发错人，请人工核对", ErrCustomerConflict, fromMeta, cus, fromCustomer)
	case fromMeta != 0:
		// metadata 可能指向一个已被删除的用户。不校验的话 ResetMonthly 会给一个
		// 不存在的 user_id 建账户行，那笔钱谁也拿不到且不会有人发现。
		var n int64
		if err := tx.Model(&model.User{}).Where("id = ?", fromMeta).Count(&n).Error; err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, fmt.Errorf("metadata 指向的用户 %d 不存在", fromMeta)
		}
		return fromMeta, nil
	case fromCustomer != 0:
		return fromCustomer, nil
	default:
		return 0, fmt.Errorf("既没有 metadata.user_id 也无法按 customer %q 反查到用户", cus)
	}
}

func parseUserID(s string) (uint, error) {
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q 不是合法的用户 id: %w", s, err)
	}
	return uint(v), nil
}

// customerID 从若干个可能为 nil 的 Customer 里取第一个非空 id。
func customerID(primary, fallback *stripe.Customer) string {
	if primary != nil && primary.ID != "" {
		return primary.ID
	}
	if fallback != nil {
		return fallback.ID
	}
	return ""
}

// unixTime 把 Stripe 的秒级时间戳转成 time.Time。0 转成零值而不是 1970 年，
// 免得 /me 上显示一个 1970 的续费日期。
func unixTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func parentType(p *stripe.InvoiceParent) string {
	if p == nil {
		return "nil"
	}
	return string(p.Type)
}
