package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

type ModelsHandler struct {
	DB *gorm.DB
}

// modelResponse 的字段名与前端 image-front 的 ImageModel 类型一一对应。
// 改这里就要同步改前端 lib/generation-types.ts。
type modelResponse struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Credits              int    `json:"credits"`
	SupportsImageToImage bool   `json:"supportsImageToImage"`
}

// Get 返回启用的模型，按 sort_order 排序。公开接口——定价页与落地页都可能要展示。
func (h *ModelsHandler) Get(c *gin.Context) {
	var rows []model.ImageModel
	if err := h.DB.Where("enabled = ?", true).Order("sort_order asc, id asc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	out := make([]modelResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, modelResponse{
			ID:                   m.ID,
			Name:                 m.DisplayName,
			Credits:              m.Credits,
			SupportsImageToImage: m.SupportsImageToImage,
		})
	}
	c.JSON(http.StatusOK, gin.H{"models": out})
}
