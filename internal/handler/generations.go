package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"image-backend/internal/credit"
	"image-backend/internal/generation"
	"image-backend/internal/middleware"
	"image-backend/internal/model"
)

const (
	errCodeBadRequest          = 40000
	errCodeInsufficientCredits = 40001
	errCodeModelUnavailable    = 40003
	errCodeInternal            = 50000
)

// upstreamTimeout 覆盖最慢模型（Flux 实测 21 秒，慢时更久）并留余量。
const upstreamTimeout = 5 * time.Minute

type GenerationsHandler struct {
	DB       *gorm.DB
	Adapters generation.Registry
}

type generateRequest struct {
	Prompt      string `json:"prompt" binding:"required"`
	Model       string `json:"model" binding:"required"`
	AspectRatio string `json:"aspectRatio" binding:"required"`
	IsPublic    bool   `json:"isPublic"`
}

// Create 同步生成一张图。
//
// 编排顺序**不能换**：建 processing 行 → 扣费 → 调上游。
//
// 反过来（先扣费再建行）有一个无法补救的窗口：扣费成功后、建行之前进程崩溃，
// 流水里留下一条扣费记录但没有任何 generations 行指向它。启动兜底扫描靠扫
// processing 行找回退款，找不到行就永远退不回来——用户的钱凭空消失且无人知晓。
func (h *GenerationsHandler) Create(c *gin.Context) {
	userID := c.GetUint(middleware.CtxUserIDKey)

	var req generateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "invalid request body"})
		return
	}

	width, height, ok := generation.Dimensions(req.AspectRatio)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "unsupported aspect ratio"})
		return
	}

	var m model.ImageModel
	if err := h.DB.Where("id = ?", req.Model).First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "unknown model"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	if !m.Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeModelUnavailable, "message": "model is not available"})
		return
	}

	adapter, err := h.Adapters.Get(m.Provider)
	if err != nil {
		// 配置错误（表里有这个 provider 但没注册 adapter），不是用户的问题。
		log.Printf("[generations] provider %q 没有注册 adapter: %v", m.Provider, err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	gen := model.Generation{
		ID:          uuid.NewString(),
		UserID:      userID,
		Model:       m.ID,
		Prompt:      req.Prompt,
		AspectRatio: req.AspectRatio,
		Width:       width,
		Height:      height,
		Status:      model.GenStatusProcessing,
		IsPublic:    req.IsPublic,
	}
	if err := h.DB.Create(&gen).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	if _, err := credit.Spend(h.DB, userID, m.Credits, gen.ID); err != nil {
		// 扣费失败要把行标成 failed，否则它会一直挂在 processing——既让运维误以为
		// 系统卡住，也会被每次启动的兜底扫描反复扫到。
		h.markFailed(&gen, "insufficient credits")
		if errors.Is(err, credit.ErrInsufficientCredits) {
			c.JSON(http.StatusPaymentRequired,
				gin.H{"code": errCodeInsufficientCredits, "message": "not enough credits"})
			return
		}
		log.Printf("[generations] 扣费异常 gen=%s: %v", gen.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	// 刻意**不**继承 c.Request.Context()：客户端断开不应该取消一次已经付过费的
	// 生成。服务端必须把活干完并落库，用户回来能在历史里找到。Flux 实测 21 秒，
	// "中途关标签页"是常见情况而非边缘情况。
	upstreamCtx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer cancel()

	started := time.Now()
	res, genErr := adapter.Generate(upstreamCtx, generation.GenerateRequest{
		Prompt: req.Prompt, Width: width, Height: height,
	})
	elapsed := time.Since(started).Milliseconds()

	if genErr != nil {
		log.Printf("[generations] 上游失败 gen=%s user=%d: %v", gen.ID, userID, genErr)
		if err := credit.Refund(h.DB, gen.ID); err != nil {
			// 退款失败是严重问题：用户付了钱没拿到图也没拿回次数。必须留痕，
			// 启动兜底扫描会再试一次。
			log.Printf("[generations] 退款失败 gen=%s: %v", gen.ID, err)
		}
		gen.Status = model.GenStatusFailed
		gen.Error = genErr.Error()
		gen.CreditsSpent = 0
		gen.DurationMs = elapsed
		h.save(&gen)
		c.JSON(http.StatusOK, toGenerationResponse(gen))
		return
	}

	gen.Status = model.GenStatusSucceeded
	gen.ImageURL = res.ImageURL
	gen.UpstreamID = res.UpstreamID
	gen.UpstreamCost = res.UpstreamCost
	gen.CreditsSpent = m.Credits
	gen.DurationMs = elapsed
	h.save(&gen)
	c.JSON(http.StatusOK, toGenerationResponse(gen))
}

func (h *GenerationsHandler) markFailed(gen *model.Generation, reason string) {
	gen.Status = model.GenStatusFailed
	gen.Error = reason
	gen.CreditsSpent = 0
	h.save(gen)
}

func (h *GenerationsHandler) save(gen *model.Generation) {
	if err := h.DB.Save(gen).Error; err != nil {
		// 落库失败不改变已经发生的事实（次数已扣/已退、图已生成），所以不改
		// 响应，只留痕。
		log.Printf("[generations] 落库失败 gen=%s: %v", gen.ID, err)
	}
}

// toGenerationResponse 的字段名与前端 image-front 的 Generation 判别联合一一对应。
// 改这里就要同步改 image-front/lib/generation-types.ts。
func toGenerationResponse(g model.Generation) gin.H {
	out := gin.H{
		"id":           g.ID,
		"model":        g.Model,
		"prompt":       g.Prompt,
		"aspectRatio":  g.AspectRatio,
		"isPublic":     g.IsPublic,
		"status":       g.Status,
		"creditsSpent": g.CreditsSpent,
		"createdAt":    g.CreatedAt.UTC().Format(time.RFC3339),
	}
	if g.Status == model.GenStatusSucceeded {
		out["imageUrl"] = g.ImageURL
	} else {
		out["error"] = g.Error
	}
	return out
}
