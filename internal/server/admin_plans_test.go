package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"image-backend/internal/model"
)

type adminPlanRow struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	PriceUSDCents  int    `json:"priceUsdCents"`
	MonthlyCredits int    `json:"monthlyCredits"`
	StripePriceID  string `json:"stripePriceID"`
	Enabled        bool   `json:"enabled"`
	SortOrder      int    `json:"sortOrder"`
}

func decodeAdminPlans(t *testing.T, w *httptest.ResponseRecorder) []adminPlanRow {
	t.Helper()
	var out struct {
		Plans []adminPlanRow `json:"plans"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v; body=%s", err, w.Body.String())
	}
	return out.Plans
}

// TestAdminListPlansIncludesDisabledAndPriceID 后台列表与公开的 GET /plans 相反。
//
// 必须返回 stripePriceID：它是运营确认"播种命令跑过没有"的唯一线索——为空说明还没
// 建 Stripe Price，该档位无法下单，而定价页看着完全正常。
// 必须返回已下架的档位：否则下架之后就再也找不到它、无法重新上架。
func TestAdminListPlansIncludesDisabledAndPriceID(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-list-plans@example.com")

	// 给一个播种过的档位回填 Price ID，模拟 cmd/seed-stripe 跑过之后的状态。
	if err := db.Model(&model.Plan{}).Where("id = ?", "pro").
		Update("stripe_price_id", "price_live_pro_123").Error; err != nil {
		t.Fatalf("回填 price id: %v", err)
	}

	off := model.Plan{
		ID: "retired-plan", DisplayName: "Retired", PriceUSDCents: 100,
		MonthlyCredits: 10, Enabled: false, SortOrder: 90,
	}
	if err := db.Create(&off).Error; err != nil {
		t.Fatalf("造下架档位: %v", err)
	}
	// 必须显式再写一次 enabled=false：Plan.Enabled 带 `default:true`，GORM 插入时会把
	// 零值字段整列省掉、让数据库填默认值 true。少了这行，这个"下架"夹具其实是上架的，
	// 这条测试会假绿。
	if err := db.Model(&model.Plan{}).Where("id = ?", off.ID).
		Update("enabled", false).Error; err != nil {
		t.Fatalf("下架夹具: %v", err)
	}

	w := authJSON(r, http.MethodGet, "/api/v1/admin/plans", token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	rows := decodeAdminPlans(t, w)

	var retired, pro *adminPlanRow
	for i := range rows {
		switch rows[i].ID {
		case "retired-plan":
			retired = &rows[i]
		case "pro":
			pro = &rows[i]
		}
	}
	if retired == nil {
		t.Fatalf("后台列表缺少已下架档位，下架后将无法重新上架: %s", w.Body.String())
	}
	if retired.Enabled {
		t.Fatalf("已下架档位的 enabled 应当是 false: %+v", *retired)
	}
	if pro == nil {
		t.Fatalf("后台列表缺少 pro 档: %s", w.Body.String())
	}
	if pro.StripePriceID != "price_live_pro_123" {
		t.Fatalf("后台必须返回 stripePriceID（运营靠它确认播种是否跑过）: %+v", *pro)
	}
	if pro.MonthlyCredits != 800 || pro.PriceUSDCents != 2990 {
		t.Fatalf("pro 档字段不对: %+v", *pro)
	}

	// 对照：公开接口既不返回 Price ID，也不返回已下架的档位。
	pub := httptest.NewRequest(http.MethodGet, "/api/v1/plans", nil)
	pw := httptest.NewRecorder()
	r.ServeHTTP(pw, pub)
	if strings.Contains(pw.Body.String(), "price_live_pro_123") {
		t.Fatalf("公开接口不该暴露 Stripe Price ID: %s", pw.Body.String())
	}
	if strings.Contains(pw.Body.String(), "retired-plan") {
		t.Fatalf("公开接口不该返回已下架档位: %s", pw.Body.String())
	}
}

// TestAdminPatchPlanMonthlyCredits 只传 monthlyCredits，其余字段必须原样不动。
//
// 若请求结构体的字段不是指针，"没传 enabled" 与 "传了 enabled:false" 无法区分——
// 一个只想把 pro 的月度次数从 800 调成 1000 的请求会顺手把这一档下架，定价页上
// 它凭空消失，而接口回的是 200。
func TestAdminPatchPlanMonthlyCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-plan-credits@example.com")

	if err := db.Model(&model.Plan{}).Where("id = ?", "pro").
		Update("stripe_price_id", "price_live_pro_123").Error; err != nil {
		t.Fatalf("回填 price id: %v", err)
	}
	var before model.Plan
	if err := db.Where("id = ?", "pro").First(&before).Error; err != nil {
		t.Fatalf("读播种档位: %v", err)
	}

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/plans/pro", token, `{"monthlyCredits":1000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}

	var after model.Plan
	if err := db.Where("id = ?", "pro").First(&after).Error; err != nil {
		t.Fatalf("重读档位: %v", err)
	}
	if after.MonthlyCredits != 1000 {
		t.Fatalf("monthlyCredits 应当改成 1000: got %d", after.MonthlyCredits)
	}
	if !after.Enabled {
		t.Fatalf("只改 monthlyCredits 的请求把档位下架了——enabled 必须是 *bool，否则缺省的 false 会被当成'要下架'")
	}
	if after.PriceUSDCents != before.PriceUSDCents {
		t.Fatalf("priceUsdCents 不该变: got %d, want %d", after.PriceUSDCents, before.PriceUSDCents)
	}
	if after.StripePriceID != before.StripePriceID {
		t.Fatalf("stripePriceID 不该变: got %q, want %q", after.StripePriceID, before.StripePriceID)
	}
	if after.DisplayName != before.DisplayName {
		t.Fatalf("displayName 不该变: got %q, want %q", after.DisplayName, before.DisplayName)
	}
	if after.SortOrder != before.SortOrder {
		t.Fatalf("sortOrder 不该变: got %d, want %d", after.SortOrder, before.SortOrder)
	}
}

// TestAdminPatchPlanRejectsPriceChange 传 priceUsdCents 必须 400，而不是静默忽略。
//
// Stripe 的 Price 金额不可变。改我们这边的数字不会改变 Stripe 实际收多少钱，只会让
// 定价页显示 $29.90 而用户被扣 $49.90——这是最难向用户解释的一类不一致。
//
// 静默忽略不行：运营会以为改成功了，直到有人对账才发现没生效。
func TestAdminPatchPlanRejectsPriceChange(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-plan-price@example.com")

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/plans/pro", token,
		`{"priceUsdCents":1990}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("改价应当 400（静默忽略会让运营以为改成功了）: got %d; body=%s",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "40000") {
		t.Fatalf("应当是 handler 的结构化 40000: body=%s", w.Body.String())
	}
	// message 必须点名 priceUsdCents。否则一个只忽略它、随后因"没有可修改的字段"
	// 而回 400 的实现也会让这条假绿——那种实现在混着合法字段时就会放行改价。
	if !strings.Contains(w.Body.String(), "priceUsdCents") {
		t.Fatalf("message 必须说明 priceUsdCents 不可改，而不是笼统的'没有可修改的字段': body=%s",
			w.Body.String())
	}

	var after model.Plan
	if err := db.Where("id = ?", "pro").First(&after).Error; err != nil {
		t.Fatalf("重读档位: %v", err)
	}
	if after.PriceUSDCents != 2990 {
		t.Fatalf("价格被改了——Stripe 那边分文未变，定价页与实际扣款不一致: got %d", after.PriceUSDCents)
	}

	// 和合法字段混在一起也必须整条拒绝：部分生效比全不生效更难排查。
	w2 := authJSON(r, http.MethodPatch, "/api/v1/admin/plans/pro", token,
		`{"monthlyCredits":1234,"priceUsdCents":1990}`)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("混着改价也应当 400: got %d; body=%s", w2.Code, w2.Body.String())
	}
	var after2 model.Plan
	if err := db.Where("id = ?", "pro").First(&after2).Error; err != nil {
		t.Fatalf("重读档位: %v", err)
	}
	if after2.MonthlyCredits != 800 {
		t.Fatalf("被拒绝的请求不该部分落库: got monthlyCredits=%d", after2.MonthlyCredits)
	}
}

// TestAdminPatchPlanRejectsStripePriceIDChange 传 stripePriceId 必须 400。
//
// 手填一个 Price ID 就是"付了 Pro 的钱、拿到 Starter 的次数"——webhook 靠这一列
// 反查档位，指错了就发错次数。
func TestAdminPatchPlanRejectsStripePriceIDChange(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-plan-priceid@example.com")

	if err := db.Model(&model.Plan{}).Where("id = ?", "pro").
		Update("stripe_price_id", "price_live_pro_123").Error; err != nil {
		t.Fatalf("回填 price id: %v", err)
	}

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/plans/pro", token,
		`{"stripePriceId":"price_live_starter_999"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("改 stripePriceId 应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "40000") {
		t.Fatalf("应当是 handler 的结构化 40000: body=%s", w.Body.String())
	}
	// message 必须点名 stripePriceId，同 RejectsPriceChange 的理由：只忽略它、随后
	// 因"没有可修改的字段"回 400 的实现在混着合法字段时会放行。
	if !strings.Contains(w.Body.String(), "stripePriceId") {
		t.Fatalf("message 必须说明 stripePriceId 不可改: body=%s", w.Body.String())
	}
	var after model.Plan
	if err := db.Where("id = ?", "pro").First(&after).Error; err != nil {
		t.Fatalf("重读档位: %v", err)
	}
	if after.StripePriceID != "price_live_pro_123" {
		t.Fatalf("Price ID 被改了——会导致'付了 Pro 的钱拿到别档的次数': got %q", after.StripePriceID)
	}

	// 混着合法字段也必须整条拒绝。
	w2 := authJSON(r, http.MethodPatch, "/api/v1/admin/plans/pro", token,
		`{"monthlyCredits":1234,"stripePriceId":"price_live_starter_999"}`)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("混着改 Price ID 也应当 400: got %d; body=%s", w2.Code, w2.Body.String())
	}
	var after2 model.Plan
	if err := db.Where("id = ?", "pro").First(&after2).Error; err != nil {
		t.Fatalf("重读档位: %v", err)
	}
	if after2.MonthlyCredits != 800 || after2.StripePriceID != "price_live_pro_123" {
		t.Fatalf("被拒绝的请求不该部分落库: %+v", after2)
	}
}

// TestAdminPatchPlanRejectsNegativeCredits 负数必须 400。
//
// credit.ResetMonthly 是"设置"语义，接受 0（该档暂时不发次数是合法配置），但负数
// 会把余额设成负的，之后每次生成都因余额不足失败。这里的校验与它保持一致。
func TestAdminPatchPlanRejectsNegativeCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-plan-neg@example.com")

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/plans/pro", token,
		`{"monthlyCredits":-5}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("负 monthlyCredits 应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "40000") {
		t.Fatalf("应当是 handler 的结构化 40000: body=%s", w.Body.String())
	}
	var after model.Plan
	if err := db.Where("id = ?", "pro").First(&after).Error; err != nil {
		t.Fatalf("重读档位: %v", err)
	}
	if after.MonthlyCredits != 800 {
		t.Fatalf("被拒绝的请求不该落库: got monthlyCredits=%d", after.MonthlyCredits)
	}
}

// TestAdminPatchPlanAllowsZeroCredits 0 是合法值，不能连坐进负数校验里。
//
// monthlyCredits=0 等于"该档暂时不发次数"，credit.ResetMonthly 接受它（设置语义）。
// 用 map + Updates 才能让 0 真的落库——Updates 传结构体会把零值静默跳过。
func TestAdminPatchPlanAllowsZeroCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-plan-zero@example.com")

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/plans/pro", token,
		`{"monthlyCredits":0}`)
	if w.Code != http.StatusOK {
		t.Fatalf("monthlyCredits=0 应当允许: got %d; body=%s", w.Code, w.Body.String())
	}
	var after model.Plan
	if err := db.Where("id = ?", "pro").First(&after).Error; err != nil {
		t.Fatalf("重读档位: %v", err)
	}
	if after.MonthlyCredits != 0 {
		t.Fatalf("0 被静默跳过了（Updates 传结构体会跳过零值）: got %d", after.MonthlyCredits)
	}
}

// TestAdminPatchPlanCanDisableAndReEnable 下架与重新上架都必须真的生效。
//
// enabled:false 是零值，Updates 传结构体会静默跳过它——运营点了"下架"，接口回
// 200，档位却还挂在定价页上。
func TestAdminPatchPlanCanDisableAndReEnable(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-plan-disable@example.com")

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/plans/pro", token, `{"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var off model.Plan
	if err := db.Where("id = ?", "pro").First(&off).Error; err != nil {
		t.Fatalf("重读档位: %v", err)
	}
	if off.Enabled {
		t.Fatalf("下架未生效——enabled:false 被当成零值跳过了")
	}
	pub := httptest.NewRequest(http.MethodGet, "/api/v1/plans", nil)
	pw := httptest.NewRecorder()
	r.ServeHTTP(pw, pub)
	if strings.Contains(pw.Body.String(), `"pro"`) {
		t.Fatalf("下架后公开定价页仍返回该档位: %s", pw.Body.String())
	}

	// 重新上架：这一步依赖后台列表能看到已下架的档位（否则运营找不到它）。
	w2 := authJSON(r, http.MethodPatch, "/api/v1/admin/plans/pro", token, `{"enabled":true}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("重新上架: got %d; body=%s", w2.Code, w2.Body.String())
	}
	var on model.Plan
	if err := db.Where("id = ?", "pro").First(&on).Error; err != nil {
		t.Fatalf("重读档位: %v", err)
	}
	if !on.Enabled {
		t.Fatalf("重新上架未生效: %+v", on)
	}
}

// TestAdminPatchUnknownPlanReturns404 未知档位 → handler 的结构化 404。
//
// 断言响应体里的业务错误码，而不是只看状态码——只看状态码的话，路由压根没注册时
// 吃到的 Gin 路由 404 也会让这条假绿。
func TestAdminPatchUnknownPlanReturns404(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-plan-404@example.com")

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/plans/nope", token,
		`{"monthlyCredits":100}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("未知档位应当 404: got %d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "40400") {
		t.Fatalf("应当是 handler 的 40400，而不是路由缺失的 404: body=%s", w.Body.String())
	}
}

// TestAdminPlanRoutesRequireAdmin 两条路由都必须挡住普通用户。
//
// 漏挂中间件的后果是任意登录用户能把 starter 的 monthlyCredits 改成 100000——
// 花 $9.90 拿到 Max 档的量，直接的收入损失。
func TestAdminPlanRoutesRequireAdmin(t *testing.T) {
	r := setupRouter(t)
	token := registerAndLogin(t, r, "plain-plans@example.com", "secret12345")

	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/admin/plans", ""},
		{http.MethodPatch, "/api/v1/admin/plans/pro", `{"monthlyCredits":100000}`},
	}
	for _, tc := range cases {
		w := authJSON(r, tc.method, tc.path, token, tc.body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s 普通用户应当 403: got %d; body=%s",
				tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}
