package main

import (
	"log"

	"image-backend/internal/config"
	"image-backend/internal/database"
	"image-backend/internal/server"
)

func main() {
	cfg := config.Load()
	if cfg.DatabaseURL != "" && cfg.JWTSecret == config.DevDefaultJWTSecret {
		log.Fatal("refusing to start with default JWT secret in non-dev mode: set JWT_SECRET")
	}
	db, err := database.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	r := server.NewRouter(db, cfg)
	log.Printf("listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
