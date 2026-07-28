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
)

// ErrUpstream 上游生成失败（模型拒绝、超时、返回错误状态）。调用方据此退款并把
// generation 标成 failed。与"我们自己写错了"区分开：后者应当是 500。
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
	return a, nil
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

func TestStubRespectsContextCancellation(t *testing.T) {
	// stub 的默认延迟是 15 秒；ctx 取消时必须立刻返回，否则测试套件会被拖死。
	s := NewStubAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.Generate(ctx, GenerateRequest{Prompt: "a normal prompt", Width: 1, Height: 1})
	if err == nil {
		t.Fatal("ctx 超时应当返回错误")
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
type StubAdapter struct{}

func NewStubAdapter() *StubAdapter { return &StubAdapter{} }

func (s *StubAdapter) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	delay, fail := stubBehavior(req.Prompt)

	// 用 select 而不是 time.Sleep：ctx 取消时要立刻返回，否则默认 15 秒的延迟
	// 会把测试套件拖死，而且也不符合真实 adapter 该有的行为。
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return GenerateResult{}, ctx.Err()
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

期望：7 个测试全过。

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
	"testing"
)

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

	a := NewFluxAdapter(srv.URL, "test-key", "flux-2-max")
	seed := 42
	res, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "a cat", Width: 1024, Height: 1024, Seed: &seed,
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

	a := NewFluxAdapter(srv.URL, "k", "flux-2-max")
	if _, err := a.Generate(context.Background(), GenerateRequest{Prompt: "p", Width: 1, Height: 1}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, present := gotBody["seed"]; present {
		t.Fatalf("Seed 为 nil 时不该出现在请求体里: %+v", gotBody)
	}
}

func TestFluxFallsBackToGetResultWhenNotReady(t *testing.T) {
	var getResultAuth, getResultKey, getResultID string
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			getResultAuth = r.Header.Get("Authorization")
			getResultKey = r.Header.Get("x-key")
			getResultID = r.URL.Query().Get("id")
			_, _ = w.Write([]byte(`{"id":"abc","result":{"sample":"https://cdn.example/late.png"},"status":"Ready"}`))
			return
		}
		calls++
		// 提交返回未就绪，且没有给出图片 URL——必须走兜底查询。
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	a := NewFluxAdapter(srv.URL, "test-key", "flux-2-max")
	res, err := a.Generate(context.Background(), GenerateRequest{Prompt: "p", Width: 1, Height: 1})
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
}

func TestFluxUpstreamErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"prompt rejected by safety filter"}`))
	}))
	defer srv.Close()

	a := NewFluxAdapter(srv.URL, "k", "flux-2-max")
	_, err := a.Generate(context.Background(), GenerateRequest{Prompt: "p", Width: 1, Height: 1})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("上游 4xx 应当归一成 ErrUpstream: %v", err)
	}
}

func TestFluxHonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := NewFluxAdapter(srv.URL, "k", "flux-2-max")
	if _, err := a.Generate(ctx, GenerateRequest{Prompt: "p", Width: 1, Height: 1}); err == nil {
		t.Fatal("已取消的 ctx 应当立即返回错误")
	}
}
```

- [ ] **Step 3: 运行确认失败**

```bash
go test ./internal/generation/ -run TestFlux
```

期望：`undefined: NewFluxAdapter`。

- [ ] **Step 4: 实现**

`internal/generation/flux.go`：

```go
package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FluxAdapter 对接 ezlinkai 的 Flux 端点。
//
// 上游有两处反直觉的地方，都是 2026-07-28 实测所得，不要"顺手统一"掉：
//
//  1. **提交响应里的 `polling_url` 装的是最终图片 URL**，不是给你去轮询的地址。
//     ezlinkai 在内部替我们轮询了 BFL，挂住连接直到出图（实测约 21 秒）。
//  2. **两个端点的认证头不一样**：提交用 `x-key`，`get_result` 用
//     `Authorization: Bearer`。改成统一会 401。
type FluxAdapter struct {
	baseURL       string
	apiKey        string
	upstreamModel string
	client        *http.Client
}

func NewFluxAdapter(baseURL, apiKey, upstreamModel string) *FluxAdapter {
	return &FluxAdapter{
		baseURL:       strings.TrimRight(baseURL, "/"),
		apiKey:        apiKey,
		upstreamModel: upstreamModel,
		// 不设 Timeout：超时由调用方通过 ctx 控制，那样才能和"脱离请求的
		// context"配合。这里再设一个会变成两个互相打架的期限。
		client: &http.Client{},
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

const fluxStatusReady = "Ready"

func (a *FluxAdapter) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	body := map[string]any{
		"prompt":            req.Prompt,
		"width":             req.Width,
		"height":            req.Height,
		"output_format":     "jpeg",
		"safety_tolerance":  2,
	}
	// Seed 为 nil 时**不能**塞 0 进去——0 是一个合法的 seed，会让"不指定"变成
	// "每次都用同一个 seed"，用户会发现同样的 prompt 永远出同一张图。
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}

	sub, err := a.submit(ctx, body)
	if err != nil {
		return GenerateResult{}, err
	}

	if sub.Status == fluxStatusReady && sub.PollingURL != "" {
		return GenerateResult{
			ImageURL:     sub.PollingURL,
			UpstreamID:   sub.ID,
			UpstreamCost: sub.Cost,
		}, nil
	}

	// 未就绪：走兜底查询。这条路径在实测中没出现过（提交总是直接返回 Ready），
	// 但一旦 ezlinkai 内部超时先返回，没有它就是扣了次数拿不到图且无从补救。
	url, err := a.getResult(ctx, sub.ID)
	if err != nil {
		return GenerateResult{}, err
	}
	return GenerateResult{ImageURL: url, UpstreamID: sub.ID, UpstreamCost: sub.Cost}, nil
}

func (a *FluxAdapter) submit(ctx context.Context, body map[string]any) (fluxSubmitResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return fluxSubmitResponse{}, err
	}
	endpoint := fmt.Sprintf("%s/flux/v1/%s", a.baseURL, a.upstreamModel)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fluxSubmitResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-key", a.apiKey) // 提交端点用 x-key（见类型注释）

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return fluxSubmitResponse{}, fmt.Errorf("%w: 提交请求失败: %v", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fluxSubmitResponse{}, fmt.Errorf("%w: 读取响应失败: %v", ErrUpstream, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fluxSubmitResponse{}, fmt.Errorf("%w: 提交返回 %d: %s",
			ErrUpstream, resp.StatusCode, truncate(string(payload), 300))
	}
	var out fluxSubmitResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return fluxSubmitResponse{}, fmt.Errorf("%w: 响应不是合法 JSON: %s",
			ErrUpstream, truncate(string(payload), 300))
	}
	return out, nil
}

// getResult 轮询兜底端点直到就绪或 ctx 到期。
func (a *FluxAdapter) getResult(ctx context.Context, id string) (string, error) {
	endpoint := fmt.Sprintf("%s/flux/v1/get_result?id=%s", a.baseURL, id)
	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return "", err
		}
		// 兜底端点用 Bearer，与提交端点相反（见类型注释）。
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)

		resp, err := a.client.Do(httpReq)
		if err != nil {
			return "", fmt.Errorf("%w: 兜底查询失败: %v", ErrUpstream, err)
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return "", fmt.Errorf("%w: 读取兜底响应失败: %v", ErrUpstream, readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("%w: 兜底查询返回 %d: %s",
				ErrUpstream, resp.StatusCode, truncate(string(payload), 300))
		}
		var out fluxResultResponse
		if err := json.Unmarshal(payload, &out); err != nil {
			return "", fmt.Errorf("%w: 兜底响应不是合法 JSON: %s",
				ErrUpstream, truncate(string(payload), 300))
		}
		if out.Status == fluxStatusReady && out.Result.Sample != "" {
			return out.Result.Sample, nil
		}

		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
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

期望：12 个测试全过（3 画幅 + 4 stub + 5 flux）。

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
	})
	elapsed := time.Since(started).Milliseconds()

	if genErr != nil {
		log.Printf("[generations] 上游失败 gen=%s user=%d: %v", gen.ID, userID, genErr)
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

在 `internal/server/router.go` 的 `authed` 组之后：

```go
	adapters := generation.Registry{"flux": buildFluxAdapter(cfg)}
	generationsHandler := &handler.GenerationsHandler{DB: db, Adapters: adapters}
	authed.POST("/generations", generationsHandler.Create)
```

文件末尾加：

```go
// buildFluxAdapter 在没有配置 key 时退化成 stub。
//
// 这不是"方便"，是必需：接真上游后端到端测试每跑一次都真调 Flux——每次约 21
// 秒、每次花钱。stub 保留 fail/slow/quick 关键词，让测试既快又免费。
func buildFluxAdapter(cfg *config.Config) generation.Adapter {
	if cfg.FluxAPIKey == "" {
		log.Println("generation: FLUX_API_KEY 未配置，使用 stub adapter（返回占位图）")
		return generation.NewStubAdapter()
	}
	return generation.NewFluxAdapter(cfg.EZLinkAIBaseURL, cfg.FluxAPIKey, "flux-2-max")
}
```

import 补 `"log"` 与 `"image-backend/internal/generation"`。

- [ ] **Step 5: 运行确认通过**

```bash
go test ./internal/server/ -run TestGenerate -v 2>&1 | grep -E "^(=== RUN|--- |ok|FAIL)"
```

期望：7 个测试全过。

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
```

扫描失败不阻止启动——回收不了孤儿是坏事，但拒绝启动是更坏的事。

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
