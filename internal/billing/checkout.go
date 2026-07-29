package billing

import (
	"context"
	"strconv"

	stripe "github.com/stripe/stripe-go/v86"
)

// CreateCheckoutSession 为用户创建订阅结账会话，返回 Stripe 托管页面的 URL。
//
// priceID 由调用方从 plans 表查出——**不接受客户端传入的 Price**，否则等于让
// 客户端决定自己付多少钱。
//
// customerID 为空时不传 Customer，让 Stripe 新建；用户已有 customer 就必须复用，
// 否则同一用户会产生多个 customer，Billing Portal 里只能看到其中一部分发票。
func (c *Client) CreateCheckoutSession(ctx context.Context, userID uint, planID, priceID, customerID string) (string, error) {
	uid := strconv.FormatUint(uint64(userID), 10)
	params := &stripe.CheckoutSessionCreateParams{
		Mode:              stripe.String("subscription"),
		SuccessURL:        stripe.String(c.appBaseURL + "/account?checkout=success"),
		CancelURL:         stripe.String(c.appBaseURL + "/pricing?checkout=cancel"),
		ClientReferenceID: stripe.String(uid),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{Price: stripe.String(priceID), Quantity: stripe.Int64(1)},
		},
		// metadata 挂在 subscription 上而非 session 上：invoice.paid 事件拿到的是
		// 订阅，session 的 metadata 在那时已经够不着了。
		SubscriptionData: &stripe.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: map[string]string{
				"user_id": uid,
				"plan_id": planID,
			},
		},
	}
	if customerID != "" {
		params.Customer = stripe.String(customerID)
	}
	sess, err := c.sc.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}

// CreatePortalSession 换卡/取消/看发票统一跳 Stripe 自己的页面——
// 我们不做这些 UI，自己做是纯浪费且要处理 PCI 相关问题。
func (c *Client) CreatePortalSession(ctx context.Context, customerID string) (string, error) {
	sess, err := c.sc.V1BillingPortalSessions.Create(ctx, &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(c.appBaseURL + "/account"),
	})
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}
