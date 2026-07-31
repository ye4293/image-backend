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
