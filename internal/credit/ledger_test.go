package credit_test

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// uniqueSuffix 给每个夹具值一个进程内唯一、跨进程也唯一的后缀。
//
// TEST_DATABASE_URL 为空时每个测试拿一个全新临时 SQLite，怎么起名都无所谓；
// 但一旦指向 Postgres（也就是跑并发测试、也就是上线前那道验证关口），所有测试
// 共用一个**持久**库且没有清理逻辑。用 t.Name() 派生邮箱的话第二次 go test 就
// 会撞唯一约束，多个测试共用字面量 "gen-1" 则会跨测试污染。专门用来验证钱的
// 那套测试自己在 Postgres 上跑不干净，是最糟糕的一种夹具缺陷。
var fixtureSeq atomic.Int64

func uniqueSuffix() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), fixtureSeq.Add(1))
}

// seedUser 建一个用户并给定初始余额。
func seedUser(t *testing.T, db *gorm.DB, monthly, addon int) uint {
	t.Helper()
	u := model.User{Email: "u" + uniqueSuffix() + "@example.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	acct := model.CreditAccount{UserID: u.ID, MonthlyCredits: monthly, AddonCredits: addon}
	if err := db.Create(&acct).Error; err != nil {
		t.Fatalf("create account: %v", err)
	}
	return u.ID
}

// newGenID 每次返回不同的 generation ID，避免跨测试撞 (generation_id, type) 唯一索引。
func newGenID() string { return "gen-" + uniqueSuffix() }

func TestSpendUsesMonthlyFirst(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 10, 5)

	split, err := credit.Spend(db, uid, 3, newGenID())
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

	split, err := credit.Spend(db, uid, 5, newGenID())
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
	if _, err := credit.Spend(db, uid, 5, newGenID()); err != nil {
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

	_, err := credit.Spend(db, uid, 5, newGenID())
	if !errors.Is(err, credit.ErrInsufficientCredits) {
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
		if _, err := credit.Spend(db, uid, cost, newGenID()); err == nil {
			t.Fatalf("cost=%d 应当被拒绝——负数会凭空造出次数", cost)
		}
	}
}

// TestSpendWithoutAccountIsRejected 覆盖"用户根本没有账户行"这条分支：
// 没有账户等于没有余额，必须按余额不足拒绝，而不是把 record not found 抛出去。
func TestSpendWithoutAccountIsRejected(t *testing.T) {
	db := newDB(t)
	u := model.User{Email: "noacct" + uniqueSuffix() + "@example.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := credit.Spend(db, u.ID, 1, newGenID())
	if !errors.Is(err, credit.ErrInsufficientCredits) {
		t.Fatalf("无账户扣费应当返回 ErrInsufficientCredits: got %v", err)
	}
}

// TestSpendRecordsAccurateSnapshot 钉住扣费流水的快照列。
//
// 快照列存在的意义就是对账：它必须等于扣费后账户的真实状态，否则出问题时
// 反而会把人引向错误的结论。
func TestSpendRecordsAccurateSnapshot(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 4, 6)
	genID := newGenID()

	if _, err := credit.Spend(db, uid, 7, genID); err != nil {
		t.Fatalf("spend: %v", err)
	}
	var tx model.CreditTransaction
	if err := db.Where("generation_id = ? AND type = ?", genID, model.TxGenerationCost).
		First(&tx).Error; err != nil {
		t.Fatalf("缺少扣费流水: %v", err)
	}
	if tx.MonthlyDelta != -4 || tx.AddonDelta != -3 {
		t.Fatalf("流水拆分错误: got %d/%d, want -4/-3", tx.MonthlyDelta, tx.AddonDelta)
	}
	bal, _ := credit.Balance(db, uid)
	if tx.MonthlyAfter != bal.MonthlyCredits || tx.AddonAfter != bal.AddonCredits {
		t.Fatalf("快照与账户实际状态不一致: 流水 %d/%d, 账户 %d/%d",
			tx.MonthlyAfter, tx.AddonAfter, bal.MonthlyCredits, bal.AddonCredits)
	}
	if tx.MonthlyAfter != 0 || tx.AddonAfter != 3 {
		t.Fatalf("快照数值错误: got %d/%d, want 0/3", tx.MonthlyAfter, tx.AddonAfter)
	}
}

// TestSpendTwiceOnSameGenerationIsRejected 钉住 (generation_id, type) 唯一索引：
// 同一次生成不能被扣两次费。
func TestSpendTwiceOnSameGenerationIsRejected(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 10, 0)
	genID := newGenID()

	if _, err := credit.Spend(db, uid, 3, genID); err != nil {
		t.Fatalf("首次扣费: %v", err)
	}
	if _, err := credit.Spend(db, uid, 3, genID); !errors.Is(err, credit.ErrAlreadySpent) {
		t.Fatalf("重复扣费应当返回 ErrAlreadySpent: got %v", err)
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 7 {
		t.Fatalf("重复扣费不得真的扣钱（事务应回滚）: got %d, want 7", bal.MonthlyCredits)
	}
}

func TestRefundRestoresOriginalSplit(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 1, 10)
	genID := newGenID()

	// 月度 1 + 加量包 4，退款必须还回 1 月度 + 4 加量包，而不是 5 月度——
	// 后者会在月底重置时凭空蒸发 4 次。
	split, err := credit.Spend(db, uid, 5, genID)
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if split.Monthly != 1 || split.Addon != 4 {
		t.Fatalf("拆分前提不成立: %+v", split)
	}
	if err := credit.Refund(db, genID); err != nil {
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
	genID := newGenID()
	if _, err := credit.Spend(db, uid, 3, genID); err != nil {
		t.Fatalf("spend: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := credit.Refund(db, genID); err != nil {
			t.Fatalf("第 %d 次退款报错: %v", i+1, err)
		}
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 10 {
		t.Fatalf("重复退款二次入账: got %d, want 10", bal.MonthlyCredits)
	}
	var n int64
	db.Model(&model.CreditTransaction{}).
		Where("generation_id = ? AND type = ?", genID, model.TxGenerationRefund).Count(&n)
	if n != 1 {
		t.Fatalf("退款流水应当只有 1 条: got %d", n)
	}
}

// TestRefundGoesToSpenderNotCaller 钉住"退给谁由扣费流水说了算"。
//
// Refund 不接收 userID 正是为了让"退到别人账上"不可表达。本测试证明退款落在
// 真正扣过费的那个用户身上，且旁观者账户分毫未动。
func TestRefundGoesToSpenderNotCaller(t *testing.T) {
	db := newDB(t)
	spender := seedUser(t, db, 10, 0)
	bystander := seedUser(t, db, 10, 0)
	genID := newGenID()

	if _, err := credit.Spend(db, spender, 4, genID); err != nil {
		t.Fatalf("spend: %v", err)
	}
	if err := credit.Refund(db, genID); err != nil {
		t.Fatalf("refund: %v", err)
	}

	sBal, _ := credit.Balance(db, spender)
	if sBal.MonthlyCredits != 10 {
		t.Fatalf("扣费者应当被退回: got %d, want 10", sBal.MonthlyCredits)
	}
	bBal, _ := credit.Balance(db, bystander)
	if bBal.MonthlyCredits != 10 {
		t.Fatalf("旁观者余额不得变动: got %d, want 10", bBal.MonthlyCredits)
	}
	var tx model.CreditTransaction
	if err := db.Where("generation_id = ? AND type = ?", genID, model.TxGenerationRefund).
		First(&tx).Error; err != nil {
		t.Fatalf("缺少退款流水: %v", err)
	}
	if tx.UserID != spender {
		t.Fatalf("退款流水应当挂在扣费者名下: got %d, want %d", tx.UserID, spender)
	}
}

func TestRefundWithoutSpendIsNoop(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 10, 0)
	if err := credit.Refund(db, "never-spent-"+uniqueSuffix()); err != nil {
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
	if tx.GenerationID != nil {
		t.Fatalf("发放流水的 generation_id 必须是 NULL（否则多条发放会撞唯一索引）: got %q", *tx.GenerationID)
	}
}

// TestGrantTwiceDoesNotCollide 发放流水的 generation_id 是 NULL，而 NULL 之间
// 互不相等，所以同一用户多次发放不会撞 (generation_id, type) 唯一索引。
func TestGrantTwiceDoesNotCollide(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 0, 0)
	for i := 0; i < 3; i++ {
		if err := credit.Grant(db, uid, 5, 0, "重复发放"); err != nil {
			t.Fatalf("第 %d 次发放报错（唯一索引把 NULL 当相等了？）: %v", i+1, err)
		}
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 15 {
		t.Fatalf("三次发放 5 应当是 15: got %d", bal.MonthlyCredits)
	}
}

// TestGrantRejectsNegative 钉住"Grant 不是扣款通道"。
//
// Grant 走的是不带 WHERE 守卫、不校验 RowsAffected 的相对 UPDATE，放负数进来
// 能把余额扣成负数——那会直接推翻本包"是余额变动唯一安全路径"的声明。
func TestGrantRejectsNegative(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 10, 10)
	for _, c := range []struct{ monthly, addon int }{{-100, 0}, {0, -1}, {-1, -1}} {
		if err := credit.Grant(db, uid, c.monthly, c.addon, "冲正"); err == nil {
			t.Fatalf("Grant(%d, %d) 应当被拒绝——它没有防止扣成负数的守卫", c.monthly, c.addon)
		}
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 10 || bal.AddonCredits != 10 {
		t.Fatalf("被拒绝的发放不得改动余额: got %d/%d, want 10/10", bal.MonthlyCredits, bal.AddonCredits)
	}
}

// TestGrantCreatesAccountWhenMissing 覆盖账户行不存在的发放路径。
func TestGrantCreatesAccountWhenMissing(t *testing.T) {
	db := newDB(t)
	u := model.User{Email: "grantnew" + uniqueSuffix() + "@example.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := credit.Grant(db, u.ID, 20, 5, "首次发放"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	bal, _ := credit.Balance(db, u.ID)
	if bal.MonthlyCredits != 20 || bal.AddonCredits != 5 {
		t.Fatalf("应当创建账户并发放: got %d/%d, want 20/5", bal.MonthlyCredits, bal.AddonCredits)
	}
	var tx model.CreditTransaction
	if err := db.Where("user_id = ? AND type = ?", u.ID, model.TxAdminGrant).First(&tx).Error; err != nil {
		t.Fatalf("缺少发放流水: %v", err)
	}
	if tx.MonthlyAfter != 20 || tx.AddonAfter != 5 {
		t.Fatalf("新建账户时快照应当从 0 起算: got %d/%d, want 20/5", tx.MonthlyAfter, tx.AddonAfter)
	}
}

// newExternalID 每次返回不同的外部事件 id，避免跨测试撞 (external_id, type) 唯一索引。
func newExternalID() string { return "evt-" + uniqueSuffix() }

// TestResetMonthlySetsNotAdds 钉住 ResetMonthly 与 Grant 的根本区别：**设置**而非累加。
//
// 续费若累加，用不完的次数会攒起来，与定价页承诺的"月度次数不累积到下月"矛盾。
// 同时钉住加量包次数分毫未动——加量包是单独付费买的，永不过期。
func TestResetMonthlySetsNotAdds(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 50, 30) // 月度 50，加量包 30

	err := db.Transaction(func(tx *gorm.DB) error {
		return credit.ResetMonthly(tx, uid, 800, newExternalID(), "订阅续费")
	})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 800 {
		t.Errorf("月度应被**设置**为 800（而非累加成 850），得到 %d", bal.MonthlyCredits)
	}
	if bal.AddonCredits != 30 {
		t.Errorf("加量包不该被动，期望 30，得到 %d", bal.AddonCredits)
	}
}

// TestResetMonthlyCanLowerBalanceOnDowngrade 允许结果低于当前余额（高档降到低档）。
//
// 这与 Grant 拒绝负数不冲突：那里拒绝的是"负的发放量"（会把余额扣成负数），
// 这里是把余额**设**到一个由套餐决定的非负值。
func TestResetMonthlyCanLowerBalanceOnDowngrade(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 3000, 0)

	err := db.Transaction(func(tx *gorm.DB) error {
		return credit.ResetMonthly(tx, uid, 200, newExternalID(), "降档")
	})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 200 {
		t.Errorf("降档应下调到 200，得到 %d", bal.MonthlyCredits)
	}
	// 快照与账户必须对得上，否则对账时无从判断哪笔开始错的。
	var last model.CreditTransaction
	if err := db.Where("user_id = ? AND type = ?", uid, model.TxSubscriptionGrant).
		Order("id desc").First(&last).Error; err != nil {
		t.Fatalf("缺少续费流水: %v", err)
	}
	if last.MonthlyDelta != -2800 || last.MonthlyAfter != 200 {
		t.Errorf("流水应记 delta=-2800 after=200，得到 delta=%d after=%d",
			last.MonthlyDelta, last.MonthlyAfter)
	}
	if last.AddonDelta != 0 || last.AddonAfter != 0 {
		t.Errorf("加量包列不该被动，得到 delta=%d after=%d", last.AddonDelta, last.AddonAfter)
	}
}

// TestResetMonthlyRejectsCallOutsideTransaction 钉住"必须由调用方提供事务"。
//
// webhook 要求"幂等记录与发放同生共死"，脱离事务调用会让 stripe_events 与发放
// 分属两个事务，中间崩溃就永久漏发一次——比重复发放更难发现。
func TestResetMonthlyRejectsCallOutsideTransaction(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 0, 0)

	err := credit.ResetMonthly(db, uid, 800, newExternalID(), "x")
	if err == nil {
		t.Fatal("脱离事务调用必须报错，否则原子性保证形同虚设")
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 0 {
		t.Errorf("被拒绝时不得改动余额，得到 %d", bal.MonthlyCredits)
	}
}

// TestResetMonthlyRejectsDuplicateExternalID 钉住 (external_id, type) 唯一索引：
// 同一个 Stripe 事件重投只发一次额度。
func TestResetMonthlyRejectsDuplicateExternalID(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 0, 0)
	extID := newExternalID()
	run := func() error {
		return db.Transaction(func(tx *gorm.DB) error {
			return credit.ResetMonthly(tx, uid, 800, extID, "x")
		})
	}
	if err := run(); err != nil {
		t.Fatalf("首次发放: %v", err)
	}
	if err := run(); !errors.Is(err, credit.ErrAlreadyGranted) {
		t.Fatalf("同一事件重复发放应返回 ErrAlreadyGranted，得到 %v", err)
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 800 {
		t.Errorf("第二次不该改变余额，得到 %d", bal.MonthlyCredits)
	}
}

// TestResetMonthlyRejectsBadArgs 负数会把余额设成负数；空 externalID 会让多条
// 续费流水在唯一索引上互相冲突，还断了对账线索。
func TestResetMonthlyRejectsBadArgs(t *testing.T) {
	db := newDB(t)
	uid := seedUser(t, db, 100, 0)

	if err := db.Transaction(func(tx *gorm.DB) error {
		return credit.ResetMonthly(tx, uid, -1, newExternalID(), "x")
	}); !errors.Is(err, credit.ErrInvalidGrantAmount) {
		t.Errorf("负数应返回 ErrInvalidGrantAmount，得到 %v", err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return credit.ResetMonthly(tx, uid, 800, "", "x")
	}); err == nil {
		t.Error("空 externalID 必须被拒绝——它既是对账线索也是兜底幂等键")
	}
	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 100 {
		t.Errorf("被拒绝时不得改动余额，得到 %d", bal.MonthlyCredits)
	}
}

// TestResetMonthlyCreatesAccountWhenMissing 覆盖账户行不存在的续费路径：
// 用户可能在拿到第一笔发放前就订阅了。
func TestResetMonthlyCreatesAccountWhenMissing(t *testing.T) {
	db := newDB(t)
	u := model.User{Email: "resetnew" + uniqueSuffix() + "@example.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return credit.ResetMonthly(tx, u.ID, 800, newExternalID(), "首次订阅")
	}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	bal, _ := credit.Balance(db, u.ID)
	if bal.MonthlyCredits != 800 || bal.AddonCredits != 0 {
		t.Fatalf("应当创建账户并发放: got %d/%d, want 800/0", bal.MonthlyCredits, bal.AddonCredits)
	}
	var tx model.CreditTransaction
	if err := db.Where("user_id = ? AND type = ?", u.ID, model.TxSubscriptionGrant).
		First(&tx).Error; err != nil {
		t.Fatalf("缺少续费流水: %v", err)
	}
	if tx.MonthlyDelta != 800 || tx.MonthlyAfter != 800 {
		t.Fatalf("新建账户时快照应当从 0 起算: delta=%d after=%d", tx.MonthlyDelta, tx.MonthlyAfter)
	}
	if tx.GenerationID != nil {
		t.Fatalf("续费流水的 generation_id 必须是 NULL: got %q", *tx.GenerationID)
	}
}

// TestBalanceReturnsZeroWhenAccountMissing —— Balance 明确**不**创建任何东西，
// 它对没有账户行的用户返回零值。
func TestBalanceReturnsZeroWhenAccountMissing(t *testing.T) {
	db := newDB(t)
	u := model.User{Email: "nobal" + uniqueSuffix() + "@example.com", PasswordHash: "x"}
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
	// 读余额不得有副作用：不能悄悄建出账户行来。
	var n int64
	db.Model(&model.CreditAccount{}).Where("user_id = ?", u.ID).Count(&n)
	if n != 0 {
		t.Fatalf("Balance 不该创建账户行: got %d 行", n)
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
			// 每个 worker 用不同的 generation ID：这里要测的是余额上的竞争，
			// 不是 (generation_id, type) 唯一索引。共用一个 ID 的话 29 个请求
			// 会被唯一索引挡掉，测试就从"测超卖"变成了"测幂等"。
			if _, err := credit.Spend(db, uid, 1, newGenID()); err == nil {
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

// TestConcurrentRefundRefundsOnce 与上一条同理，只在 Postgres 上有意义。
//
// 它钉的是"先 Count 再 INSERT"那个窗口：READ COMMITTED 下两个并发退款会各数到
// 0 然后都插进去，退两次。唯一索引才是真正的门，本测试验证那扇门有效。
func TestConcurrentRefundRefundsOnce(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("跳过：并发退款测试需要 TEST_DATABASE_URL 指向 Postgres；" +
			"SQLite 单连接会串行化并发，测不出 Count→INSERT 之间的窗口")
	}
	db := newDB(t)
	uid := seedUser(t, db, 10, 0)
	genID := newGenID()
	if _, err := credit.Spend(db, uid, 4, genID); err != nil {
		t.Fatalf("spend: %v", err)
	}

	const workers = 20
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = credit.Refund(db, genID)
		}()
	}
	wg.Wait()

	bal, _ := credit.Balance(db, uid)
	if bal.MonthlyCredits != 10 {
		t.Fatalf("并发退款重复入账: got %d, want 10", bal.MonthlyCredits)
	}
	var n int64
	db.Model(&model.CreditTransaction{}).
		Where("generation_id = ? AND type = ?", genID, model.TxGenerationRefund).Count(&n)
	if n != 1 {
		t.Fatalf("退款流水应当只有 1 条: got %d", n)
	}
}
