package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"image-backend/internal/config"
	"image-backend/internal/database"
	"image-backend/internal/model"
)

func settingsAdmin(t *testing.T) (r *gin.Engine, adminToken string) {
	t.Helper()
	eng, db := setupRouterWithDB(t)
	adminToken = registerAndLogin(t, eng, "settings-admin@example.com", "secret12345")
	if err := db.Model(&model.User{}).
		Where("email = ?", "settings-admin@example.com").
		Update("role", "admin").Error; err != nil {
		t.Fatalf("提权: %v", err)
	}
	return eng, adminToken
}

func doGetSettings(r *gin.Engine, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doPatchSettings(r *gin.Engine, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestNewRouterRefusesInvalidEncryptionKey 钉住"绝不退化成全零密钥"。
//
// 这条测试防的是一次**静默的安全降级**：如果 NewRouter 在密钥缺失时回落到全零
// 密钥，所有 secret 都会用一把人人都猜得到的密钥"加密"入库——任何拿到库的人都能
// 解开，而日志、响应、行为全都看不出异常，直到某天被拖库才暴露。
//
// 用 panic 而不是打告警：告警会被淹在启动日志里，panic 让这个错误不可能被忽略。
func TestNewRouterRefusesInvalidEncryptionKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, badKey := range []string{
		"",                         // 完全没配
		"not-base64!!!",            // 不是合法 base64
		"MDEyMzQ1Njc4OWFiY2RlZg==", // 只有 16 字节（AES-128），强度不符
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("密钥 %q 应当让 NewRouter panic——"+
						"退化成全零密钥等于所有 secret 明文入库且毫无告警", badKey)
				}
			}()
			db, err := database.Open("")
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			NewRouter(db, &config.Config{JWTSecret: "test-secret", ConfigEncryptionKey: badKey})
		}()
	}
}

// 1. GET /api/v1/admin/settings 非管理员 403
func TestAdminSettingsGetRequiresAdmin(t *testing.T) {
	eng, db := setupRouterWithDB(t)
	// 注册普通用户（不提权）
	token := registerAndLogin(t, eng, "plain-settings@example.com", "secret12345")
	_ = db

	w := doGetSettings(eng, token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("普通用户应当 403, got %d; body=%s", w.Code, w.Body.String())
	}
}

// 2. GET 返回非 secret 项的 value、secret 项只有 configured + masked
func TestAdminSettingsGetReturnsCorrectShape(t *testing.T) {
	eng, adminToken := settingsAdmin(t)

	// 先设一个非 secret 项和一个 secret 项
	w := doPatchSettings(eng, adminToken, `{"r2Bucket":"images","fluxApiKey":"sk-test-12345678"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH 应当 200, got %d; body=%s", w.Code, w.Body.String())
	}

	w = doGetSettings(eng, adminToken)
	if w.Code != http.StatusOK {
		t.Fatalf("GET 应当 200, got %d; body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, w.Body.String())
	}

	settingsRaw, ok := resp["settings"]
	if !ok {
		t.Fatalf("响应缺少 settings 字段: %s", w.Body.String())
	}
	settingsMap := settingsRaw.(map[string]any)

	// 非 secret 项：必须有 value 字段，不能有 configured/masked
	bucketRaw, ok := settingsMap["r2Bucket"]
	if !ok {
		t.Fatal("响应缺少 r2Bucket 项")
	}
	bucket := bucketRaw.(map[string]any)
	if bucket["value"] != "images" {
		t.Errorf("r2Bucket.value 期望 'images', got %v", bucket["value"])
	}
	if _, hasConf := bucket["configured"]; hasConf {
		t.Error("非 secret 项不应有 configured 字段")
	}

	// secret 项：必须有 configured + masked，绝不能有 value
	fluxRaw, ok := settingsMap["fluxApiKey"]
	if !ok {
		t.Fatal("响应缺少 fluxApiKey 项")
	}
	flux := fluxRaw.(map[string]any)
	if _, hasValue := flux["value"]; hasValue {
		t.Error("secret 项不能有 value 字段——会泄露明文")
	}
	configured, hasConf := flux["configured"]
	if !hasConf {
		t.Error("secret 项必须有 configured 字段")
	}
	if configured != true {
		t.Errorf("fluxApiKey 已设置，configured 应当是 true, got %v", configured)
	}
	if _, hasMasked := flux["masked"]; !hasMasked {
		t.Error("secret 项必须有 masked 字段")
	}

	// storageEnabled 字段必须存在
	if _, ok := resp["storageEnabled"]; !ok {
		t.Error("响应缺少 storageEnabled 字段")
	}
}

// 3. GET 响应体里不含任何 secret 明文
func TestAdminSettingsGetNeverReturnsSecretPlaintext(t *testing.T) {
	eng, adminToken := settingsAdmin(t)

	// 设置带有明显特征的 secret 明文
	const secretValue = "sk-live-plaintext-must-not-appear-9999"
	w := doPatchSettings(eng, adminToken, `{"fluxApiKey":"`+secretValue+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH 应当 200, got %d; body=%s", w.Code, w.Body.String())
	}

	w = doGetSettings(eng, adminToken)
	if w.Code != http.StatusOK {
		t.Fatalf("GET 应当 200, got %d; body=%s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if strings.Contains(body, secretValue) {
		t.Errorf("GET 响应体里出现了 secret 明文——设置页成了凭据泄露端点: body=%s", body)
	}

	// 同样断言其他 secret 字段（通过 PATCH 设置 r2AccessKeyId 和 r2SecretAccessKey）
	const r2Secret = "r2-secret-key-plaintext-8888"
	w2 := doPatchSettings(eng, adminToken, `{"r2SecretAccessKey":"`+r2Secret+`"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("PATCH r2SecretAccessKey 应当 200, got %d; body=%s", w2.Code, w2.Body.String())
	}
	w3 := doGetSettings(eng, adminToken)
	body3 := w3.Body.String()
	if strings.Contains(body3, r2Secret) {
		t.Errorf("GET 响应里出现了 r2SecretAccessKey 明文: body=%s", body3)
	}
}

// 4. PATCH 未知 key → 400
func TestAdminSettingsPatchUnknownKeyReturns400(t *testing.T) {
	eng, adminToken := settingsAdmin(t)

	w := doPatchSettings(eng, adminToken, `{"unknownConfigKey":"value"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知 key 应当 400, got %d; body=%s", w.Code, w.Body.String())
	}
}

// 5. PATCH 非法 r2PublicBaseUrl → 400
func TestAdminSettingsPatchInvalidR2PublicBaseUrlReturns400(t *testing.T) {
	eng, adminToken := settingsAdmin(t)

	// S3 API 域名（不允许匿名读）
	w := doPatchSettings(eng, adminToken, `{"r2PublicBaseUrl":"https://acct.r2.cloudflarestorage.com"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("S3 域名应当 400, got %d; body=%s", w.Code, w.Body.String())
	}

	// 缺少 scheme
	w2 := doPatchSettings(eng, adminToken, `{"r2PublicBaseUrl":"img.example.com"}`)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("缺 scheme 应当 400, got %d; body=%s", w2.Code, w2.Body.String())
	}
}

// 6. PATCH 成功 → 200，且同一进程内下一次 GET 反映新值（热重载生效，不重启）
func TestAdminSettingsPatchSuccessAndHotReload(t *testing.T) {
	eng, adminToken := settingsAdmin(t)

	// 初始 GET：r2Bucket 应当为空
	w := doGetSettings(eng, adminToken)
	if w.Code != http.StatusOK {
		t.Fatalf("初始 GET 应当 200, got %d", w.Code)
	}
	var before map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &before); err != nil {
		t.Fatalf("解析: %v", err)
	}
	settingsBefore := before["settings"].(map[string]any)
	bucketBefore := settingsBefore["r2Bucket"].(map[string]any)
	if bucketBefore["value"] != "" {
		t.Fatalf("初始 r2Bucket 应当为空, got %v", bucketBefore["value"])
	}

	// PATCH
	w2 := doPatchSettings(eng, adminToken, `{"r2Bucket":"hot-reload-bucket"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("PATCH 应当 200, got %d; body=%s", w2.Code, w2.Body.String())
	}

	// 验证 PATCH 响应本身已含新值
	var patchResp map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &patchResp); err != nil {
		t.Fatalf("解析 PATCH 响应: %v", err)
	}
	patchSettings := patchResp["settings"].(map[string]any)
	bucketPatch := patchSettings["r2Bucket"].(map[string]any)
	if bucketPatch["value"] != "hot-reload-bucket" {
		t.Errorf("PATCH 响应里 r2Bucket 应当是 hot-reload-bucket, got %v", bucketPatch["value"])
	}

	// 热重载验证：下一次 GET 也反映新值（不重启）
	w3 := doGetSettings(eng, adminToken)
	if w3.Code != http.StatusOK {
		t.Fatalf("GET 应当 200, got %d", w3.Code)
	}
	var after map[string]any
	if err := json.Unmarshal(w3.Body.Bytes(), &after); err != nil {
		t.Fatalf("解析: %v", err)
	}
	settingsAfter := after["settings"].(map[string]any)
	bucketAfter := settingsAfter["r2Bucket"].(map[string]any)
	if bucketAfter["value"] != "hot-reload-bucket" {
		t.Errorf("热重载后 GET 的 r2Bucket 应当是 hot-reload-bucket, got %v", bucketAfter["value"])
	}
}

// 7. PATCH secret 传空串 → 该项 configured:false
func TestAdminSettingsPatchSecretEmptyStringClears(t *testing.T) {
	eng, adminToken := settingsAdmin(t)

	// 先设一个 secret
	w := doPatchSettings(eng, adminToken, `{"fluxApiKey":"sk-initial-key-value"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("初始 PATCH 应当 200, got %d; body=%s", w.Code, w.Body.String())
	}

	// 验证 configured:true
	var patchResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &patchResp)
	fluxAfterSet := patchResp["settings"].(map[string]any)["fluxApiKey"].(map[string]any)
	if fluxAfterSet["configured"] != true {
		t.Errorf("设置后 configured 应当是 true, got %v", fluxAfterSet["configured"])
	}

	// 用空串清空
	w2 := doPatchSettings(eng, adminToken, `{"fluxApiKey":""}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("空串 PATCH 应当 200, got %d; body=%s", w2.Code, w2.Body.String())
	}

	// 验证 configured:false
	var clearResp map[string]any
	json.Unmarshal(w2.Body.Bytes(), &clearResp)
	fluxAfterClear := clearResp["settings"].(map[string]any)["fluxApiKey"].(map[string]any)
	if fluxAfterClear["configured"] != false {
		t.Errorf("清空后 configured 应当是 false, got %v", fluxAfterClear["configured"])
	}
}

// 8. PATCH 空 body / 没有任何已知 key → 400
func TestAdminSettingsPatchEmptyBodyReturns400(t *testing.T) {
	eng, adminToken := settingsAdmin(t)

	// 空 JSON 对象 {}
	w := doPatchSettings(eng, adminToken, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 body 应当 400, got %d; body=%s", w.Code, w.Body.String())
	}
}
