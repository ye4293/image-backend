package server

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/billing"
	"image-backend/internal/config"
	"image-backend/internal/generation"
	"image-backend/internal/handler"
	"image-backend/internal/middleware"
)

// RouterOption 覆盖路由内部构造的依赖。
//
// 存在的理由只有一个：webhook 的业务规则（按 Price 反查档位、交叉校验 user_id）
// 依赖一次"拉取订阅"的上游调用，而那几条规则失守时不会有任何征兆——必须能在不
// 联网的情况下测。生产代码不传任何 option。
type RouterOption func(*routerDeps)

type routerDeps struct {
	subs billing.SubscriptionFetcher
}

// WithSubscriptionFetcher 替换 webhook 拉取订阅的实现（测试注入假实现用）。
func WithSubscriptionFetcher(f billing.SubscriptionFetcher) RouterOption {
	return func(d *routerDeps) { d.subs = f }
}

func NewRouter(db *gorm.DB, cfg *config.Config, opts ...RouterOption) *gin.Engine {
	return NewRouterWithAdapters(db, cfg, BuildAdapters(cfg), opts...)
}

// NewRouterWithAdapters 让调用方自己提供 Registry。cmd/server/main.go 用它是为了能在
// 开始接流量之前，拿**同一个** Registry 跑 generation.ValidateProviders——各建一个的
// 话校验的就不是真正在服务的那份了。
func NewRouterWithAdapters(db *gorm.DB, cfg *config.Config, adapters generation.Registry, opts ...RouterOption) *gin.Engine {
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

	// billing.New 在没有 STRIPE_SECRET_KEY 时返回 nil，handler 据此回 503。
	billingClient := billing.New(cfg.StripeSecretKey, cfg.AppBaseURL)

	// webhook 拉取订阅用的实现。**必须先判 billingClient 是否为 nil**：把一个 nil
	// 的 *billing.Client 直接赋给接口字段，接口本身是非 nil 的，调用时会 panic 在
	// webhook 里，而那会让 Stripe 无限重投同一个事件。
	deps := routerDeps{}
	if billingClient != nil {
		deps.subs = billingClient
	}
	for _, opt := range opts {
		opt(&deps)
	}

	// 公开：Stripe 不带我们的 cookie，所以 webhook **不能**挂在 authed 组下
	// （挂上去线上所有事件都会 401 被丢弃）。安全性由 handler 内的验签保证。
	api.POST("/webhooks/stripe", (&handler.StripeWebhookHandler{
		DB: db, Secret: cfg.StripeWebhookSecret, Subs: deps.subs,
	}).Handle)

	meHandler := &handler.MeHandler{DB: db}
	authed := api.Group("", middleware.Auth(cfg.JWTSecret), middleware.RequireActiveUser(db))
	authed.GET("/me", meHandler.Get)

	generationsHandler := &handler.GenerationsHandler{DB: db, Adapters: adapters}
	authed.POST("/generations", generationsHandler.Create)

	billingHandler := &handler.BillingHandler{DB: db, Billing: billingClient}
	authed.POST("/billing/subscribe", billingHandler.Subscribe)
	authed.POST("/billing/portal", billingHandler.Portal)

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
