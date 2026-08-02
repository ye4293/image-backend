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

// genRow 构造一行生成记录（不落库）。
//
// 抽出来是因为批量插入不能走 insertGen 的逐行 Create——钳制测试要 55 行，那是
// 55 次往返，而 server 包已经跑到 ~16 秒。
func genRow(id string, userID uint, createdAt time.Time, status string, stored bool) model.Generation {
	return model.Generation{
		ID: id, UserID: userID, Model: "flux-2-max", Prompt: "p",
		AspectRatio: "1:1", Width: 1024, Height: 1024,
		Status: status, ImageURL: "https://img.example.com/" + id + ".png",
		Stored: stored, CreatedAt: createdAt,
	}
}

// insertGen 直接落一行生成记录（stored=true）。
//
// 不走 POST /generations：stub 默认延迟 15 秒，而分页测试要插好几行；而且这里
// 需要精确控制 created_at 来构造游标边界。
func insertGen(t *testing.T, db *gorm.DB, id string, userID uint, createdAt time.Time, status string) {
	t.Helper()
	insertGenStored(t, db, id, userID, createdAt, status, true)
}

// insertGenStored 是 insertGen 的显式 stored 变体，供需要区分转存状态的测试使用。
func insertGenStored(t *testing.T, db *gorm.DB, id string, userID uint, createdAt time.Time, status string, stored bool) {
	t.Helper()
	g := genRow(id, userID, createdAt, status, stored)
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
	// 插**超过 maxListLimit** 行（55 > 50）：只插 3 行的话 limit=999 无论上限是
	// 50 还是 5000 都返回 3 行，对上界的断言完全是空的。
	// CreateInBatches 一次落库，而不是 55 次往返——server 包已经跑到 ~16 秒。
	rows55 := make([]model.Generation, 0, 55)
	for i := 0; i < 55; i++ {
		rows55 = append(rows55, genRow(fmt.Sprintf("lim-%02d", i), u.ID,
			base.Add(time.Duration(i)*time.Second), model.GenStatusSucceeded, true))
	}
	if err := db.CreateInBatches(rows55, 55).Error; err != nil {
		t.Fatalf("批量插入: %v", err)
	}

	// limit=999 必须被钳到恰好 maxListLimit，不是"某个 <= 50 的数"。
	rows, _ := decodeList(t, getList(r, token, "?limit=999"))
	if len(rows) != 50 {
		t.Errorf("?limit=999 应当被钳到 50 行, got %d", len(rows))
	}

	// limit=0 与非法值都要被钳制而不是报错——上游客户端传个 0 是常见的
	// off-by-one，回 400 只是把问题推给调用方。
	for _, q := range []string{"?limit=0", "?limit=abc", "?limit=-5"} {
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
	// 两行**不同** stored 值，而不是只插一行 true：只断言 true 的话，一个把
	// out["stored"] 硬编码成 true 的实现照样全绿——而这个字段的全部意义就是区分
	// 永久链接与约一小时后失效的临时链接，认不出"永远 true"就等于没测。
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "list-stored@example.com", "secret12345")
	var u model.User
	db.Where("email = ?", "list-stored@example.com").First(&u)

	base := time.Now().UTC().Truncate(time.Second)
	insertGenStored(t, db, "stored-yes", u.ID, base.Add(time.Second), model.GenStatusSucceeded, true)
	insertGenStored(t, db, "stored-no", u.ID, base, model.GenStatusSucceeded, false)

	rows, _ := decodeList(t, getList(r, token, ""))
	if len(rows) != 2 {
		t.Fatalf("行数: %d", len(rows))
	}
	want := map[string]bool{"stored-yes": true, "stored-no": false}
	for _, row := range rows {
		id, _ := row["id"].(string)
		expected, ok := want[id]
		if !ok {
			t.Errorf("意外的行 %q", id)
			continue
		}
		if row["stored"] != expected {
			t.Errorf("%s 的 stored: got %v, want %v", id, row["stored"], expected)
		}
	}
}

// TestListPaginatesRowsWithGormAssignedTimestamps 用 **GORM 自动填充的 created_at**
// 翻页，而不是测试里显式传进去的 UTC 时间。
//
// 存在的唯一理由是补上一个真实漏过的 bug：本文件其他所有分页测试都自己传
// time.Now().UTC()，于是"GORM 按什么时区写库"这条路径从来没被走过。生产上
// created_at 由 GORM 的 autoCreateTime 填，glebarez/sqlite 又把 time.Time 序列化成
// **带时区的字符串**；在 +08:00 的机器上库里是 "14:xx+08:00"，而游标参数是
// "06:xx+00:00"，SQLite 做字符串比较得出 "14" > "06"，于是 created_at < cursor
// 恒假——第二页永远是空的，而全部 Go 测试照样绿。这个 bug 是靠浏览器端到端测试
// 才发现的。
//
// 断言"翻完能拿到全部 3 行"就锁住了这条路径：只要有人把 NowFunc 的 UTC 去掉，
// 这里立刻红。
func TestListPaginatesRowsWithGormAssignedTimestamps(t *testing.T) {
	r, db := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "list-gormts@example.com", "secret12345")
	var u model.User
	db.Where("email = ?", "list-gormts@example.com").First(&u)

	// **刻意不设 CreatedAt**，让 GORM 自己填——这正是生产的写入路径。
	for _, id := range []string{"ts-a", "ts-b", "ts-c"} {
		g := model.Generation{
			ID: id, UserID: u.ID, Model: "flux-2-max", Prompt: "p",
			AspectRatio: "1:1", Width: 1024, Height: 1024,
			Status: model.GenStatusSucceeded, ImageURL: "https://img.example.com/" + id + ".png",
			Stored: true,
		}
		if err := db.Create(&g).Error; err != nil {
			t.Fatalf("插入 %s: %v", id, err)
		}
		// 拉开一点时间，避免三行撞在同一纳秒上——那样考的就变成 id 兜底了。
		time.Sleep(2 * time.Millisecond)
	}

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
		t.Fatalf("按 GORM 填的 created_at 翻页应当拿到全部 3 行，实际 %v——"+
			"很可能是时间戳没有以 UTC 入库，SQLite 字符串比较把游标条件变成了恒假", seen)
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		if uniq[id] {
			t.Fatalf("重复返回 %s: %v", id, seen)
		}
		uniq[id] = true
	}
}
