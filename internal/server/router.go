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
	"image-backend/internal/settings"
	"image-backend/internal/storage"
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

// NewRouter creates a router backed by a settings.Runtime built from the DB.
//
// In tests cfg.ConfigEncryptionKey is typically empty; we fall back to a
// zero-byte key so the empty settings table (no encrypted rows) still works.
// Tests that need a fixed Registry inject it via NewRouterWithAdapters.
func NewRouter(db *gorm.DB, cfg *config.Config, opts ...RouterOption) *gin.Engine {
	key, err := settings.ParseKey(cfg.ConfigEncryptionKey)
	if err != nil {
		// Tests and local dev with no key set: use a zero key.
		// The settings table is empty in these environments, so no row ever
		// needs decryption — the key is irrelevant.
		key = make([]byte, 32)
	}
	st := settings.NewStore(db, key)
	rt, err := settings.NewRuntime(st)
	if err != nil {
		// Store.All() on an empty DB never fails; this is purely defensive.
		log.Printf("server: settings runtime init failed (%v); falling back to cfg", err)
		return NewRouterWithAdapters(db, cfg, BuildAdapters(cfg), opts...)
	}
	return newRouterFull(db, cfg, rt.Adapters, &handler.AdminSettingsHandler{Store: st, Runtime: rt}, rt.AppBaseURL(), opts...)
}

// NewRouterWithAdapters lets callers supply a fixed Registry.
//
// **Signature must not change**: ~25 existing server tests inject stub
// adapters through this entry point (TestGeneratePassesUpstreamModelAndDimensions,
// TestGeneratePassesGenerationIDToAdapter, TestGenerateStoresImageAndReportsStoredTrue).
// Breaking this breaks all of them.
func NewRouterWithAdapters(db *gorm.DB, cfg *config.Config, adapters generation.Registry, opts ...RouterOption) *gin.Engine {
	// Wrap the fixed registry in a closure so GenerationsHandler.Adapters
	// (now func() generation.Registry) resolves it on every request.
	// No AdminSettingsHandler is wired here: this path is for isolated tests
	// that provide their own adapter; they don't need the settings UI.
	return newRouterFull(db, cfg, func() generation.Registry { return adapters }, nil, cfg.AppBaseURL, opts...)
}

// NewRouterWithRuntime is the production entry point used by cmd/server/main.go.
//
// main.go pre-builds the Store and Runtime so it can seed, validate, and run
// ValidateProviders before the router starts accepting traffic.  Accepting both
// avoids exposing Runtime.store (private field) or adding a Store() getter.
func NewRouterWithRuntime(db *gorm.DB, cfg *config.Config, st *settings.Store, rt *settings.Runtime, opts ...RouterOption) *gin.Engine {
	return newRouterFull(db, cfg, rt.Adapters, &handler.AdminSettingsHandler{Store: st, Runtime: rt}, rt.AppBaseURL(), opts...)
}

// newRouterFull is the single place all routing lives.
//
// getAdapters is called per-request inside GenerationsHandler so hot-reloaded
// settings are always picked up.  adminSettings may be nil when the caller
// does not want the admin settings endpoints (test injection path).
func newRouterFull(
	db *gorm.DB,
	cfg *config.Config,
	getAdapters func() generation.Registry,
	adminSettings *handler.AdminSettingsHandler,
	appBaseURL string,
	opts ...RouterOption,
) *gin.Engine {
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
	billingClient := billing.New(cfg.StripeSecretKey, appBaseURL)

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

	generationsHandler := &handler.GenerationsHandler{DB: db, Adapters: getAdapters}
	authed.POST("/generations", generationsHandler.Create)
	authed.GET("/generations", generationsHandler.List)

	billingHandler := &handler.BillingHandler{DB: db, Billing: billingClient}
	authed.POST("/billing/subscribe", billingHandler.Subscribe)
	authed.POST("/billing/portal", billingHandler.Portal)

	adminHandler := &handler.AdminHandler{DB: db}
	admin := authed.Group("/admin", middleware.RequireAdmin(db))
	admin.POST("/credits", adminHandler.GrantCredits)

	// 模型配置：改扣费、上下架、接新模型都不必改代码发版。
	// getAdapters() 在构造时取一次快照：provider 集合是静态的，即使 Reload
	// 改了 API key，provider 名字不会变，校验结果不变。
	adminModels := &handler.AdminModelsHandler{DB: db, Adapters: getAdapters()}
	admin.GET("/models", adminModels.List)
	admin.POST("/models", adminModels.Create)
	admin.PATCH("/models/:id", adminModels.Patch)

	// 档位配置：调月度次数、上下架。与公开的 GET /plans 相反，后台要能看到
	// stripe_price_id 和已下架的档位。价格与 Price ID 不可改（见 handler 注释）。
	adminPlans := &handler.AdminPlansHandler{DB: db}
	admin.GET("/plans", adminPlans.List)
	admin.PATCH("/plans/:id", adminPlans.Patch)

	// 后台设置：仅当调用方提供了 AdminSettingsHandler（生产路径 & NewRouter）。
	// NewRouterWithAdapters（测试注入路径）不注册这两条路由，保持原有测试行为。
	if adminSettings != nil {
		admin.GET("/settings", adminSettings.Get)
		admin.PATCH("/settings", adminSettings.Patch)
	}

	return r
}

// BuildAdapters 构造 provider → adapter 注册表。
//
// 每个 adapter 都被 StoringAdapter 包一层：上游返回的图片 URL 约一小时后失效，
// 不转存的话历史记录里全是死链，而用户为那些图付过费。包在这里而不是各 adapter
// 内部，新增 provider 就自动获得转存，不依赖谁记得加代码。
//
// 导出是为了让 cmd/server/main.go 能先建好、校验完 provider 再交给路由，以及
// 让 TestBuildAdaptersWrapsEveryProviderInStoringAdapter 直接测试这条路径。
// 注意：生产路径（NewRouter / NewRouterWithRuntime）现在走 Runtime.buildAdapters，
// 该路径由 TestRuntimeAdaptersAlwaysWrappedInStoringAdapter 覆盖。
func BuildAdapters(cfg *config.Config) generation.Registry {
	store := buildStorage(cfg)
	return generation.Registry{
		"flux": generation.NewStoringAdapter(buildFluxAdapter(cfg), store),
	}
}

// buildStorage 在没配 R2 时退化成 NoopStorage。
//
// 与 FluxAPIKey 为空退化成 stub、StripeSecretKey 为空禁用计费是同一个约定：
// 本地开发不必凑齐所有外部依赖也能跑完整流程。
func buildStorage(cfg *config.Config) storage.Storage {
	if !cfg.StorageEnabled() {
		log.Println("storage: R2 未完整配置，图片不转存——image_url 存的是上游临时链接，约一小时后失效")
		return storage.NoopStorage{}
	}
	return storage.NewR2Storage(
		cfg.R2Endpoint, cfg.R2AccessKeyID, cfg.R2SecretAccessKey,
		cfg.R2Bucket, cfg.R2PublicBaseURL,
	)
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
