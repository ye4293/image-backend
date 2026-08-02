package main

import (
	"log"
	"os"

	"image-backend/internal/bootstrap"
	"image-backend/internal/config"
	"image-backend/internal/database"
	"image-backend/internal/generation"
	"image-backend/internal/server"
	"image-backend/internal/settings"
)

func main() {
	cfg := config.Load()
	weakSecrets := map[string]bool{config.DevDefaultJWTSecret: true, "change-me-in-production": true, "": true}
	if cfg.DatabaseURL != "" && weakSecrets[cfg.JWTSecret] {
		log.Fatal("refusing to start with weak JWT secret in non-dev mode: set JWT_SECRET")
	}
	if err := cfg.ValidateStripe(); err != nil {
		log.Fatalf("config: %v", err)
	}
	// CORS 来源列表写错时**不会**以报错的形式暴露：进程照常起来、curl 测全通，
	// 只有浏览器里所有请求被拦掉，而日志里只有一串 404 OPTIONS。这个值只可能由
	// 一次部署引入，拒绝启动会被立刻发现，所以这里 Fatal 而不是告警。
	if err := cfg.ValidateCORS(); err != nil {
		log.Fatalf("config: %v", err)
	}
	if len(cfg.AllowedOrigins()) == 0 {
		log.Println("cors: CORS_ALLOWED_ORIGINS 未配置，不会发送任何 CORS 头。" +
			"前端与后端同源（或前端走服务端代理）时这是正确状态；" +
			"若前端在别的域名下直连本后端，浏览器会拦掉所有请求，而 curl 测起来一切正常")
	}

	// CONFIG_ENCRYPTION_KEY 解不开的话，库里所有上游凭据等于全部失效——那时候
	// 起来也没用，而且故障会以"上游认证失败"的形式出现，把排查方向完全带偏。
	// 所以这里**拒绝启动**（见设计文档 §2.5）。
	encKey, err := settings.ParseKey(cfg.ConfigEncryptionKey)
	if err != nil {
		log.Fatalf("config: CONFIG_ENCRYPTION_KEY: %v\n"+
			"生成方法：openssl rand -base64 32", err)
	}

	// 原先 cfg.ValidateStorage() 的 Fatal 已移除：R2 那几项现在由数据库管理、
	// 在写入时校验（settings.Validate），启动期只降级为告警。库里的值可能是上一个
	// 管理员改坏的，此时拒绝启动等于让一次误操作把服务打死，而管理员连登录进来
	// 改回去的机会都没有（见设计文档 §2.5）。

	if !cfg.BillingEnabled() {
		log.Println("billing: STRIPE_SECRET_KEY / STRIPE_WEBHOOK_SECRET 未配齐，计费功能已禁用")
	}
	db, err := database.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}

	// 建设置存储，后续从库里读当前生效的配置。
	st := settings.NewStore(db, encKey)

	// 首次启动播种：把 env 里的配置写进 DB（仅在 DB 里没有时才写）。
	// 播种后这些 env 变量不再被读取，DB 是权威来源（见设计文档 §2.4）。
	n, seedErr := st.SeedFromEnv(os.Getenv)
	if seedErr != nil {
		log.Printf("settings: 播种失败（继续启动）: %v", seedErr)
	} else if n > 0 {
		log.Printf("settings: 从环境变量播种了 %d 项配置——今后以数据库为准；"+
			"env 里的 FLUX_API_KEY / R2_* / APP_BASE_URL 不再被读取", n)
	}

	// Runtime 加载快照并构造客户端。
	rt, err := settings.NewRuntime(st)
	if err != nil {
		log.Fatalf("settings: 无法初始化 Runtime: %v", err)
	}

	// 启动期校验降级为告警（见设计文档 §2.5）：库里的值可能是上一个管理员
	// 改坏的，拒绝启动会让服务死锁——管理员无法登录去修复。
	for _, problem := range rt.Validate() {
		log.Printf("settings 告警（可在后台修复）: %s", problem)
	}

	if _, err := generation.SweepStuck(db); err != nil {
		log.Printf("启动兜底扫描失败（继续启动）: %v", err)
	}
	// 第一个管理员的引导：此前只能手工执行 UPDATE users SET role='admin'。
	// 不阻止启动——提权失败时其他功能仍然可用，且下次重启还会再试。
	if _, err := bootstrap.PromoteAdmin(db, cfg.BootstrapAdminEmail); err != nil {
		log.Printf("引导管理员失败（继续启动）: %v", err)
	}
	// provider 拼错一个字母的话，不该等第一个选中该模型的用户以 500 的形式替我们发现。
	// 不阻止启动：其他模型仍然可用，拒绝启动是更坏的结果。
	for _, p := range generation.ValidateProviders(db, rt.Adapters()) {
		log.Printf("启动校验: %s", p)
	}
	r := server.NewRouterWithRuntime(db, cfg, st, rt)
	log.Printf("listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
