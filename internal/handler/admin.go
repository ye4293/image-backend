package handler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/credit"
	"image-backend/internal/middleware"
	"image-backend/internal/model"
)

type AdminHandler struct {
	DB *gorm.DB
}

type grantRequest struct {
	Email   string `json:"email" binding:"required,email"`
	Monthly int    `json:"monthly"`
	Addon   int    `json:"addon"`
}

// GrantCredits 给指定邮箱的用户发放次数。内测期间替代手工 SQL——手工改库不留流水。
func (h *AdminHandler) GrantCredits(c *gin.Context) {
	var req grantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "invalid request body"})
		return
	}
	var user model.User
	if err := h.DB.Where("email = ?", req.Email).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": 40400, "message": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	// 记下是**哪个管理员**发的。credit.Grant 的 note 之前写死成 "admin grant"，
	// 那样流水只能回答"发生过一笔发放"，回答不了"谁发的"——等真有钱之后，
	// 这就是审计缺口。actor 由 Auth 中间件放进 context，客户端伪造不了。
	actorID := c.GetUint(middleware.CtxUserIDKey)
	note := fmt.Sprintf("admin grant by user #%d", actorID)

	if err := credit.Grant(h.DB, user.ID, req.Monthly, req.Addon, note); err != nil {
		if errors.Is(err, credit.ErrInvalidGrantAmount) {
			c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": err.Error()})
			return
		}
		// 其余都是基础设施故障（连接断了、约束冲突）。不能原样回传 err.Error()：
		// 那会把数据库内部信息泄露给调用方，也会把 500 级故障伪装成 400 让人以为
		// 是自己参数写错了。
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	bal, err := credit.Balance(h.DB, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"email": user.Email,
		"credits": gin.H{
			"monthly": bal.MonthlyCredits,
			"addon":   bal.AddonCredits,
		},
	})
}
