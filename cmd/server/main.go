package main

import (
	"log"

	"image-backend/internal/config"
	"image-backend/internal/database"
	"image-backend/internal/generation"
	"image-backend/internal/server"
)

func main() {
	cfg := config.Load()
	weakSecrets := map[string]bool{config.DevDefaultJWTSecret: true, "change-me-in-production": true, "": true}
	if cfg.DatabaseURL != "" && weakSecrets[cfg.JWTSecret] {
		log.Fatal("refusing to start with weak JWT secret in non-dev mode: set JWT_SECRET")
	}
	db, err := database.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	if _, err := generation.SweepStuck(db); err != nil {
		log.Printf("启动兜底扫描失败（继续启动）: %v", err)
	}
	// provider 拼错一个字母的话，不该等第一个选中该模型的用户以 500 的形式替我们发现。
	// 不阻止启动：其他模型仍然可用，拒绝启动是更坏的结果。
	adapters := server.BuildAdapters(cfg)
	for _, p := range generation.ValidateProviders(db, adapters) {
		log.Printf("启动校验: %s", p)
	}
	r := server.NewRouterWithAdapters(db, cfg, adapters)
	log.Printf("listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
