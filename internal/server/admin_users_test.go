package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

type adminUserRow struct {
	ID             uint   `json:"id"`
	Email          string `json:"email"`
	Role           string `json:"role"`
	Status         string `json:"status"`
	MonthlyCredits int    `json:"monthlyCredits"`
	AddonCredits   int    `json:"addonCredits"`
}

func decodeUserList(t *testing.T, w *httptest.ResponseRecorder) ([]adminUserRow, string) {
	t.Helper()
	var resp struct {
		Users      []adminUserRow `json:"users"`
		NextCursor *string        `json:"nextCursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析用户列表: %v; body=%s", err, w.Body.String())
	}
	cursor := ""
	if resp.NextCursor != nil {
		cursor = *resp.NextCursor
	}
	return resp.Users, cursor
}

func decodeUserRow(t *testing.T, w *httptest.ResponseRecorder) adminUserRow {
	t.Helper()
	var row adminUserRow
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
		t.Fatalf("解析用户: %v; body=%s", err, w.Body.String())
	}
	return row
}

// userIDOf 按邮箱查 id，用于拼 PATCH 路径。
func userIDOf(t *testing.T, db *gorm.DB, email string) uint {
	t.Helper()
	var u model.User
	if err := db.Where("email = ?", email).First(&u).Error; err != nil {
		t.Fatalf("查用户 %s: %v", email, err)
	}
	return u.ID
}

func TestAdminUsersListRequiresAdmin(t *testing.T) {
	// 普通用户不能列出所有人的邮箱。这是**信息泄露**边界，不只是功能边界。
	r, _ := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "plain@example.com", "secret12345")

	w := authJSON(r, http.MethodGet, "/api/v1/admin/users", token, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("非管理员访问用户列表必须 403，得到 %d；body=%s", w.Code, w.Body.String())
	}
	// 也不能通过响应体泄露任何邮箱。
	if strings.Contains(w.Body.String(), "@example.com") {
		t.Errorf("403 响应里不该出现任何邮箱，body=%s", w.Body.String())
	}
}

func TestAdminUsersListReturnsBalances(t *testing.T) {
	// 余额随行返回，省掉前端为每一行再发一次请求。
	// 刚注册且没拿到赠送额度的用户没有账户行，此时必须是 0 而不是报错。
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "list-admin@example.com")
	registerAndLogin(t, r, "u1@example.com", "secret12345")

	// 给其中一个发点额度，确认不是所有行都硬编码 0。
	if w := authJSON(r, http.MethodPost, "/api/v1/admin/credits", token,
		`{"email":"u1@example.com","monthly":12,"addon":3}`); w.Code != http.StatusOK {
		t.Fatalf("发额度失败：%d %s", w.Code, w.Body.String())
	}

	w := authJSON(r, http.MethodGet, "/api/v1/admin/users", token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("列表应当 200，得到 %d；body=%s", w.Code, w.Body.String())
	}
	users, _ := decodeUserList(t, w)

	var found bool
	for _, u := range users {
		if u.Email == "u1@example.com" {
			found = true
			if u.MonthlyCredits != 12 || u.AddonCredits != 3 {
				t.Errorf("u1 的余额应当是 12/3，得到 %d/%d", u.MonthlyCredits, u.AddonCredits)
			}
		}
		if u.Email == "list-admin@example.com" {
			// 管理员自己没发过额度，没有账户行——必须是 0 而不是漏行或报错。
			if u.MonthlyCredits != 0 || u.AddonCredits != 0 {
				t.Errorf("没有账户行的用户余额应当是 0，得到 %d/%d", u.MonthlyCredits, u.AddonCredits)
			}
		}
	}
	if !found {
		t.Error("列表里应当包含 u1@example.com")
	}
}

func TestAdminUsersListSearchAndFilter(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "filter-admin@example.com")
	registerAndLogin(t, r, "alice@example.com", "secret12345")
	registerAndLogin(t, r, "bob@example.com", "secret12345")

	// 搜索必须大小写不敏感：注册时邮箱 ToLower 入库，而运营会按原样粘贴。
	for _, kw := range []string{"alice", "ALICE", "Alice@Example.com"} {
		w := authJSON(r, http.MethodGet, "/api/v1/admin/users?q="+kw, token, "")
		users, _ := decodeUserList(t, w)
		if len(users) != 1 || users[0].Email != "alice@example.com" {
			t.Errorf("q=%q 应当只搜到 alice，得到 %d 条 %+v", kw, len(users), users)
		}
	}

	// 按角色过滤。
	w := authJSON(r, http.MethodGet, "/api/v1/admin/users?role=admin", token, "")
	users, _ := decodeUserList(t, w)
	if len(users) != 1 || users[0].Email != "filter-admin@example.com" {
		t.Errorf("role=admin 应当只有那一个管理员，得到 %+v", users)
	}

	// **打错字的过滤值必须报错，不能返回空列表。** 静默返回空会让运营以为
	// "真的没有管理员"，而实际只是多打了一个 s。
	for _, bad := range []string{"role=admins", "status=band", "status=Active"} {
		w := authJSON(r, http.MethodGet, "/api/v1/admin/users?"+bad, token, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s 应当 400（而不是静默返回空列表），得到 %d", bad, w.Code)
		}
	}
}

func TestAdminUsersListPaginates(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "page-admin@example.com")
	for i := range 4 {
		registerAndLogin(t, r, fmt.Sprintf("p%d@example.com", i), "secret12345")
	}

	// limit=2：应当拿到 2 条 + 一个游标。
	w := authJSON(r, http.MethodGet, "/api/v1/admin/users?limit=2", token, "")
	first, cursor := decodeUserList(t, w)
	if len(first) != 2 {
		t.Fatalf("limit=2 应当返回 2 条，得到 %d", len(first))
	}
	if cursor == "" {
		t.Fatal("还有更多数据时必须给 nextCursor")
	}

	// 翻页不能重复也不能漏。
	w = authJSON(r, http.MethodGet, "/api/v1/admin/users?limit=2&cursor="+cursor, token, "")
	second, _ := decodeUserList(t, w)
	if len(second) == 0 {
		t.Fatal("第二页不该为空")
	}
	seen := map[uint]bool{}
	for _, u := range append(first, second...) {
		if seen[u.ID] {
			t.Errorf("翻页出现重复行 id=%d", u.ID)
		}
		seen[u.ID] = true
	}

	// **非法 cursor 必须报错，不能静默当第一页**：静默的后果是翻页翻着翻着悄悄回到
	// 开头，而使用者以为自己看完了全部。
	w = authJSON(r, http.MethodGet, "/api/v1/admin/users?cursor=not-base64!!", token, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 cursor 应当 400（不能静默当第一页），得到 %d", w.Code)
	}
}

func TestAdminUsersPatchBanTakesEffectImmediately(t *testing.T) {
	// 封禁必须立刻生效，不能等 JWT 过期（7 天）。这也是 Status 第一次真正被写入
	// ——此前全仓没有任何代码写过它，封人只能手改数据库。
	r, db := setupRouterWithDB(t)
	adminToken := adminTokenFor(t, r, db, "ban-admin@example.com")
	victimToken := registerAndLogin(t, r, "victim@example.com", "secret12345")

	// 封禁前能正常访问。
	if w := authJSON(r, http.MethodGet, "/api/v1/me", victimToken, ""); w.Code != http.StatusOK {
		t.Fatalf("前提不成立：封禁前应当能访问 /me，得到 %d", w.Code)
	}

	id := userIDOf(t, db, "victim@example.com")
	w := authJSON(r, http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", id), adminToken,
		`{"status":"banned"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("封禁应当 200，得到 %d；body=%s", w.Code, w.Body.String())
	}
	if got := decodeUserRow(t, w).Status; got != model.StatusBanned {
		t.Errorf("响应里的 status 应当是 banned，得到 %q", got)
	}

	// **同一个 token** 现在必须被拒——不重新登录也要立即失效。
	if w := authJSON(r, http.MethodGet, "/api/v1/me", victimToken, ""); w.Code != http.StatusForbidden {
		t.Errorf("封禁后同一个 token 必须立刻失效（403），得到 %d；"+
			"若这里是 200，说明 status 判定被塞进了 JWT，封禁会有 7 天窗口", w.Code)
	}

	// 解封同样要立刻生效。
	authJSON(r, http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", id), adminToken,
		`{"status":"active"}`)
	if w := authJSON(r, http.MethodGet, "/api/v1/me", victimToken, ""); w.Code != http.StatusOK {
		t.Errorf("解封后应当恢复访问，得到 %d", w.Code)
	}
}

func TestAdminUsersPatchCannotModifySelf(t *testing.T) {
	// **防自锁守卫 1。** 管理员手滑把自己封了或降权了，后台就再也登不进去，
	// 只能连数据库恢复——而这个状态没有任何 UI 出路。
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "self-admin@example.com")
	id := userIDOf(t, db, "self-admin@example.com")

	for _, body := range []string{`{"status":"banned"}`, `{"role":"user"}`} {
		w := authJSON(r, http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", id), token, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("改自己（%s）必须被拒（400），得到 %d；body=%s", body, w.Code, w.Body.String())
		}
	}

	// 确认库里没被改动——报了错但实际改了是最坏的情况。
	var u model.User
	if err := db.First(&u, id).Error; err != nil {
		t.Fatalf("查用户: %v", err)
	}
	if u.Role != model.RoleAdmin || u.Status != model.StatusActive {
		t.Errorf("被拒的请求不该改动任何东西，得到 role=%s status=%s", u.Role, u.Status)
	}
}

func TestAdminUsersPatchKeepsAtLeastOneAdmin(t *testing.T) {
	// 系统里必须始终至少有一个管理员，否则后台永远进不去、只能连数据库恢复。
	//
	// 有意思的是这条不变量在 API 层面**只可能被"降自己"触发**：要降别人必须自己是
	// 管理员，而若目标是最后一个管理员、操作者也是管理员，那目标就是操作者自己
	// ——于是守卫 1（不能改自己）先拦下。handler 里那条"最后一个管理员不能降权"
	// 因此是守卫 1 的兜底，今天走不到；留着是为了守卫 1 哪天被改坏时系统不会被清空。
	r, db := setupRouterWithDB(t)
	adminA := adminTokenFor(t, r, db, "a-admin@example.com")

	// 两个管理员时，降别人是允许的。
	registerAndLogin(t, r, "b-admin@example.com", "secret12345")
	if err := db.Model(&model.User{}).Where("email = ?", "b-admin@example.com").
		Update("role", model.RoleAdmin).Error; err != nil {
		t.Fatalf("提权 B: %v", err)
	}
	idB := userIDOf(t, db, "b-admin@example.com")
	if w := authJSON(r, http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", idB), adminA,
		`{"role":"user"}`); w.Code != http.StatusOK {
		t.Fatalf("还有两个管理员时降权应当成功，得到 %d；body=%s", w.Code, w.Body.String())
	}

	// 现在只剩 A。A 降自己必须被拒，且降完之后系统里仍有管理员。
	idA := userIDOf(t, db, "a-admin@example.com")
	if w := authJSON(r, http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", idA), adminA,
		`{"role":"user"}`); w.Code != http.StatusBadRequest {
		t.Errorf("最后一个管理员降自己必须被拒，得到 %d；body=%s", w.Code, w.Body.String())
	}

	var admins int64
	if err := db.Model(&model.User{}).Where("role = ?", model.RoleAdmin).Count(&admins).Error; err != nil {
		t.Fatalf("统计管理员: %v", err)
	}
	if admins < 1 {
		t.Error("系统里必须始终至少有一个管理员，否则后台永远进不去")
	}
}

func TestAdminUsersPatchValidatesValues(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "val-admin@example.com")
	registerAndLogin(t, r, "target@example.com", "secret12345")
	id := userIDOf(t, db, "target@example.com")
	path := fmt.Sprintf("/api/v1/admin/users/%d", id)

	for _, body := range []string{
		`{"role":"superadmin"}`, // 未知角色
		`{"status":"deleted"}`,  // 未知状态
		`{}`,                    // 没有可改字段
	} {
		w := authJSON(r, http.MethodPatch, path, token, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s 应当 400，得到 %d；body=%s", body, w.Code, w.Body.String())
		}
	}

	// 不存在的用户 404。
	if w := authJSON(r, http.MethodPatch, "/api/v1/admin/users/999999", token,
		`{"status":"banned"}`); w.Code != http.StatusNotFound {
		t.Errorf("不存在的用户应当 404，得到 %d", w.Code)
	}
}

// loginAs 只登录不注册，用于已存在的账号。
func loginAs(t *testing.T, r *gin.Engine, email, password string) string {
	t.Helper()
	w := postJSON(r, "/api/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":%q}`, email, password))
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析登录响应: %v; body=%s", err, w.Body.String())
	}
	token, _ := resp["token"].(string)
	if token == "" {
		t.Fatalf("登录 %s 没拿到 token；body=%s", email, w.Body.String())
	}
	return token
}
