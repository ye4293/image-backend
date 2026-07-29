package server

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/config"
	"image-backend/internal/generation"
	"image-backend/internal/handler"
	"image-backend/internal/middleware"
)

func NewRouter(db *gorm.DB, cfg *config.Config) *gin.Engine {
	return NewRouterWithAdapters(db, cfg, BuildAdapters(cfg))
}

// NewRouterWithAdapters 让调用方自己提供 Registry。cmd/server/main.go 用它是为了能在
// 开始接流量之前，拿**同一个** Registry 跑 generation.ValidateProviders——各建一个的
// 话校验的就不是真正在服务的那份了。
func NewRouterWithAdapters(db *gorm.DB, cfg *config.Config, adapters generation.Registry) *gin.Engine {
	r := gin.Default()
	api := r.Group("/api/v1")
	api.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	authHandler := &handler.AuthHandler{DB: db, Cfg: cfg}
	api.POST("/auth/register", authHandler.Register)
	api.POST("/auth/login", authHandler.Login)

	modelsHandler := &handler.ModelsHandler{DB: db}
	api.GET("/models", modelsHandler.Get)

	// 公开：定价页在未登录时就要能看到档位。
	plansHandler := &handler.PlansHandler{DB: db}
	api.GET("/plans", plansHandler.List)

	meHandler := &handler.MeHandler{DB: db}
	authed := api.Group("", middleware.Auth(cfg.JWTSecret), middleware.RequireActiveUser(db))
	authed.GET("/me", meHandler.Get)

	generationsHandler := &handler.GenerationsHandler{DB: db, Adapters: adapters}
	authed.POST("/generations", generationsHandler.Create)

	adminHandler := &handler.AdminHandler{DB: db}
	admin := authed.Group("/admin", middleware.RequireAdmin(db))
	admin.POST("/credits", adminHandler.GrantCredits)
	return r
}

// BuildAdapters 构造 provider → adapter 注册表。
//
// 导出是为了让 cmd/server/main.go 能先建好、校验完 provider 再交给路由。
func BuildAdapters(cfg *config.Config) generation.Registry {
	return generation.Registry{"flux": buildFluxAdapter(cfg)}
}

// buildFluxAdapter 在没有配置 key 时退化成 stub。
//
// 这不是"方便"，是必需：接真上游后端到端测试每跑一次都真调 Flux——每次约 21
// 秒、每次花钱。stub 保留 fail/slow/quick 关键词，让测试既快又免费。
func buildFluxAdapter(cfg *config.Config) generation.Adapter {
	if cfg.FluxAPIKey == "" {
		log.Println("generation: FLUX_API_KEY 未配置，使用 stub adapter（返回占位图）")
		return generation.NewStubAdapter()
	}
	// 上游模型名不在这里传：它按请求从 image_models.upstream_model 取，这样同一个
	// provider 下的多个模型才不会被静默提交到同一个上游路径。
	return generation.NewFluxAdapter(cfg.EZLinkAIBaseURL, cfg.FluxAPIKey)
}
