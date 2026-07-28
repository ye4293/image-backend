package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/config"
	"image-backend/internal/handler"
	"image-backend/internal/middleware"
)

func NewRouter(db *gorm.DB, cfg *config.Config) *gin.Engine {
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

	meHandler := &handler.MeHandler{DB: db}
	authed := api.Group("", middleware.Auth(cfg.JWTSecret))
	authed.GET("/me", meHandler.Get)

	adminHandler := &handler.AdminHandler{DB: db}
	admin := authed.Group("/admin", middleware.RequireAdmin(db))
	admin.POST("/credits", adminHandler.GrantCredits)
	return r
}
