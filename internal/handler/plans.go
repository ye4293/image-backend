package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

type PlansHandler struct {
	DB *gorm.DB
}

// planResponse 是**显式**的输出结构，不是 model.Plan 的别名。
//
// 之所以不直接序列化 model.Plan：那样会把 StripePriceID 一起吐给前端。
// 前端下单只传 planId，价格与 Price 由后端查表决定——把 Price ID 交给客户端
// 等于让客户端参与决定自己付多少钱。少一个字段的代价远小于加一个字段的风险，
// 所以这里宁可手写映射，也不用"从 model 里排除某些字段"的写法。
type planResponse struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	PriceUSDCents  int    `json:"priceUsdCents"`
	MonthlyCredits int    `json:"monthlyCredits"`
}

// List 返回启用的档位，按 sort_order 升序。公开接口——定价页在未登录时就要能看。
//
// 只返回 enabled 的行：运营下架某档后，前端不该还能看到它，否则用户会去点一个
// 已经不卖的档位。
func (h *PlansHandler) List(c *gin.Context) {
	var plans []model.Plan
	if err := h.DB.Where("enabled = ?", true).Order("sort_order asc, id asc").Find(&plans).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	out := make([]planResponse, 0, len(plans))
	for _, p := range plans {
		out = append(out, planResponse{
			ID:             p.ID,
			DisplayName:    p.DisplayName,
			PriceUSDCents:  p.PriceUSDCents,
			MonthlyCredits: p.MonthlyCredits,
		})
	}
	c.JSON(http.StatusOK, gin.H{"plans": out})
}
