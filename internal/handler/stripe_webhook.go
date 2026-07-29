package handler

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
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

// StripeWebhookHandler 接收 Stripe 的事件回调。
//
// 这个入口**不过认证中间件**（Stripe 不带我们的 cookie），安全性完全由验签
// 保证：Secret 是与 Stripe 共享的 HMAC 密钥，签名对不上的请求一律拒绝。
type StripeWebhookHandler struct {
	DB      *gorm.DB
	Secret  string
	Billing *billing.Client
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
	ev, err := webhook.ConstructEvent(payload, c.GetHeader("Stripe-Signature"), h.Secret)
	if err != nil {
		// 不验签的后果不是"可能被伪造"，是任何人 POST 一个 invoice.paid
		// 就能给自己发额度。验签失败一律拒绝，且**不留幂等记录**——留了就等于
		// 把伪造事件的 id 占掉，真事件到达时会被当成重复而丢弃。
		log.Printf("[stripe-webhook] 验签失败: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"code": 40100, "message": "invalid signature"})
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

// dispatch 目前只是骨架：记日志并返回 nil（→200）。具体事件处理见 Task 8。
//
// 未知类型也返回 nil：回 5xx 会让 Stripe 一直重投我们根本不处理的事件类型。
func (h *StripeWebhookHandler) dispatch(_ context.Context, _ *gorm.DB, ev stripe.Event) error {
	log.Printf("[stripe-webhook] 收到 %s(%s)，暂未处理", ev.Type, ev.ID)
	return nil
}
