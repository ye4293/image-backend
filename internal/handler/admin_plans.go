package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

// AdminPlansHandler 让运营调档位次数、上下架档位，不必改代码发版。
//
// 不需要 Adapters（与 AdminModelsHandler 不同）：档位不指向任何上游 provider。
type AdminPlansHandler struct {
	DB *gorm.DB
}

// adminPlanResponse 后台视角的档位行。
//
// 与公开的 planResponse 刻意相反：这里**要**返回 StripePriceID 和 Enabled。
// StripePriceID 是运营确认"cmd/seed-stripe 跑过没有"的唯一线索——为空说明还没建
// Stripe Price，该档位下单会失败，而定价页看着完全正常。
type adminPlanResponse struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	PriceUSDCents  int    `json:"priceUsdCents"`
	MonthlyCredits int    `json:"monthlyCredits"`
	StripePriceID  string `json:"stripePriceID"`
	Enabled        bool   `json:"enabled"`
	SortOrder      int    `json:"sortOrder"`
}

func toAdminPlanResponse(p model.Plan) adminPlanResponse {
	return adminPlanResponse{
		ID:             p.ID,
		DisplayName:    p.DisplayName,
		PriceUSDCents:  p.PriceUSDCents,
		MonthlyCredits: p.MonthlyCredits,
		StripePriceID:  p.StripePriceID,
		Enabled:        p.Enabled,
		SortOrder:      p.SortOrder,
	}
}

// List 返回**所有**档位，包括已下架的。
//
// 公开的 GET /plans 只返回 enabled=true。后台若也过滤，下架一个档位之后就再也找不到
// 它、无法重新上架——运营只剩手改数据库这条路。
func (h *AdminPlansHandler) List(c *gin.Context) {
	var rows []model.Plan
	if err := h.DB.Order("sort_order asc, id asc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	out := make([]adminPlanResponse, 0, len(rows))
	for _, p := range rows {
		out = append(out, toAdminPlanResponse(p))
	}
	c.JSON(http.StatusOK, gin.H{"plans": out})
}

// patchPlanRequest 只开放五个字段，且**全是指针**。
//
// 非指针的话，"没传 enabled"和"传了 enabled:false"无法区分——一个只想把 pro 的月度
// 次数从 800 调成 1000 的请求会顺手把这一档下架，而且没有任何报错：定价页上它凭空
// 消失，没人会联想到是一次调量造成的。
//
// **stripePriceId 永不可改。** webhook 靠这一列反查档位，手填一个 Price ID 就是
// "付了 Pro 的钱、拿到 Starter 的次数"。
//
// **priceUsdCents 只在该档尚未播种（stripe_price_id 为空）时可改**，见 Patch 里的说明。
type patchPlanRequest struct {
	DisplayName *string `json:"displayName"`
	// MonthlyCredits 允许 0（该档暂时不发次数是合法配置），拒绝负数——与
	// credit.ResetMonthly 的校验保持一致。
	//
	// **改动下次 invoice.paid 才生效，不追溯**：credit.ResetMonthly 在续费时才读 plan
	// 行，已订阅用户当期的余额不会因为这次修改而变化。
	MonthlyCredits *int `json:"monthlyCredits"`
	// PriceUSDCents 只在尚未播种时可改。已播种后传它会被拒绝。
	PriceUSDCents *int  `json:"priceUsdCents"`
	Enabled       *bool `json:"enabled"`
	SortOrder     *int  `json:"sortOrder"`
}

// alwaysImmutablePlanFields 这些 key 出现在请求体里就整条拒绝，与是否播种无关。
//
// 为什么不能静默忽略：运营发出 {"stripePriceId":"price_x"} 拿到 200，会认为改成功了，
// 直到某次对账才发现完全没生效。这条保护失效时没有任何征兆，所以必须显式报错，并在
// message 里告诉运营正确的路径。
var alwaysImmutablePlanFields = []string{"stripePriceId", "stripePriceID", "id"}

func (h *AdminPlansHandler) Patch(c *gin.Context) {
	id := c.Param("id")

	// 先解成 map 检查有没有出现不可改字段。ShouldBindJSON 会消耗 body，所以只读一次
	// 原始字节，两次解析都用它。
	raw, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "invalid request body"})
		return
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "invalid request body"})
		return
	}
	for _, k := range alwaysImmutablePlanFields {
		if _, ok := probe[k]; ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"code": errCodeBadRequest,
				"message": k + " 不可通过 API 修改：webhook 靠 stripe_price_id 反查档位，" +
					"手填一个 Price ID 就是让用户付了一档的钱、拿到另一档的次数。" +
					"它由 cmd/seed-stripe 回填。",
			})
			return
		}
	}

	var req patchPlanRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "invalid request body"})
		return
	}

	var p model.Plan
	if err := h.DB.Where("id = ?", id).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": 40400, "message": "plan not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	// 用 map 而不是结构体收集要改的列：Updates 传结构体会跳过零值，正好会静默漏掉
	// enabled:false、monthlyCredits:0 和 sortOrder:0——运营点了"下架"却毫无反应。
	updates := map[string]any{}
	if req.PriceUSDCents != nil {
		// **规则跟着理由走。** 价格不可改的理由是"Stripe 的 Price 金额不可变，改我们
		// 这边会造成两边不一致"。而 stripe_price_id 为空时 Stripe 那边**还没有任何
		// Price 对象**，不存在可以不一致的东西——此时锁死价格只会逼人去手改数据库，
		// 那比开放这个字段危险得多（手改没有任何校验）。
		//
		// 一旦播种过就锁死：那时改我们这边的数字不会改变 Stripe 实际收多少钱，只会
		// 让定价页显示 $29.99 而用户被扣 $99.99——最难向用户解释的一类不一致。
		if p.StripePriceID != "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"code": errCodeBadRequest,
				"message": "priceUsdCents 不可再改：该档位已在 Stripe 建好 Price（" + p.StripePriceID +
					"），而 Stripe 的 Price 金额不可变。调价需新建 Price 并迁移已有订阅。" +
					"改这里只会让定价页显示的价格与用户实际被扣的钱不一致。",
			})
			return
		}
		if *req.PriceUSDCents <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    errCodeBadRequest,
				"message": "priceUsdCents 必须为正整数（单位：美分）",
			})
			return
		}
		updates["price_usd_cents"] = *req.PriceUSDCents
	}
	if req.DisplayName != nil {
		if *req.DisplayName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "displayName 不能为空"})
			return
		}
		updates["display_name"] = *req.DisplayName
	}
	if req.MonthlyCredits != nil {
		// 0 合法（该档暂时不发次数），负数不合法：credit.ResetMonthly 是"设置"语义，
		// 负数会把余额设成负的，之后每次生成都因余额不足失败。
		if *req.MonthlyCredits < 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    errCodeBadRequest,
				"message": "monthlyCredits 不能为负（0 合法，等于该档暂时不发次数）",
			})
			return
		}
		updates["monthly_credits"] = *req.MonthlyCredits
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if req.SortOrder != nil {
		updates["sort_order"] = *req.SortOrder
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "没有可修改的字段"})
		return
	}

	if err := h.DB.Model(&p).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	// 重读而不是拿内存里那份回传：让响应反映库里真正的状态，避免"以为改了但没落库"。
	if err := h.DB.Where("id = ?", id).First(&p).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, toAdminPlanResponse(p))
}
