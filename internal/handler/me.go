package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

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
		c.JSON(http.StatusNotFound, gin.H{"code": 40400, "message": "user not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":    user.ID,
		"email": user.Email,
		"role":  user.Role,
	})
}
