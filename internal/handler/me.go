package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/credit"
	"image-backend/internal/middleware"
	"image-backend/internal/model"
)

type MeHandler struct {
	DB *gorm.DB
}

// subscriptionResponse 是 /me 里的订阅摘要。
//
// 用指针装它是为了未订阅时序列化成 JSON null——前端靠 null 区分"没订阅"与
// "订阅了但状态未知"，退化成空对象就分不出来了。
type subscriptionResponse struct {
	PlanID            string    `json:"planId"`
	Status            string    `json:"status"`
	CurrentPeriodEnd  time.Time `json:"currentPeriodEnd"`
	CancelAtPeriodEnd bool      `json:"cancelAtPeriodEnd"`
}

func (h *MeHandler) Get(c *gin.Context) {
	userID := c.GetUint(middleware.CtxUserIDKey)
	var user model.User
	if err := h.DB.First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": 40400, "message": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	bal, err := credit.Balance(h.DB, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	// nil → JSON null。
	var subOut *subscriptionResponse
	var sub model.Subscription
	err = h.DB.Where("user_id = ?", userID).First(&sub).Error
	switch {
	case err == nil:
		subOut = &subscriptionResponse{
			PlanID:            sub.PlanID,
			Status:            sub.Status,
			CurrentPeriodEnd:  sub.CurrentPeriodEnd,
			CancelAtPeriodEnd: sub.CancelAtPeriodEnd,
		}
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 查不到是正常的（没订阅），留 nil。
	default:
		// DB 报错不是"没订阅"——把故障伪装成未订阅会让付过费的用户在故障期间
		// 看到未订阅界面，甚至被引导去二次付款。
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":    user.ID,
		"email": user.Email,
		"role":  user.Role,
		"credits": gin.H{
			"monthly": bal.MonthlyCredits,
			"addon":   bal.AddonCredits,
		},
		"subscription": subOut,
	})
}
