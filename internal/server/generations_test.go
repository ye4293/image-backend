package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/credit"
	"image-backend/internal/model"
)

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
