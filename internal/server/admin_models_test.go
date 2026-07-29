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

// adminTokenFor 注册一个用户、提权成 admin、返回它的 JWT。
//
// 提权只能直接改库——注册接口不会创建 admin（见 admin_test.go 里的同样做法）。
// RequireAdmin 每次请求都读库，所以登录之后再提权也生效。
func adminTokenFor(t *testing.T, r *gin.Engine, db *gorm.DB, email string) string {
	t.Helper()
	token := registerAndLogin(t, r, email, "secret12345")
	if err := db.Model(&model.User{}).Where("email = ?", email).
		Update("role", "admin").Error; err != nil {
		t.Fatalf("提权: %v", err)
	}
	return token
}

// authJSON 发一个带 Authorization 的 JSON 请求。body 为空串时不带请求体。
func authJSON(r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type adminModelRow struct {
	ID                   string `json:"id"`
	DisplayName          string `json:"displayName"`
	Provider             string `json:"provider"`
	UpstreamModel        string `json:"upstreamModel"`
	Credits              int    `json:"credits"`
	SupportsImageToImage bool   `json:"supportsImageToImage"`
	Enabled              bool   `json:"enabled"`
	SortOrder            int    `json:"sortOrder"`
}

func decodeAdminModels(t *testing.T, w *httptest.ResponseRecorder) []adminModelRow {
	t.Helper()
	var out struct {
		Models []adminModelRow `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v; body=%s", err, w.Body.String())
	}
	return out.Models
}

// TestAdminListModelsIncludesDisabled 后台列表必须包含已下架的模型。
//
// 公开的 GET /models 只返回 enabled=true。后台若也过滤，下架一个模型之后就再也
// 找不到它、无法重新上架——运营只能回去手改数据库。
func TestAdminListModelsIncludesDisabled(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-list-models@example.com")

	off := model.ImageModel{
		ID: "retired-model", DisplayName: "Retired", Provider: "flux",
		UpstreamModel: "flux-2-max", Credits: 2, Enabled: false, SortOrder: 50,
	}
	if err := db.Create(&off).Error; err != nil {
		t.Fatalf("造下架模型: %v", err)
	}
	// 必须显式再写一次 enabled=false：ImageModel.Enabled 带 `default:true`，GORM 插入
	// 时会把零值字段整列省掉、让数据库填默认值 true。少了这行，这个"下架"夹具其实
	// 是上架的，测试会以看不懂的方式失败。
	if err := db.Model(&model.ImageModel{}).Where("id = ?", off.ID).
		Update("enabled", false).Error; err != nil {
		t.Fatalf("下架夹具: %v", err)
	}

	w := authJSON(r, http.MethodGet, "/api/v1/admin/models", token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	rows := decodeAdminModels(t, w)
	var found *adminModelRow
	for i := range rows {
		if rows[i].ID == "retired-model" {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatalf("后台列表缺少已下架模型，下架后将无法重新上架: %s", w.Body.String())
	}
	if found.Enabled {
		t.Fatalf("已下架模型的 enabled 应当是 false: %+v", *found)
	}
	// 后台要能看到 provider / upstreamModel 才能判断配置是否正确。
	if found.Provider != "flux" || found.UpstreamModel != "flux-2-max" {
		t.Fatalf("后台列表应当暴露 provider 与 upstreamModel: %+v", *found)
	}

	// 对照：公开接口仍然不应该看到它。
	pub := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	pw := httptest.NewRecorder()
	r.ServeHTTP(pw, pub)
	if strings.Contains(pw.Body.String(), "retired-model") {
		t.Fatalf("公开接口不该返回已下架模型: %s", pw.Body.String())
	}
}

// TestAdminPatchModelCreditsOnly 本轮**最重要**的一条。
//
// 只传 {"credits":7}，其余字段必须原样不动。若请求结构体的字段不是指针，
// "没传 enabled" 与 "传了 enabled:false" 无法区分，这个请求会顺手把模型下架，
// 而且没有任何报错——线上模型从列表里消失，没人会联想到是一次改价造成的。
func TestAdminPatchModelCreditsOnly(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-credits@example.com")

	var before model.ImageModel
	if err := db.Where("id = ?", "flux-2-max").First(&before).Error; err != nil {
		t.Fatalf("读播种模型: %v", err)
	}

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/models/flux-2-max", token, `{"credits":7}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}

	var after model.ImageModel
	if err := db.Where("id = ?", "flux-2-max").First(&after).Error; err != nil {
		t.Fatalf("重读模型: %v", err)
	}
	if after.Credits != 7 {
		t.Fatalf("credits 应当改成 7: got %d", after.Credits)
	}
	if !after.Enabled {
		t.Fatalf("只改 credits 的请求把模型下架了——enabled 必须是 *bool，否则缺省的 false 会被当成'要下架'")
	}
	if after.DisplayName != before.DisplayName {
		t.Fatalf("displayName 不该变: got %q, want %q", after.DisplayName, before.DisplayName)
	}
	if after.SortOrder != before.SortOrder {
		t.Fatalf("sortOrder 不该变: got %d, want %d", after.SortOrder, before.SortOrder)
	}
	if after.Provider != before.Provider || after.UpstreamModel != before.UpstreamModel {
		t.Fatalf("provider/upstreamModel 不该变: %+v", after)
	}
}

// TestAdminPatchModelRejectsZeroCredits credits=0 必须 400。
//
// credit.Spend 拒绝 cost <= 0，所以 0 不是"免费模型"，而是"该模型每次生成都返回
// 错误"。放过去的话故障点离改动很远，没人会想到是那次配置造成的。
func TestAdminPatchModelRejectsZeroCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-zero@example.com")

	// 先读原值再比，不要硬编码 seed 出来的数字——seed 值是可调的产品参数，
	// 写死会让一次正常的调价把这条无关的测试搞红。
	var before model.ImageModel
	if err := db.Where("id = ?", "flux-2-max").First(&before).Error; err != nil {
		t.Fatalf("读原始模型: %v", err)
	}

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/models/flux-2-max", token, `{"credits":0}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("credits=0 应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
	var after model.ImageModel
	if err := db.Where("id = ?", "flux-2-max").First(&after).Error; err != nil {
		t.Fatalf("重读模型: %v", err)
	}
	if after.Credits != before.Credits {
		t.Fatalf("被拒绝的请求不该落库: got credits=%d, want %d", after.Credits, before.Credits)
	}
}

func TestAdminPatchModelRejectsNegativeCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-neg@example.com")

	// 先读原值再比，不要硬编码 seed 出来的数字——seed 值是可调的产品参数，
	// 写死会让一次正常的调价把这条无关的测试搞红。
	var before model.ImageModel
	if err := db.Where("id = ?", "flux-2-max").First(&before).Error; err != nil {
		t.Fatalf("读原始模型: %v", err)
	}

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/models/flux-2-max", token, `{"credits":-3}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("负 credits 应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
	var after model.ImageModel
	if err := db.Where("id = ?", "flux-2-max").First(&after).Error; err != nil {
		t.Fatalf("重读模型: %v", err)
	}
	if after.Credits != before.Credits {
		t.Fatalf("被拒绝的请求不该落库: got credits=%d, want %d", after.Credits, before.Credits)
	}
}

func TestAdminPatchUnknownModelReturns404(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-404@example.com")

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/models/nope", token, `{"credits":5}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("未知模型应当 404: got %d; body=%s", w.Code, w.Body.String())
	}
	// 断言是 handler 返回的结构化 404，而不是 Gin 的"路由不存在"404——否则这条
	// 测试在路由压根没注册时也会绿。
	if !strings.Contains(w.Body.String(), "40400") {
		t.Fatalf("应当是 handler 的 40400，而不是路由缺失的 404: body=%s", w.Body.String())
	}
}

// TestAdminCreateModelRejectsUnregisteredProvider provider 没注册 adapter → 400。
//
// 否则模型建出来了、在列表里看着完全正常，用户一点生成就 500（Adapters.Get 失败）。
func TestAdminCreateModelRejectsUnregisteredProvider(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-create-badprovider@example.com")

	body := `{"id":"mj-v7","displayName":"MJ v7","provider":"midjourney","upstreamModel":"mj-v7","credits":4}`
	w := authJSON(r, http.MethodPost, "/api/v1/admin/models", token, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未注册 provider 应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
	var n int64
	db.Model(&model.ImageModel{}).Where("id = ?", "mj-v7").Count(&n)
	if n != 0 {
		t.Fatalf("被拒绝的模型不该落库")
	}
}

func TestAdminCreateModelRejectsZeroCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-create-zero@example.com")

	body := `{"id":"free-model","displayName":"Free","provider":"flux","upstreamModel":"flux-2-max","credits":0}`
	w := authJSON(r, http.MethodPost, "/api/v1/admin/models", token, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("credits=0 应当 400: got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestAdminCreateModelDuplicateIDReturns409 已存在的 id 必须 409。
//
// 静默覆盖会把线上模型的配置（扣费、上游模型名）整行冲掉；500 则让调用方以为
// 是我们坏了、去重试。
func TestAdminCreateModelDuplicateIDReturns409(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-create-dup@example.com")

	body := `{"id":"flux-2-max","displayName":"Hijacked","provider":"flux","upstreamModel":"other","credits":9}`
	w := authJSON(r, http.MethodPost, "/api/v1/admin/models", token, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("重复 id 应当 409: got %d; body=%s", w.Code, w.Body.String())
	}
	var after model.ImageModel
	if err := db.Where("id = ?", "flux-2-max").First(&after).Error; err != nil {
		t.Fatalf("重读模型: %v", err)
	}
	if after.DisplayName == "Hijacked" || after.Credits == 9 {
		t.Fatalf("已存在的模型被静默覆盖了: %+v", after)
	}
}

// TestAdminPatchModelCanDisable 下架必须真的生效。
//
// 这条与上一条互为对照：Updates 若传结构体而不是 map，enabled:false 是零值、会被
// GORM 静默跳过——运营点了"下架"，接口回 200，模型却还挂在公开列表上。
func TestAdminPatchModelCanDisable(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-patch-disable@example.com")

	w := authJSON(r, http.MethodPatch, "/api/v1/admin/models/flux-2-max", token, `{"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d; body=%s", w.Code, w.Body.String())
	}
	var after model.ImageModel
	if err := db.Where("id = ?", "flux-2-max").First(&after).Error; err != nil {
		t.Fatalf("重读模型: %v", err)
	}
	if after.Enabled {
		t.Fatalf("下架未生效——enabled:false 被当成零值跳过了")
	}
	pub := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	pw := httptest.NewRecorder()
	r.ServeHTTP(pw, pub)
	if strings.Contains(pw.Body.String(), "flux-2-max") {
		t.Fatalf("下架后公开列表仍返回该模型: %s", pw.Body.String())
	}
}

// TestAdminCreateModelDisabledStaysDisabled 建模型时传 enabled:false 必须真的不上架。
//
// ImageModel.Enabled 带 `default:true`，GORM 插入会省掉零值列让库填默认值——不加
// Select("*") 的话这个模型会被建成已上架，立刻对所有用户可见，而运营的本意是先建
// 好、确认上游配置正确之后再开。
func TestAdminCreateModelDisabledStaysDisabled(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "admin-create-off@example.com")

	body := `{"id":"staging-model","displayName":"Staging","provider":"flux",` +
		`"upstreamModel":"flux-2-max","credits":2,"enabled":false}`
	w := authJSON(r, http.MethodPost, "/api/v1/admin/models", token, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("新增模型应当 201: got %d; body=%s", w.Code, w.Body.String())
	}
	var m model.ImageModel
	if err := db.Where("id = ?", "staging-model").First(&m).Error; err != nil {
		t.Fatalf("读新模型: %v", err)
	}
	if m.Enabled {
		t.Fatalf("enabled:false 的新模型被建成了已上架——它会立刻对所有用户可见")
	}
	pub := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	pw := httptest.NewRecorder()
	r.ServeHTTP(pw, pub)
	if strings.Contains(pw.Body.String(), "staging-model") {
		t.Fatalf("未上架模型出现在公开列表: %s", pw.Body.String())
	}
}

// TestAdminCreateModelSucceedsAndIsSpendable 把"配置"和"生效"两端连起来。
//
// 只断言"库里的值变了"证明不了扣费路径读到了它。这里建一个 credits=3 的模型，
// 然后**真的用它调一次生成**，断言扣了 3。
func TestAdminCreateModelSucceedsAndIsSpendable(t *testing.T) {
	r, db := setupRouterWithDB(t)
	adminToken := adminTokenFor(t, r, db, "admin-create-ok@example.com")

	body := `{"id":"flux-2-pro","displayName":"Flux 2 Pro","provider":"flux",` +
		`"upstreamModel":"flux-2-max","credits":3,"supportsImageToImage":true,"sortOrder":20}`
	w := authJSON(r, http.MethodPost, "/api/v1/admin/models", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("新增模型应当 201: got %d; body=%s", w.Code, w.Body.String())
	}

	// 新模型必须立刻出现在公开列表里（enabled 默认 true）。
	pub := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	pw := httptest.NewRecorder()
	r.ServeHTTP(pw, pub)
	if !strings.Contains(pw.Body.String(), "flux-2-pro") {
		t.Fatalf("新模型未出现在公开列表: %s", pw.Body.String())
	}

	userToken := registerAndLogin(t, r, "buyer-newmodel@example.com", "secret12345")
	uid := grantTo(t, db, "buyer-newmodel@example.com", 10)

	gw := postGenerate(r, userToken,
		`{"prompt":"quick cat","model":"flux-2-pro","aspectRatio":"1:1"}`)
	if gw.Code != http.StatusOK {
		t.Fatalf("用新模型生成应当 200: got %d; body=%s", gw.Code, gw.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(gw.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v; body=%s", err, gw.Body.String())
	}
	if out["status"] != "succeeded" {
		t.Fatalf("应当成功: %+v", out)
	}
	if out["creditsSpent"] != float64(3) {
		t.Fatalf("应当按后台配置扣 3: got %v", out["creditsSpent"])
	}
	bal, err := credit.Balance(db, uid)
	if err != nil {
		t.Fatalf("查余额: %v", err)
	}
	if bal.MonthlyCredits != 7 {
		t.Fatalf("10 - 3 应当剩 7: got %d", bal.MonthlyCredits)
	}
}

// TestAdminModelRoutesRequireAdmin 三条路由都必须挡住普通用户。
//
// 漏挂中间件的后果是任意登录用户能把所有模型的 credits 改成 1——直接的收入损失。
func TestAdminModelRoutesRequireAdmin(t *testing.T) {
	r := setupRouter(t)
	token := registerAndLogin(t, r, "plain-models@example.com", "secret12345")

	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/admin/models", ""},
		{http.MethodPost, "/api/v1/admin/models",
			`{"id":"x","displayName":"X","provider":"flux","upstreamModel":"flux-2-max","credits":1}`},
		{http.MethodPatch, "/api/v1/admin/models/flux-2-max", `{"credits":7}`},
	}
	for _, tc := range cases {
		w := authJSON(r, tc.method, tc.path, token, tc.body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s 普通用户应当 403: got %d; body=%s",
				tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}
