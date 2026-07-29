package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"image-backend/internal/credit"
	"image-backend/internal/model"
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

func TestMeIncludesCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "credits@example.com", "secret12345")

	// 直接给该用户发放，验证 /me 能读到
	var u model.User
	if err := db.Where("email = ?", "credits@example.com").First(&u).Error; err != nil {
		t.Fatalf("查用户: %v", err)
	}
	if err := credit.Grant(db, u.ID, 7, 3, "测试"); err != nil {
		t.Fatalf("发放: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Credits struct {
			Monthly int `json:"monthly"`
			Addon   int `json:"addon"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v; body=%s", err, w.Body.String())
	}
	if body.Credits.Monthly != 7 || body.Credits.Addon != 3 {
		t.Fatalf("余额: got %d/%d, want 7/3", body.Credits.Monthly, body.Credits.Addon)
	}
}

func TestMeCreditsAreZeroWithoutAccount(t *testing.T) {
	r := setupRouter(t)
	token := registerAndLogin(t, r, "noaccount@example.com", "secret12345")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var body struct {
		Credits struct {
			Monthly int `json:"monthly"`
			Addon   int `json:"addon"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v", err)
	}
	// 新用户还没有账户行——必须返回 0/0 而不是 500。
	if body.Credits.Monthly != 0 || body.Credits.Addon != 0 {
		t.Fatalf("应当是 0/0: got %d/%d", body.Credits.Monthly, body.Credits.Addon)
	}
}

// TestMeIncludesSubscriptionNullWhenNone：未订阅用户的 subscription 字段必须是
// null 而不是缺失或零值对象，前端靠它区分"没订阅"和"订阅了但状态未知"。
func TestMeIncludesSubscriptionNullWhenNone(t *testing.T) {
	r := setupRouter(t)
	token := registerAndLogin(t, r, "nosub@example.com", "secret12345")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	// 用 RawMessage 才能区分"字段缺失"与"字段是 null"——解到 any 里两者都是 nil。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("解析: %v; body=%s", err, w.Body.String())
	}
	sub, ok := raw["subscription"]
	if !ok {
		t.Fatalf("subscription 字段必须存在（值为 null），body=%s", w.Body.String())
	}
	if string(sub) != "null" {
		t.Fatalf("未订阅时 subscription 必须是 null，得到 %s", string(sub))
	}
}

// TestMeIncludesSubscriptionWhenPresent：建一行 subscription，断言
// planId / status / currentPeriodEnd / cancelAtPeriodEnd 都在。
func TestMeIncludesSubscriptionWhenPresent(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "hassub@example.com", "secret12345")

	var u model.User
	if err := db.Where("email = ?", "hassub@example.com").First(&u).Error; err != nil {
		t.Fatalf("查用户: %v", err)
	}
	periodEnd := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sub := model.Subscription{
		UserID:               u.ID,
		PlanID:               "pro",
		StripeSubscriptionID: "sub_test_123",
		Status:               "active",
		CurrentPeriodStart:   periodEnd.AddDate(0, -1, 0),
		CurrentPeriodEnd:     periodEnd,
		CancelAtPeriodEnd:    true,
	}
	if err := db.Create(&sub).Error; err != nil {
		t.Fatalf("建订阅: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Subscription *struct {
			PlanID            string    `json:"planId"`
			Status            string    `json:"status"`
			CurrentPeriodEnd  time.Time `json:"currentPeriodEnd"`
			CancelAtPeriodEnd bool      `json:"cancelAtPeriodEnd"`
		} `json:"subscription"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v; body=%s", err, w.Body.String())
	}
	if body.Subscription == nil {
		t.Fatalf("已订阅用户的 subscription 不该是 null，body=%s", w.Body.String())
	}
	if body.Subscription.PlanID != "pro" {
		t.Errorf("planId: got %q, want \"pro\"", body.Subscription.PlanID)
	}
	if body.Subscription.Status != "active" {
		t.Errorf("status: got %q, want \"active\"", body.Subscription.Status)
	}
	if !body.Subscription.CurrentPeriodEnd.Equal(periodEnd) {
		t.Errorf("currentPeriodEnd: got %v, want %v", body.Subscription.CurrentPeriodEnd, periodEnd)
	}
	if !body.Subscription.CancelAtPeriodEnd {
		t.Errorf("cancelAtPeriodEnd: got false, want true")
	}
}
