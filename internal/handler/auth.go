package handler

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"image-backend/internal/auth"
	"image-backend/internal/bootstrap"
	"image-backend/internal/config"
	"image-backend/internal/credit"
	"image-backend/internal/model"
)

type AuthHandler struct {
	DB  *gorm.DB
	Cfg *config.Config
	// SignupBonus 返回当前生效的注册赠送次数，**按请求调用**——后台改完立刻生效，
	// 与 GenerationsHandler.Adapters 同一个约定。
	//
	// 可以为 nil（测试注入路径不给它），此时视为不赠送。不用 int 字段是因为那样
	// 后台改了要重启才生效，而这一项恰恰是会被反复微调的运营参数。
	SignupBonus func() int
}

type registerRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8,max=72"`
}

func (h *AuthHandler) Register(c *gin.Context) {
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "invalid email or password format"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	user := model.User{Email: strings.ToLower(req.Email), PasswordHash: string(hash)}
	if err := h.DB.Create(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			c.JSON(http.StatusConflict, gin.H{"code": 40901, "message": "email already registered"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	// 引导管理员的第二个触发点。启动时那次（cmd/server/main.go）只能提权**已存在**的
	// 用户，而全新环境里最常见的顺序恰好相反：先起服务，再注册。少了这里，配了
	// BOOTSTRAP_ADMIN_EMAIL 的操作者必须"注册完再重启一次"才拿到 admin——而 dev 模式
	// 每次启动都是一个新的临时 SQLite，重启会把刚注册的账号一起丢掉，永远拿不到。
	//
	// 安全性与启动时那次同级：只认配置里那一个邮箱，仍然要正常登录拿 JWT。
	if h.Cfg.BootstrapAdminEmail != "" && strings.EqualFold(user.Email, h.Cfg.BootstrapAdminEmail) {
		if _, err := bootstrap.PromoteAdmin(h.DB, user.Email); err != nil {
			// 提权失败不影响注册本身已经成功的事实，留痕即可——下次启动还会再试。
			log.Printf("[auth] 注册后引导管理员失败 email=%s: %v", user.Email, err)
		}
	}
	// 注册赠送体验额度。**失败不影响注册本身**：用户已经建好了，因为送不了额度就
	// 让注册失败是更坏的结果（用户看到"注册失败"，但账号其实已存在，再试会撞 409）。
	//
	// 但必须留痕，否则"新用户没拿到额度"是完全无声的——它不会报错、不会影响登录，
	// 只会表现为转化率莫名偏低，而那要几周后看数据才可能被注意到。
	//
	// ErrAlreadyGranted 不算失败：GrantSignupBonus 按 userID 幂等，重复调用是正常的
	// （客户端重发、请求重放），此时静默跳过。
	if h.SignupBonus != nil {
		if bonus := h.SignupBonus(); bonus > 0 {
			err := credit.GrantSignupBonus(h.DB, user.ID, bonus)
			switch {
			case err == nil:
				log.Printf("[auth] 注册赠送 %d 次额度 user=%d email=%s", bonus, user.ID, user.Email)
			case errors.Is(err, credit.ErrAlreadyGranted):
				// 幂等命中，什么都不用做。
			default:
				log.Printf("[auth] 注册赠送失败（注册本身已成功）user=%d email=%s bonus=%d: %v",
					user.ID, user.Email, bonus, err)
			}
		}
	}

	c.JSON(http.StatusCreated, gin.H{"id": user.ID, "email": user.Email})
}

type loginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

func (h *AuthHandler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "invalid email or password format"})
		return
	}
	var user model.User
	if err := h.DB.Where("email = ?", strings.ToLower(req.Email)).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusUnauthorized, gin.H{"code": 40101, "message": "invalid email or password"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 40101, "message": "invalid email or password"})
		return
	}
	token, err := auth.GenerateToken(user.ID, h.Cfg.JWTSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token})
}
