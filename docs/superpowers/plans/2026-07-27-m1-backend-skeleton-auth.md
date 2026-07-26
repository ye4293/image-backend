# M1：后端骨架与邮箱认证 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 搭建 image-backend 的 Go 服务骨架，实现邮箱注册/登录、JWT 认证与 `/me` 接口，全部带测试。

**Architecture:** Gin 提供 HTTP 层，GORM 连接数据库（生产 Postgres、测试/本地默认 SQLite 内存库），JWT (HS256) 做无状态会话。路由构造函数 `server.NewRouter(db, cfg)` 与 `main.go` 分离，便于 httptest 全链路测试。

**Tech Stack:** Go 1.22+、Gin、GORM（`gorm.io/driver/postgres` + `github.com/glebarez/sqlite` 纯 Go 驱动，Windows 免 CGO）、`golang-jwt/jwt/v5`、`golang.org/x/crypto/bcrypt`。

**设计文档:** `docs/superpowers/specs/2026-07-27-image-platform-design.md`

**里程碑路线图**（本计划只做 M1，后续每个里程碑单独出计划）：
- M1 后端骨架 + 邮箱认证（本计划）
- M2 计费：plans/subscriptions/credit 账本 + Stripe 订阅与加量包 + webhook 幂等
- M3 生成链路：models/generations + ezlinkai 调用 + 扣费退款 + reconcile worker + R2 转存
- M4 前端 image-front：Next.js 骨架 + 认证页 + 生成工作台 + 定价页
- M5 画廊、历史、账户页、管理后台、Google/GitHub OAuth、邮箱验证邮件

**约定（所有任务遵守）：**
- 工作目录：`C:\Users\brows\Desktop\image-backend`
- 每个任务完成后必须 `go build ./... && go vet ./...` 通过再 commit
- 错误响应统一 `{"code": <int>, "message": <string>}`
- commit message 用中文，遵循 `feat:/fix:/docs:/test:` 前缀

---

### Task 1: Go 模块 + 路由骨架 + 健康检查

**Files:**
- Create: `go.mod`（由命令生成）
- Create: `internal/config/config.go`
- Create: `internal/server/router.go`
- Create: `cmd/server/main.go`
- Test: `internal/server/router_test.go`

- [ ] **Step 1: 初始化模块并安装依赖**

```bash
cd /c/Users/brows/Desktop/image-backend
go mod init image-backend
go get github.com/gin-gonic/gin@latest
```

- [ ] **Step 2: 写配置加载**

`internal/config/config.go`：

```go
package config

import "os"

type Config struct {
	Port        string
	DatabaseURL string
	JWTSecret   string
}

func Load() *Config {
	return &Config{
		Port:        getEnv("PORT", "8080"),
		DatabaseURL: getEnv("DATABASE_URL", ""),
		JWTSecret:   getEnv("JWT_SECRET", "dev-secret-change-me"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

- [ ] **Step 3: 写健康检查的失败测试**

`internal/server/router_test.go`：

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"image-backend/internal/config"
)

func setupRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{JWTSecret: "test-secret"}
	return NewRouter(nil, cfg)
}

func TestHealth(t *testing.T) {
	r := setupRouter(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}
```

- [ ] **Step 4: 运行测试确认失败**

Run: `go test ./internal/server/ -run TestHealth -v`
Expected: FAIL（`NewRouter` 未定义，编译错误）

- [ ] **Step 5: 实现路由骨架**

`internal/server/router.go`：

```go
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/config"
)

func NewRouter(db *gorm.DB, cfg *config.Config) *gin.Engine {
	r := gin.Default()
	api := r.Group("/api/v1")
	api.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	return r
}
```

`cmd/server/main.go`：

```go
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
```

（`db` 参数本任务先传 nil，Task 2 接入。）

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./internal/server/ -run TestHealth -v`
Expected: PASS

- [ ] **Step 7: 编译检查并提交**

```bash
go build ./... && go vet ./...
git add go.mod go.sum cmd internal
git commit -m "feat: 初始化 Go 服务骨架与健康检查接口"
```

---

### Task 2: 数据库层 + User 模型

**Files:**
- Create: `internal/model/user.go`
- Create: `internal/database/database.go`
- Modify: `cmd/server/main.go`
- Test: `internal/database/database_test.go`

- [ ] **Step 1: 安装依赖**

```bash
go get gorm.io/gorm@latest gorm.io/driver/postgres@latest github.com/glebarez/sqlite@latest
```

- [ ] **Step 2: 写失败测试**

`internal/database/database_test.go`：

```go
package database

import (
	"testing"

	"image-backend/internal/model"
)

func TestOpenMigratesUserTable(t *testing.T) {
	db, err := Open("") // 空 DSN → SQLite 内存库
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	u := model.User{Email: "a@b.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}
	dup := model.User{Email: "a@b.com", PasswordHash: "y"}
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("expected unique constraint error for duplicate email")
	}
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/database/ -v`
Expected: FAIL（`Open`、`model.User` 未定义）

- [ ] **Step 4: 实现模型与数据库层**

`internal/model/user.go`：

```go
package model

import "time"

type User struct {
	ID           uint   `gorm:"primaryKey"`
	Email        string `gorm:"uniqueIndex;size:255;not null"`
	PasswordHash string `gorm:"size:255"`
	Role         string `gorm:"size:20;not null;default:user"`
	Status       string `gorm:"size:20;not null;default:active"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
```

`internal/database/database.go`：

```go
package database

import (
	"log"
	"os"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

// Open 连接数据库。databaseURL 为空时使用临时文件 SQLite（本地开发/测试，dev 模式）。
//
// 注意：不能用 ":memory:" 内存库——glebarez/sqlite（modernc 纯 Go 驱动）不支持
// cache=shared，每个新连接都会得到一个独立的空库，连接池新开连接时会随机出现
// "no such table" 错误。临时文件库跨连接共享同一份数据，没有该问题。
func Open(databaseURL string) (*gorm.DB, error) {
	var dialector gorm.Dialector
	devMode := databaseURL == ""
	var devPath string
	if devMode {
		f, err := os.CreateTemp("", "image-backend-dev-*.db")
		if err != nil {
			return nil, err
		}
		devPath = f.Name()
		if err := f.Close(); err != nil {
			return nil, err
		}
		dialector = sqlite.Open(devPath)
	} else {
		dialector = postgres.Open(databaseURL)
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		return nil, err
	}
	if devMode {
		// SQLite 是单写者，限制为单连接避免并发写锁冲突。
		sqlDB, err := db.DB()
		if err != nil {
			return nil, err
		}
		sqlDB.SetMaxOpenConns(1)
		log.Printf("database: using temporary SQLite %s (dev mode), no DATABASE_URL configured", devPath)
	}
	if err := db.AutoMigrate(&model.User{}); err != nil {
		return nil, err
	}
	return db, nil
}
```

修改 `cmd/server/main.go` 接入数据库：

```go
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
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/database/ -v`
Expected: PASS

- [ ] **Step 6: 编译检查并提交**

```bash
go build ./... && go vet ./...
git add cmd internal go.mod go.sum
git commit -m "feat: 添加数据库层与 User 模型（Postgres/SQLite 双驱动）"
```

---

### Task 3: 邮箱注册接口

**Files:**
- Create: `internal/handler/auth.go`
- Modify: `internal/server/router.go`
- Modify: `internal/server/router_test.go`（setupRouter 改为带内存库）
- Test: `internal/server/auth_test.go`

- [ ] **Step 1: 安装依赖**

```bash
go get golang.org/x/crypto/bcrypt
```

- [ ] **Step 2: 修改 setupRouter 使用内存库**

`internal/server/router_test.go` 中 `setupRouter` 替换为：

```go
func setupRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{JWTSecret: "test-secret"}
	return NewRouter(db, cfg)
}
```

import 增加 `"image-backend/internal/database"`。

- [ ] **Step 3: 写失败测试**

`internal/server/auth_test.go`：

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func postJSON(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestRegister(t *testing.T) {
	r := setupRouter(t)

	w := postJSON(r, "/api/v1/auth/register", `{"email":"user@test.com","password":"secret123"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["email"] != "user@test.com" {
		t.Fatalf("unexpected email: %v", resp["email"])
	}

	// 重复邮箱 → 409
	w = postJSON(r, "/api/v1/auth/register", `{"email":"user@test.com","password":"secret123"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}

	// 密码过短 → 400
	w = postJSON(r, "/api/v1/auth/register", `{"email":"x@test.com","password":"short"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
```

- [ ] **Step 4: 运行测试确认失败**

Run: `go test ./internal/server/ -run TestRegister -v`
Expected: FAIL（404，路由不存在）

- [ ] **Step 5: 实现注册 handler**

`internal/handler/auth.go`：

```go
package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"image-backend/internal/config"
	"image-backend/internal/model"
)

type AuthHandler struct {
	DB  *gorm.DB
	Cfg *config.Config
}

type registerRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
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
		c.JSON(http.StatusConflict, gin.H{"code": 40901, "message": "email already registered"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": user.ID, "email": user.Email})
}
```

`internal/server/router.go` 注册路由（在 health 之后追加）：

```go
	authHandler := &handler.AuthHandler{DB: db, Cfg: cfg}
	api.POST("/auth/register", authHandler.Register)
```

import 增加 `"image-backend/internal/handler"`。

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./internal/server/ -v`
Expected: 全部 PASS（含 TestHealth）

- [ ] **Step 7: 编译检查并提交**

```bash
go build ./... && go vet ./...
git add internal go.mod go.sum
git commit -m "feat: 邮箱注册接口（bcrypt 加密、重复邮箱 409）"
```

---

### Task 4: 登录接口 + JWT 签发

**Files:**
- Create: `internal/auth/jwt.go`
- Modify: `internal/handler/auth.go`
- Modify: `internal/server/router.go`
- Test: `internal/auth/jwt_test.go`、`internal/server/auth_test.go`（追加）

- [ ] **Step 1: 安装依赖**

```bash
go get github.com/golang-jwt/jwt/v5
```

- [ ] **Step 2: 写 JWT 单元失败测试**

`internal/auth/jwt_test.go`：

```go
package auth

import "testing"

func TestGenerateAndParseToken(t *testing.T) {
	token, err := GenerateToken(42, "test-secret")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	id, err := ParseToken(token, "test-secret")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if id != 42 {
		t.Fatalf("expected 42, got %d", id)
	}
	if _, err := ParseToken(token, "wrong-secret"); err == nil {
		t.Fatal("expected error with wrong secret")
	}
	if _, err := ParseToken("garbage", "test-secret"); err == nil {
		t.Fatal("expected error with garbage token")
	}
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/auth/ -v`
Expected: FAIL（函数未定义）

- [ ] **Step 4: 实现 JWT**

`internal/auth/jwt.go`：

```go
package auth

import (
	"errors"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func GenerateToken(userID uint, secret string) (string, error) {
	claims := jwt.MapClaims{
		"sub": strconv.FormatUint(uint64(userID), 10),
		"exp": time.Now().Add(7 * 24 * time.Hour).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func ParseToken(tokenString, secret string) (uint, error) {
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return 0, errors.New("invalid token")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return 0, errors.New("invalid claims")
	}
	sub, _ := claims["sub"].(string)
	id, err := strconv.ParseUint(sub, 10, 64)
	if err != nil {
		return 0, errors.New("invalid subject")
	}
	return uint(id), nil
}
```

- [ ] **Step 5: 运行 JWT 测试确认通过**

Run: `go test ./internal/auth/ -v`
Expected: PASS

- [ ] **Step 6: 写登录接口失败测试**

`internal/server/auth_test.go` 追加：

```go
func TestLogin(t *testing.T) {
	r := setupRouter(t)
	postJSON(r, "/api/v1/auth/register", `{"email":"login@test.com","password":"secret123"}`)

	w := postJSON(r, "/api/v1/auth/login", `{"email":"login@test.com","password":"secret123"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if token, _ := resp["token"].(string); token == "" {
		t.Fatal("expected non-empty token")
	}

	// 密码错误 → 401
	w = postJSON(r, "/api/v1/auth/login", `{"email":"login@test.com","password":"wrongpass"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// 不存在的用户 → 401
	w = postJSON(r, "/api/v1/auth/login", `{"email":"nobody@test.com","password":"secret123"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
```

- [ ] **Step 7: 运行测试确认失败**

Run: `go test ./internal/server/ -run TestLogin -v`
Expected: FAIL（404，路由不存在）

- [ ] **Step 8: 实现登录 handler**

`internal/handler/auth.go` 追加：

```go
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
		c.JSON(http.StatusUnauthorized, gin.H{"code": 40101, "message": "invalid email or password"})
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
```

import 增加 `"image-backend/internal/auth"`。

`internal/server/router.go` 追加路由：

```go
	api.POST("/auth/login", authHandler.Login)
```

- [ ] **Step 9: 运行全部测试确认通过**

Run: `go test ./... -v`
Expected: 全部 PASS

- [ ] **Step 10: 编译检查并提交**

```bash
go build ./... && go vet ./...
git add internal go.mod go.sum
git commit -m "feat: 登录接口与 JWT 签发/解析"
```

---

### Task 5: 认证中间件 + /me 接口

**Files:**
- Create: `internal/middleware/auth.go`
- Create: `internal/handler/me.go`
- Modify: `internal/server/router.go`
- Test: `internal/server/me_test.go`

- [ ] **Step 1: 写失败测试**

`internal/server/me_test.go`：

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMe(t *testing.T) {
	r := setupRouter(t)
	postJSON(r, "/api/v1/auth/register", `{"email":"me@test.com","password":"secret123"}`)
	w := postJSON(r, "/api/v1/auth/login", `{"email":"me@test.com","password":"secret123"}`)
	var loginResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &loginResp); err != nil {
		t.Fatal(err)
	}
	token, _ := loginResp["token"].(string)

	// 无 token → 401
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Code)
	}

	// 带 token → 200
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	var meResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &meResp); err != nil {
		t.Fatal(err)
	}
	if meResp["email"] != "me@test.com" {
		t.Fatalf("unexpected email: %v", meResp["email"])
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/ -run TestMe -v`
Expected: FAIL（404，路由不存在）

- [ ] **Step 3: 实现中间件与 /me**

`internal/middleware/auth.go`：

```go
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
```

`internal/handler/me.go`：

```go
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/middleware"
	"image-backend/internal/model"
)

type MeHandler struct {
	DB *gorm.DB
}

func (h *MeHandler) Get(c *gin.Context) {
	userID := c.GetUint(middleware.CtxUserIDKey)
	var user model.User
	if err := h.DB.First(&user, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 40400, "message": "user not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":    user.ID,
		"email": user.Email,
		"role":  user.Role,
	})
}
```

`internal/server/router.go` 追加：

```go
	meHandler := &handler.MeHandler{DB: db}
	authed := api.Group("", middleware.Auth(cfg.JWTSecret))
	authed.GET("/me", meHandler.Get)
```

import 增加 `"image-backend/internal/middleware"`。

- [ ] **Step 4: 运行全部测试确认通过**

Run: `go test ./... -v`
Expected: 全部 PASS

- [ ] **Step 5: 编译检查并提交**

```bash
go build ./... && go vet ./...
git add internal
git commit -m "feat: JWT 认证中间件与 /me 接口"
```

---

### Task 6: 本地运行配套（docker-compose、.env.example、README、.gitignore）

**Files:**
- Create: `docker-compose.yml`
- Create: `.env.example`
- Create: `.gitignore`
- Create: `README.md`

- [ ] **Step 1: 写配套文件**

`docker-compose.yml`：

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: imageapp
      POSTGRES_PASSWORD: imageapp
      POSTGRES_DB: imageapp
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data

volumes:
  pgdata:
```

`.env.example`：

```
PORT=8080
# 留空则使用 SQLite 内存库（仅开发）
DATABASE_URL=postgres://imageapp:imageapp@localhost:5432/imageapp?sslmode=disable
JWT_SECRET=change-me-in-production
```

`.gitignore`：

```
*.exe
*.db
.env
```

`README.md`：

```markdown
# image-backend

AI 图像生成订阅平台后端（Go + Gin + GORM + PostgreSQL）。

设计文档：`docs/superpowers/specs/2026-07-27-image-platform-design.md`

## 本地运行

​```bash
docker compose up -d          # 启动 Postgres
cp .env.example .env          # 按需修改
go run ./cmd/server           # 启动服务，默认 :8080
​```

## 开发命令

​```bash
go build ./... && go vet ./...   # 提交前必跑
go test ./...                    # 运行测试
​```
```

（注意：README 中的代码围栏写成正常的三反引号，上面的 `​``` ` 仅为嵌套转义。）

- [ ] **Step 2: 验证服务能真实启动**

Run: `go run ./cmd/server &`（或新终端），然后 `curl http://localhost:8080/api/v1/health`
Expected: `{"status":"ok"}`，验证后停止进程

- [ ] **Step 3: 最终全量检查并提交**

```bash
go build ./... && go vet ./... && go test ./...
git add docker-compose.yml .env.example .gitignore README.md
git commit -m "docs: 本地运行配套（docker-compose/env 示例/README）"
```

---

## Self-Review 记录

- **Spec 覆盖**：本计划仅覆盖设计文档中 M1 范围（`users` 表、邮箱注册/登录、JWT、`/me` 骨架版）。`/me` 的双余额与订阅状态字段依赖 M2 的表，届时在 M2 计划中扩展该接口——不属于本计划缺口。OAuth 与邮箱验证明确划入 M5。
- **占位符**：无 TBD/TODO；每个代码步骤均给出完整代码。
- **类型一致性**：`NewRouter(db *gorm.DB, cfg *config.Config)`、`AuthHandler{DB, Cfg}`、`middleware.CtxUserIDKey` 在各任务间引用一致；Task 1 的 `setupRouter` 在 Task 3 Step 2 升级为带内存库版本，后续任务统一使用升级版。
