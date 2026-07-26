package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"image-backend/internal/auth"
)

const CtxUserIDKey = "userID"

func Auth(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 40100, "message": "missing token"})
			return
		}
		userID, err := auth.ParseToken(strings.TrimPrefix(header, "Bearer "), secret)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 40100, "message": "invalid token"})
			return
		}
		c.Set(CtxUserIDKey, userID)
		c.Next()
	}
}
