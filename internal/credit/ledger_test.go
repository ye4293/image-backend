package credit_test

import (
	"os"
	"sync"
	"testing"

	"image-backend/internal/credit"
	"image-backend/internal/database"
	"image-backend/internal/model"

	"gorm.io/gorm"
)

func newDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := database.Open(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return db
}

// seedUser 建一个用户并给定初始余额。
func seedUser(t *testing.T, db *gorm.DB, monthly, addon int) uint {
	t.Helper()
	u := model.User{Email: "u" + t.Name() + "@example.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	acct := model.CreditAccount{UserID: u.ID, MonthlyCredits: monthly, AddonCredits: addon}
	if err := db.Create(&acct).Error; err != nil {
		t.Fatalf("create account: %v", err)
	}
	return u.ID
}

func TestSpendUsesMonthlyFirst(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 10, 5)

	split, err := credit.Spend(db, uid, 3, "gen-1")
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if split.Monthly != 3 || split.Addon != 0 {
		t.Fatalf("拆分错误: got %+v, want {3 0}", split)
	}

	bal, err := credit.Balance(db, uid)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.MonthlyCredits != 7 || bal.AddonCredits != 5 {
		t.Fatalf("余额错误: got %d/%d, want 7/5", bal.MonthlyCredits, bal.AddonCredits)
	}
}

func TestSpendSpillsIntoAddon(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 2, 10)

	split, err := credit.Spend(db, uid, 5, "gen-1")
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if split.Monthly != 2 || split.Addon != 3 {
		t.Fatalf("拆分错误: got %+v, want {2 3}", split)
	}
}

func TestSpendExactBalanceSucceeds(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 2, 3)
	if _, err := credit.Spend(db, uid, 5, "gen-1"); err != nil {
		t.Fatalf("恰好等于余额应当通过: %v", err)
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 0 || bal.AddonCredits != 0 {
		t.Fatalf("应当扣光: got %d/%d", bal.MonthlyCredits, bal.AddonCredits)
	}
}

func TestSpendOneShortIsRejected(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 2, 2)

	_, err := credit.Spend(db, uid, 5, "gen-1")
	if err != credit.ErrInsufficientCredits {
		t.Fatalf("差一次应当拒绝: got %v", err)
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 2 || bal.AddonCredits != 2 {
		t.Fatalf("被拒绝时不得扣减: got %d/%d", bal.MonthlyCredits, bal.AddonCredits)
	}
	var n int64
	db.Model(&model.CreditTransaction{}).Where("user_id = ?", uid).Count(&n)
	if n != 0 {
		t.Fatalf("被拒绝时不得写流水: got %d 条", n)
	}
}

func TestSpendRejectsNonPositiveCost(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 10, 10)
	for _, cost := range []int{0, -5} {
		if _, err := credit.Spend(db, uid, cost, "gen-1"); err == nil {
			t.Fatalf("cost=%d 应当被拒绝——负数会凭空造出次数", cost)
		}
	}
}

func TestRefundRestoresOriginalSplit(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 1, 10)

	// 月度 1 + 加量包 4，退款必须还回 1 月度 + 4 加量包，而不是 5 月度——
	// 后者会在月底重置时凭空蒸发 4 次。
	split, err := credit.Spend(db, uid, 5, "gen-1")
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if split.Monthly != 1 || split.Addon != 4 {
		t.Fatalf("拆分前提不成立: %+v", split)
	}
	if err := credit.Refund(db, uid, "gen-1"); err != nil {
		t.Fatalf("refund: %v", err)
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 1 || bal.AddonCredits != 10 {
		t.Fatalf("退款未按原拆分还回: got %d/%d, want 1/10", bal.MonthlyCredits, bal.AddonCredits)
	}
}

func TestRefundIsIdempotent(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 10, 0)
	if _, err := credit.Spend(db, uid, 3, "gen-1"); err != nil {
		t.Fatalf("spend: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := credit.Refund(db, uid, "gen-1"); err != nil {
			t.Fatalf("第 %d 次退款报错: %v", i+1, err)
		}
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 10 {
		t.Fatalf("重复退款二次入账: got %d, want 10", bal.MonthlyCredits)
	}
	var n int64
	db.Model(&model.CreditTransaction{}).
		Where("generation_id = ? AND type = ?", "gen-1", model.TxGenerationRefund).Count(&n)
	if n != 1 {
		t.Fatalf("退款流水应当只有 1 条: got %d", n)
	}
}

func TestRefundWithoutSpendIsNoop(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 10, 0)
	if err := credit.Refund(db, uid, "never-spent"); err != nil {
		t.Fatalf("无对应扣费的退款应当静默成功: %v", err)
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 10 {
		t.Fatalf("不该凭空退出次数: got %d", bal.MonthlyCredits)
	}
}

func TestGrantAddsAndRecords(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 0, 0)
	if err := credit.Grant(db, uid, 50, 10, "内测发放"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 50 || bal.AddonCredits != 10 {
		t.Fatalf("发放错误: got %d/%d", bal.MonthlyCredits, bal.AddonCredits)
	}
	var tx model.CreditTransaction
	if err := db.Where("user_id = ? AND type = ?", uid, model.TxAdminGrant).First(&tx).Error; err != nil {
		t.Fatalf("缺少发放流水: %v", err)
	}
	if tx.MonthlyAfter != 50 || tx.AddonAfter != 10 {
		t.Fatalf("流水快照错误: got %d/%d", tx.MonthlyAfter, tx.AddonAfter)
	}
}

func TestBalanceCreatesAccountIfMissing(t *testing.T) {
	db := newDB(t)
	u := model.User{Email: "noacct" + t.Name() + "@example.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	bal, err := credit.Balance(db, u.ID)
	if err != nil {
		t.Fatalf("无账户时读余额应当返回零值而非报错: %v", err)
	}
	if bal.MonthlyCredits != 0 || bal.AddonCredits != 0 {
		t.Fatalf("应当是 0/0: got %d/%d", bal.MonthlyCredits, bal.AddonCredits)
	}
}

// TestConcurrentSpendNeverOversells 只在 TEST_DATABASE_URL 指向 Postgres 时有意义。
//
// 默认 dev 配置用临时 SQLite 且 SetMaxOpenConns(1)，连接池会把并发请求串行化——
// 在那种环境下本测试**必然通过且什么都没证明**。所以显式跳过并打印原因，
// 而不是留一个看起来在测并发、实际在测串行的绿灯。
func TestConcurrentSpendNeverOversells(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("跳过：并发扣费测试需要 TEST_DATABASE_URL 指向 Postgres；" +
			"SQLite 单连接会串行化并发，测不出竞争")
	}
	db := newDB(t)
	uid := seedUser(t, db, 10, 0)

	const workers = 30
	var wg sync.WaitGroup
	ok := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := credit.Spend(db, uid, 1, "gen-concurrent"); err == nil {
				ok <- struct{}{}
			}
		}(i)
	}
	wg.Wait()
	close(ok)

	succeeded := len(ok)
	if succeeded != 10 {
		t.Fatalf("30 个并发各扣 1 次、余额 10，应当恰好成功 10 次: got %d", succeeded)
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 0 || bal.AddonCredits != 0 {
		t.Fatalf("余额应当恰好扣光且不为负: got %d/%d", bal.MonthlyCredits, bal.AddonCredits)
	}
}
