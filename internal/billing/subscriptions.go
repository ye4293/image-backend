package billing

import (
	"context"

	stripe "github.com/stripe/stripe-go/v86"
)

// SubscriptionFetcher 拉取订阅的当前状态。
//
// 抽成接口而不是直接调 *Client 的方法，是为了让 webhook 的业务规则能被独立测试：
// "按 Price 反查档位"和"交叉校验 user_id"这两条都依赖订阅当前长什么样，而那份
// 数据来自一次 API 调用。若只能走真实网络，这些测试就要密钥、会随网络抖动变红，
// 于是迟早被跳过——而它们恰恰是最不能失守的两条（失守的表现分别是"付了 Pro 的钱
// 拿到 Starter 的次数"和"把 A 的额度发给 B"，都不会自己冒出来）。
type SubscriptionFetcher interface {
	FetchSubscription(ctx context.Context, subscriptionID string) (*stripe.Subscription, error)
}

// FetchSubscription 读订阅当前状态。
//
// **为什么不直接用 webhook 载荷里的订阅对象**：invoice.paid 的载荷里只有订阅 id，
// 而我们需要当前的 Price 与周期。即便有内嵌对象，它也是事件发出那一刻的快照，
// 而事件可能积压重投，快照会过期。以 API 的当前状态为准。
func (c *Client) FetchSubscription(ctx context.Context, subscriptionID string) (*stripe.Subscription, error) {
	// 周期与 Price 在 v86 里挂在 items.data[] 上，默认就会返回（items 是订阅对象的
	// 内联列表），所以不需要 expand。
	return c.sc.V1Subscriptions.Retrieve(ctx, subscriptionID, nil)
}

var _ SubscriptionFetcher = (*Client)(nil)
