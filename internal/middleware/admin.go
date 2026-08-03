package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

// RequireAdmin 必须挂在 Auth 之后——它依赖 Auth 放进 context 的 userID。
//
// 每次请求查一次库而不是把 role 塞进 JWT：role 变更（封禁、降权）要能立即生效，
// 而 JWT 有 7 天有效期，塞进去就意味着降权后还有 7 天的窗口。
func RequireAdmin(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetUint(CtxUserIDKey)
		var user model.User
		if err := db.First(&user, userID).Error; err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"code": 40300, "message": "forbidden"})
			return
		}
		if user.Role != model.RoleAdmin {
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"code": 40300, "message": "forbidden"})
			return
		}
		c.Next()
	}
}
