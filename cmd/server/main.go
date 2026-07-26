package main

import (
	"log"

	"image-backend/internal/config"
	"image-backend/internal/database"
	"image-backend/internal/server"
)

func main() {
	cfg := config.Load()
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
