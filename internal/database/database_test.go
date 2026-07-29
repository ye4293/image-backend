package database

import (
	"path/filepath"
	"strings"
	"testing"

	"image-backend/internal/model"
)

func TestOpenMigratesUserTable(t *testing.T) {
	db, err := Open("") // 空 DSN → dev 模式 SQLite（临时文件库）
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	u := model.User{Email: "a@b.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}
	// 验证 Role/Status 数据库默认值
	var got model.User
	if err := db.First(&got, u.ID).Error; err != nil {
		t.Fatalf("load user failed: %v", err)
	}
	if got.Role != "user" || got.Status != "active" {
		t.Fatalf("expected default role=user status=active, got role=%q status=%q", got.Role, got.Status)
	}
	dup := model.User{Email: "a@b.com", PasswordHash: "y"}
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("expected unique constraint error for duplicate email")
	}
}

// 回归测试：dev 模式数据库必须在连接池新开连接后仍可见同一份数据。
// 若用 ":memory:" 内存库（glebarez 驱动不支持跨连接共享），新连接会得到独立空库，
// 出现 "no such table: users"。SetMaxIdleConns(0) 强制每次操作新建连接来复现该场景。
func TestOpenDevModeSurvivesNewConnections(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB failed: %v", err)
	}
	sqlDB.SetMaxIdleConns(0) // 不保留空闲连接，强制后续操作走新连接
	u := model.User{Email: "fresh-conn@b.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user on fresh connection failed (in-memory db not shared across connections?): %v", err)
	}
}

func TestCreditTxHasBothUniqueIndexes(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	var ddl []string
	// SQLite：从 sqlite_master 读索引定义。这是**实测**而不是相信 GORM 标签——
	// 同一列挂两个 uniqueIndex 的写法能不能生效，只有 DDL 说了算。
	if err := db.Raw(
		"SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name='credit_transactions'").
		Scan(&ddl).Error; err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(ddl, "\n")
	for _, want := range []string{"idx_credit_tx_gen_type", "idx_credit_tx_ext_type"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少唯一索引 %s，实际 DDL:\n%s", want, joined)
		}
	}
	if strings.Count(joined, "UNIQUE") < 2 {
		t.Errorf("两个索引都应是 UNIQUE，实际 DDL:\n%s", joined)
	}
}

func TestSeedPlansIsIdempotentAndDoesNotClobber(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	// 模拟运营调价 + 播种命令回填了 Price ID
	if err := db.Model(&model.Plan{}).Where("id = ?", "pro").
		Updates(map[string]any{"price_usd_cents": 3990, "stripe_price_id": "price_manual"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := seedPlans(db); err != nil {
		t.Fatal(err)
	}
	var p model.Plan
	if err := db.First(&p, "id = ?", "pro").Error; err != nil {
		t.Fatal(err)
	}
	if p.PriceUSDCents != 3990 {
		t.Errorf("播种覆盖了运营调的价：期望 3990，得到 %d", p.PriceUSDCents)
	}
	if p.StripePriceID != "price_manual" {
		t.Errorf("播种抹掉了 StripePriceID，会导致重复创建 Stripe Price：得到 %q", p.StripePriceID)
	}
	var n int64
	db.Model(&model.Plan{}).Count(&n)
	if n != 3 {
		t.Errorf("期望 3 个档位，得到 %d", n)
	}
}

// TestOpenWithFilePathPersistsAcrossReopen 钉住"文件路径的 DATABASE_URL 数据持久"。
//
// 这条不是为了测 SQLite（那是驱动的事），是为了钉住 Open 的**分支判断**：早先的
// 实现把任何非空 databaseURL 都交给 Postgres 驱动，于是本地开发只有"临时库"一个
// 选项——重启即失忆。订阅联调跨越多次重启，在那种库上走不完；而 cmd/seed-stripe
// 在临时库上跑还会把真花钱建出来的 Price ID 扔掉。
func TestOpenWithFilePathPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")
	email := "persist@example.com"

	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db1.Create(&model.User{Email: email, Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db1.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 上不关连接，t.TempDir() 的清理会因为文件仍被占用而让测试失败
	// （断言本身早就过了，失败发生在收尾阶段）。
	t.Cleanup(func() {
		if sqlDB, err := db2.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	var n int64
	if err := db2.Model(&model.User{}).Where("email = ?", email).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("重开同一路径应当读回那行，得到 %d 行——DATABASE_URL 指文件时数据没有持久", n)
	}
}

// TestOpenEmptyURLIsEphemeralAndIsolated 空 URL 必须仍然是一次性的独立库。
//
// 大量测试依赖这个隔离性（每次 Open("") 拿到自己的空库），所以加了文件路径分支
// 之后要确认没把它破坏掉。IsEphemeral 是 cmd/seed-stripe 那道守卫的依据，一起钉住。
func TestOpenEmptyURLIsEphemeralAndIsolated(t *testing.T) {
	if !IsEphemeral("") {
		t.Error(`IsEphemeral("") 必须为 true——cmd/seed-stripe 靠它拒绝在一次性库上创建真实 Stripe 对象`)
	}
	if IsEphemeral("./local.db") {
		t.Error(`IsEphemeral("./local.db") 必须为 false`)
	}

	dbA, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if err := dbA.Create(&model.User{Email: "a@example.com", Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	dbB, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := dbB.Model(&model.User{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("两次 Open(\"\") 应当互相看不见，第二个库里却有 %d 行用户", n)
	}
}
