package handler

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/billing"
	"image-backend/internal/middleware"
	"image-backend/internal/model"
)

// BillingHandler 结账与 Billing Portal。
//
// Billing 为 nil 表示未配置 Stripe（见 billing.New 的契约）——本地开发没配密钥
// 是正常状态，所以每个入口都要先判 nil 返回 503，而不是拿 nil 去调方法 panic。
type BillingHandler struct {
	DB      *gorm.DB
	Billing *billing.Client
}

type subscribeRequest struct {
	// 只收 planId。**不收 priceId**：价格由服务端查表决定，让客户端传 Price
	// 等于让它指定自己付多少钱。
	PlanID string `json:"planId"`
}

func (h *BillingHandler) Subscribe(c *gin.Context) {
	if h.Billing == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 50300, "message": "billing is not configured"})
		return
	}
	userID := c.GetUint(middleware.CtxUserIDKey)

	var req subscribeRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.PlanID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "planId is required"})
		return
	}

	// enabled = true 是必须的条件：运营下架某档后，前端缓存的旧定价页仍可能发来
	// 请求，不能让它下单。
	var plan model.Plan
	if err := h.DB.Where("id = ? AND enabled = ?", req.PlanID, true).First(&plan).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "unknown plan"})
			return
		}
		log.Printf("[billing] 查档位失败 plan=%s: %v", req.PlanID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	if plan.StripePriceID == "" {
		// 播种命令（cmd/seed-stripe）还没跑。这是我们的运维状态问题，不是用户
		// 参数错误——回 400 会让前端显示"套餐无效"，误导排查方向。
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 50301, "message": "plan is not available for purchase yet"})
		return
	}

	var user model.User
	if err := h.DB.First(&user, userID).Error; err != nil {
		log.Printf("[billing] 查用户失败 user=%d: %v", userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	// 复用已有 customer，否则同一用户会产生多个 customer，Billing Portal 里
	// 只能看到其中一部分发票。
	customerID := ""
	if user.StripeCustomerID != nil {
		customerID = *user.StripeCustomerID
	}

	url, err := h.Billing.CreateCheckoutSession(c.Request.Context(), userID, plan.ID, plan.StripePriceID, customerID)
	if err != nil {
		// 上游错误详情只进日志，不回给客户端。
		log.Printf("[billing] 创建 Checkout 失败 user=%d plan=%s: %v", userID, plan.ID, err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 50200, "message": "payment provider unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"checkoutUrl": url})
}

// Portal 打开 Stripe 托管的账单中心（换卡/取消/看发票）。
//
// 没有 customer 时回 400 而不是造一个：没进过结账流程的用户，Portal 里什么都
// 没有，跳过去只会看到空页面。
func (h *BillingHandler) Portal(c *gin.Context) {
	if h.Billing == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 50300, "message": "billing is not configured"})
		return
	}
	userID := c.GetUint(middleware.CtxUserIDKey)

	var user model.User
	if err := h.DB.First(&user, userID).Error; err != nil {
		log.Printf("[billing] 查用户失败 user=%d: %v", userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	if user.StripeCustomerID == nil || *user.StripeCustomerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40001, "message": "no billing account yet"})
		return
	}

	url, err := h.Billing.CreatePortalSession(c.Request.Context(), *user.StripeCustomerID)
	if err != nil {
		log.Printf("[billing] 创建 Portal 失败 user=%d: %v", userID, err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 50200, "message": "payment provider unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"portalUrl": url})
}
