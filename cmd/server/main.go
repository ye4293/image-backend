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

	// CONFIG_ENCRYPTION_KEY は解けなければ全上流クレデンシャルが使えない——
	// 起動しても「上流認証失敗」という誤った方向で調査が始まるだけなので、
	// ここで落とす（設計文書 §2.5）。
	encKey, err := settings.ParseKey(cfg.ConfigEncryptionKey)
	if err != nil {
		log.Fatalf("config: CONFIG_ENCRYPTION_KEY: %v\n"+
			"生成方法: openssl rand -base64 32", err)
	}

	// ValidateStorage の Fatal は削除。設定はDBで管理され、書き込み時に
	// 検証されるため（§2.5）。不正な値があっても起動して管理画面で直せる。

	if !cfg.BillingEnabled() {
		log.Println("billing: STRIPE_SECRET_KEY / STRIPE_WEBHOOK_SECRET 未配齐，计费功能已禁用")
	}
	db, err := database.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}

	// 設定ストアを構築してDBから設定を読み込む。
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
