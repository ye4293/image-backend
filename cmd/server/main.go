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
	r := server.NewRouter(db, cfg)
	log.Printf("listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
