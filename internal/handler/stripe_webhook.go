package handler

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	stripe "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
	"gorm.io/gorm"

	"image-backend/internal/billing"
	"image-backend/internal/model"
)

// maxWebhookBody 限制读入的字节数。不设上限的话，一个超大 body 就能吃光内存。
const maxWebhookBody = 1 << 20 // 1 MiB

// apiVersionCompatible 事件的 API 版本是否与 SDK 属于同一个**发布列车**。
//
// 为什么比列车而不是比完整版本串（原先是 ev.APIVersion != stripe.APIVersion）：
// Stripe 的版本形如 yyyy-MM-dd.train（如 2026-07-29.dahlia），同一列车内载荷结构
// 保持稳定——这正是 Stripe 划分列车的目的，stripe-go 自己的 isCompatibleAPIVersion
// 也只比列车名。
//
// 精确相等是过严的，而且严到**无解**：SDK 钉在 2026-06-24.dahlia，而 Dashboard 的
// 版本下拉里给的是 2026-07-29.dahlia（最新）、2026-06-24.preview、2024-11-20.acacia
// 和账号建号时的老版本——一个都不等于 SDK 那一串。四个选项全部会被拒，表现是每笔
// 真实付款都回 500、Stripe 无限重投、额度永远发不出。
//
// 收紧的部分一分不放：没有列车后缀的老版本（2023-10-16）与别的列车（acacia、
// preview）仍然被拦住，因为它们的载荷结构确实不同。
func apiVersionCompatible(eventVersion string) bool {
	_, train, ok := strings.Cut(eventVersion, ".")
	if !ok {
		// 早于发布列车机制的版本（如 2023-10-16），载荷结构不同，一律不兼容。
		return false
	}
	// SDK 自己钉在预览版时，预览版之间不保证兼容，要求完全一致——与 stripe-go
	// 的 isCompatibleAPIVersion 保持同一判断。
	if stripe.APIMajorVersion == "preview" {
		return eventVersion == stripe.APIVersion
	}
	return train == stripe.APIMajorVersion
}

// StripeWebhookHandler 接收 Stripe 的事件回调。
//
// 这个入口**不过认证中间件**（Stripe 不带我们的 cookie），安全性完全由验签
// 保证：Secret 是与 Stripe 共享的 HMAC 密钥，签名对不上的请求一律拒绝。
type StripeWebhookHandler struct {
	DB     *gorm.DB
	Secret string
	// Subs 用来拉取订阅当前状态（Price 与周期）。是接口而非 *billing.Client，
	// 这样"按 Price 反查档位"与"交叉校验 user_id"能在不联网的情况下被测试。
	//
	// 可以为 nil（没配 STRIPE_SECRET_KEY）。**不要**把一个 nil 的 *billing.Client
	// 赋给它：那样接口本身非 nil，调用时会 panic 在 webhook 里，导致 Stripe 无限重投。
	Subs billing.SubscriptionFetcher
}

// errAlreadyProcessed 内部哨兵：撞幂等主键时用它把事务回滚掉，再在事务外转成
// 200。不能在闭包里直接 return nil——那会把已经跑了一半的业务变更提交掉
// （credit/ledger.go 的 errAlreadyRefunded 是同一个模式）。
var errAlreadyProcessed = errors.New("event already processed")

func (h *StripeWebhookHandler) Handle(c *gin.Context) {
	if h.Secret == "" {
		// 空密钥不能拿去验签：那样任何人都能自己算出"合法"签名。没配就整体停用。
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 50300, "message": "billing is not configured"})
		return
	}
	// **必须用原始字节验签。** 任何 ShouldBindJSON 再序列化都会改变字节，
	// 导致签名对不上。这是这个 handler 最容易写错的一行。
	payload, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "cannot read body"})
		return
	}
	// 验签与版本校验**刻意分成两步**。
	//
	// webhook.ConstructEvent 会同时做两件事：验签，以及校验事件的 api_version
	// 等于 SDK 的 stripe.APIVersion。两件事挤在一个 error 里的后果是：版本不匹配
	// 会被报成"invalid signature"，而那是个把人引向完全错误方向的诊断——签名明明
	// 是好的，问题在 endpoint 的版本配置上。这不是假设：Stripe 账号的默认 API 版本
	// 常年停在建号时那一版（本项目实测是 2023-10-16），而 stripe listen 默认就按
	// 账号默认版本转发，于是**每一个真实事件都会被拒**。
	//
	// 所以这里用 IgnoreAPIVersionMismatch 只验签（安全性一分不减），版本自己单独
	// 校验并给出可执行的报错。
	ev, err := webhook.ConstructEventWithOptions(payload, c.GetHeader("Stripe-Signature"), h.Secret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		// 不验签的后果不是"可能被伪造"，是任何人 POST 一个 invoice.paid
		// 就能给自己发额度。验签失败一律拒绝，且**不留幂等记录**——留了就等于
		// 把伪造事件的 id 占掉，真事件到达时会被当成重复而丢弃。
		log.Printf("[stripe-webhook] 验签失败: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"code": 40100, "message": "invalid signature"})
		return
	}

	// 版本不匹配**不能放过去**。旧版本的载荷是不同的结构：2023-10-16 的发票把
	// subscription 放在顶层，而本代码读的是 inv.Parent.SubscriptionDetails——
	// 硬解会得到一堆 nil，表现为"事件收到了但什么都没发生"。
	//
	// 回 500 而不是 200：200 等于确认收到一笔付款却永不发放额度，是静默的丢钱。
	// 500 会让这个失败一直挂在 Stripe Dashboard 的失败列表里，直到有人处理。
	// （注意事件的 api_version 是不可变的，重投也还是老版本，所以修好配置后
	// 卡住的那几个仍需人工重放——这条缺口计划里已记录。）
	if !apiVersionCompatible(ev.APIVersion) {
		log.Printf("[stripe-webhook] API 版本不兼容：事件是 %q，本服务需要 %q 发布列车（SDK 钉在 %q）。"+
			"事件 %s(%s) 未处理。修法：Dashboard 里把该 webhook endpoint 的 API 版本换成 %q 列车下的任一版本；"+
			"本地用 `stripe listen --latest`（默认走账号默认版本，通常是老的）。",
			ev.APIVersion, stripe.APIMajorVersion, stripe.APIVersion,
			ev.Type, ev.ID, stripe.APIMajorVersion)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    50302,
			"message": "stripe api version mismatch",
		})
		return
	}

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		// 幂等记录与业务处理必须**同一事务**：分开的话，进程在两步之间崩溃
		// 会留下"记了已处理但没处理"，那是永久漏发一次，比重复发放更难发现
		// （重复发放至少余额对不上，漏发看起来一切正常）。
		if err := tx.Create(&model.StripeEvent{
			ID:          ev.ID,
			Type:        string(ev.Type),
			ProcessedAt: time.Now(),
		}).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return errAlreadyProcessed
			}
			return err
		}
		return h.dispatch(c.Request.Context(), tx, ev)
	})
	switch {
	case errors.Is(err, errAlreadyProcessed):
		c.JSON(http.StatusOK, gin.H{"received": true, "duplicate": true})
	case err != nil:
		// 回 5xx 让 Stripe 重投——业务处理失败是唯一该重投的情况。
		log.Printf("[stripe-webhook] 处理 %s(%s) 失败: %v", ev.Type, ev.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "processing failed"})
	default:
		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}

// dispatch 把事件派发给对应的业务处理。**只处理这五种**。
//
// 全部在调用方的事务内执行：返回 error 会连幂等记录一起回滚（Stripe 随后重投），
// 返回 nil 表示"处理完毕，不必重投"。
func (h *StripeWebhookHandler) dispatch(ctx context.Context, tx *gorm.DB, ev stripe.Event) error {
	switch ev.Type {
	case "checkout.session.completed":
		// **只**绑 customer id，绝不发额度——和 invoice.paid 都发就是双倍到账。
		return billing.HandleCheckoutCompleted(tx, ev)
	case "invoice.paid":
		// 发放额度的唯一入口（首次订阅与每月续费都触发它）。
		return billing.HandleInvoicePaid(ctx, tx, h.Subs, ev)
	case "invoice.payment_failed":
		return billing.HandlePaymentFailed(tx, ev)
	case "customer.subscription.updated":
		return billing.HandleSubscriptionUpdated(tx, ev)
	case "customer.subscription.deleted":
		return billing.HandleSubscriptionDeleted(tx, ev)
	default:
		// 未知类型也返回 nil → 200。回 5xx 会让 Stripe 一直重投我们根本不处理的
		// 事件类型（Stripe 的 endpoint 默认订阅面很宽）。
		log.Printf("[stripe-webhook] 收到 %s(%s)，本服务不处理此类型", ev.Type, ev.ID)
		return nil
	}
}
