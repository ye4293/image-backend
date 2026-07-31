package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/config"
	"image-backend/internal/credit"
	"image-backend/internal/database"
	"image-backend/internal/generation"
	"image-backend/internal/model"
)

// modelCredits 读某个模型实际扣多少 credits。
//
// 测试里的发放量必须按它算，**不能写死**：credits 是运营可调的产品参数
// （见 PATCH /admin/models/:id），写死 5 意味着任何一次调价都会让一批
// 与计价无关的测试因"余额不足"变红，而真正的信号被埋在噪音里。
func modelCredits(t *testing.T, db *gorm.DB, id string) int {
	t.Helper()
	var m model.ImageModel
	if err := db.Where("id = ?", id).First(&m).Error; err != nil {
		t.Fatalf("读模型 %s: %v", id, err)
	}
	return m.Credits
}

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
	uid := grantTo(t, db, "gen-ok@example.com", 5*modelCredits(t, db, "flux-2-max"))

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
	cost := modelCredits(t, db, "flux-2-max")
	if out["creditsSpent"] != float64(cost) {
		t.Fatalf("应当扣 %d 次（模型配置值，不是写死的 1）: %+v", cost, out)
	}
	if out["isPublic"] != true {
		t.Fatalf("isPublic 应当回传: %+v", out)
	}

	bal, _ := credit.Balance(db, uid)
	if want := 5*cost - cost; bal.MonthlyCredits != want {
		t.Fatalf("余额应当从 %d 减到 %d: got %d", 5*cost, want, bal.MonthlyCredits)
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
	uid := grantTo(t, db, "gen-fail@example.com", 5*modelCredits(t, db, "flux-2-max"))

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

	// 期望值按发放量算（发放量本身是 5×模型 credits），不写死数字。
	want := 5 * modelCredits(t, db, "flux-2-max")
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != want {
		t.Fatalf("失败应当全额退回，余额仍应为 %d: got %d", want, bal.MonthlyCredits)
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
	grantTo(t, db, "gen-badmodel@example.com", 5*modelCredits(t, db, "flux-2-max"))

	w := postGenerate(r, token, `{"prompt":"quick cat","model":"nope","aspectRatio":"1:1"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知模型应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestGenerateUnsupportedAspectRatioReturns400(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "gen-badratio@example.com", "secret12345")
	grantTo(t, db, "gen-badratio@example.com", 5*modelCredits(t, db, "flux-2-max"))

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
	grantTo(t, db, "gen-passthrough@example.com", 5*modelCredits(t, db, "flux-2-max"))

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

// —— 转存端到端用的本包测试替身 ——
//
// 刻意**不**复用 internal/generation 里的 fakeInner/fakeStore：那两个是那个包的
// 私有类型，跨包用不了；名字带 storedTest 前缀是为了让人一眼看出这是 server 包
// 自己的替身，别去 generation 包里找。

// storedTestPNG 最小合法 PNG 头，足够让 http.DetectContentType 认出 image/png。
// 装饰器嗅探内容而不信 Content-Type，所以这几个字节是必需的。
var storedTestPNG = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 32))

// storedTestAdapter 假 inner adapter：返回一个真实可下载的 http:// URL 当作
// 上游临时链接。必须是 http(s)，否则装饰器会按"相对路径"直接跳过转存。
type storedTestAdapter struct{ imageURL string }

func (a storedTestAdapter) Generate(ctx context.Context, req generation.GenerateRequest) (generation.GenerateResult, error) {
	return generation.GenerateResult{ImageURL: a.imageURL, UpstreamID: "upstream-1"}, nil
}

// storedTestStorage 假 Storage：把 key 拼成固定前缀的永久 URL。
type storedTestStorage struct{ base string }

func (s storedTestStorage) Put(ctx context.Context, key, contentType string, body []byte) (string, error) {
	return s.base + key, nil
}

// TestGenerateStoresImageAndReportsStoredTrue 覆盖 handler → 装饰器 → DB → 响应
// 这条 stored=true 的完整链路。
//
// 在它之前，删掉 handler 里的 `gen.Stored = res.Stored` 一行**全部测试照样绿**：
// storing_test.go 只验装饰器自己返回 true（到不了 DB）；
// TestGenerateResponseIncludesStoredFlag 只断言字段存在且为 false，而 false 正是
// 零值——拷不拷贝那个字段结果一模一样；TestListIncludesStoredFlag 直接往库里插行，
// 完全绕过 handler 的写路径。
//
// 漏掉那一行的真实后果：图确实永久转存了、URL 也是永久的，但每条响应和每条历史
// 记录都说 stored=false，于是前端对**每一张**好图永远提示"链接可能已失效"。
func TestGenerateStoresImageAndReportsStoredTrue(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(storedTestPNG)
	}))
	defer upstream.Close()

	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	const permanentBase = "https://img.example.com/"
	adapter := generation.NewStoringAdapter(
		storedTestAdapter{imageURL: upstream.URL + "/tmp-upstream.png"},
		storedTestStorage{base: permanentBase},
	)
	cfg := &config.Config{JWTSecret: "test-secret"}
	r := NewRouterWithAdapters(db, cfg, generation.Registry{"flux": adapter})

	token := registerAndLogin(t, r, "gen-storedtrue@example.com", "secret12345")
	uid := grantTo(t, db, "gen-storedtrue@example.com", 5*modelCredits(t, db, "flux-2-max"))

	w := postGenerate(r, token, `{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v; body=%s", err, w.Body.String())
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("响应没有 id")
	}
	wantURL := permanentBase + "g/" + id + ".png"

	if out["stored"] != true {
		t.Errorf("响应 stored: got %v, want true——转存成功却报 false，"+
			"前端会对一条好链接永久提示已失效", out["stored"])
	}
	if out["imageUrl"] != wantURL {
		t.Errorf("响应 imageUrl: got %v, want %q（应当是永久 URL，不是上游临时链接）",
			out["imageUrl"], wantURL)
	}

	// 库里也要断言：响应和落库是两行不同的代码写的，将来改动可能只破坏一边。
	var g model.Generation
	if err := db.Where("user_id = ?", uid).First(&g).Error; err != nil {
		t.Fatalf("缺少 generations 行: %v", err)
	}
	if !g.Stored {
		t.Errorf("库里 stored: got false, want true——历史接口会把每张图都标成会失效")
	}
	if g.ImageURL != wantURL {
		t.Errorf("库里 image_url: got %q, want %q", g.ImageURL, wantURL)
	}
}

func TestGeneratePassesGenerationIDToAdapter(t *testing.T) {
	// 对象 key 由 GenerationID 推导。handler 漏传的话 key 会变成 g/.png——
	// 所有用户的所有图**互相覆盖**，而没有任何地方报错：上传成功、库里 stored=true、
	// 页面上也能看到图（就是最后那一张）。这是本轮最隐蔽的一个失败模式。
	//
	// **必须断言 adapter 真的收到了那个 id**，不能只断言库里的字段。StubAdapter
	// 的 LastRequest() 就是为这类断言存在的；配合 NewRouterWithAdapters 注入我们
	// 自己持有的 stub，就能看到 handler 到底传了什么。
	stub := generation.NewStubAdapter()
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{JWTSecret: "test-secret"}
	r := NewRouterWithAdapters(db, cfg, generation.Registry{"flux": stub})

	token := registerAndLogin(t, r, "gen-genid@example.com", "secret12345")
	grantTo(t, db, "gen-genid@example.com", 5*modelCredits(t, db, "flux-2-max"))

	w := postGenerate(r, token, `{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("响应没有 id")
	}

	got, ok := stub.LastRequest()
	if !ok {
		t.Fatal("adapter 没收到任何请求")
	}
	if got.GenerationID != id {
		t.Errorf("adapter 收到的 GenerationID = %q，期望响应里的 id %q——"+
			"为空则对象 key 退化成 g/.<ext>，所有图互相覆盖", got.GenerationID, id)
	}
}

// TestGenerateSpendsPerModelCredits 钉住"每个模型扣多少 credits 由库里的行说了算"。
//
// 这条测试存在的理由：在它之前，**所有**生成测试都用 seed 出来的 credits=1，
// 于是"某处硬编码了 1"这类 bug 会全绿通过。而 credits 是产品的计价货币——
// 贵模型多扣、便宜模型少扣，运营在后台调整——扣错就是直接的收入或口碑损失。
//
// 一并覆盖失败退款：退的必须是**实际扣的那个数**，退成 1 会让贵模型的用户
// 每失败一次就亏掉 6 次额度。
func TestGenerateSpendsPerModelCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "gen-cost@example.com", "secret12345")
	grantTo(t, db, "gen-cost@example.com", 30)

	// 造一个"贵"模型：同一个 provider、同一个上游模型，只有 credits 不同。
	expensive := model.ImageModel{
		ID: "pricey-model", DisplayName: "Pricey", Provider: "flux",
		UpstreamModel: "flux-2-max", Credits: 7, Enabled: true, SortOrder: 99,
	}
	if err := db.Create(&expensive).Error; err != nil {
		t.Fatalf("造模型: %v", err)
	}

	w := postGenerate(r, token, `{"prompt":"quick cat","model":"pricey-model","aspectRatio":"1:1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["creditsSpent"] != float64(7) {
		t.Errorf("应当按模型配置扣 7，响应说扣了 %v——credits 若被硬编码成 1，贵模型就白送", out["creditsSpent"])
	}
	var u model.User
	if err := db.Where("email = ?", "gen-cost@example.com").First(&u).Error; err != nil {
		t.Fatal(err)
	}
	bal, err := credit.Balance(db, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bal.MonthlyCredits != 23 {
		t.Errorf("30 - 7 应当剩 23，实际 %d", bal.MonthlyCredits)
	}

	// 失败时必须原额退回，而不是退 1。
	w2 := postGenerate(r, token, `{"prompt":"fail please","model":"pricey-model","aspectRatio":"1:1"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("上游失败是业务失败，仍应 200，得到 %d: %s", w2.Code, w2.Body.String())
	}
	bal2, err := credit.Balance(db, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bal2.MonthlyCredits != 23 {
		t.Errorf("失败应当把 7 全额退回、余额仍为 23，实际 %d——退成 1 的话贵模型每失败一次亏 6", bal2.MonthlyCredits)
	}
}
