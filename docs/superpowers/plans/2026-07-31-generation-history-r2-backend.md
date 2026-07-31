# 生成历史与 R2 转存 · 后端实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 生成出的图在同一个请求内转存到 R2 拿到永久 URL，并开出 `GET /generations` 让用户翻回自己所有的历史生成。

**Architecture:** 转存做成包住任意 `Adapter` 的装饰器（`StoringAdapter`），顺着项目已有的 `Registry`/`Adapter`/`StubAdapter` 结构长，新增 provider 自动获得转存。存储抽象成最小的 `Storage` 接口，未配置时用 `NoopStorage` 让降级路径在本地开发天天被走到。历史接口用游标分页，复用已有的 `toGenerationResponse` 保证与 `POST /generations` 的响应形状不分叉。

**Tech Stack:** Go 1.25 / Gin / GORM / `aws-sdk-go-v2`（S3 兼容 API 打 Cloudflare R2）。

**前置：** 设计文档 `docs/superpowers/specs/2026-07-30-generation-history-r2-design.md`（提交 `75a910d`）。HEAD 为 `75a910d`，`go test ./...` 全绿。

**配套：** 前端部分见 `docs/superpowers/plans/2026-07-31-generation-history-r2-frontend.md`。**本计划 6 个任务全部完成且 `go test ./...` 全绿之后**才开始前端——否则前端会对着一个还在变的契约写代码。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/model/generation.go`（改） | 加 `Stored` 列，`UserID`+`CreatedAt` 换复合索引 |
| `internal/config/config.go`（改） | 五个 `R2_*` 配置项、`StorageEnabled`、`ValidateStorage` |
| `internal/storage/storage.go`（新） | `Storage` 接口、`ErrNotConfigured` |
| `internal/storage/noop.go`（新） | `NoopStorage`——未配置时顶替，固定返回错误 |
| `internal/storage/r2.go`（新） | `R2Storage`——aws-sdk-go-v2 打 R2 |
| `internal/storage/r2_test.go`（新） | 指向 `httptest.Server` 断言请求与返回 URL |
| `internal/generation/adapter.go`（改） | `GenerateResult` 加 `Stored`；`GenerateRequest` 加 `GenerationID` |
| `internal/generation/storing.go`（新） | 转存装饰器 |
| `internal/generation/storing_test.go`（新） | 假 inner + 假 storage，覆盖降级契约 |
| `internal/handler/generations.go`（改） | 传 `GenerationID`、落 `gen.Stored`、响应加 `stored` |
| `internal/handler/generations_list.go`（新） | 游标编解码 + `List` |
| `internal/server/router.go`（改） | `BuildAdapters` 包装饰器、注册 `GET /generations` |
| `internal/server/generations_list_test.go`（新） | 历史接口集成测试 |
| `cmd/server/main.go`（改） | 调 `ValidateStorage` |
| `.env.example` / `.env.prod.example`（改） | 五个变量 |

**为什么 `List` 单独开一个文件而不是塞进 `generations.go`：** `generations.go` 已经 200+ 行且 `Create` 的编排注释很密（顺序不能换的那段）。游标编解码是一组独立的纯函数，和 `Create` 没有共享状态，放一起只会让两件事都更难读。

---

## Task 1：`Stored` 列与复合索引

**Files:**
- Modify: `internal/model/generation.go`
- Test: `internal/database/database_test.go`

- [ ] **Step 1：先写失败的测试**

追加到 `internal/database/database_test.go` 末尾：

```go
func TestGenerationHasStoredColumnAndCompositeIndex(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	m := db.Migrator()

	if !m.HasColumn(&model.Generation{}, "stored") {
		t.Error("generations 缺 stored 列——历史页无法区分永久链接与降级后的临时链接")
	}

	// 历史查询是 WHERE user_id = ? ORDER BY created_at DESC。单列索引只能过滤，
	// 排序要落到额外的 sort。现在几十行看不出差别，攒到几千行时这是"翻页 200ms"
	// 与"翻页 3 秒"的差别，而那时候加索引要锁表。
	if !m.HasIndex(&model.Generation{}, "idx_gen_user_created") {
		t.Error("generations 缺 (user_id, created_at) 复合索引")
	}
}

func TestGenerationStoredDefaultsFalse(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// 不显式设 Stored，落库后读回来必须是 false——默认值写错成 true 的后果是
	// 前端对一批一小时后会失效的链接显示"永久有效"。
	g := model.Generation{
		ID: "gen-default-1", UserID: 1, Model: "flux-2-max", Prompt: "p",
		AspectRatio: "1:1", Width: 1024, Height: 1024,
		Status: model.GenStatusSucceeded,
	}
	if err := db.Create(&g).Error; err != nil {
		t.Fatalf("落库: %v", err)
	}
	var got model.Generation
	if err := db.Where("id = ?", "gen-default-1").First(&got).Error; err != nil {
		t.Fatalf("读回: %v", err)
	}
	if got.Stored {
		t.Error("Stored 默认值应当是 false")
	}
}
```

- [ ] **Step 2：跑测试确认失败**

Run: `go test ./internal/database/ -run 'TestGenerationHas|TestGenerationStored' -v`
Expected: FAIL — `generations 缺 stored 列` 与 `generations 缺 (user_id, created_at) 复合索引`（`HasColumn` 对不存在的列返回 false，不会 panic）。

- [ ] **Step 3：实现**

改 `internal/model/generation.go` 的 `Generation` 结构体。把 `UserID`、`Status`、`ImageURL` 之后到 `CreatedAt` 的部分改成：

```go
type Generation struct {
	ID string `gorm:"primaryKey;size:64"`
	// UserID 与 CreatedAt 组成复合索引 idx_gen_user_created，专门服务历史查询
	// （WHERE user_id = ? ORDER BY created_at DESC）。只留单列索引的话排序要落到
	// 额外的 sort。
	UserID      uint   `gorm:"index:idx_gen_user_created,priority:1;not null"`
	Model       string `gorm:"size:64;not null"`
	Prompt      string `gorm:"type:text;not null"`
	AspectRatio string `gorm:"size:16;not null"`
	Width       int    `gorm:"not null"`
	Height      int    `gorm:"not null"`
	// Status 见上面三个常量。索引是给启动扫描用的——它要按 status 找卡住的行。
	Status       string `gorm:"index;size:16;not null"`
	ImageURL     string `gorm:"type:text"`
	CreditsSpent int    `gorm:"not null;default:0"`
	// Stored 图片是否已转存到我们自己的存储。
	//
	// false 有两种来源：R2 未配置（本地开发），或转存失败后降级。两种情况下
	// ImageURL 都是上游的临时链接，约一小时后失效——历史接口把这一列透出去，
	// 前端才能诚实地提示"链接可能已失效"，而不是让用户对着坏图猜。
	//
	// **不配套加 storage_key 列**：key 从 ID 确定性推导（g/<id>.<ext>），再存
	// 一份就是两份可能不一致的真相。
	Stored bool `gorm:"not null;default:false"`
	// UpstreamID 是上游返回的任务 id，出问题时凭它去上游对账。
	UpstreamID   string `gorm:"size:128"`
	UpstreamCost int    `gorm:"not null;default:0"`
	Error        string `gorm:"type:text"`
	IsPublic     bool   `gorm:"not null;default:false"`
	DurationMs   int64  `gorm:"not null;default:0"`
	CreatedAt    time.Time `gorm:"index:idx_gen_user_created,priority:2"`
	UpdatedAt    time.Time
}
```

- [ ] **Step 4：跑测试确认通过**

Run: `go test ./internal/database/ -run 'TestGenerationHas|TestGenerationStored' -v`
Expected: PASS ×2

- [ ] **Step 5：确认存量测试没被打破**

Run: `go test ./...`
Expected: 全部 ok。`AutoMigrate` 会自动加列和索引，不需要写迁移脚本（存量数据是测试数据，见设计文档 §3.3）。

- [ ] **Step 6：提交**

```bash
git add internal/model/generation.go internal/database/database_test.go
git commit -m "feat: generations 加 stored 列与 (user_id, created_at) 复合索引"
```

**实施补记（本计划原先漏掉的一步）：** 把 `UserID` 从 `gorm:"index"` 改成命名复合索引之后，旧的单列索引 `idx_generations_user_id` **不会被 `AutoMigrate` 删掉**——GORM 对索引只加不删。既有库会一直留着一个没有任何查询用、但每次写 `generations` 都要维护的死索引，而测试套件看不见它（`Open("")` 每次都建全新的空库）。

所以 `internal/database/database.go` 在 `AutoMigrate` **之后**（这样表一定存在，`HasIndex` 在全新库上不会报错）加一段清理，并配一个真正驱动该路径的测试（用临时文件库：`Open(path)` → 手工建旧索引 → 关闭 → 重新 `Open(path)` → 断言已消失，同时断言复合索引还在，防止清理过界）。旧索引名必须**实测确认**而不是猜——名字写错会让这段变成静默的空操作，那比不写更糟，因为它看起来已经处理了。

---

## Task 2：五个 R2 配置项与启动期误配拦截

**Files:**
- Modify: `internal/config/config.go`, `cmd/server/main.go`, `.env.example`, `.env.prod.example`
- Test: `internal/config/config_test.go`

- [ ] **Step 1：先写失败的测试**

追加到 `internal/config/config_test.go` 末尾：

```go
func TestStorageEnabledRequiresAllFive(t *testing.T) {
	full := func() *Config {
		return &Config{
			R2Endpoint:        "https://acct.r2.cloudflarestorage.com",
			R2AccessKeyID:     "ak",
			R2SecretAccessKey: "sk",
			R2Bucket:          "images",
			R2PublicBaseURL:   "https://img.example.com",
		}
	}
	if !full().StorageEnabled() {
		t.Fatal("五项齐全应当算已配置")
	}

	// 少任何一项都必须退化成未配置。半套配置比没配置更危险：它会让上传走到
	// 一半才失败，而失败点在生产才第一次出现。
	blanks := map[string]func(*Config){
		"R2_ENDPOINT":          func(c *Config) { c.R2Endpoint = "" },
		"R2_ACCESS_KEY_ID":     func(c *Config) { c.R2AccessKeyID = "" },
		"R2_SECRET_ACCESS_KEY": func(c *Config) { c.R2SecretAccessKey = "" },
		"R2_BUCKET":            func(c *Config) { c.R2Bucket = "" },
		"R2_PUBLIC_BASE_URL":   func(c *Config) { c.R2PublicBaseURL = "" },
	}
	for name, blank := range blanks {
		c := full()
		blank(c)
		if c.StorageEnabled() {
			t.Errorf("缺 %s 时不该算已配置", name)
		}
	}
}

func TestValidateStorageRejectsCredentialsWithoutPublicURL(t *testing.T) {
	// 这个组合不报错，只静默产出坏数据：少了公开域名就只能拿 S3 endpoint 拼 URL，
	// 而那个地址不允许匿名读——上传全部成功、stored=true、每张图在浏览器里 401。
	c := &Config{
		R2Endpoint:        "https://acct.r2.cloudflarestorage.com",
		R2AccessKeyID:     "ak",
		R2SecretAccessKey: "sk",
		R2Bucket:          "images",
	}
	err := c.ValidateStorage()
	if err == nil {
		t.Fatal("配了凭证但没配 R2_PUBLIC_BASE_URL 必须拒绝启动")
	}
	if !strings.Contains(err.Error(), "R2_PUBLIC_BASE_URL") {
		t.Errorf("错误信息要点名缺的是哪个变量，得到：%v", err)
	}
}

func TestValidateStorageAllowsFullyUnconfigured(t *testing.T) {
	// 完全没配 R2 是合法的本地开发状态，不能拦。
	if err := (&Config{}).ValidateStorage(); err != nil {
		t.Errorf("完全未配置不该报错：%v", err)
	}
}

func TestValidateStorageAllowsFullConfig(t *testing.T) {
	c := &Config{
		R2Endpoint:        "https://acct.r2.cloudflarestorage.com",
		R2AccessKeyID:     "ak",
		R2SecretAccessKey: "sk",
		R2Bucket:          "images",
		R2PublicBaseURL:   "https://img.example.com",
	}
	if err := c.ValidateStorage(); err != nil {
		t.Errorf("五项齐全不该报错：%v", err)
	}
}
```

若 `config_test.go` 尚未 import `strings`，加上。

- [ ] **Step 2：跑测试确认失败**

Run: `go test ./internal/config/ -run TestStorage -run TestValidateStorage -v`
Expected: 编译失败——`Config` 没有 `R2Endpoint` 等字段，也没有 `StorageEnabled` / `ValidateStorage` 方法。

- [ ] **Step 3：实现**

`internal/config/config.go`。在 `AppBaseURL` 字段后追加：

```go
	// R2Endpoint Cloudflare R2 的 S3 兼容 endpoint，形如
	// https://<account_id>.r2.cloudflarestorage.com
	//
	// **存完整 endpoint 而不是 account id**：这样测试能把它指向
	// httptest.Server 或本地 minio，不必为了跑测试去连真的 R2。
	R2Endpoint        string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string
	// R2PublicBaseURL 绑在桶上的自定义域，形如 https://img.example.com。
	// 最终写进 generations.image_url 的 URL 由它拼出来。
	//
	// **不能用 R2Endpoint 代替**：S3 endpoint 不允许匿名读，拿它拼出来的 URL
	// 每一个都会 401。ValidateStorage 会拦这个误配。
	R2PublicBaseURL string
```

`Load()` 里在 `AppBaseURL` 之后追加：

```go
		R2Endpoint:        getEnv("R2_ENDPOINT", ""),
		R2AccessKeyID:     getEnv("R2_ACCESS_KEY_ID", ""),
		R2SecretAccessKey: getEnv("R2_SECRET_ACCESS_KEY", ""),
		R2Bucket:          getEnv("R2_BUCKET", ""),
		R2PublicBaseURL:   getEnv("R2_PUBLIC_BASE_URL", ""),
```

文件末尾（`getEnv` 之前）追加两个方法：

```go
// StorageEnabled 图片转存是否可用。
//
// **五项必须齐全。** 半套配置比没配置更危险：它会让转存走到一半才失败，而那个
// 失败点只在生产才第一次出现。缺任何一项都退化成 NoopStorage——与 FluxAPIKey
// 为空退化成 stub、StripeSecretKey 为空禁用计费是同一个约定。
func (c *Config) StorageEnabled() bool {
	return c.R2Endpoint != "" && c.R2AccessKeyID != "" && c.R2SecretAccessKey != "" &&
		c.R2Bucket != "" && c.R2PublicBaseURL != ""
}

// ValidateStorage 启动时的误配拦截。
//
// 只拦一种组合：有凭证、没公开域名。这个组合**不报错，只静默产出坏数据**——
// 上传全部成功、库里 stored=true、而每张图在浏览器里 401，因为 URL 是拿不允许
// 匿名读的 S3 endpoint 拼出来的。等发现时已经攒了一批 URL 全错的记录，而它们
// 指向的对象是好的，得写脚本回头改。
//
// 完全未配置是合法的本地开发状态，不拦。
func (c *Config) ValidateStorage() error {
	hasCreds := c.R2Endpoint != "" || c.R2AccessKeyID != "" ||
		c.R2SecretAccessKey != "" || c.R2Bucket != ""
	if hasCreds && c.R2PublicBaseURL == "" {
		return fmt.Errorf(
			"检测到 R2 凭证但 R2_PUBLIC_BASE_URL 为空——上传会成功但每张图的 URL 都不可匿名访问；" +
				"请填绑在桶上的自定义域，如 https://img.example.com")
	}
	return nil
}
```

- [ ] **Step 4：跑测试确认通过**

Run: `go test ./internal/config/ -v`
Expected: 新增 4 个测试全 PASS，既有测试不变。

- [ ] **Step 5：在 `cmd/server/main.go` 里调用校验**

找到调用 `cfg.ValidateStripe()` 的那几行，在其后紧跟同样形状的一段：

```go
	if err := cfg.ValidateStorage(); err != nil {
		log.Fatalf("存储配置错误：%v", err)
	}
```

- [ ] **Step 6：更新两个 env 示例文件**

`.env.example` 与 `.env.prod.example` 各追加：

```
# 图片转存（Cloudflare R2）。五项必须齐全，缺任何一项都退化成不转存——
# 那时 image_url 存的是上游临时链接，约一小时后失效。
# R2_PUBLIC_BASE_URL 必须是绑在桶上的自定义域，不能填 R2_ENDPOINT：
# S3 endpoint 不允许匿名读，拿它拼出来的图片 URL 每一个都会 401。
R2_ENDPOINT=
R2_ACCESS_KEY_ID=
R2_SECRET_ACCESS_KEY=
R2_BUCKET=
R2_PUBLIC_BASE_URL=
```

- [ ] **Step 7：跑全量测试并提交**

Run: `go test ./...`
Expected: 全部 ok。

```bash
git add internal/config/config.go internal/config/config_test.go cmd/server/main.go .env.example .env.prod.example
git commit -m "feat: R2 五项配置与启动期误配拦截（有凭证无公开域名拒绝启动）"
```

**实施补记（本计划原先漏掉的两处拦截）：** 上面的 `ValidateStorage` 只拦"有凭证、没公开域名"，但 `R2PublicBaseURL` 的字段注释却写着"`ValidateStorage` 会拦这个误配"来说明不能拿 S3 endpoint 当公开域名——**注释承诺了代码没做的事**，而那比不写注释更糟：它会让下一个人以为已经拦住了。实际还漏两种同样只产出坏数据、不报错的误配：

1. **公开域名填成 S3 endpoint**（两个变量长得像，是最容易犯的错）：上传全成功、`stored=true`、每张图 401。
2. **公开域名少了 scheme**（`img.example.com`）：拼出来的地址被浏览器当成**相对路径**，每个页面上的图都指向各自不同的错地址，比 404 更难认出来。

所以 `ValidateStorage` 补上：`url.Parse` → 要求 scheme 是 `http`/`https` → 拒绝 hostname 以 `.r2.cloudflarestorage.com` 结尾。

**用后缀匹配，不要用 `R2PublicBaseURL == R2Endpoint` 字符串相等**：带末尾斜杠、带路径、换个 account id 的粘贴都坏得一模一样，而字符串相等一个都拦不住。测试里必须覆盖这三种变体，否则实现退化成字符串相等时没人会发现。同时要有反向用例守住**不能误伤**：`*.r2.dev` 是 R2 正经的公开域名，`https://img.example.com/`（带末尾斜杠）由下游 `NewR2Storage` 自己 trim，两者都必须放行。

---

## Task 3：`internal/storage` 包

**Files:**
- Create: `internal/storage/storage.go`, `internal/storage/noop.go`, `internal/storage/r2.go`, `internal/storage/r2_test.go`
- Modify: `go.mod` / `go.sum`（`go get`）

- [ ] **Step 1：装依赖**

```bash
go get github.com/aws/aws-sdk-go-v2/service/s3@latest
go get github.com/aws/aws-sdk-go-v2/credentials@latest
go get github.com/aws/aws-sdk-go-v2@latest
```

选 `aws-sdk-go-v2` 而不是手写 SigV4：签错的表现是 403 且错误信息不指向出错的那一步，调它能耗掉一整天，换来的只是几 MB 二进制体积——对容器里的服务毫无意义。也不选 `minio-go`：R2 的官方兼容性声明与可查资料都是针对 AWS SDK 的。

- [ ] **Step 2：先写失败的测试**

Create `internal/storage/r2_test.go`：

```go
package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestR2StoragePutSendsObjectAndReturnsPublicURL(t *testing.T) {
	var gotMethod, gotPath, gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewR2Storage(srv.URL, "ak", "sk", "images", "https://img.example.com")
	url, err := s.Put(context.Background(), "g/abc.png", "image/png", []byte("PNGDATA"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// 返回的 URL **必须**由公开域拼出，不能是 S3 endpoint——后者不允许匿名读，
	// 用它拼出来的每一个链接都会 401。这正是 config.ValidateStorage 要防的错。
	if url != "https://img.example.com/g/abc.png" {
		t.Errorf("返回 URL: got %q", url)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("方法: got %q, want PUT", gotMethod)
	}
	// UsePathStyle 之下路径是 /<bucket>/<key>。
	if gotPath != "/images/g/abc.png" {
		t.Errorf("路径: got %q, want /images/g/abc.png", gotPath)
	}
	if gotType != "image/png" {
		t.Errorf("Content-Type: got %q", gotType)
	}
	if string(gotBody) != "PNGDATA" {
		t.Errorf("body: got %q", gotBody)
	}
}

func TestR2StoragePutPropagatesUpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	s := NewR2Storage(srv.URL, "ak", "sk", "images", "https://img.example.com")
	if _, err := s.Put(context.Background(), "g/abc.png", "image/png", []byte("x")); err == nil {
		t.Fatal("上传失败必须返回错误——吞掉它会让库里存下一个指向不存在对象的永久 URL")
	}
}

func TestR2StorageTrimsTrailingSlashOnPublicBase(t *testing.T) {
	// 运维在 R2_PUBLIC_BASE_URL 末尾多打一个斜杠是必然会发生的事，
	// 而后果是每个 URL 里出现 // ——有些 CDN 会 404。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewR2Storage(srv.URL, "ak", "sk", "images", "https://img.example.com/")
	url, err := s.Put(context.Background(), "g/abc.png", "image/png", []byte("x"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if strings.Contains(strings.TrimPrefix(url, "https://"), "//") {
		t.Errorf("URL 里出现重复斜杠: %q", url)
	}
}

func TestNoopStorageAlwaysReturnsNotConfigured(t *testing.T) {
	// Noop **返回错误而不是返回原 URL**：这样"未配置"与"配置了但失败"在装饰器里
	// 走同一条代码路径。否则降级分支只在生产才会被走到，而那是最不该第一次运行
	// 的地方。
	_, err := NoopStorage{}.Put(context.Background(), "g/a.png", "image/png", []byte("x"))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}
```

- [ ] **Step 3：跑测试确认失败**

Run: `go test ./internal/storage/ -v`
Expected: 编译失败——`NewR2Storage`、`NoopStorage`、`ErrNotConfigured` 都不存在。

- [ ] **Step 4：实现三个文件**

Create `internal/storage/storage.go`：

```go
// Package storage 把生成好的图片转存到我们自己控制的对象存储。
//
// 存在理由：上游返回的图片 URL 指向它自己的 CDN，约一小时后失效。不转存的话
// 历史记录里全是死链，而用户为那些图付过费。
package storage

import (
	"context"
	"errors"
)

// ErrNotConfigured NoopStorage 的固定返回，调用方据此走降级路径。
var ErrNotConfigured = errors.New("storage is not configured")

type Storage interface {
	// Put 上传并返回可公开访问的永久 URL。
	//
	// body 收 []byte 而不是 io.Reader：aws-sdk-go-v2 的 PutObject 需要可重放的
	// body 才能签名和重试，给它一个不可 seek 的 reader 会让 SDK 自己先缓冲一遍
	// ——同一份数据在内存里两份。反正上游就是一张图、调用方本来就要限大小，
	// 直接收字节更诚实。
	Put(ctx context.Context, key, contentType string, body []byte) (string, error)
}
```

Create `internal/storage/noop.go`：

```go
package storage

import "context"

// NoopStorage 在没有配置 R2 时顶替 R2Storage。
//
// **它返回错误而不是返回传入的原 URL。** 这样"未配置"与"配置了但上传失败"在
// 调用方那里是同一条代码路径——降级分支于是在本地开发天天被走到，而不是只在
// 生产才第一次运行。
type NoopStorage struct{}

func (NoopStorage) Put(context.Context, string, string, []byte) (string, error) {
	return "", ErrNotConfigured
}
```

Create `internal/storage/r2.go`：

```go
package storage

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type R2Storage struct {
	client     *s3.Client
	bucket     string
	publicBase string
}

// NewR2Storage 构造打 Cloudflare R2 的 S3 客户端。
//
// endpoint 传完整地址（而非 account id）是为了让测试能指向 httptest.Server。
func NewR2Storage(endpoint, accessKeyID, secretAccessKey, bucket, publicBaseURL string) *R2Storage {
	client := s3.New(s3.Options{
		// R2 要求 region 固定为 "auto"。
		Region:       "auto",
		BaseEndpoint: aws.String(endpoint),
		// R2 用 path-style（/<bucket>/<key>）。virtual-host style 需要按桶名解析
		// 子域，R2 的 S3 endpoint 不提供。
		UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			accessKeyID, secretAccessKey, ""),
		// 只在上游明确要求时才算 checksum。
		//
		// **这不是"checksum 没用"**：默认的 WhenSupported 会附上
		// x-amz-checksum-crc32，R2 收到后自己重算、不一致就拒绝上传——那是 TLS
		// 给不了的、落盘那一刻的完整性保证。选 WhenRequired 是拿它换两件事：
		// 一是 S3 兼容实现对这些头的处理并不一致，二是目前还没有真 R2 凭证，
		// 改了也没法验证。拿到真凭证跑通人工验证后应当重新评估切回 WhenSupported。
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	})
	return &R2Storage{
		client: client,
		bucket: bucket,
		// 去掉末尾斜杠：运维多打一个斜杠是必然会发生的事，而后果是每个 URL 里
		// 出现 // ，有些 CDN 会对此 404。
		publicBase: strings.TrimSuffix(publicBaseURL, "/"),
	}
}

func (s *R2Storage) Put(ctx context.Context, key, contentType string, body []byte) (string, error) {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		return "", fmt.Errorf("上传对象 %s: %w", key, err)
	}
	return s.publicBase + "/" + key, nil
}
```

- [ ] **Step 5：跑测试确认通过**

Run: `go test ./internal/storage/ -v`
Expected: 4 个测试全 PASS。

若 `TestR2StoragePutSendsObjectAndReturnsPublicURL` 因为 SDK 附加了额外的 checksum 头而失败（表现为 body 不是纯 `PNGDATA`，而是带 chunk 编码的内容），说明 `RequestChecksumCalculationWhenRequired` 没生效——检查装的 SDK 版本是否支持该选项，必要时改用 `s3.Options.APIOptions` 关闭 `aws-chunked` 编码。**不要**为了让测试过而放宽断言：body 被改写意味着真 R2 上存进去的字节也不是原图。

- [ ] **Step 6：提交**

```bash
git add internal/storage go.mod go.sum
git commit -m "feat: storage 包——R2 上传与未配置时的 NoopStorage"
```

**实施补记（三处）：**

1. **`go get` 之后要 `go mod tidy`。** Step 1 在任何文件 import 之前先装依赖，于是三个直接依赖的 AWS 模块会被标成 `// indirect`——`go.mod` 从此对"这个服务到底依赖什么"给出错误答案，而下一个人跑 tidy 会得到一个混在自己改动里的意外 diff。
2. **`Put` 要把 `key` 的前导斜杠 trim 掉。** `publicBase + "/" + key` 在 key 形如 `/g/abc.png` 时拼出 `//`，永久写进库，而有些 CDN 对 `//` 404——正是末尾斜杠那条已经在防的同一类错。Task 4 的装饰器用字符串拼 key，这条路径是可达的。注意它只归一化前导斜杠，**内部的双斜杠（`g//abc.png`）仍会原样穿过**，所以调用方不能依赖它兜底。
3. **`r2_test.go` 要写明自己测不到什么。** 四个测试都打 `httptest.Server`，它接受任何 `Authorization` 头、只回裸状态码，所以**签名、R2 的 XML 错误体、ETag 全都没有被覆盖**——签名写错的话这四个测试照样全绿。这个缺口本身可以接受（完成检查里有真凭证的人工验证），但必须写在测试文件里：等这份计划过期之后，"storage 的测试过了"会被读成"转存链路是通的"。

---

## Task 4：转存装饰器 `StoringAdapter`

**Files:**
- Modify: `internal/generation/adapter.go`
- Create: `internal/generation/storing.go`, `internal/generation/storing_test.go`

- [ ] **Step 1：先写失败的测试**

Create `internal/generation/storing_test.go`：

```go
package generation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"image-backend/internal/storage"
)

// fakeInner 是可编排的 inner adapter。
type fakeInner struct {
	url     string
	err     error
	calls   int
	lastReq GenerateRequest
}

func (f *fakeInner) Generate(_ context.Context, req GenerateRequest) (GenerateResult, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return GenerateResult{}, f.err
	}
	return GenerateResult{ImageURL: f.url, UpstreamID: "up-1"}, nil
}

// fakeStore 记录调用并可编排失败。
type fakeStore struct {
	calls    int
	lastKey  string
	lastType string
	lastBody []byte
	err      error
}

func (f *fakeStore) Put(_ context.Context, key, contentType string, body []byte) (string, error) {
	f.calls++
	f.lastKey, f.lastType, f.lastBody = key, contentType, body
	if f.err != nil {
		return "", f.err
	}
	return "https://img.example.com/" + key, nil
}

// pngBytes 是一个最小的合法 PNG 头，足够让 http.DetectContentType 认出 image/png。
var pngBytes = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 32))

func serveBytes(t *testing.T, ct string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStoringAdapterReplacesURLOnSuccess(t *testing.T) {
	srv := serveBytes(t, "image/png", pngBytes)
	inner := &fakeInner{url: srv.URL + "/upstream.png"}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-1"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.ImageURL != "https://img.example.com/g/gen-1.png" {
		t.Errorf("ImageURL: got %q", res.ImageURL)
	}
	if !res.Stored {
		t.Error("转存成功必须置 Stored=true，否则前端会对永久链接显示'可能已失效'")
	}
	if store.lastKey != "g/gen-1.png" {
		t.Errorf("key: got %q, want g/gen-1.png", store.lastKey)
	}
	if store.lastType != "image/png" {
		t.Errorf("contentType: got %q", store.lastType)
	}
	if string(store.lastBody) != string(pngBytes) {
		t.Error("上传的字节与下载的不一致")
	}
	// UpstreamID 等既有字段不能被装饰器吃掉。
	if res.UpstreamID != "up-1" {
		t.Errorf("UpstreamID 被丢了: %q", res.UpstreamID)
	}
}

func TestStoringAdapterDegradesWhenStoreFails(t *testing.T) {
	// 这是整个装饰器最重要的一条契约，也是最容易被后人"修"掉的一条。
	// 图已经出了、钱已经花在上游了。因为我们自己的存储抖动就判失败退款，等于把
	// 一次成功且已付费的上游调用白扔，用户还得重排队等 21 秒。
	srv := serveBytes(t, "image/png", pngBytes)
	upstreamURL := srv.URL + "/upstream.png"
	inner := &fakeInner{url: upstreamURL}
	store := &fakeStore{err: errors.New("R2 挂了")}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-2"})
	if err != nil {
		t.Fatalf("转存失败**不能**让生成失败，得到错误: %v", err)
	}
	if res.ImageURL != upstreamURL {
		t.Errorf("降级时要保留上游 URL: got %q", res.ImageURL)
	}
	if res.Stored {
		t.Error("降级时 Stored 必须是 false")
	}
}

func TestStoringAdapterDegradesWhenStorageNotConfigured(t *testing.T) {
	srv := serveBytes(t, "image/png", pngBytes)
	inner := &fakeInner{url: srv.URL + "/upstream.png"}
	a := NewStoringAdapter(inner, storage.NoopStorage{})

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-3"})
	if err != nil {
		t.Fatalf("未配置存储不能让生成失败: %v", err)
	}
	if res.Stored {
		t.Error("未配置时 Stored 必须是 false")
	}
}

func TestStoringAdapterSkipsStoreWhenInnerFails(t *testing.T) {
	inner := &fakeInner{err: errors.New("上游拒绝")}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	if _, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-4"}); err == nil {
		t.Fatal("inner 的错误必须原样透出——吞掉它会让用户被扣费却拿不到图")
	}
	if store.calls != 0 {
		t.Errorf("inner 失败时不该调存储，调了 %d 次", store.calls)
	}
}

func TestStoringAdapterSkipsNonHTTPURL(t *testing.T) {
	// StubAdapter 返回的是前端 public/ 下的相对路径，不是可下载的 URL。
	// **必须显式跳过，而不是让它走失败降级**：否则本地开发和 e2e 每次生成都会打
	// 一条转存告警，而那条告警正是生产上唯一提示"这张图一小时后会失效"的信号——
	// 让它变成日常噪音等于把它关掉。
	inner := &fakeInner{url: StubImageURL}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-5"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.ImageURL != StubImageURL {
		t.Errorf("相对路径要原样保留: got %q", res.ImageURL)
	}
	if res.Stored {
		t.Error("没转存就不能说 Stored")
	}
	if store.calls != 0 {
		t.Errorf("相对路径不该调存储，调了 %d 次", store.calls)
	}
}

func TestStoringAdapterRejectsNonImageContent(t *testing.T) {
	// 这个字节流要挂到**我们自己的域名**下。上游若返回 HTML，我们就在自己的
	// origin 上托管了一个别人可控的 HTML 文件——那是 XSS。
	// 注意 handler 谎报 Content-Type 为 image/png，嗅探必须不信它。
	srv := serveBytes(t, "image/png", []byte("<html><script>alert(1)</script></html>"))
	inner := &fakeInner{url: srv.URL + "/evil"}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-6"})
	if err != nil {
		t.Fatalf("拒绝非图片内容要走降级而不是报错: %v", err)
	}
	if store.calls != 0 {
		t.Errorf("嗅探出非图片就不能上传，调了 %d 次", store.calls)
	}
	if res.Stored {
		t.Error("没上传就不能说 Stored")
	}
}

func TestStoringAdapterRejectsOversizedImage(t *testing.T) {
	// 无上限地下载进内存是内存耗尽向量：并发几十个请求 + 上游返回一个巨大的
	// 响应，就能把服务打死。
	big := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, maxImageBytes+1)...)
	srv := serveBytes(t, "image/png", big)
	inner := &fakeInner{url: srv.URL + "/big.png"}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-7"})
	if err != nil {
		t.Fatalf("超限要走降级而不是报错: %v", err)
	}
	if store.calls != 0 {
		t.Errorf("超限就不能上传，调了 %d 次", store.calls)
	}
	if res.Stored {
		t.Error("没上传就不能说 Stored")
	}
}

func TestStoringAdapterDegradesOnDownloadHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	inner := &fakeInner{url: srv.URL + "/gone.png"}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-8"})
	if err != nil {
		t.Fatalf("下载失败要走降级: %v", err)
	}
	if store.calls != 0 || res.Stored {
		t.Error("下载失败就不该上传、不该置 Stored")
	}
}

func TestStoringAdapterPassesRequestThrough(t *testing.T) {
	// 装饰器不能改写请求——把画幅或上游模型名吃掉的话，用户按 pro 付费会拿到
	// 别的模型的结果，而没有任何地方报错。
	srv := serveBytes(t, "image/png", pngBytes)
	inner := &fakeInner{url: srv.URL + "/a.png"}
	a := NewStoringAdapter(inner, &fakeStore{})

	req := GenerateRequest{
		Prompt: "cat", Width: 1344, Height: 768,
		UpstreamModel: "flux-pro-1.1", GenerationID: "gen-9",
	}
	if _, err := a.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if inner.lastReq != req {
		t.Errorf("请求被改写了: got %+v, want %+v", inner.lastReq, req)
	}
}
```

- [ ] **Step 2：跑测试确认失败**

Run: `go test ./internal/generation/ -run TestStoringAdapter -v`
Expected: 编译失败——`NewStoringAdapter`、`maxImageBytes`、`GenerateRequest.GenerationID`、`GenerateResult.Stored` 都不存在。

- [ ] **Step 3：改 `adapter.go` 加两个字段**

`internal/generation/adapter.go`，在 `GenerateRequest` 的 `UpstreamModel` 字段后追加：

```go
	// GenerationID 是我们自己的 generations 行 id，用来拼转存后的对象 key
	// （g/<id>.<ext>）。
	//
	// 这不是 §3 拒绝的"provider 专属字段"——它是**我们的**领域标识，而且正因为
	// key 由它确定性推导，generations 表才不需要额外存一列 storage_key（两份
	// 可能不一致的真相）。
	GenerationID string
```

把 `GenerateResult` 改成：

```go
type GenerateResult struct {
	ImageURL string
	// UpstreamID 上游任务 id，落库便于事后对账。
	UpstreamID string
	// UpstreamCost 上游报告的成本，与我们扣的次数是两回事，落库便于核算毛利。
	UpstreamCost int
	// Stored ImageURL 是否已经指向我们自己的存储。
	//
	// false 表示它还是上游的临时链接，约一小时后失效。落到
	// generations.stored，历史接口透给前端做提示。
	Stored bool
}
```

- [ ] **Step 4：实现装饰器**

Create `internal/generation/storing.go`：

```go
package generation

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"image-backend/internal/storage"
)

const (
	// transferTimeout 转存自己的超时。
	//
	// **不继承上游那 5 分钟**：上游花了 4 分 50 秒的话，共用 ctx 只剩 10 秒给
	// 转存、必然降级——而这时候本来是可以再等一会儿的。
	transferTimeout = 60 * time.Second
	// maxImageBytes 下载上限。无上限地读进内存是内存耗尽向量：并发几十个请求
	// 加上游返回一个巨大或永不结束的响应，就能把服务打死。
	maxImageBytes = 20 << 20 // 20 MiB
)

// allowedImageTypes 白名单，值是落地用的扩展名。
//
// 白名单而非黑名单：这个字节流要挂到我们自己的域名下，能想到要拦什么的人总会
// 漏掉一种，而漏掉的那种如果是 HTML，就是我们自己 origin 上的 XSS。
var allowedImageTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
}

// StoringAdapter 包住任意 Adapter，把上游返回的临时图片 URL 转存到我们自己的
// 存储，换成永久 URL。
//
// 做成装饰器而不是写在 handler 里，是为了顺着项目已有的结构长：Registry +
// Adapter + StubAdapter 已经建立了"provider 行为可替换、可注入假实现"的模式。
// 新增 provider 会自动获得转存，不依赖谁记得加代码；而塞进 handler 则会让
// "转存"只能靠跑完整生成流程来测，而那正是 stub adapter 存在要避免的东西。
type StoringAdapter struct {
	inner  Adapter
	store  storage.Storage
	client *http.Client
}

func NewStoringAdapter(inner Adapter, store storage.Storage) *StoringAdapter {
	return &StoringAdapter{
		inner:  inner,
		store:  store,
		client: &http.Client{Timeout: transferTimeout},
	}
}

// Generate 先让 inner 生成，再尽力转存。
//
// **转存的任何失败都降级，不返回错误。** 图已经出了、钱已经花在上游了。因为我们
// 自己的存储抖动就判失败并退款，等于把一次成功且已付费的上游调用白扔，用户还得
// 重新排队等 21 秒。降级的最坏后果只是这一张图一小时后失效——比彻底没有强。
func (a *StoringAdapter) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	res, err := a.inner.Generate(ctx, req)
	if err != nil {
		return res, err
	}
	if res.ImageURL == "" {
		return res, nil
	}
	// StubAdapter 返回的是前端 public/ 下的相对路径，不是可下载的 URL。
	//
	// **显式跳过，而不是让它走失败降级**：否则本地开发与 e2e 每次生成都会打一条
	// 转存告警，而那条告警正是生产上唯一提示"这张图一小时后会失效"的信号——让它
	// 变成日常噪音，等于把它关掉。
	if !strings.HasPrefix(res.ImageURL, "http://") && !strings.HasPrefix(res.ImageURL, "https://") {
		return res, nil
	}

	url, err := a.transfer(ctx, req.GenerationID, res.ImageURL)
	if err != nil {
		log.Printf("[storing] 转存失败，降级为上游临时链接（约一小时后失效）gen=%s: %v",
			req.GenerationID, err)
		return res, nil
	}
	res.ImageURL = url
	res.Stored = true
	return res, nil
}

func (a *StoringAdapter) transfer(ctx context.Context, genID, srcURL string) (string, error) {
	// WithoutCancel 之后重新计时：见 transferTimeout 的注释。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transferTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return "", fmt.Errorf("构造下载请求: %w", err)
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("下载图片: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载图片返回 %d", resp.StatusCode)
	}

	// 多读 1 字节：正好读满上限说明后面还有内容，即超限。
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取图片: %w", err)
	}
	if len(body) > maxImageBytes {
		return "", fmt.Errorf("图片超过 %d 字节上限", maxImageBytes)
	}

	// **嗅探内容，不信上游的 Content-Type 头。** 这个字节流要挂到我们自己的域名
	// 下，上游若返回 HTML（无论它把 Content-Type 写成什么），我们就在自己的
	// origin 上托管了一个别人可控的 HTML 文件——那是 XSS。
	ct := http.DetectContentType(body)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	ext, ok := allowedImageTypes[ct]
	if !ok {
		return "", fmt.Errorf("拒绝非图片内容：嗅探到 %q", ct)
	}

	return a.store.Put(ctx, "g/"+genID+"."+ext, ct, body)
}
```

- [ ] **Step 5：跑测试确认通过**

Run: `go test ./internal/generation/ -v`
Expected: 新增 10 个 `TestStoringAdapter*` 全 PASS；既有 adapter/aspect/flux/stub/sweep 测试不变。

- [ ] **Step 6：提交**

```bash
git add internal/generation/adapter.go internal/generation/storing.go internal/generation/storing_test.go
git commit -m "feat: StoringAdapter 转存装饰器——失败降级不退款，嗅探内容拒绝非图片"
```

---

## Task 5：接线——handler 落 `stored`，`BuildAdapters` 包装饰器

**Files:**
- Modify: `internal/handler/generations.go`, `internal/server/router.go`
- Test: `internal/server/generations_test.go`

- [ ] **Step 1：先写失败的测试**

追加到 `internal/server/generations_test.go` 末尾：

```go
func TestGenerateResponseIncludesStoredFlag(t *testing.T) {
	// 默认测试配置没有 R2，且 stub 返回的是相对路径——两个原因都会让 stored
	// 为 false。这里断言的是**字段存在**：漏掉它前端就无从判断链接会不会失效，
	// 而那是静默的（页面照样渲染，只是一小时后变成坏图）。
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "gen-stored@example.com", "secret12345")
	grantTo(t, db, "gen-stored@example.com", 5*modelCredits(t, db, "flux-2-max"))

	w := postGenerate(r, token, `{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	stored, ok := out["stored"]
	if !ok {
		t.Fatalf("响应缺 stored 字段: %s", w.Body.String())
	}
	if stored != false {
		t.Errorf("stub 返回相对路径，不该转存: got %v", stored)
	}
}

func TestGeneratePassesGenerationIDToAdapter(t *testing.T) {
	// 对象 key 由 GenerationID 推导。handler 漏传的话 key 会变成 g/.png——
	// 所有用户的所有图**互相覆盖**，而没有任何地方报错。
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "gen-genid@example.com", "secret12345")
	grantTo(t, db, "gen-genid@example.com", 5*modelCredits(t, db, "flux-2-max"))

	w := postGenerate(r, token, `{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("响应没有 id")
	}

	var g model.Generation
	if err := db.Where("id = ?", id).First(&g).Error; err != nil {
		t.Fatalf("读回生成行: %v", err)
	}
	if g.Stored {
		t.Error("stub 路径不该被标成已转存")
	}
}
```

`generations_test.go` 已 import `encoding/json`、`net/http`、`model`，无需改 import。

- [ ] **Step 2：跑测试确认失败**

Run: `go test ./internal/server/ -run 'TestGenerateResponseIncludesStored|TestGeneratePassesGenerationID' -v`
Expected: `TestGenerateResponseIncludesStoredFlag` FAIL —— `响应缺 stored 字段`。

- [ ] **Step 3：改 handler**

`internal/handler/generations.go`。

第一处，`adapter.Generate` 调用里补 `GenerationID`（在 `UpstreamModel` 之后）：

```go
		UpstreamModel: m.UpstreamModel,
		// 转存装饰器用它拼对象 key（g/<id>.<ext>）。漏传的话 key 会变成
		// g/.png——所有用户的所有图互相覆盖，而没有任何地方报错。
		GenerationID: gen.ID,
```

第二处，成功分支里在 `gen.ImageURL = res.ImageURL` 之后加一行：

```go
	gen.Stored = res.Stored
```

第三处，`toGenerationResponse` 的成功分支：

```go
	if g.Status == model.GenStatusSucceeded {
		out["imageUrl"] = g.ImageURL
		// stored=false 表示这是上游的临时链接，约一小时后失效（R2 未配置或转存
		// 降级）。前端据此提示"链接可能已失效"，而不是让用户对着坏图猜。
		out["stored"] = g.Stored
	} else {
		out["error"] = g.Error
	}
```

- [ ] **Step 4：改 `BuildAdapters` 包上装饰器**

`internal/server/router.go`。把 `BuildAdapters` 换成：

```go
// BuildAdapters 构造 provider → adapter 注册表。
//
// 每个 adapter 都被 StoringAdapter 包一层：上游返回的图片 URL 约一小时后失效，
// 不转存的话历史记录里全是死链，而用户为那些图付过费。包在这里而不是各 adapter
// 内部，新增 provider 就自动获得转存，不依赖谁记得加代码。
//
// 导出是为了让 cmd/server/main.go 能先建好、校验完 provider 再交给路由。
func BuildAdapters(cfg *config.Config) generation.Registry {
	store := buildStorage(cfg)
	return generation.Registry{
		"flux": generation.NewStoringAdapter(buildFluxAdapter(cfg), store),
	}
}

// buildStorage 在没配 R2 时退化成 NoopStorage。
//
// 与 FluxAPIKey 为空退化成 stub、StripeSecretKey 为空禁用计费是同一个约定：
// 本地开发不必凑齐所有外部依赖也能跑完整流程。
func buildStorage(cfg *config.Config) storage.Storage {
	if !cfg.StorageEnabled() {
		log.Println("storage: R2 未完整配置，图片不转存——image_url 存的是上游临时链接，约一小时后失效")
		return storage.NoopStorage{}
	}
	return storage.NewR2Storage(
		cfg.R2Endpoint, cfg.R2AccessKeyID, cfg.R2SecretAccessKey,
		cfg.R2Bucket, cfg.R2PublicBaseURL,
	)
}
```

在 import 块里加 `"image-backend/internal/storage"`。

- [ ] **Step 5：跑测试确认通过**

Run: `go test ./internal/server/ -v`
Expected: 新增 2 个测试 PASS，既有 server 测试全部不变。

- [ ] **Step 6：跑全量并提交**

Run: `go test ./...`
Expected: 全部 ok。

```bash
git add internal/handler/generations.go internal/server/router.go internal/server/generations_test.go
git commit -m "feat: 生成路径接上转存——响应加 stored，BuildAdapters 包 StoringAdapter"
```

---

## Task 6：`GET /generations` 历史接口

**Files:**
- Create: `internal/handler/generations_list.go`, `internal/server/generations_list_test.go`
- Modify: `internal/server/router.go`

- [ ] **Step 1：先写失败的测试**

Create `internal/server/generations_list_test.go`：

```go
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

func getList(r *gin.Engine, token, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/generations"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// insertGen 直接落一行生成记录。
//
// 不走 POST /generations：stub 默认延迟 15 秒，而分页测试要插好几行；而且这里
// 需要精确控制 created_at 来构造游标边界。
func insertGen(t *testing.T, db *gorm.DB, id string, userID uint, createdAt time.Time, status string) {
	t.Helper()
	g := model.Generation{
		ID: id, UserID: userID, Model: "flux-2-max", Prompt: "p",
		AspectRatio: "1:1", Width: 1024, Height: 1024,
		Status: status, ImageURL: "https://img.example.com/" + id + ".png",
		Stored: true, CreatedAt: createdAt,
	}
	if err := db.Create(&g).Error; err != nil {
		t.Fatalf("插入 %s: %v", id, err)
	}
}

func decodeList(t *testing.T, w *httptest.ResponseRecorder) ([]map[string]any, string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Generations []map[string]any `json:"generations"`
		NextCursor  *string          `json:"nextCursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v; body=%s", err, w.Body.String())
	}
	next := ""
	if out.NextCursor != nil {
		next = *out.NextCursor
	}
	return out.Generations, next
}

func ids(rows []map[string]any) []string {
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		id, _ := r["id"].(string)
		got = append(got, id)
	}
	return got
}

func TestListRequiresAuth(t *testing.T) {
	r := setupRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/generations", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应当 401: got %d", w.Code)
	}
}

func TestListOnlyReturnsOwnGenerations(t *testing.T) {
	// 最容易写错、后果最严重的一条：漏掉 user_id 过滤 = 每个用户都能看到别人的
	// prompt 和图，而功能表面上完全正常。
	r, db := setupRouterWithDB(t)
	mineToken := registerAndLogin(t, r, "list-mine@example.com", "secret12345")
	registerAndLogin(t, r, "list-other@example.com", "secret12345")

	var mine, other model.User
	db.Where("email = ?", "list-mine@example.com").First(&mine)
	db.Where("email = ?", "list-other@example.com").First(&other)

	base := time.Now().UTC().Truncate(time.Second)
	insertGen(t, db, "mine-1", mine.ID, base, model.GenStatusSucceeded)
	insertGen(t, db, "other-1", other.ID, base.Add(time.Second), model.GenStatusSucceeded)

	rows, _ := decodeList(t, getList(r, mineToken, ""))
	if got := ids(rows); len(got) != 1 || got[0] != "mine-1" {
		t.Fatalf("只应看到自己的记录，得到 %v", got)
	}
}

func TestListExcludesProcessing(t *testing.T) {
	// processing 要么很快转终态，要么会被启动兜底扫描回收。露出来只会让用户看到
	// 一个永远转圈的格子。
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "list-proc@example.com", "secret12345")
	var u model.User
	db.Where("email = ?", "list-proc@example.com").First(&u)

	base := time.Now().UTC().Truncate(time.Second)
	insertGen(t, db, "done-1", u.ID, base, model.GenStatusSucceeded)
	insertGen(t, db, "stuck-1", u.ID, base.Add(time.Second), model.GenStatusProcessing)

	rows, _ := decodeList(t, getList(r, token, ""))
	if got := ids(rows); len(got) != 1 || got[0] != "done-1" {
		t.Fatalf("processing 不该返回，得到 %v", got)
	}
}

func TestListIncludesFailed(t *testing.T) {
	// 失败记录**要**返回。用户看到"我明明生成过一张"却在历史里找不到，会怀疑是不是
	// 被吞了钱；而失败记录恰恰能证明没扣钱（creditsSpent: 0）。
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "list-failed@example.com", "secret12345")
	var u model.User
	db.Where("email = ?", "list-failed@example.com").First(&u)

	insertGen(t, db, "failed-1", u.ID, time.Now().UTC(), model.GenStatusFailed)

	rows, _ := decodeList(t, getList(r, token, ""))
	if len(rows) != 1 {
		t.Fatalf("失败记录要返回，得到 %d 行", len(rows))
	}
	if rows[0]["status"] != model.GenStatusFailed {
		t.Errorf("status: got %v", rows[0]["status"])
	}
	if _, ok := rows[0]["error"]; !ok {
		t.Error("失败记录要带 error 字段")
	}
}

func TestListPaginatesWithoutGapsOrDuplicates(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "list-page@example.com", "secret12345")
	var u model.User
	db.Where("email = ?", "list-page@example.com").First(&u)

	base := time.Now().UTC().Truncate(time.Second)
	// 倒序期望：g5, g4, g3, g2, g1
	for i := 1; i <= 5; i++ {
		insertGen(t, db, fmt.Sprintf("g%d", i), u.ID,
			base.Add(time.Duration(i)*time.Second), model.GenStatusSucceeded)
	}

	var seen []string
	cursor := ""
	for page := 0; page < 5; page++ {
		q := "?limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		rows, next := decodeList(t, getList(r, token, q))
		seen = append(seen, ids(rows)...)
		cursor = next
		if cursor == "" {
			break
		}
	}

	want := []string{"g5", "g4", "g3", "g2", "g1"}
	if len(seen) != len(want) {
		t.Fatalf("翻页结果数量不对: got %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("翻页顺序/内容不对: got %v, want %v", seen, want)
		}
	}
}

func TestListPaginatesWithIdenticalTimestamps(t *testing.T) {
	// created_at 完全相同时，只按时间戳做游标会漏行或重复。这条同时也是驱动层
	// 时间精度的守卫：若 SQLite/Postgres 存回来的时间被截断，边界比较会出错，
	// 而症状就是这里的重复或缺失。
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "list-tie@example.com", "secret12345")
	var u model.User
	db.Where("email = ?", "list-tie@example.com").First(&u)

	same := time.Now().UTC().Truncate(time.Second)
	insertGen(t, db, "tie-a", u.ID, same, model.GenStatusSucceeded)
	insertGen(t, db, "tie-b", u.ID, same, model.GenStatusSucceeded)
	insertGen(t, db, "tie-c", u.ID, same, model.GenStatusSucceeded)

	var seen []string
	cursor := ""
	for page := 0; page < 5; page++ {
		q := "?limit=1"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		rows, next := decodeList(t, getList(r, token, q))
		seen = append(seen, ids(rows)...)
		cursor = next
		if cursor == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Fatalf("同时间戳翻页应当恰好拿到 3 行不重复的记录，得到 %v", seen)
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		if uniq[id] {
			t.Fatalf("重复返回 %s: %v", id, seen)
		}
		uniq[id] = true
	}
}

func TestListRejectsInvalidCursor(t *testing.T) {
	// 不能静默当第一页处理：那会让翻页在游标格式变更后无声地从头开始，用户以为
	// 图丢了。
	r := setupRouter(t)
	token := registerAndLogin(t, r, "list-badcur@example.com", "secret12345")

	w := getList(r, token, "?cursor=not-a-valid-cursor!!")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 cursor 应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["code"] != float64(40000) {
		t.Errorf("code: got %v, want 40000", out["code"])
	}
}

func TestListClampsLimit(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "list-limit@example.com", "secret12345")
	var u model.User
	db.Where("email = ?", "list-limit@example.com").First(&u)

	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		insertGen(t, db, fmt.Sprintf("lim-%d", i), u.ID,
			base.Add(time.Duration(i)*time.Second), model.GenStatusSucceeded)
	}

	// limit=0 与 limit=999 都要被钳制而不是报错——上游客户端传个 0 是常见的
	// off-by-one，回 400 只是把问题推给调用方。
	for _, q := range []string{"?limit=0", "?limit=999", "?limit=abc", "?limit=-5"} {
		w := getList(r, token, q)
		if w.Code != http.StatusOK {
			t.Errorf("%s 应当被钳制而不是报错: got %d", q, w.Code)
			continue
		}
		rows, _ := decodeList(t, w)
		if len(rows) == 0 || len(rows) > 50 {
			t.Errorf("%s 返回行数不合理: %d", q, len(rows))
		}
	}
}

func TestListReturnsNullCursorOnLastPage(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "list-lastpage@example.com", "secret12345")
	var u model.User
	db.Where("email = ?", "list-lastpage@example.com").First(&u)
	insertGen(t, db, "only-1", u.ID, time.Now().UTC(), model.GenStatusSucceeded)

	_, next := decodeList(t, getList(r, token, "?limit=10"))
	if next != "" {
		t.Errorf("没有下一页时 nextCursor 应当是 null, got %q", next)
	}
}

func TestListIncludesStoredFlag(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "list-stored@example.com", "secret12345")
	var u model.User
	db.Where("email = ?", "list-stored@example.com").First(&u)
	insertGen(t, db, "stored-1", u.ID, time.Now().UTC(), model.GenStatusSucceeded)

	rows, _ := decodeList(t, getList(r, token, ""))
	if len(rows) != 1 {
		t.Fatalf("行数: %d", len(rows))
	}
	if rows[0]["stored"] != true {
		t.Errorf("stored 应当透出: got %v", rows[0]["stored"])
	}
}
```

- [ ] **Step 2：跑测试确认失败**

Run: `go test ./internal/server/ -run TestList -v`
Expected: 全部 FAIL——路由未注册，`GET /api/v1/generations` 返回 404（`decodeList` 会因状态码不是 200 而 `t.Fatalf`）。

- [ ] **Step 3：实现游标与 List**

Create `internal/handler/generations_list.go`：

```go
package handler

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"image-backend/internal/middleware"
	"image-backend/internal/model"
)

const (
	defaultListLimit = 20
	maxListLimit     = 50
)

// encodeCursor 把一行的排序键编成不透明游标。
//
// **不透明**（base64）是为了以后能换实现而不破坏已经拿着游标的客户端。带上 id
// 而不只是时间戳：created_at 可能完全相同（同一秒内的两次生成），只按时间戳翻页
// 会漏行或重复。
func encodeCursor(g model.Generation) string {
	raw := g.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + g.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor 不是合法 base64: %w", err)
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", errors.New("cursor 结构不对")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor 时间戳不合法: %w", err)
	}
	return ts, parts[1], nil
}

// List 返回当前用户的生成历史，游标分页，倒序。
//
// 这个接口是"用户付了钱能拿回自己的图"的唯一读路径——在它存在之前，客户端一旦
// 丢掉 POST /generations 的响应（关标签页、断网、刷新），图片就永久不可达，而
// 次数已经扣了。
func (h *GenerationsHandler) List(c *gin.Context) {
	userID := c.GetUint(middleware.CtxUserIDKey)

	// limit 越界**钳制而不报错**：客户端传个 0 是常见的 off-by-one，回 400 只是
	// 把问题推回去，而这里没有任何需要调用方修正的语义。
	limit := defaultListLimit
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	// 不返回 processing：它要么很快转终态，要么会被启动兜底扫描回收，露出来只会
	// 让用户看到一个永远转圈的格子。
	q := h.DB.Where("user_id = ? AND status <> ?", userID, model.GenStatusProcessing)

	if raw := c.Query("cursor"); raw != "" {
		ts, id, err := decodeCursor(raw)
		if err != nil {
			// **不能静默当第一页处理**：那会让翻页在游标格式变更后无声地从头开始，
			// 用户以为图丢了。
			c.JSON(http.StatusBadRequest,
				gin.H{"code": errCodeBadRequest, "message": "invalid cursor"})
			return
		}
		// 展开写而不用行值比较元组 (created_at, id) < (?, ?)：SQLite 与 Postgres
		// 对行值比较的支持不一致，而本项目两边都要跑。
		q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", ts, ts, id)
	}

	// 多取一行来判断有没有下一页——比额外跑一次 COUNT 便宜，也不会像 COUNT 那样
	// 因为并发插入而出现"说有下一页、翻过去是空的"。
	var rows []model.Generation
	if err := q.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		log.Printf("[generations] 历史查询失败 user=%d: %v", userID, err)
		c.JSON(http.StatusInternalServerError,
			gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	// nextCursor 用 any 而不是 string：没有下一页时要序列化成 JSON null，空字符串
	// 会让前端把"没有更多"与"游标是空串"混在一起。
	var next any
	if len(rows) > limit {
		rows = rows[:limit]
		next = encodeCursor(rows[len(rows)-1])
	}

	out := make([]gin.H, 0, len(rows))
	for _, g := range rows {
		out = append(out, toGenerationResponse(g))
	}
	c.JSON(http.StatusOK, gin.H{"generations": out, "nextCursor": next})
}
```

- [ ] **Step 4：注册路由**

`internal/server/router.go`，在 `authed.POST("/generations", generationsHandler.Create)` 之后加：

```go
	authed.GET("/generations", generationsHandler.List)
```

- [ ] **Step 5：跑测试确认通过**

Run: `go test ./internal/server/ -run TestList -v`
Expected: 全部 PASS。

若 `TestListPaginatesWithIdenticalTimestamps` 失败并出现重复行，说明数据库驱动把 `created_at` 的精度截断了，`created_at = ?` 匹配不上。这时**不要放宽测试**——它守的是真实的翻页正确性。改法是把游标比较收敛到单一单调键（例如改成只按 `id` 排序分页，前提是 id 单调；当前 id 是 UUID v4，不单调，所以更可能的正确改法是给 `generations` 加一个自增 `seq` 列并按它分页）。这是一个需要回到设计文档确认的变更，先停下来汇报。

- [ ] **Step 6：跑全量并提交**

Run: `go test ./...`
Expected: 全部 ok。

```bash
git add internal/handler/generations_list.go internal/server/generations_list_test.go internal/server/router.go
git commit -m "feat: GET /generations 历史接口——游标分页、排除 processing、保留失败记录"
```

---

## 后端完成检查

全部 6 个任务做完后确认：

- [ ] `go test ./...` 全绿
- [ ] `go vet ./...` 无输出
- [ ] 手工冒烟（无需 R2）：起服务 → 注册 → `POST /admin/credits` 发次数 → `POST /generations`（prompt 带 `quick`）→ `GET /api/v1/generations` 能看到那一行且 `stored: false`
- [ ] 手工冒烟（有真实 R2 凭证时，见设计文档 §9"人工"）：确认对象进桶、自定义域能匿名打开；然后故意填错 `R2_BUCKET`，确认生成仍成功、`stored=false`、日志有告警、次数**没有**被退

**只有前两条全绿之后**才开始前端计划。第三条冒烟建议一起做完——它是前端唯一的真实数据来源。
