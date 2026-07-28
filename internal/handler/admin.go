package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/credit"
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
	if err := credit.Grant(h.DB, user.ID, req.Monthly, req.Addon, "admin grant"); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": err.Error()})
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
