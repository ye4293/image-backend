# M3b：接通 Flux 与生成编排 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让用户在界面上点一次生成，拿到一张由 Flux 真实产出的图；失败时次数按原拆分退回。

**Architecture:** `POST /api/v1/generations` 同步返回结果（上游 ezlinkai 对图像同步返回 URL，实测 21 秒）。编排顺序是**先落 `processing` 行 → 扣费 → 调上游 → 落结果或退款**，这个顺序不能换（见"动手前必读"）。provider 差异全部关在 adapter 里，`image_models.provider` 决定运行时用哪个。

**Tech Stack:** Go + Gin + GORM + `github.com/google/uuid`；前端 Next.js 16 只改四个 Route Handler 的内部实现。

**设计文档：** `docs/superpowers/specs/2026-07-28-m3-flux-integration-design.md`

**起点：** 分支 `main`，HEAD `c7679f3`（M3a 已合并），工作树干净。

---

## 动手前必读

### 1. 编排顺序不能换：先落行，再扣费

必须是 **建 `generations` 行（`processing`）→ 扣费 → 调上游**。

反过来（先扣费再建行）有一个无法补救的窗口：扣费成功后、建行之前进程崩溃，`credit_transactions` 里留下一条扣费流水，但没有任何 `generations` 行指向它——启动兜底扫描是靠扫 `processing` 行找回退款的，找不到行就永远退不回来，**用户的钱凭空消失且无人知晓**。

反之先建行、扣费失败（余额不足），留下的是一条 `processing` 行没有扣费流水；扫描调 `Refund` 时找不到扣费流水会静默返回，再把行标成 `failed` 即可，没有损失。

### 2. 上游调用必须用脱离请求的 context

```go
// 刻意**不**用 c.Request.Context()：客户端断开不应该取消一次已经付过费的生成。
upstreamCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()
```

Flux 实测 21 秒、慢时更久，"用户中途关标签页"是常见情况而非边缘情况。用请求 context 的话，关页面 = 扣了次数丢了图。

### 3. 上游的两个坑（实测所得，不是文档摘抄）

- **`polling_url` 字段名有误导性**：它装的是**最终图片 URL**，不是给你去轮询的地址。
- **两个端点的认证头不一致**：提交用 `x-key`，`get_result` 用 `Authorization: Bearer`。这是上游的不一致，不是我们写错，注释里要写明，否则后人会"统一"掉然后 401。

### 4. 无 key 时必须能跑，否则测试既慢又烧钱

`fail` / `slow` / `quick` 关键词触发在 M2 是前端假数据的机制。接真后端后它们必须**挪到后端的 stub adapter**：`FLUX_API_KEY` 为空时用 stub，返回本地占位图并保留关键词行为。否则前端 e2e 每跑一次都真调 Flux——每次 21 秒、每次花钱。

### 5. 本机没有 Docker 也没有 Postgres

并发相关的测试仍会跳过。M3a 遗留的 **Postgres 关口尚未通过**（`Refund` 的唯一键冲突回滚分支至今没执行过），本计划不解决它，但不要因为"测试都绿"就以为已经验证过。

---

## File Structure

```
internal/generation/
  adapter.go             Adapter 接口 + GenerateRequest/Result + provider 注册
  stub.go                无 key 时的 stub，保留 fail/slow/quick 关键词
  flux.go                Flux adapter（真实上游 + get_result 兜底）
  flux_test.go           用 httptest.Server 打桩上游，测请求构造与响应解析
  aspect.go              画幅 → 宽高映射
  sweep.go               启动兜底扫描
internal/model/
  generation.go          generations 表
internal/handler/
  generations.go         POST /api/v1/generations 编排
internal/middleware/
  active.go              RequireActiveUser：封禁用户拦截
internal/config/
  config.go              (改) FluxAPIKey / EZLinkAIBaseURL
internal/server/
  router.go              (改) 注册路由与中间件
  generations_test.go    接口层测试（走 stub adapter）
```

职责边界：**handler 只做编排，不构造任何上游请求**；adapter 不碰数据库也不碰次数。两者通过 `GenerateRequest`/`GenerateResult` 通信。

---

## Task 1: generations 表与画幅映射

**Files:**
- Create: `internal/model/generation.go`
- Create: `internal/generation/aspect.go`
- Test: `internal/generation/aspect_test.go`
- Modify: `internal/database/database.go`

- [ ] **Step 1: 写 generations 表模型**

`internal/model/generation.go`：

```go
package model

import "time"

// 生成任务状态。processing 是**落库时的初始状态**——调上游之前就要落行，
// 这样进程崩溃后启动扫描才能找到它并退款。
const (
	GenStatusProcessing = "processing"
	GenStatusSucceeded  = "succeeded"
	GenStatusFailed     = "failed"
)

type Generation struct {
	ID          string `gorm:"primaryKey;size:64"`
	UserID      uint   `gorm:"index;not null"`
	Model       string `gorm:"size:64;not null"`
	Prompt      string `gorm:"type:text;not null"`
	AspectRatio string `gorm:"size:16;not null"`
	Width       int    `gorm:"not null"`
	Height      int    `gorm:"not null"`
	// Status 见上面三个常量。索引是给启动扫描用的——它要按 status 找卡住的行。
	Status       string `gorm:"index;size:16;not null"`
	ImageURL     string `gorm:"type:text"`
	CreditsSpent int    `gorm:"not null;default:0"`
	// UpstreamID 是上游返回的任务 id，出问题时凭它去上游对账。
	UpstreamID   string `gorm:"size:128"`
	UpstreamCost int    `gorm:"not null;default:0"`
	Error        string `gorm:"type:text"`
	IsPublic     bool   `gorm:"not null;default:false"`
	DurationMs   int64  `gorm:"not null;default:0"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
```

- [ ] **Step 2: 写画幅映射的失败测试**

`internal/generation/aspect_test.go`：

```go
package generation

import "testing"

func TestAspectDimensions(t *testing.T) {
	cases := []struct {
		ratio         string
		wantW, wantH  int
	}{
		{"1:1", 1024, 1024},
		{"16:9", 1344, 768},
		{"9:16", 768, 1344},
	}
	for _, c := range cases {
		w, h, ok := Dimensions(c.ratio)
		if !ok {
			t.Fatalf("%s 应当被支持", c.ratio)
		}
		if w != c.wantW || h != c.wantH {
			t.Fatalf("%s: got %dx%d, want %dx%d", c.ratio, w, h, c.wantW, c.wantH)
		}
	}
}

func TestAspectRejectsUnknown(t *testing.T) {
	for _, r := range []string{"4:3", "", "1:1 ", "21:9"} {
		if _, _, ok := Dimensions(r); ok {
			t.Fatalf("%q 不该被接受——静默纠正成 1:1 会让前端以为自己传对了", r)
		}
	}
}

func TestAspectDimensionsAreAboutOneMegapixel(t *testing.T) {
	// 上游按输出百万像素计价（实测 cost:7 对应 output_mp:1）。三种画幅都压在
	// 约 1MP，成本才可预测；否则同样"扣 1 次"的两张图真实成本能差一倍。
	for _, r := range []string{"1:1", "16:9", "9:16"} {
		w, h, _ := Dimensions(r)
		mp := float64(w*h) / 1e6
		if mp < 0.9 || mp > 1.2 {
			t.Fatalf("%s 是 %.2fMP，超出 0.9~1.2 区间", r, mp)
		}
	}
}
```

- [ ] **Step 3: 运行确认失败**

```bash
go test ./internal/generation/
```

期望：编译失败，`undefined: Dimensions`。

- [ ] **Step 4: 实现**

`internal/generation/aspect.go`：

```go
package generation

// Dimensions 把画幅映射成具体宽高。第二个返回值为 false 表示不支持该画幅。
//
// **不做静默纠正。** 传了不支持的画幅就该报错：静默改成 1:1 会让前端以为自己
// 传对了，而用户拿到的是另一个比例的图，且没有任何地方提示出了问题。
//
// 三种尺寸都压在约 1MP：上游按输出百万像素计价（实测 cost:7 ↔ output_mp:1），
// 尺寸浮动会让"扣 1 次"对应的真实成本浮动。
func Dimensions(aspectRatio string) (width, height int, ok bool) {
	switch aspectRatio {
	case "1:1":
		return 1024, 1024, true
	case "16:9":
		return 1344, 768, true
	case "9:16":
		return 768, 1344, true
	default:
		return 0, 0, false
	}
}
```

- [ ] **Step 5: 迁移**

在 `internal/database/database.go` 的 `AutoMigrate` 参数列表末尾加 `&model.Generation{}`。

- [ ] **Step 6: 验证**

```bash
go build ./... && go vet ./... && go test ./...
```

期望：全绿，`internal/generation` 3 个测试通过。

- [ ] **Step 7: 提交**

```bash
git add internal/model/generation.go internal/generation internal/database
git commit -m "feat: generations 表与画幅到宽高的映射"
```

---

## Task 2: Adapter 接口与 stub 实现

**Files:**
- Create: `internal/generation/adapter.go`
- Create: `internal/generation/stub.go`
- Test: `internal/generation/stub_test.go`

- [ ] **Step 1: 定义接口**

`internal/generation/adapter.go`：

```go
package generation

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"image-backend/internal/model"
)

// ErrUpstream 上游生成失败（模型拒绝、超时、返回错误状态）。调用方据此退款并把
// generation 标成 failed。与"我们自己写错了"区分开：后者应当是 500。
//
// **实现方注意**：这个契约要求所有"会让用户看到失败"的错误都包上 ErrUpstream，
// 超时与 ctx 取消也算。分类不能按阶段浮动——同一个用户可见的失败，发生在提交阶段
// 还是轮询阶段，归类必须一致，否则调用方按 errors.Is 判断时行为会随时机变化。
var ErrUpstream = errors.New("upstream generation failed")

// GenerateRequest 只包含**我们自己的**领域概念。
//
// 各 provider 如何把它翻译成自家请求体、又如何从自家响应里挖出图片 URL，全部
// 关在各自的 adapter 内。**不要**为了"统一"往这里加 provider 专属字段——产品
// 要求兼容各官方 API 的功能，那种通用参数结构在第三家一定漏。
type GenerateRequest struct {
	Prompt string
	Width  int
	Height int
	Seed   *int // nil 表示不指定

	// UpstreamModel 是上游认识的模型标识，由调用方从 image_models 行里取。
	//
	// 它**放在请求里而不是 adapter 的构造参数里**：Registry 只按 provider 索引，
	// 每个 provider 只有一个 adapter 实例。把上游模型名焊死在实例上的话，表里
	// 一旦出现第二行同 provider 的模型（比如 flux-2-pro），请求会被静默提交到
	// 前一个模型的路径——用户按 pro 付费、拿到 max 的结果，而 generations 行记
	// 的是 pro，没有任何地方报错。
	//
	// 这不是设计文档 §3 拒绝的"通用参数结构"：上游模型标识是**路由信息**，设计
	// 文档正是为了让它按行变化才把它存进 image_models 表的。
	UpstreamModel string
}

type GenerateResult struct {
	ImageURL string
	// UpstreamID 上游任务 id，落库便于事后对账。
	UpstreamID string
	// UpstreamCost 上游报告的成本，与我们扣的次数是两回事，落库便于核算毛利。
	UpstreamCost int
}

type Adapter interface {
	// Generate 同步返回结果。实现内部负责兜底查询与错误归一化。
	//
	// ctx **必须**是脱离 HTTP 请求生命周期的 context——客户端断开不应该取消
	// 一次已经付过费的生成。
	Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error)
}

// Registry 按 provider 名字选 adapter。
type Registry map[string]Adapter

func (r Registry) Get(provider string) (Adapter, error) {
	a, ok := r[provider]
	if !ok {
		return nil, fmt.Errorf("没有注册 provider %q 的 adapter", provider)
	}
	// 注册成 nil 与没注册一样致命，但不挡的话返回的是 (nil, nil)：调用方拿着 nil
	// 接口去调 Generate 直接 panic，而那时行已经建了、次数已经扣了。
	if a == nil {
		return nil, fmt.Errorf("provider %q 注册的 adapter 是 nil", provider)
	}
	return a, nil
}

// ValidateProviders 校验 image_models 里所有启用行的 provider 都能在 Registry 里
// 解析出来，返回人类可读的问题列表（空列表表示没问题）。
//
// **要在开始接流量之前调用。** 否则 provider 字段打错一个字母，得等第一个选中该
// 模型的用户以 500 的形式替我们发现——那是最贵的一种发现方式。
func ValidateProviders(db *gorm.DB, r Registry) []string {
	var models []model.ImageModel
	if err := db.Where("enabled = ?", true).Find(&models).Error; err != nil {
		return []string{fmt.Sprintf("无法读取 image_models 校验 provider: %v", err)}
	}
	var problems []string
	for _, m := range models {
		if _, err := r.Get(m.Provider); err != nil {
			problems = append(problems, fmt.Sprintf("模型 %q 的 provider 无法解析: %v", m.ID, err))
		}
	}
	return problems
}
```

- [ ] **Step 2: 写 stub 的失败测试**

`internal/generation/stub_test.go`：

```go
package generation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStubQuickSucceedsFast(t *testing.T) {
	s := NewStubAdapter()
	start := time.Now()
	res, err := s.Generate(context.Background(), GenerateRequest{Prompt: "quick cat", Width: 1024, Height: 1024})
	if err != nil {
		t.Fatalf("quick 应当成功: %v", err)
	}
	if res.ImageURL == "" {
		t.Fatal("应当返回占位图 URL")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("quick 应当很快返回，实际 %v", elapsed)
	}
}

func TestStubFailKeywordReturnsUpstreamError(t *testing.T) {
	s := NewStubAdapter()
	_, err := s.Generate(context.Background(), GenerateRequest{Prompt: "please fail", Width: 1024, Height: 1024})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("fail 关键词应当返回 ErrUpstream: %v", err)
	}
}

func TestStubKeywordPriority(t *testing.T) {
	// 与前端 M2 假数据一致：fail > slow > quick > 默认。变了会让端到端测试的
	// 预期悄悄失效。
	s := NewStubAdapter()
	_, err := s.Generate(context.Background(), GenerateRequest{Prompt: "quick fail", Width: 1, Height: 1})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("fail 优先级应当高于 quick: %v", err)
	}
}

// slow > quick 此前没测——交换 stub.go 里那两个 case 仍然全绿，但会把 90 秒那条
// 端到端路径悄悄改成 200 毫秒。这里靠"400 毫秒内不该返回成功"来钉住。
func TestStubSlowBeatsQuick(t *testing.T) {
	s := NewStubAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := s.Generate(ctx, GenerateRequest{Prompt: "quick slow", Width: 1, Height: 1}); err == nil {
		t.Fatal("slow 优先级应当高于 quick：400 毫秒内不该返回成功")
	}
}

// stub 记下收到的请求，这样 server 层测试才能发现 handler 把画幅译错或漏传 Seed。
// 忽略入参的 stub 会让接口层的这类 bug 完全隐形。
func TestStubRecordsLastRequest(t *testing.T) {
	s := NewStubAdapter()
	seed := 7
	if _, err := s.Generate(context.Background(), GenerateRequest{
		Prompt: "quick cat", Width: 1344, Height: 768, Seed: &seed, UpstreamModel: "flux-2-max",
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, ok := s.LastRequest()
	if !ok {
		t.Fatal("应当记下收到的请求")
	}
	if got.Width != 1344 || got.Height != 768 {
		t.Fatalf("宽高未记下: %dx%d", got.Width, got.Height)
	}
	if got.Seed == nil || *got.Seed != 7 {
		t.Fatalf("Seed 未记下: %v", got.Seed)
	}
	if got.UpstreamModel != "flux-2-max" {
		t.Fatalf("上游模型名未记下: %q", got.UpstreamModel)
	}
}

func TestStubRespectsContextCancellation(t *testing.T) {
	// stub 的默认延迟是 15 秒；ctx 取消时必须立刻返回，否则测试套件会被拖死。
	s := NewStubAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.Generate(ctx, GenerateRequest{Prompt: "a normal prompt", Width: 1, Height: 1})
	if err == nil {
		t.Fatal("ctx 超时应当返回错误")
	}
	// C2：ctx 取消也是"用户可见的失败"，必须与其他上游失败同一分类，否则调用方按
	// errors.Is 判断时行为会随时机变化。
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("ctx 超时应当归一成 ErrUpstream: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("包装后原错误仍应可判定: %v", err)
	}
	if !strings.Contains(err.Error(), "context") && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应当是 context 错误: %v", err)
	}
}
```

- [ ] **Step 3: 运行确认失败**

```bash
go test ./internal/generation/ -run TestStub
```

期望：`undefined: NewStubAdapter`。

- [ ] **Step 4: 实现 stub**

`internal/generation/stub.go`：

```go
package generation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// StubImageURL 是 stub 返回的占位图。前端 public/ 下已有同名文件。
const StubImageURL = "/placeholder-generation.svg"

// StubAdapter 在没有配置上游 key 时顶替真实 adapter。
//
// 存在理由：接真上游后，端到端测试每跑一次都会真调 Flux——每次约 21 秒、每次
// 花钱。stub 让 CI 与本地测试既快又免费，同时保留 M2 前端假数据那套**确定性**
// 关键词触发，这样端到端测试仍能稳定复现失败路径。
//
// 关键词优先级：fail > slow > quick > 默认。与前端 M2 的行为保持一致。
type StubAdapter struct {
	mu      sync.Mutex
	lastReq *GenerateRequest
}

func NewStubAdapter() *StubAdapter { return &StubAdapter{} }

// LastRequest 返回最后一次收到的请求。
//
// stub 记下入参是为了让**接口层**测试能发现 handler 把画幅译错、漏传上游模型名或漏传
// Seed。一个忽略入参的 stub 会让这类 bug 完全隐形——上层测试只能看到"成功了"。
func (s *StubAdapter) LastRequest() (GenerateRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastReq == nil {
		return GenerateRequest{}, false
	}
	return *s.lastReq, true
}

func (s *StubAdapter) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	s.mu.Lock()
	captured := req
	s.lastReq = &captured
	s.mu.Unlock()

	delay, fail := stubBehavior(req.Prompt)

	// 用 select 而不是 time.Sleep：ctx 取消时要立刻返回，否则默认 15 秒的延迟
	// 会把测试套件拖死，而且也不符合真实 adapter 该有的行为。
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		// 双 %w：ctx 取消同样是"用户可见的失败"，分类必须与其他上游失败一致（见
		// ErrUpstream 的注释），同时保留原错误能被 errors.Is 判定。
		return GenerateResult{}, fmt.Errorf("%w: 等待 stub 结果被取消: %w", ErrUpstream, ctx.Err())
	}

	if fail {
		return GenerateResult{}, fmt.Errorf("%w: stub 模拟的上游失败", ErrUpstream)
	}
	return GenerateResult{
		ImageURL:     StubImageURL,
		UpstreamID:   "stub-" + fmt.Sprint(time.Now().UnixNano()),
		UpstreamCost: 0,
	}, nil
}

func stubBehavior(prompt string) (delay time.Duration, fail bool) {
	p := strings.ToLower(prompt)
	switch {
	case strings.Contains(p, "fail"):
		return 800 * time.Millisecond, true
	case strings.Contains(p, "slow"):
		return 90 * time.Second, false
	case strings.Contains(p, "quick"):
		return 200 * time.Millisecond, false
	default:
		return 15 * time.Second, false
	}
}
```

注意 stub 的 `fail` 延迟比前端假数据的 8 秒短得多（800ms）——stub 只需要证明失败路径走通，不需要模拟真实耗时，端到端测试因此能快很多。

- [ ] **Step 5: 运行确认通过**

```bash
go test ./internal/generation/ -v 2>&1 | tail -20
```

期望：10 个测试全过（3 画幅 + 6 stub + 1 Registry/校验组）。

- [ ] **Step 6: 提交**

```bash
git add internal/generation/adapter.go internal/generation/stub.go internal/generation/stub_test.go
git commit -m "feat: adapter 接口与无 key 时的 stub（保留关键词触发）"
```

---

## Task 3: Flux adapter

**Files:**
- Create: `internal/generation/flux.go`
- Test: `internal/generation/flux_test.go`
- Modify: `internal/config/config.go`

- [ ] **Step 1: 配置项**

在 `internal/config/config.go` 的 `Config` 结构体加两个字段，并在 `Load()` 里读取：

```go
	// EZLinkAIBaseURL 上游网关地址。可覆盖是为了让测试指向 httptest.Server。
	EZLinkAIBaseURL string
	// FluxAPIKey 为空时使用 stub adapter（见 internal/generation/stub.go）。
	FluxAPIKey string
```

```go
		EZLinkAIBaseURL: getEnv("EZLINKAI_BASE_URL", "https://api.ezlinkai.com"),
		FluxAPIKey:      getEnv("FLUX_API_KEY", ""),
```

- [ ] **Step 2: 写失败的测试**

`internal/generation/flux_test.go`：

```go
package generation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestFluxAdapter 造一个轮询间隔极短的 adapter：生产间隔是 3 秒，测试里等 3 秒
// 只为看它循环一次不值得。
func newTestFluxAdapter(baseURL, apiKey string) *FluxAdapter {
	a := NewFluxAdapter(baseURL, apiKey)
	a.pollInterval = 5 * time.Millisecond
	return a
}

func TestFluxSubmitReturnsImageFromPollingURL(t *testing.T) {
	var gotPath, gotKey, gotAuth string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-key")
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cost":7,"id":"abc123","input_mp":0,"output_mp":1,
			"polling_url":"https://cdn.example/img.png","status":"Ready"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "test-key")
	seed := 42
	res, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "a cat", Width: 1024, Height: 1024, Seed: &seed, UpstreamModel: "flux-2-max",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if gotPath != "/flux/v1/flux-2-max" {
		t.Fatalf("提交路径错误: %s", gotPath)
	}
	// 提交端点用 x-key，**不是** Authorization。这是上游的不一致，实测所得。
	if gotKey != "test-key" {
		t.Fatalf("提交应当用 x-key 头: got %q", gotKey)
	}
	if gotAuth != "" {
		t.Fatalf("提交不该带 Authorization: got %q", gotAuth)
	}
	if gotBody["prompt"] != "a cat" || gotBody["width"] != float64(1024) {
		t.Fatalf("请求体错误: %+v", gotBody)
	}
	if gotBody["seed"] != float64(42) {
		t.Fatalf("seed 未透传: %+v", gotBody)
	}
	// output_format 与 safety_tolerance 是实测确定的上游契约。没有断言的话，谁改掉
	// 都不会有人发现，直到用户拿到 png 或被安全过滤挡下。
	if gotBody["output_format"] != "jpeg" {
		t.Fatalf("output_format 应当是 jpeg: %+v", gotBody)
	}
	if gotBody["safety_tolerance"] != float64(2) {
		t.Fatalf("safety_tolerance 应当是 2: %+v", gotBody)
	}

	// polling_url 装的是最终图片 URL，不是轮询地址。
	if res.ImageURL != "https://cdn.example/img.png" {
		t.Fatalf("图片 URL 错误: %s", res.ImageURL)
	}
	if res.UpstreamID != "abc123" || res.UpstreamCost != 7 {
		t.Fatalf("上游元数据错误: %+v", res)
	}
}

func TestFluxOmitsSeedWhenNil(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"id":"x","polling_url":"https://cdn.example/a.png","status":"Ready"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	if _, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, present := gotBody["seed"]; present {
		t.Fatalf("Seed 为 nil 时不该出现在请求体里: %+v", gotBody)
	}
}

// I1：上游模型名来自请求而不是构造参数。同一个 adapter 实例必须能提交到不同路径，
// 否则 image_models 里第二行同 provider 的模型会被静默提交到前一行的上游模型。
func TestFluxRoutesByRequestUpstreamModel(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"id":"x","polling_url":"https://cdn.example/a.png","status":"Ready"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	for _, m := range []string{"flux-2-max", "flux-2-pro"} {
		if _, err := a.Generate(context.Background(), GenerateRequest{
			Prompt: "p", Width: 1, Height: 1, UpstreamModel: m,
		}); err != nil {
			t.Fatalf("generate %s: %v", m, err)
		}
	}
	if len(paths) != 2 || paths[0] != "/flux/v1/flux-2-max" || paths[1] != "/flux/v1/flux-2-pro" {
		t.Fatalf("同一实例应当按请求路由到不同上游模型: %v", paths)
	}
}

func TestFluxRejectsMissingUpstreamModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("不该发出任何请求")
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(context.Background(), GenerateRequest{Prompt: "p", Width: 1, Height: 1})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("缺少上游模型名应当归一成 ErrUpstream: %v", err)
	}
}

func TestFluxFallsBackToGetResultWhenNotReady(t *testing.T) {
	var getResultAuth, getResultKey, getResultID string
	var submits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			getResultAuth = r.Header.Get("Authorization")
			getResultKey = r.Header.Get("x-key")
			getResultID = r.URL.Query().Get("id")
			_, _ = w.Write([]byte(`{"id":"abc","result":{"sample":"https://cdn.example/late.png"},"status":"Ready"}`))
			return
		}
		atomic.AddInt32(&submits, 1)
		// 提交返回未就绪，且没有给出图片 URL——必须走兜底查询。
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "test-key")
	res, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if err != nil {
		t.Fatalf("应当通过兜底查询拿到结果: %v", err)
	}
	if res.ImageURL != "https://cdn.example/late.png" {
		t.Fatalf("兜底结果错误: %s", res.ImageURL)
	}
	if getResultID != "abc" {
		t.Fatalf("兜底查询未带 id: %q", getResultID)
	}
	// 兜底端点用 Authorization: Bearer，**不是** x-key。与提交端点相反。
	if getResultAuth != "Bearer test-key" {
		t.Fatalf("兜底应当用 Bearer: got %q", getResultAuth)
	}
	if getResultKey != "" {
		t.Fatalf("兜底不该带 x-key: got %q", getResultKey)
	}
	// 走兜底不等于可以重复提交——重复提交等于重复计费。
	if n := atomic.LoadInt32(&submits); n != 1 {
		t.Fatalf("提交应当只发生一次: got %d", n)
	}
}

// I5：轮询循环此前从未以"循环"的形式跑过——桩第一次调用就返回 Ready。这条盯着第
// 二次迭代真的会发生。
func TestFluxPollsUntilReady(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			if atomic.AddInt32(&polls, 1) < 3 {
				_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"abc","result":{"sample":"https://cdn.example/loop.png"},"status":"Ready"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	res, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if err != nil {
		t.Fatalf("应当轮询到就绪: %v", err)
	}
	if res.ImageURL != "https://cdn.example/loop.png" {
		t.Fatalf("结果错误: %s", res.ImageURL)
	}
	if n := atomic.LoadInt32(&polls); n != 3 {
		t.Fatalf("应当轮询 3 次才就绪: got %d", n)
	}
}

// C1：终态失败状态在第一次轮询就已知，不能当成"还没好"接着轮到超时。
func TestFluxTerminalFailureStatusesFailFast(t *testing.T) {
	statuses := []string{"Error", "Content Moderated", "Request Moderated", "Task not found",
		// 大小写不该影响判定：上游改个大小写就退化成 5 分钟空转是不可接受的。
		"content moderated", "error"}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			var polls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/flux/v1/get_result" {
					atomic.AddInt32(&polls, 1)
					_, _ = w.Write([]byte(`{"id":"abc","status":"` + status + `"}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
			}))
			defer srv.Close()

			a := NewFluxAdapter(srv.URL, "k")
			// 一旦它选择"再轮一次"，这条测试会挂住而不是悄悄变慢。
			a.pollInterval = time.Hour
			start := time.Now()
			_, err := a.Generate(context.Background(), GenerateRequest{
				Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
			})
			if !errors.Is(err, ErrUpstream) {
				t.Fatalf("终态失败应当归一成 ErrUpstream: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("终态失败应当立即返回，实际 %v", elapsed)
			}
			if n := atomic.LoadInt32(&polls); n != 1 {
				t.Fatalf("终态失败不该继续轮询: got %d 次", n)
			}
		})
	}
}

// C1：Ready 但 sample 为空同样是终态——Ready 意味着上游认为这事完了，sample 不会
// 再出现。当成"还没好"就是在一个已知答案上空转到超时。
func TestFluxReadyWithoutSampleFailsFast(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			atomic.AddInt32(&polls, 1)
			_, _ = w.Write([]byte(`{"id":"abc","status":"Ready","result":{"sample":""}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	a := NewFluxAdapter(srv.URL, "k")
	a.pollInterval = time.Hour
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("Ready 但无 sample 应当归一成 ErrUpstream: %v", err)
	}
	if n := atomic.LoadInt32(&polls); n != 1 {
		t.Fatalf("不该继续轮询: got %d 次", n)
	}
}

// C3：网关额度不足时会返回 HTTP 200 加错误信封，json.Unmarshal 会成功（未知字段被
// 忽略），于是 id/status/polling_url 全空。拿着空 id 去轮询五分钟是真 key 上线后最
// 可能遇到的第一个生产事故。
func TestFluxSubmit200WithoutIDFailsFast(t *testing.T) {
	var polled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			polled = true
			_, _ = w.Write([]byte(`{"id":"","status":"Pending"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":{"message":"insufficient user quota"}}`))
	}))
	defer srv.Close()

	a := NewFluxAdapter(srv.URL, "k")
	a.pollInterval = time.Hour
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("200 但拿不到 id 应当归一成 ErrUpstream: %v", err)
	}
	if polled {
		t.Fatal("不该拿着空 id 去轮询")
	}
	// I3：网关信封可能带我们的额度/账号信息，不能进入用户可见的错误文案。
	if strings.Contains(err.Error(), "insufficient user quota") {
		t.Fatalf("上游原始响应体不该出现在错误里: %v", err)
	}
}

func TestFluxUpstreamErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"prompt rejected by safety filter"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("上游 4xx 应当归一成 ErrUpstream: %v", err)
	}
}

func TestFluxGetResultNon2xxIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"gateway exploded"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("兜底查询非 2xx 应当归一成 ErrUpstream: %v", err)
	}
}

// I3：上游原始响应体只进日志，不进用户可见的错误文案。今天是无害的 detail，明天可
// 能是带着我们账号、额度、内部主机名或 key 前缀的网关信封。
func TestFluxDoesNotLeakUpstreamBodyIntoError(t *testing.T) {
	const secret = "internal-host-10.0.0.7 key=sk-abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"` + secret + `"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if err == nil {
		t.Fatal("应当失败")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "10.0.0.7") {
		t.Fatalf("上游响应体泄漏进错误文案（会直达用户浏览器）: %v", err)
	}
}

// I4：401/403 是**我们的**配置问题（key 过期），必须能与"prompt 被安全过滤拒绝"区
// 分开。退款动作两者都对，但混在一起会烧掉一个下午——key 一死，所有请求都报"上游
// 拒绝了你的 prompt"，没有任何信号指向真正的原因。
func TestFluxAuthFailureIsDistinguishable(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"detail":"invalid api key"}`))
		}))

		a := newTestFluxAdapter(srv.URL, "k")
		_, err := a.Generate(context.Background(), GenerateRequest{
			Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
		})
		srv.Close()

		if !errors.Is(err, ErrUpstream) {
			t.Fatalf("%d 仍应当归一成 ErrUpstream（仍要退款）: %v", code, err)
		}
		if !errors.Is(err, ErrUpstreamAuth) {
			t.Fatalf("%d 应当能与普通上游错误区分开: %v", code, err)
		}
	}
}

// C2：提交阶段的 ctx 取消也要包成 ErrUpstream，且保留原错误可判定。
func TestFluxHonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(ctx, GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if err == nil {
		t.Fatal("已取消的 ctx 应当立即返回错误")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("提交阶段的 ctx 取消也要归一成 ErrUpstream: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("包装后原错误仍应可判定: %v", err)
	}
}

// C2：轮询阶段的超时此前返回**裸的** context.DeadlineExceeded——同一个用户可见的失
// 败，分类却因发生阶段而不同。这条把两个阶段钉成一致。
func TestFluxPollingTimeoutIsWrappedAsUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 永远 Pending：只能靠 ctx 到期退出。
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(ctx, GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if err == nil {
		t.Fatal("ctx 到期应当返回错误")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("轮询阶段超时也要归一成 ErrUpstream: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("包装后原错误仍应可判定: %v", err)
	}
}
```

- [ ] **Step 3: 运行确认失败**

```bash
go test ./internal/generation/ -run TestFlux
```

期望：`undefined: NewFluxAdapter`。

> `NewFluxAdapter` 只收 `(baseURL, apiKey)`。上游模型名**按请求**传（`GenerateRequest.UpstreamModel`），
> 不作构造参数——`Registry` 只按 provider 索引，焊死在实例上的话 `image_models` 里第二行同
> provider 的模型会被静默提交到前一行的上游模型路径。`pollInterval` 是字段而不是常量，只
> 为让测试能把 3 秒调小。

- [ ] **Step 4: 实现**

`internal/generation/flux.go`：

```go
package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUpstreamAuth 上游拒绝了我们的凭据（401/403）。它同时也满足 ErrUpstream（仍然
// 要退款），但必须能被单独判定出来。
//
// 为什么值得单独一个哨兵：key 过期是**我们的配置错误**，长得却和"prompt 被安全过滤
// 拒绝"一模一样。混在一起的话，key 一死所有请求都报"上游拒绝了你的 prompt"，没有
// 任何信号能区分"我们的 key 死了导致 100% 失败"与"用户在写触发过滤的 prompt"。
var ErrUpstreamAuth = errors.New("upstream authentication failed")

// FluxAdapter 对接 ezlinkai 的 Flux 端点。
//
// 上游有两处反直觉的地方，都是 2026-07-28 实测所得，不要"顺手统一"掉：
//
//  1. **提交响应里的 `polling_url` 装的是最终图片 URL**，不是给你去轮询的地址。
//     ezlinkai 在内部替我们轮询了 BFL，挂住连接直到出图（实测约 21 秒）。
//  2. **两个端点的认证头不一样**：提交用 `x-key`，`get_result` 用
//     `Authorization: Bearer`。改成统一会 401。
//
// 上游模型名**不在这个结构体里**——它按请求传，见 GenerateRequest.UpstreamModel。
type FluxAdapter struct {
	baseURL string
	apiKey  string
	client  *http.Client
	// pollInterval 兜底轮询的间隔。做成字段只为让测试能把它调小，生产用默认值。
	pollInterval time.Duration
}

func NewFluxAdapter(baseURL, apiKey string) *FluxAdapter {
	return &FluxAdapter{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// 不设 Timeout：超时由调用方通过 ctx 控制，那样才能和"脱离请求的
		// context"配合。这里再设一个会变成两个互相打架的期限。
		client:       &http.Client{},
		pollInterval: 3 * time.Second,
	}
}

type fluxSubmitResponse struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	PollingURL string `json:"polling_url"`
	Cost       int    `json:"cost"`
}

type fluxResultResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result struct {
		Sample string `json:"sample"`
	} `json:"result"`
}

const (
	fluxStatusReady   = "Ready"
	fluxStatusPending = "Pending"
)

// fluxTerminalFailureStatuses 是**答案已知**的失败状态：命中就立刻失败，不再轮询。
//
// 此前唯一的退出条件是"Ready 且有 sample"，其余一律睡 3 秒再轮。于是一个被内容审核
// 拦下的 prompt——第一次轮询就已经拿到 Content Moderated——会被当成"还没好"，空转
// 约 100 次、挂住用户连接整整 5 分钟，最后以超时收场。
//
// 比较用 strings.EqualFold：上游改个大小写不该让我们退化回那条 5 分钟空转。
var fluxTerminalFailureStatuses = []string{
	"Error",
	"Content Moderated",
	"Request Moderated",
	"Task not found",
}

func fluxIsTerminalFailure(status string) bool {
	for _, s := range fluxTerminalFailureStatuses {
		if strings.EqualFold(status, s) {
			return true
		}
	}
	return false
}

func (a *FluxAdapter) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	if req.UpstreamModel == "" {
		// 我们自己的配置问题（image_models 行缺 upstream_model），但对用户表现为一次
		// 失败的生成，所以仍归到 ErrUpstream，让调用方走同一条退款路径。
		log.Printf("[flux] 请求没带 UpstreamModel，检查 image_models.upstream_model")
		return GenerateResult{}, fmt.Errorf("%w: 未指定上游模型", ErrUpstream)
	}

	body := map[string]any{
		"prompt":           req.Prompt,
		"width":            req.Width,
		"height":           req.Height,
		"output_format":    "jpeg",
		"safety_tolerance": 2,
	}
	// Seed 为 nil 时**不能**塞 0 进去——0 是一个合法的 seed，会让"不指定"变成
	// "每次都用同一个 seed"，用户会发现同样的 prompt 永远出同一张图。
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}

	sub, err := a.submit(ctx, req.UpstreamModel, body)
	if err != nil {
		return GenerateResult{}, err
	}

	if strings.EqualFold(sub.Status, fluxStatusReady) && sub.PollingURL != "" {
		return GenerateResult{
			ImageURL:     sub.PollingURL,
			UpstreamID:   sub.ID,
			UpstreamCost: sub.Cost,
		}, nil
	}

	// 未就绪：走兜底查询。这条路径在实测中没出现过（提交总是直接返回 Ready），
	// 但一旦 ezlinkai 内部超时先返回，没有它就是扣了次数拿不到图且无从补救。
	imageURL, err := a.getResult(ctx, sub.ID)
	if err != nil {
		return GenerateResult{}, err
	}
	return GenerateResult{ImageURL: imageURL, UpstreamID: sub.ID, UpstreamCost: sub.Cost}, nil
}

func (a *FluxAdapter) submit(ctx context.Context, upstreamModel string, body map[string]any) (fluxSubmitResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return fluxSubmitResponse{}, err
	}
	// upstreamModel 来自数据库（运维可填），转义后再拼进路径。
	endpoint := fmt.Sprintf("%s/flux/v1/%s", a.baseURL, url.PathEscape(upstreamModel))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fluxSubmitResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-key", a.apiKey) // 提交端点用 x-key（见类型注释）

	resp, err := a.client.Do(httpReq)
	if err != nil {
		// 双 %w：既满足 ErrUpstream 的分类契约，又保留 context.Canceled 之类的原错误
		// 能被 errors.Is 判定。
		return fluxSubmitResponse{}, fmt.Errorf("%w: 提交请求失败: %w", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fluxSubmitResponse{}, fmt.Errorf("%w: 读取响应失败: %w", ErrUpstream, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fluxSubmitResponse{}, httpError("提交", resp.StatusCode, payload)
	}
	var out fluxSubmitResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		// 原始响应体只进日志：它会经 handler 落进 generations.error 并直达用户浏览器，
		// 而网关信封可能带着我们的账号、额度、内部主机名或 key 前缀。
		log.Printf("[flux] 提交响应不是合法 JSON: %s", truncate(string(payload), 300))
		return fluxSubmitResponse{}, fmt.Errorf("%w: 上游响应格式无法解析", ErrUpstream)
	}
	// 网关额度不足时会返回 **HTTP 200 加错误信封**（例如
	// {"error":{"message":"insufficient user quota"}}）。json.Unmarshal 会成功——未知
	// 字段被忽略——于是 id/status/polling_url 全空。不挡的话就是拿着空 id 去轮询
	// `?id=` 五分钟然后 500。这是真 key 上线后最可能遇到的第一个生产事故。
	if out.ID == "" && out.PollingURL == "" {
		log.Printf("[flux] 提交返回 %d 但既无 id 也无图片 URL，原始响应: %s",
			resp.StatusCode, truncate(string(payload), 300))
		return fluxSubmitResponse{}, fmt.Errorf("%w: 上游未返回任务 id", ErrUpstream)
	}
	return out, nil
}

// getResult 轮询兜底端点直到就绪、终态失败或 ctx 到期。
func (a *FluxAdapter) getResult(ctx context.Context, id string) (string, error) {
	// id 来自上游响应，转义后再拼进查询串。
	endpoint := fmt.Sprintf("%s/flux/v1/get_result?id=%s", a.baseURL, url.QueryEscape(id))
	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return "", err
		}
		// 兜底端点用 Bearer，与提交端点相反（见类型注释）。
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)

		resp, err := a.client.Do(httpReq)
		if err != nil {
			return "", fmt.Errorf("%w: 兜底查询失败: %w", ErrUpstream, err)
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return "", fmt.Errorf("%w: 读取兜底响应失败: %w", ErrUpstream, readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", httpError("兜底查询", resp.StatusCode, payload)
		}
		var out fluxResultResponse
		if err := json.Unmarshal(payload, &out); err != nil {
			log.Printf("[flux] 兜底响应不是合法 JSON: %s", truncate(string(payload), 300))
			return "", fmt.Errorf("%w: 上游响应格式无法解析", ErrUpstream)
		}
		if strings.EqualFold(out.Status, fluxStatusReady) {
			if out.Result.Sample != "" {
				return out.Result.Sample, nil
			}
			// Ready 意味着上游认为这事完了，sample 不会再出现。送回循环就是在一个已知
			// 答案上空转到超时，所以当成"上游返回了畸形的成功响应"。
			log.Printf("[flux] id=%s 状态 Ready 但没有 result.sample，原始响应: %s",
				id, truncate(string(payload), 300))
			return "", fmt.Errorf("%w: 上游报告就绪但没有返回图片", ErrUpstream)
		}
		if fluxIsTerminalFailure(out.Status) {
			log.Printf("[flux] id=%s 终态失败，状态 %q", id, out.Status)
			return "", fmt.Errorf("%w: 上游任务失败（状态 %s）", ErrUpstream, out.Status)
		}
		if !strings.EqualFold(out.Status, fluxStatusPending) {
			// 没见过的状态：可能是上游新增的终态。只留痕不改行为——继续轮询由 ctx 兜底，
			// 但日志里要能看出"我们不认识这个状态"，否则它表现为一次莫名的超时。
			log.Printf("[flux] id=%s 出现未识别状态 %q，继续轮询", id, out.Status)
		}

		select {
		case <-time.After(a.pollInterval):
		case <-ctx.Done():
			// 双 %w：分类归一到 ErrUpstream（此前这里返回**裸的**
			// context.DeadlineExceeded，与提交阶段分类不一致），同时保留原错误可判定。
			return "", fmt.Errorf("%w: 等待上游结果超时: %w", ErrUpstream, ctx.Err())
		}
	}
}

// httpError 把非 2xx 归一成错误，并把原始响应体记进**日志**而不是错误文案。
//
// 一个字符串没法同时服务两个不兼容的受众：运维要原始响应体来诊断，终端用户不该看到
// 我们的网关信封（handler 会把错误文案落进 generations.error 并回给浏览器）。边界划在
// adapter 这里，而不是指望 handler 去过滤。
func httpError(stage string, statusCode int, payload []byte) error {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		// 喊出来：key 过期会让 100% 的请求失败，混在普通上游错误里要烧掉一个下午。
		log.Printf("[flux] **认证失败**（%s返回 %d）——检查 FLUX_API_KEY 是否过期或额度用尽。原始响应: %s",
			stage, statusCode, truncate(string(payload), 300))
		return fmt.Errorf("%w: %w: %s返回 %d", ErrUpstream, ErrUpstreamAuth, stage, statusCode)
	}
	log.Printf("[flux] %s返回 %d，原始响应: %s", stage, statusCode, truncate(string(payload), 300))
	return fmt.Errorf("%w: %s返回 %d", ErrUpstream, stage, statusCode)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
```

- [ ] **Step 5: 运行确认通过**

```bash
go test ./internal/generation/ -v 2>&1 | tail -25
```

期望：全过。`internal/generation` 共 26 个顶层测试（3 画幅 + 6 stub + 4 adapter/Registry + 13 flux，flux 的终态状态那条另有 6 个子测试）。

- [ ] **Step 6: 提交**

```bash
git add internal/generation/flux.go internal/generation/flux_test.go internal/config/config.go
git commit -m "feat: Flux adapter，含 get_result 兜底与两端点认证头差异"
```

---

## Task 4: 生成编排接口

**Files:**
- Create: `internal/handler/generations.go`
- Modify: `internal/server/router.go`
- Test: `internal/server/generations_test.go`

这是本计划的核心，也是唯一把账本、adapter、数据库编排到一起的地方。

- [ ] **Step 1: 写失败的测试**

`internal/server/generations_test.go`（`package server`，需 import `gin`、`gorm`、`credit`、`model`）：

```go
// grantTo 直接给用户发次数（测试夹具，走账本以便留下流水）。
func grantTo(t *testing.T, db *gorm.DB, email string, monthly int) uint {
	t.Helper()
	var u model.User
	if err := db.Where("email = ?", email).First(&u).Error; err != nil {
		t.Fatalf("查用户: %v", err)
	}
	if err := credit.Grant(db, u.ID, monthly, 0, "test fixture"); err != nil {
		t.Fatalf("发放: %v", err)
	}
	return u.ID
}

func postGenerate(r *gin.Engine, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGenerateRequiresAuth(t *testing.T) {
	r := setupRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/generations",
		strings.NewReader(`{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应当 401: got %d", w.Code)
	}
}

func TestGenerateSucceedsAndSpendsCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "gen-ok@example.com", "secret12345")
	uid := grantTo(t, db, "gen-ok@example.com", 5)

	w := postGenerate(r, token, `{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1","isPublic":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v; body=%s", err, w.Body.String())
	}
	if out["status"] != "succeeded" {
		t.Fatalf("应当成功: %+v", out)
	}
	if out["imageUrl"] == nil || out["imageUrl"] == "" {
		t.Fatalf("应当有图片 URL: %+v", out)
	}
	if out["creditsSpent"] != float64(1) {
		t.Fatalf("应当扣 1 次: %+v", out)
	}
	if out["isPublic"] != true {
		t.Fatalf("isPublic 应当回传: %+v", out)
	}

	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 4 {
		t.Fatalf("余额应当从 5 减到 4: got %d", bal.MonthlyCredits)
	}

	var g model.Generation
	if err := db.Where("user_id = ?", uid).First(&g).Error; err != nil {
		t.Fatalf("缺少 generations 行: %v", err)
	}
	if g.Status != model.GenStatusSucceeded {
		t.Fatalf("行状态: got %s", g.Status)
	}
	if g.Width != 1024 || g.Height != 1024 {
		t.Fatalf("宽高未按画幅落库: %dx%d", g.Width, g.Height)
	}
}

func TestGenerateFailureRefundsCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "gen-fail@example.com", "secret12345")
	uid := grantTo(t, db, "gen-fail@example.com", 5)

	w := postGenerate(r, token, `{"prompt":"please fail","model":"flux-2-max","aspectRatio":"1:1"}`)
	// 上游失败是**业务失败**不是传输失败，HTTP 仍然 200。
	if w.Code != http.StatusOK {
		t.Fatalf("got %d; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["status"] != "failed" {
		t.Fatalf("应当是 failed: %+v", out)
	}
	// creditsSpent 必须是 0——次数已退回，记成 1 会让用户对不上账。
	if out["creditsSpent"] != float64(0) {
		t.Fatalf("失败时 creditsSpent 必须为 0: %+v", out)
	}

	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 5 {
		t.Fatalf("失败应当退回，余额仍为 5: got %d", bal.MonthlyCredits)
	}
	var refunds int64
	db.Model(&model.CreditTransaction{}).Where("type = ?", model.TxGenerationRefund).Count(&refunds)
	if refunds != 1 {
		t.Fatalf("应当恰好一条退款流水: got %d", refunds)
	}
}

func TestGenerateInsufficientCreditsReturns402(t *testing.T) {
	r, _ := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "gen-broke@example.com", "secret12345")

	w := postGenerate(r, token, `{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("余额不足应当 402: got %d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "40001") {
		t.Fatalf("应当返回 40001: %s", w.Body.String())
	}
}

func TestGenerateUnknownModelReturns400(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "gen-badmodel@example.com", "secret12345")
	grantTo(t, db, "gen-badmodel@example.com", 5)

	w := postGenerate(r, token, `{"prompt":"quick cat","model":"nope","aspectRatio":"1:1"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知模型应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestGenerateUnsupportedAspectRatioReturns400(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "gen-badratio@example.com", "secret12345")
	grantTo(t, db, "gen-badratio@example.com", 5)

	// 不支持的画幅必须报错，不能静默纠正成 1:1——那样用户拿到的是另一个比例的
	// 图，却没有任何地方提示出了问题。
	w := postGenerate(r, token, `{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"4:3"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("不支持的画幅应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestGenerateInsufficientCreditsLeavesNoProcessingRow(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "gen-noproc@example.com", "secret12345")

	postGenerate(r, token, `{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1"}`)

	// 扣费失败时那行必须被标成 failed，不能留在 processing——否则每次余额不足
	// 都在库里攒一行，运维看到一堆 processing 会以为系统卡住，启动扫描也会反复
	// 扫到它们。
	var stuck int64
	db.Model(&model.Generation{}).Where("status = ?", model.GenStatusProcessing).Count(&stuck)
	if stuck != 0 {
		t.Fatalf("不该留下 processing 行: got %d", stuck)
	}
}

// I1：handler 必须把 image_models.upstream_model 传进 adapter。
//
// 这条要靠注入一个能记下入参的 stub 才成立：只看响应的话，"漏传上游模型名"和"画幅
// 译错"都完全隐形——上游是假的，照样返回成功。而漏传的真实后果是请求被提交到错误的
// 上游模型（用户按 pro 付费拿到 max 的结果）。
func TestGeneratePassesUpstreamModelAndDimensions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	stub := generation.NewStubAdapter()
	cfg := &config.Config{JWTSecret: "test-secret"}
	r := NewRouterWithAdapters(db, cfg, generation.Registry{"flux": stub})

	token := registerAndLogin(t, r, "gen-passthrough@example.com", "secret12345")
	grantTo(t, db, "gen-passthrough@example.com", 5)

	w := postGenerate(r, token, `{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"16:9"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}

	got, ok := stub.LastRequest()
	if !ok {
		t.Fatal("adapter 没有收到请求")
	}
	if got.UpstreamModel != "flux-2-max" {
		t.Fatalf("未透传 upstream_model: %q", got.UpstreamModel)
	}
	if got.Width != 1344 || got.Height != 768 {
		t.Fatalf("16:9 应当译成 1344x768，实际 %dx%d", got.Width, got.Height)
	}
	if got.Prompt != "quick cat" {
		t.Fatalf("prompt 未透传: %q", got.Prompt)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./internal/server/ -run TestGenerate
```

期望：404（路由不存在）。

- [ ] **Step 3: 实现编排**

`internal/handler/generations.go`：

```go
package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"image-backend/internal/credit"
	"image-backend/internal/generation"
	"image-backend/internal/middleware"
	"image-backend/internal/model"
)

const (
	errCodeBadRequest          = 40000
	errCodeInsufficientCredits = 40001
	errCodeModelUnavailable    = 40003
	errCodeInternal            = 50000
)

// upstreamTimeout 覆盖最慢模型（Flux 实测 21 秒，慢时更久）并留余量。
const upstreamTimeout = 5 * time.Minute

type GenerationsHandler struct {
	DB       *gorm.DB
	Adapters generation.Registry
}

type generateRequest struct {
	Prompt      string `json:"prompt" binding:"required"`
	Model       string `json:"model" binding:"required"`
	AspectRatio string `json:"aspectRatio" binding:"required"`
	IsPublic    bool   `json:"isPublic"`
}

// Create 同步生成一张图。
//
// 编排顺序**不能换**：建 processing 行 → 扣费 → 调上游。
//
// 反过来（先扣费再建行）有一个无法补救的窗口：扣费成功后、建行之前进程崩溃，
// 流水里留下一条扣费记录但没有任何 generations 行指向它。启动兜底扫描靠扫
// processing 行找回退款，找不到行就永远退不回来——用户的钱凭空消失且无人知晓。
func (h *GenerationsHandler) Create(c *gin.Context) {
	userID := c.GetUint(middleware.CtxUserIDKey)

	var req generateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "invalid request body"})
		return
	}

	width, height, ok := generation.Dimensions(req.AspectRatio)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "unsupported aspect ratio"})
		return
	}

	var m model.ImageModel
	if err := h.DB.Where("id = ?", req.Model).First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "unknown model"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	if !m.Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeModelUnavailable, "message": "model is not available"})
		return
	}

	adapter, err := h.Adapters.Get(m.Provider)
	if err != nil {
		// 配置错误（表里有这个 provider 但没注册 adapter），不是用户的问题。
		log.Printf("[generations] provider %q 没有注册 adapter: %v", m.Provider, err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	gen := model.Generation{
		ID:          uuid.NewString(),
		UserID:      userID,
		Model:       m.ID,
		Prompt:      req.Prompt,
		AspectRatio: req.AspectRatio,
		Width:       width,
		Height:      height,
		Status:      model.GenStatusProcessing,
		IsPublic:    req.IsPublic,
	}
	if err := h.DB.Create(&gen).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	if _, err := credit.Spend(h.DB, userID, m.Credits, gen.ID); err != nil {
		// 扣费失败要把行标成 failed，否则它会一直挂在 processing——既让运维误以为
		// 系统卡住，也会被每次启动的兜底扫描反复扫到。
		h.markFailed(&gen, "insufficient credits")
		if errors.Is(err, credit.ErrInsufficientCredits) {
			c.JSON(http.StatusPaymentRequired,
				gin.H{"code": errCodeInsufficientCredits, "message": "not enough credits"})
			return
		}
		log.Printf("[generations] 扣费异常 gen=%s: %v", gen.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	// 刻意**不**继承 c.Request.Context()：客户端断开不应该取消一次已经付过费的
	// 生成。服务端必须把活干完并落库，用户回来能在历史里找到。Flux 实测 21 秒，
	// "中途关标签页"是常见情况而非边缘情况。
	upstreamCtx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer cancel()

	started := time.Now()
	res, genErr := adapter.Generate(upstreamCtx, generation.GenerateRequest{
		Prompt: req.Prompt, Width: width, Height: height,
		// 上游模型名必须按行传：Registry 只按 provider 索引，adapter 实例是共享的。
		// 焊死在实例上的话，表里第二行同 provider 的模型会被静默提交到前一行的上游
		// 模型——用户按 pro 付费拿到 max 的结果，没有任何地方报错。
		UpstreamModel: m.UpstreamModel,
	})
	elapsed := time.Since(started).Milliseconds()

	if genErr != nil {
		log.Printf("[generations] 上游失败 gen=%s user=%d: %v", gen.ID, userID, genErr)
		// **刻意对任何非 nil 错误退款**，而不是只对 errors.Is(genErr, ErrUpstream)
		// 退款。adapter 的契约要求所有用户可见的失败都包 ErrUpstream，但这里不依赖
		// 那个契约被正确实现：宽松兜底在分类出错时也不会坑用户，收紧成"只退
		// ErrUpstream"则会在某个 adapter 漏包一次时静默吞掉用户的次数。
		if err := credit.Refund(h.DB, gen.ID); err != nil {
			// 退款失败是严重问题：用户付了钱没拿到图也没拿回次数。必须留痕，
			// 启动兜底扫描会再试一次。
			log.Printf("[generations] 退款失败 gen=%s: %v", gen.ID, err)
		}
		gen.Status = model.GenStatusFailed
		gen.Error = genErr.Error()
		gen.CreditsSpent = 0
		gen.DurationMs = elapsed
		h.save(&gen)
		c.JSON(http.StatusOK, toGenerationResponse(gen))
		return
	}

	gen.Status = model.GenStatusSucceeded
	gen.ImageURL = res.ImageURL
	gen.UpstreamID = res.UpstreamID
	gen.UpstreamCost = res.UpstreamCost
	gen.CreditsSpent = m.Credits
	gen.DurationMs = elapsed
	h.save(&gen)
	c.JSON(http.StatusOK, toGenerationResponse(gen))
}

func (h *GenerationsHandler) markFailed(gen *model.Generation, reason string) {
	gen.Status = model.GenStatusFailed
	gen.Error = reason
	gen.CreditsSpent = 0
	h.save(gen)
}

func (h *GenerationsHandler) save(gen *model.Generation) {
	if err := h.DB.Save(gen).Error; err != nil {
		// 落库失败不改变已经发生的事实（次数已扣/已退、图已生成），所以不改
		// 响应，只留痕。
		log.Printf("[generations] 落库失败 gen=%s: %v", gen.ID, err)
	}
}

// toGenerationResponse 的字段名与前端 image-front 的 Generation 判别联合一一对应。
// 改这里就要同步改 image-front/lib/generation-types.ts。
func toGenerationResponse(g model.Generation) gin.H {
	out := gin.H{
		"id":           g.ID,
		"model":        g.Model,
		"prompt":       g.Prompt,
		"aspectRatio":  g.AspectRatio,
		"isPublic":     g.IsPublic,
		"status":       g.Status,
		"creditsSpent": g.CreditsSpent,
		"createdAt":    g.CreatedAt.UTC().Format(time.RFC3339),
	}
	if g.Status == model.GenStatusSucceeded {
		out["imageUrl"] = g.ImageURL
	} else {
		out["error"] = g.Error
	}
	return out
}
```

- [ ] **Step 4: 注册路由与 adapter**

把 `NewRouter` 拆成两层，让 `main.go` 能拿到**同一个** Registry 去做启动校验（各建一个的话
校验的就不是真正在服务的那份）：

```go
func NewRouter(db *gorm.DB, cfg *config.Config) *gin.Engine {
	return NewRouterWithAdapters(db, cfg, BuildAdapters(cfg))
}

// NewRouterWithAdapters 让调用方自己提供 Registry。cmd/server/main.go 用它是为了能在
// 开始接流量之前，拿**同一个** Registry 跑 generation.ValidateProviders。
func NewRouterWithAdapters(db *gorm.DB, cfg *config.Config, adapters generation.Registry) *gin.Engine {
```

在 `authed` 组之后：

```go
	generationsHandler := &handler.GenerationsHandler{DB: db, Adapters: adapters}
	authed.POST("/generations", generationsHandler.Create)
```

文件末尾加：

```go
// buildFluxAdapter 在没有配置 key 时退化成 stub。
//
// 这不是"方便"，是必需：接真上游后端到端测试每跑一次都真调 Flux——每次约 21
// 秒、每次花钱。stub 保留 fail/slow/quick 关键词，让测试既快又免费。
func BuildAdapters(cfg *config.Config) generation.Registry {
	return generation.Registry{"flux": buildFluxAdapter(cfg)}
}

func buildFluxAdapter(cfg *config.Config) generation.Adapter {
	if cfg.FluxAPIKey == "" {
		log.Println("generation: FLUX_API_KEY 未配置，使用 stub adapter（返回占位图）")
		return generation.NewStubAdapter()
	}
	// 上游模型名不在这里传：它按请求从 image_models.upstream_model 取，这样同一个
	// provider 下的多个模型才不会被静默提交到同一个上游路径。
	return generation.NewFluxAdapter(cfg.EZLinkAIBaseURL, cfg.FluxAPIKey)
}
```

import 补 `"log"` 与 `"image-backend/internal/generation"`。

- [ ] **Step 5: 运行确认通过**

```bash
go test ./internal/server/ -run TestGenerate -v 2>&1 | grep -E "^(=== RUN|--- |ok|FAIL)"
```

期望：8 个测试全过。

- [ ] **Step 6: 提交**

```bash
git add internal/handler/generations.go internal/server
git commit -m "feat: 生成编排接口——先落行再扣费，失败按原拆分退回"
```

---

## Task 5: 启动兜底扫描

**Files:**
- Create: `internal/generation/sweep.go`
- Test: `internal/generation/sweep_test.go`
- Modify: `cmd/server/main.go`

- [ ] **Step 1: 写失败的测试**

`internal/generation/sweep_test.go`（`package generation_test`）：

```go
func TestSweepRefundsStuckProcessingRows(t *testing.T) {
	db, err := database.Open("")
	if err != nil {
		t.Fatal(err)
	}
	u := model.User{Email: "sweep@example.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	if err := credit.Grant(db, u.ID, 10, 0, "fixture"); err != nil {
		t.Fatal(err)
	}
	gen := model.Generation{
		ID: "stuck-1", UserID: u.ID, Model: "flux-2-max", Prompt: "p",
		AspectRatio: "1:1", Width: 1024, Height: 1024, Status: model.GenStatusProcessing,
	}
	if err := db.Create(&gen).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := credit.Spend(db, u.ID, 3, gen.ID); err != nil {
		t.Fatal(err)
	}

	n, err := generation.SweepStuck(db)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("应当回收 1 行: got %d", n)
	}
	bal, _ := credit.Balance(db, u.ID)
	if bal.MonthlyCredits != 10 {
		t.Fatalf("次数应当退回: got %d, want 10", bal.MonthlyCredits)
	}
	var after model.Generation
	_ = db.Where("id = ?", gen.ID).First(&after).Error
	if after.Status != model.GenStatusFailed {
		t.Fatalf("行应当标成 failed: got %s", after.Status)
	}
}

func TestSweepIsIdempotent(t *testing.T) {
	db, _ := database.Open("")
	u := model.User{Email: "sweep2@example.com", PasswordHash: "x"}
	_ = db.Create(&u).Error
	_ = credit.Grant(db, u.ID, 10, 0, "fixture")
	gen := model.Generation{
		ID: "stuck-2", UserID: u.ID, Model: "flux-2-max", Prompt: "p",
		AspectRatio: "1:1", Width: 1024, Height: 1024, Status: model.GenStatusProcessing,
	}
	_ = db.Create(&gen).Error
	_, _ = credit.Spend(db, u.ID, 3, gen.ID)

	// 每次重启都会跑，所以重跑必须安全。
	if _, err := generation.SweepStuck(db); err != nil {
		t.Fatal(err)
	}
	if n, err := generation.SweepStuck(db); err != nil || n != 0 {
		t.Fatalf("第二次不该再回收: n=%d err=%v", n, err)
	}
	bal, _ := credit.Balance(db, u.ID)
	if bal.MonthlyCredits != 10 {
		t.Fatalf("重复扫描导致多退: got %d, want 10", bal.MonthlyCredits)
	}
}

func TestSweepLeavesTerminalRowsAlone(t *testing.T) {
	db, _ := database.Open("")
	u := model.User{Email: "sweep3@example.com", PasswordHash: "x"}
	_ = db.Create(&u).Error
	_ = credit.Grant(db, u.ID, 10, 0, "fixture")
	done := model.Generation{
		ID: "done-1", UserID: u.ID, Model: "flux-2-max", Prompt: "p",
		AspectRatio: "1:1", Width: 1024, Height: 1024,
		Status: model.GenStatusSucceeded, CreditsSpent: 1,
	}
	_ = db.Create(&done).Error
	_, _ = credit.Spend(db, u.ID, 1, done.ID)

	n, err := generation.SweepStuck(db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("已终态的行不该被回收: got %d", n)
	}
	bal, _ := credit.Balance(db, u.ID)
	if bal.MonthlyCredits != 9 {
		t.Fatalf("成功的生成不该被退款: got %d, want 9", bal.MonthlyCredits)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./internal/generation/ -run TestSweep
```

期望：`undefined: generation.SweepStuck`。

- [ ] **Step 3: 实现**

`internal/generation/sweep.go`：

```go
package generation

import (
	"log"

	"gorm.io/gorm"

	"image-backend/internal/credit"
	"image-backend/internal/model"
)

// SweepStuck 回收卡在 processing 的生成任务，返回回收数量。
//
// 何时会有卡住的行：生成是同步的，服务端要挂住连接直到上游出图（Flux 实测 21
// 秒）。这期间进程崩溃或部署重启，那一行就永远停在 processing——次数扣了、图
// 没有、没有任何人会来收拾。异步方案有 worker 兜底，同步方案没有，所以这个扫描
// 不是可选项。
//
// **必须在开始接收流量之前调用。** 那时候任何 processing 行按定义都是上一个进程
// 遗留的孤儿；服务跑起来之后再扫，会把当前正在进行的生成误判成孤儿并退款。
//
// 幂等由 credit.Refund 保证（(generation_id, type) 唯一索引），每次重启都跑是安全的。
func SweepStuck(db *gorm.DB) (int, error) {
	var stuck []model.Generation
	if err := db.Where("status = ?", model.GenStatusProcessing).Find(&stuck).Error; err != nil {
		return 0, err
	}
	for _, g := range stuck {
		if err := credit.Refund(db, g.ID); err != nil {
			// 单行失败不中断整体——剩下的孤儿更值得被回收。留痕即可，下次重启还会再试。
			log.Printf("[sweep] 退款失败 gen=%s: %v", g.ID, err)
			continue
		}
		if err := db.Model(&model.Generation{}).Where("id = ?", g.ID).
			Updates(map[string]any{
				"status": model.GenStatusFailed,
				"error":  "服务重启时该任务仍在进行中，次数已退回",
			}).Error; err != nil {
			log.Printf("[sweep] 标记失败 gen=%s: %v", g.ID, err)
		}
	}
	if len(stuck) > 0 {
		log.Printf("[sweep] 回收了 %d 个卡住的生成任务", len(stuck))
	}
	return len(stuck), nil
}
```

- [ ] **Step 4: 在启动流程里调用**

修改 `cmd/server/main.go`，在数据库打开之后、启动 HTTP 服务**之前**：

```go
	if _, err := generation.SweepStuck(db); err != nil {
		log.Printf("启动兜底扫描失败（继续启动）: %v", err)
	}
	// provider 拼错一个字母的话，不该等第一个选中该模型的用户以 500 的形式替我们发现。
	// 不阻止启动：其他模型仍然可用，拒绝启动是更坏的结果。
	adapters := server.BuildAdapters(cfg)
	for _, p := range generation.ValidateProviders(db, adapters) {
		log.Printf("启动校验: %s", p)
	}
	r := server.NewRouterWithAdapters(db, cfg, adapters)
```

扫描失败不阻止启动——回收不了孤儿是坏事，但拒绝启动是更坏的事。provider 校验同理：
报出来，但让能用的模型继续服务。

- [ ] **Step 5: 运行确认通过**

```bash
go test ./internal/generation/ -v 2>&1 | grep -E "^(--- |ok|FAIL)"
```

- [ ] **Step 6: 提交**

```bash
git add internal/generation/sweep.go internal/generation/sweep_test.go cmd/server/main.go
git commit -m "feat: 启动兜底扫描回收卡住的生成并退款"
```

---

## Task 6: 封禁用户拦截

**Files:**
- Create: `internal/middleware/active.go`
- Modify: `internal/server/router.go`
- Test: `internal/server/active_test.go`

M1 遗留问题：`users.status` 是个**没有任何人读的字段**——封禁只是往库里写了个值，被封的用户照常使用一切功能，被封的管理员照常发次数。M3a 加了管理员接口之后，这个缺口的后果变严重了。

- [ ] **Step 1: 写失败的测试**

`internal/server/active_test.go`（`package server`）：

```go
func TestBannedUserIsRejected(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "banned@example.com", "secret12345")

	if err := db.Model(&model.User{}).Where("email = ?", "banned@example.com").
		Update("status", "banned").Error; err != nil {
		t.Fatalf("封禁: %v", err)
	}

	// 封禁必须**立即**生效，不能等 JWT 过期（7 天）。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("被封禁用户应当 403: got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestActiveUserPassesThrough(t *testing.T) {
	r := setupRouter(t)
	token := registerAndLogin(t, r, "active@example.com", "secret12345")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("正常用户应当放行: got %d; body=%s", w.Code, w.Body.String())
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./internal/server/ -run TestBannedUser
```

期望：FAIL，得到 200 而非 403。

- [ ] **Step 3: 实现**

`internal/middleware/active.go`：

```go
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
```

- [ ] **Step 4: 挂到 authed 组**

```go
	authed := api.Group("", middleware.Auth(cfg.JWTSecret), middleware.RequireActiveUser(db))
```

- [ ] **Step 5: 全量验证**

```bash
go build ./... && go vet ./... && go test ./... 2>&1 | grep -E "ok|FAIL"
```

期望：全绿。注意这给每个受保护请求增加了一次查库——可接受，`/me` 与 `RequireAdmin` 本来就要查。

- [ ] **Step 6: 提交**

```bash
git add internal/middleware/active.go internal/server
git commit -m "fix: 拦截被封禁用户——status 此前是没人读的字段"
```

---

## Task 7: 前端切换到真实后端

**Files（均在 `~/Desktop/image-front`）:**
- Modify: `lib/backend.ts`、`app/api/{models,credits,generations}/route.ts`
- Delete: `lib/fixtures.ts`、`app/api/credits/reset/`
- Modify: `app/[locale]/generate/page.tsx`、`components/credit-badge.tsx`、`app/api/plans/route.ts`
- Modify: `e2e/global-setup.ts`

**这是 M2 选择 Route Handler 假数据方案的兑现时刻**：组件、类型、端到端测试的断言都不该改，只改数据来源。**如果你发现自己在改组件，停下来想想是不是走错了。**

- [ ] **Step 1: `lib/backend.ts` 增加函数**

先读该文件，沿用既有 `Result<T>` 与 `request<T>` 写法，新增 `listModels(token)` 与 `createGeneration(token, body)`；并把 `CurrentUser` 类型扩展出 `credits: CreditBalance`（后端已把余额并进 `/me`，不需要单独的余额接口）。

- [ ] **Step 2: 改三个 Route Handler**

每个都：`getToken()` 取 cookie，无则 401；调 `lib/backend.ts`；失败走 `toClientError`。`POST /api/generations` 仍要**先过 `checkSameOrigin`**。

`app/api/credits/route.ts` 改成调 `fetchMe` 后只返回 `credits` 字段——保持前端契约不变，工作台与徽标都不用改。

- [ ] **Step 3: 删除假数据层**

```bash
rm lib/fixtures.ts && rm -r app/api/credits/reset
```

修所有引用：`app/[locale]/generate/page.tsx` 改为服务端调 `listModels` + `fetchMe`；`components/credit-badge.tsx` 改为服务端调 `fetchMe`；**套餐仍是假数据**（Stripe 未接），把 `PLANS`/`ADDON_PACKS` 就地内联进 `app/api/plans/route.ts` 并注明这是唯一残留的假数据。

- [ ] **Step 4: 改 e2e 前置准备**

`e2e/global-setup.ts` 原先调 `/api/credits/reset`（已删除）。改为通过后端 `POST /api/v1/admin/credits` 给测试账号发次数。管理员 token 由环境变量提供；**缺失时让 setup 显式失败并打印获取方法，不要静默跳过**——静默跳过会让后续测试以"余额不足"的形式失败，指向错误的方向。

- [ ] **Step 5: 验证**

后端以 stub 模式启动（不配 `FLUX_API_KEY`），然后：

```bash
npm run lint && npx tsc --noEmit && npm test && npm run build && npx playwright test
```

期望：全绿。`grep -rn "fixtures" --include="*.ts*" .` 应当只剩注释里的历史说明。

- [ ] **Step 6: 提交（image-front 仓库）**

```bash
git add -A && git commit -m "feat: 切换到真实后端，删除假数据层"
```

---

## Task 8: 文档与真实上游联调

**Files:** 两个仓库的 `README.md`、后端 `.env.example`

- [ ] **Step 1: 后端 README 增加"生成链路"一节**

必须写清楚：编排顺序及其原因、脱离请求的 context、启动兜底扫描、stub 模式，以及两个上游反直觉点（`polling_url` 装的是最终图片 URL、两端点认证头不同）。

- [ ] **Step 2: `.env.example` 补充**

```
# 上游网关。FLUX_API_KEY 留空时使用 stub adapter（返回占位图，保留
# fail/slow/quick 关键词），这样端到端测试既快又不花钱。
EZLINKAI_BASE_URL=https://api.ezlinkai.com
FLUX_API_KEY=
```

- [ ] **Step 3: 真实上游手工联调**

配上真实 key 启动后端，逐条做并记录真实输出：

1. 真实生成一次 → 拿到 `replicate.delivery` 图片 URL，浏览器能打开
2. 余额正确减 1，`credit_transactions` 有一条 `generation_cost`
3. `generations` 行的 `upstream_id`、`upstream_cost`、`duration_ms` 都落上了
4. **验证脱离请求的 context**：发起生成后立刻断开客户端（`curl --max-time 3`），等 30 秒后查库——该行应当是 `succeeded` 且有图片 URL

第 4 条是本轮最容易被跳过、也最容易在生产上出事的验证：它证明用户关掉标签页不会导致"扣了钱丢了图"。

- [ ] **Step 4: 全量验证并提交**

```bash
go build ./... && go vet ./... && go test ./...
```

---

## 不在本计划范围内

- R2 图片转存（ezlinkai 侧后续完善）——**因此生成的图约 1 小时后变死链**
- Stripe 订阅与加量包（`/pricing` 按钮仍禁用）
- 第二个模型、参考图上传（`input_image`）
- `/history`、`/gallery`、管理后台 UI
- 登录与生成接口的速率限制

## 已知缺口

- **Postgres 关口仍未通过**（M3a 遗留）：`Refund` 的唯一键冲突回滚分支至今未执行过
- 生成接口无速率限制——一个用户可以并发打满上游配额
- `SweepStuck` 在**多实例部署下会互相踩**：每个实例启动都扫全表，可能回收另一实例正在进行的生成。单实例下没问题，扩容前必须加实例标识或分布式锁
- **`get_result` 兜底路径至今没对着真实上游跑过**：实测中提交总是直接返回 `Ready`，兜底分支只在 `httptest.Server` 下验证过。终态状态集合（`Error` / `Content Moderated` / `Request Moderated` / `Task not found`）来自上游文档而非实测，上游若新增终态，表现为一次走到 ctx 到期的超时（日志会打印"出现未识别状态"）
- **`Seed` 全链路未接通**：`GenerateRequest.Seed` 与 adapter 侧都支持，但 `POST /api/v1/generations` 的请求体没有 seed 字段，handler 始终传 nil。要暴露给用户需要同时改前端契约
- `image-front` 的 `e2e/generate.spec.ts` 注释仍写着"fail 路径 8 秒"——stub 是 800 毫秒。属前端仓库，随 Task 7 一并修
- `http.Transport` 连接池未调优（用的是默认 client）
- `generations` 缺 `(user_id, created_at)` 复合索引——`/history` 落地前不痛
- prompt 无长度上限：超长 prompt 会原样发给上游并原样落库
