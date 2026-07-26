package main

import (
	"log"

	"image-backend/internal/config"
	"image-backend/internal/server"
)

func main() {
	cfg := config.Load()
	r := server.NewRouter(nil, cfg)
	log.Printf("listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
