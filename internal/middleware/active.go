package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

// RequireActiveUser 拦截被封禁的用户。必须挂在 Auth 之后。
//
// 单独一个中间件而不是塞进 Auth：Auth 保持纯粹的 token 解析（无数据库依赖），
// 这里显式承担一次查库。挂在 authed 组上，新增受保护路由自动获得保护——比要求
// 每个 handler 自己记得检查可靠。
//
// 为什么不把 status 塞进 JWT：JWT 有 7 天有效期，那样封禁就有 7 天的窗口期。
func RequireActiveUser(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetUint(CtxUserIDKey)
		var user model.User
		if err := db.First(&user, userID).Error; err != nil {
			// 查不到用户（含 userID 为 0 的情况）一律拒绝——fail closed。
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"code": 40300, "message": "forbidden"})
			return
		}
		if user.Status != "active" {
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"code": 40300, "message": "account is not active"})
			return
		}
		c.Next()
	}
}
