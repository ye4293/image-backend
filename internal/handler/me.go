package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/credit"
	"image-backend/internal/middleware"
	"image-backend/internal/model"
)

type MeHandler struct {
	DB *gorm.DB
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
	c.JSON(http.StatusOK, gin.H{
		"id":    user.ID,
		"email": user.Email,
		"role":  user.Role,
		"credits": gin.H{
			"monthly": bal.MonthlyCredits,
			"addon":   bal.AddonCredits,
		},
	})
}
