# M3a：次数账本与模型表 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立 `models` / `credit_accounts` / `credit_transactions` 三张表与经过测试的扣费—退款仓储层，并把余额暴露给前端；不涉及任何上游调用。

**Architecture:** 扣费走"事务 + 行锁 + 条件更新"三重保险：事务里先 `SELECT ... FOR UPDATE` 锁住余额行，算出月度/加量包的拆分，再用带条件的 `UPDATE` 落账并校验 `RowsAffected`，最后在同一事务内写不可变流水。拆分明细写进流水，因为退款必须按原拆分还回去。

**Tech Stack:** Go + Gin + GORM（AutoMigrate）；测试用标准 `go test`。（uuid 依赖在 M3b 建 `generations` 表时才需要，本计划用不到。）

**设计文档：** `docs/superpowers/specs/2026-07-28-m3-flux-integration-design.md`

**起点：** 分支 `main`，HEAD `bf4639a`，工作树干净。现有 M1：邮箱注册/登录、JWT 中间件、`GET /me`。

---

## 动手前必读

### 1. 默认 dev 配置下写不出真的并发测试

`internal/database/database.go` 在 `DATABASE_URL` 为空时用临时文件 SQLite，并且**显式设了 `SetMaxOpenConns(1)`**（注释里说明是因为 SQLite 单写者）。

后果：**默认配置下并发请求会被连接池串行化，"并发扣费不超卖"这类测试根本跑不出竞争，绿了也不证明任何事。** 写一个 `go func` 并发跑 100 次扣费然后断言余额非负，在 SQLite 单连接下是必然通过的——它测的是串行执行。

所以本计划这样处理：
- **正确性用确定性测试证明**（余额不足时 `RowsAffected` 为 0、拆分边界、退款往返一致、幂等）。这些不需要竞争就能证明，而且能稳定复现。
- **真并发测试单独一个用例，用 `TEST_DATABASE_URL` 环境变量开关**，只在指向 Postgres 时运行，否则 `t.Skip` 并打印跳过原因。跳过要显式打印，不能静默——静默跳过的测试等于不存在。

> **当前开发机没有 Docker 也没有 Postgres**（2026-07-28 实测：`docker`、`psql` 均不存在，5432 无监听）。因此本计划的并发测试与 Postgres 手工验证步骤**在本机会被跳过**，实现者应如实报告跳过而不是想办法绕过。
>
> **这意味着并发扣费不超卖在本机未经验证。** 这是全系统最容易出错、后果最贵的一处（扣成负数 / 超卖），因此它是**上线前的硬性前置条件**：必须在装有 Postgres 的机器上跑通 `TestConcurrentSpendNeverOversells` 才能对外服务。已记入本计划末尾的已知缺口。

### 2. GORM 的 `FOR UPDATE` 在 SQLite 上是空操作

`clause.Locking{Strength: "UPDATE"}` 在 Postgres 上生成 `SELECT ... FOR UPDATE`，在 SQLite 上被忽略。这没问题：SQLite 侧靠单连接串行化保证正确性，Postgres 侧靠行锁。但**不要因为"SQLite 上没作用"就把它删掉**——生产是 Postgres。

### 3. 拆分明细必须落库

流水表记 `monthly_delta` 和 `addon_delta` 两个字段，不是一个总数。退款按原拆分还回；把加量包次数错还成月度次数，会在月底重置时凭空蒸发。前端 `image-front/lib/fixtures.ts` 里的 `planSpend`/`applySpend`/`applyRefund` 纯函数是这套逻辑的原型，其 22 个单测可直接作为本轮实现的验收参照。

---

## File Structure

```
internal/model/
  model_config.go        models 表（避免与包名 model 冲突，类型名 ImageModel）
  credit.go              CreditAccount + CreditTransaction
internal/credit/
  ledger.go              扣费/退款/发放的唯一入口，只依赖 *gorm.DB
  ledger_test.go         确定性正确性测试 + Postgres 门控的并发测试
internal/handler/
  models.go              GET /api/v1/models
  admin.go               POST /api/v1/admin/credits
  me.go                  (改) 附带 credits
internal/middleware/
  admin.go               role=admin 校验
internal/database/
  database.go            (改) AutoMigrate 新表 + 播种 flux-2-max
internal/server/
  router.go              (改) 注册新路由
  models_test.go         GET /models 接口测试
  admin_test.go          管理员发次数接口测试
```

职责边界：**所有余额变动只能经过 `internal/credit`**。handler 不直接写 `credit_accounts`——那样迟早有人写出漏流水的路径。

---

## Task 1: 数据模型与迁移

**Files:**
- Create: `internal/model/model_config.go`
- Create: `internal/model/credit.go`
- Modify: `internal/database/database.go`

- [ ] **Step 1: 写 models 表模型**

`internal/model/model_config.go`：

```go
package model

import "time"

// ImageModel 是 models 表。类型名不叫 Model 是为了避免与包名 model 读起来像 model.Model。
//
// Provider 决定运行时用哪个 adapter。不同 provider 的上游接口路径、请求体与响应
// 格式**完全不同**（产品要求兼容各家官方功能），差异全部关在各自 adapter 里，
// 本表只存"选哪个 adapter"和"上游模型名"。
type ImageModel struct {
	ID                   string `gorm:"primaryKey;size:64"`
	DisplayName          string `gorm:"size:100;not null"`
	Provider             string `gorm:"size:32;not null"`
	UpstreamModel        string `gorm:"size:100;not null"`
	Credits              int    `gorm:"not null"`
	SupportsImageToImage bool   `gorm:"not null;default:false"`
	Enabled              bool   `gorm:"not null;default:true"`
	SortOrder            int    `gorm:"not null;default:0"`
	CreatedAt            time.Time
	UpdatedAt            time.Time
}
```

- [ ] **Step 2: 写余额与流水模型**

`internal/model/credit.go`：

```go
package model

import "time"

// CreditAccount 是用户的双余额。monthly 随订阅每月重置，addon 一次性购买永不过期。
type CreditAccount struct {
	UserID         uint `gorm:"primaryKey"`
	MonthlyCredits int  `gorm:"not null;default:0"`
	AddonCredits   int  `gorm:"not null;default:0"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// 流水类型。subscription_grant / addon_purchase 留给 Stripe 里程碑。
const (
	TxGenerationCost   = "generation_cost"
	TxGenerationRefund = "generation_refund"
	TxAdminGrant       = "admin_grant"
)

// CreditTransaction 是不可变流水。
//
// MonthlyDelta 与 AddonDelta **分开记**，不合并成一个总数：退款必须按扣费时的
// 拆分还回去。把加量包次数错还成月度次数，会在月底重置时凭空蒸发。
//
// MonthlyAfter / AddonAfter 是变动后的余额快照，用于对账——出问题时能看出是
// 哪一笔开始对不上，而不用把全部流水重放一遍。
type CreditTransaction struct {
	ID           uint   `gorm:"primaryKey"`
	UserID       uint   `gorm:"index;not null"`
	Type         string `gorm:"size:32;not null"`
	MonthlyDelta int    `gorm:"not null"`
	AddonDelta   int    `gorm:"not null"`
	MonthlyAfter int    `gorm:"not null"`
	AddonAfter   int    `gorm:"not null"`
	// GenerationID 关联生成任务；发放类流水为空。退款幂等就是靠
	// (GenerationID, Type=generation_refund) 唯一性判定的。
	GenerationID string `gorm:"index;size:64"`
	Note         string `gorm:"size:255"`
	CreatedAt    time.Time
}
```

- [ ] **Step 3: 迁移并播种模型配置**

修改 `internal/database/database.go` 的 `AutoMigrate` 调用，并在其后播种：

```go
	if err := db.AutoMigrate(
		&model.User{},
		&model.ImageModel{},
		&model.CreditAccount{},
		&model.CreditTransaction{},
	); err != nil {
		return nil, err
	}
	if err := seedModels(db); err != nil {
		return nil, err
	}
	return db, nil
}

// seedModels 幂等地播种内置模型配置。
//
// 用 FirstOrCreate 而不是 Save：Credits 等字段是**运营可改**的（后台调价），
// 每次启动覆盖回默认值会把线上调整悄悄抹掉。
func seedModels(db *gorm.DB) error {
	flux := model.ImageModel{
		ID:                   "flux-2-max",
		DisplayName:          "Flux 2 Max",
		Provider:             "flux",
		UpstreamModel:        "flux-2-max",
		Credits:              1,
		SupportsImageToImage: false,
		Enabled:              true,
		SortOrder:            10,
	}
	return db.Where(model.ImageModel{ID: flux.ID}).FirstOrCreate(&flux).Error
}
```

- [ ] **Step 4: 验证迁移与播种**

```bash
go build ./... && go vet ./...
```

期望：无输出。

- [ ] **Step 5: 提交**

```bash
git add internal/model internal/database
git commit -m "feat: models 与双余额、流水表及模型配置播种"
```

---

## Task 2: 扣费与退款仓储层（TDD，本计划核心）

**Files:**
- Create: `internal/credit/ledger.go`
- Test: `internal/credit/ledger_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/credit/ledger_test.go`：

```go
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
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/credit/ 2>&1 | head -20
```

期望：编译失败，报 `undefined: credit.Spend` 之类。

- [ ] **Step 3: 实现仓储层**

`internal/credit/ledger.go`：

```go
// Package credit 是余额变动的唯一入口。
//
// handler 不得直接写 credit_accounts——绕过本包就意味着漏流水，而漏了流水
// 的余额是无法对账的（出问题时只能猜）。
package credit

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"image-backend/internal/model"
)

// ErrInsufficientCredits 余额不足。调用方据此返回 40001 / HTTP 402。
var ErrInsufficientCredits = errors.New("insufficient credits")

// Split 是一次扣费在两种余额上的分配。
//
// 之所以要把拆分返回并落库，是因为**退款必须按同样的拆分还回去**。
type Split struct {
	Monthly int
	Addon   int
}

// Balance 读余额。账户不存在时返回零值而非报错——新注册用户在拿到第一笔
// 发放前没有账户行，那不是异常。
func Balance(db *gorm.DB, userID uint) (model.CreditAccount, error) {
	var acct model.CreditAccount
	err := db.Where("user_id = ?", userID).First(&acct).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.CreditAccount{UserID: userID}, nil
	}
	if err != nil {
		return model.CreditAccount{}, err
	}
	return acct, nil
}

// planSpend 先扣月度、不足再扣加量包。余额不够返回 false。
func planSpend(acct model.CreditAccount, cost int) (Split, bool) {
	if acct.MonthlyCredits+acct.AddonCredits < cost {
		return Split{}, false
	}
	monthly := cost
	if acct.MonthlyCredits < cost {
		monthly = acct.MonthlyCredits
	}
	return Split{Monthly: monthly, Addon: cost - monthly}, true
}

// Spend 扣费并写流水，返回实际拆分。
//
// 三重保险，缺一不可：
//  1. 整个过程在一个事务里——余额变动与流水必须同生共死；
//  2. SELECT 加 FOR UPDATE 行锁（Postgres 生效；SQLite 忽略，靠单连接串行化）；
//  3. UPDATE 带 WHERE 条件并校验 RowsAffected==1——即使前两层被绕过（比如
//     有人把隔离级别调低），带条件的更新也不会扣成负数。
//
// **不要**改成"先 SELECT 判断、再无条件 UPDATE"：那中间有窗口。
func Spend(db *gorm.DB, userID uint, cost int, generationID string) (Split, error) {
	if cost <= 0 {
		return Split{}, fmt.Errorf("cost 必须为正整数，得到 %d", cost)
	}
	var split Split
	err := db.Transaction(func(tx *gorm.DB) error {
		var acct model.CreditAccount
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ?", userID).First(&acct).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrInsufficientCredits // 没有账户等于没有余额
		}
		if err != nil {
			return err
		}

		s, ok := planSpend(acct, cost)
		if !ok {
			return ErrInsufficientCredits
		}

		res := tx.Model(&model.CreditAccount{}).
			Where("user_id = ? AND monthly_credits >= ? AND addon_credits >= ?",
				userID, s.Monthly, s.Addon).
			Updates(map[string]any{
				"monthly_credits": gorm.Expr("monthly_credits - ?", s.Monthly),
				"addon_credits":   gorm.Expr("addon_credits - ?", s.Addon),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			// 条件不成立：余额在锁之外被改过，或隔离级别不足。
			return ErrInsufficientCredits
		}

		txRow := model.CreditTransaction{
			UserID:       userID,
			Type:         model.TxGenerationCost,
			MonthlyDelta: -s.Monthly,
			AddonDelta:   -s.Addon,
			MonthlyAfter: acct.MonthlyCredits - s.Monthly,
			AddonAfter:   acct.AddonCredits - s.Addon,
			GenerationID: generationID,
		}
		if err := tx.Create(&txRow).Error; err != nil {
			return err
		}
		split = s
		return nil
	})
	return split, err
}

// Refund 按原扣费拆分退回次数。
//
// 幂等条件：该 generation 已有 generation_refund 流水则直接返回。没有对应
// 扣费流水时也静默返回——启动兜底扫描会对"扣费失败但落了 processing 行"的
// 情况调用本函数，那时本就没有要退的东西。
func Refund(db *gorm.DB, userID uint, generationID string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var refunded int64
		if err := tx.Model(&model.CreditTransaction{}).
			Where("generation_id = ? AND type = ?", generationID, model.TxGenerationRefund).
			Count(&refunded).Error; err != nil {
			return err
		}
		if refunded > 0 {
			return nil // 已退过
		}

		var cost model.CreditTransaction
		err := tx.Where("generation_id = ? AND type = ?", generationID, model.TxGenerationCost).
			First(&cost).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // 没扣过，无需退
		}
		if err != nil {
			return err
		}

		// 按原拆分还回：cost 的 delta 是负数，取反即为要加回的数量。
		monthly, addon := -cost.MonthlyDelta, -cost.AddonDelta

		var acct model.CreditAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ?", userID).First(&acct).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.CreditAccount{}).Where("user_id = ?", userID).
			Updates(map[string]any{
				"monthly_credits": gorm.Expr("monthly_credits + ?", monthly),
				"addon_credits":   gorm.Expr("addon_credits + ?", addon),
			}).Error; err != nil {
			return err
		}
		return tx.Create(&model.CreditTransaction{
			UserID:       userID,
			Type:         model.TxGenerationRefund,
			MonthlyDelta: monthly,
			AddonDelta:   addon,
			MonthlyAfter: acct.MonthlyCredits + monthly,
			AddonAfter:   acct.AddonCredits + addon,
			GenerationID: generationID,
			Note:         "生成失败退回",
		}).Error
	})
}

// Grant 发放次数（管理员操作）。账户不存在时创建。
func Grant(db *gorm.DB, userID uint, monthly, addon int, note string) error {
	if monthly == 0 && addon == 0 {
		return fmt.Errorf("发放数量不能全为 0")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		acct := model.CreditAccount{UserID: userID}
		if err := tx.Where("user_id = ?", userID).
			FirstOrCreate(&acct, model.CreditAccount{UserID: userID}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.CreditAccount{}).Where("user_id = ?", userID).
			Updates(map[string]any{
				"monthly_credits": gorm.Expr("monthly_credits + ?", monthly),
				"addon_credits":   gorm.Expr("addon_credits + ?", addon),
			}).Error; err != nil {
			return err
		}
		return tx.Create(&model.CreditTransaction{
			UserID:       userID,
			Type:         model.TxAdminGrant,
			MonthlyDelta: monthly,
			AddonDelta:   addon,
			MonthlyAfter: acct.MonthlyCredits + monthly,
			AddonAfter:   acct.AddonCredits + addon,
			Note:         note,
		}).Error
	})
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/credit/ -v 2>&1 | tail -30
```

期望：11 个测试通过，`TestConcurrentSpendNeverOversells` 显示 `SKIP` 并打印跳过原因。

- [ ] **Step 5: 在 Postgres 上跑一次并发测试**

```bash
docker compose up -d
sleep 5
TEST_DATABASE_URL="postgres://imageapp:imageapp@localhost:5432/imageapp?sslmode=disable" \
  go test ./internal/credit/ -run TestConcurrentSpendNeverOversells -v
```

期望：PASS（不再是 SKIP）。若失败，说明行锁或条件更新没起作用——**不要放宽断言**，那是真 bug。

- [ ] **Step 6: 提交**

```bash
git add internal/credit
git commit -m "feat: 扣费/退款/发放仓储层，拆分落流水且退款幂等"
```

---

## Task 3: 测试辅助函数（先做，后面三个任务都要用）

**Files:**
- Modify: `internal/server/router_test.go`

现有 `internal/server/router_test.go` 里有 `setupRouter(t) *gin.Engine`，**只返回 router 不返回 db**；`auth_test.go` 里有 `postJSON`。但后面的测试需要 db（发放次数、把用户提权成 admin），而"注册并登录取 token"这段逻辑现在是在 `me_test.go` 里内联的。

**注意：现有测试用的是 `package server`（内部测试包），不是 `package server`。** 新增测试文件必须同样用 `package server`，否则调不到 `setupRouter` 这些未导出的辅助函数。

- [ ] **Step 1: 加两个辅助函数**

在 `internal/server/router_test.go` 中，把 `setupRouter` 改为委托实现并新增两个辅助函数（保持 `setupRouter` 签名不变，避免动现有调用方）：

```go
// setupRouterWithDB 同时返回 db，供需要直接操作数据（发放次数、提权）的测试使用。
func setupRouterWithDB(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{JWTSecret: "test-secret"}
	return NewRouter(db, cfg), db
}

func setupRouter(t *testing.T) *gin.Engine {
	t.Helper()
	r, _ := setupRouterWithDB(t)
	return r
}

// registerAndLogin 注册并登录，返回 JWT。原先这段逻辑内联在 me_test.go 里。
func registerAndLogin(t *testing.T, r *gin.Engine, email, password string) string {
	t.Helper()
	body := `{"email":"` + email + `","password":"` + password + `"}`
	postJSON(r, "/api/v1/auth/register", body)
	w := postJSON(r, "/api/v1/auth/login", body)
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析登录响应: %v; body=%s", err, w.Body.String())
	}
	token, _ := resp["token"].(string)
	if token == "" {
		t.Fatalf("登录未返回 token: %s", w.Body.String())
	}
	return token
}
```

import 需补 `encoding/json` 与 `gorm.io/gorm`。

- [ ] **Step 2: 确认现有测试仍通过**

```bash
go test ./internal/server/ -v 2>&1 | tail -15
```

期望：原有测试全部通过（`setupRouter` 行为未变）。

- [ ] **Step 3: 提交**

```bash
git add internal/server/router_test.go
git commit -m "test: 抽出 setupRouterWithDB 与 registerAndLogin 辅助函数"
```

---

## Task 4: GET /api/v1/models

**Files:**
- Create: `internal/handler/models.go`
- Modify: `internal/server/router.go`
- Test: `internal/server/models_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/server/models_test.go`：

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListModelsReturnsSeededFlux(t *testing.T) {
	r := setupRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Models []struct {
			ID                   string `json:"id"`
			Name                 string `json:"name"`
			Credits              int    `json:"credits"`
			SupportsImageToImage bool   `json:"supportsImageToImage"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, w.Body.String())
	}
	if len(body.Models) != 1 {
		t.Fatalf("模型数量: got %d, want 1", len(body.Models))
	}
	m := body.Models[0]
	if m.ID != "flux-2-max" || m.Name != "Flux 2 Max" || m.Credits != 1 {
		t.Fatalf("模型内容错误: %+v", m)
	}
}
```


- [ ] **Step 2: 运行确认失败**

```bash
go test ./internal/server/ -run TestListModels -v
```

期望：FAIL（404，路由不存在）。

- [ ] **Step 3: 实现 handler**

`internal/handler/models.go`：

```go
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

type ModelsHandler struct {
	DB *gorm.DB
}

// modelResponse 的字段名与前端 image-front 的 ImageModel 类型一一对应。
// 改这里就要同步改前端 lib/generation-types.ts。
type modelResponse struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Credits              int    `json:"credits"`
	SupportsImageToImage bool   `json:"supportsImageToImage"`
}

// Get 返回启用的模型，按 sort_order 排序。公开接口——定价页与落地页都可能要展示。
func (h *ModelsHandler) Get(c *gin.Context) {
	var rows []model.ImageModel
	if err := h.DB.Where("enabled = ?", true).Order("sort_order asc, id asc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	out := make([]modelResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, modelResponse{
			ID:                   m.ID,
			Name:                 m.DisplayName,
			Credits:              m.Credits,
			SupportsImageToImage: m.SupportsImageToImage,
		})
	}
	c.JSON(http.StatusOK, gin.H{"models": out})
}
```

- [ ] **Step 4: 注册路由**

在 `internal/server/router.go` 中，`api.POST("/auth/login", ...)` 之后加：

```go
	modelsHandler := &handler.ModelsHandler{DB: db}
	api.GET("/models", modelsHandler.Get)
```

- [ ] **Step 5: 运行确认通过**

```bash
go test ./internal/server/ -run TestListModels -v
```

期望：PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/handler/models.go internal/server
git commit -m "feat: GET /models 返回启用的模型配置"
```

---

## Task 5: /me 附带余额

**Files:**
- Modify: `internal/handler/me.go`
- Test: `internal/server/me_test.go`

- [ ] **Step 1: 追加失败的测试**

在 `internal/server/me_test.go` 末尾追加：

```go
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
```

import 需补 `image-backend/internal/credit` 与 `image-backend/internal/model`。这两个测试追加在既有 `me_test.go`（`package server`）末尾。

- [ ] **Step 2: 运行确认失败**

```bash
go test ./internal/server/ -run TestMeIncludesCredits -v
```

期望：FAIL（响应里没有 `credits` 字段，读到 0/0 而期望 7/3）。

- [ ] **Step 3: 改 handler**

修改 `internal/handler/me.go` 的成功响应分支：

```go
	bal, err := credit.Balance(h.DB, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":    user.ID,
		"email": user.Email,
		"role":  user.Role,
		"credits": gin.H{
			"monthly": bal.MonthlyCredits,
			"addon":   bal.AddonCredits,
		},
	})
```

顶部补 `import "image-backend/internal/credit"`。

- [ ] **Step 4: 运行确认通过**

```bash
go test ./internal/server/ -v 2>&1 | tail -20
```

期望：全部通过，含两个新测试。

- [ ] **Step 5: 提交**

```bash
git add internal/handler/me.go internal/server/me_test.go
git commit -m "feat: /me 返回双余额"
```

---

## Task 6: 管理员发放次数接口

**Files:**
- Create: `internal/middleware/admin.go`
- Create: `internal/handler/admin.go`
- Modify: `internal/server/router.go`
- Test: `internal/server/admin_test.go`

存在理由：内测要反复给测试账号发次数。手工 SQL 既容易写错，又**不留流水**——而流水是出问题时唯一能对账的东西。

- [ ] **Step 1: 写失败的测试**

`internal/server/admin_test.go`：

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"image-backend/internal/model"
)

func TestAdminGrantRequiresAdminRole(t *testing.T) {
	r := setupRouter(t)
	token := registerAndLogin(t, r, "plain@example.com", "secret12345")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credits",
		strings.NewReader(`{"email":"plain@example.com","monthly":10,"addon":0}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("普通用户应当 403: got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestAdminGrantAddsCredits(t *testing.T) {
	r, db := setupRouterWithDB(t)
	adminToken := registerAndLogin(t, r, "admin@example.com", "secret12345")
	registerAndLogin(t, r, "target@example.com", "secret12345")

	// 提权：注册接口不会创建 admin，只能直接改库
	if err := db.Model(&model.User{}).Where("email = ?", "admin@example.com").
		Update("role", "admin").Error; err != nil {
		t.Fatalf("提权: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credits",
		strings.NewReader(`{"email":"target@example.com","monthly":50,"addon":10}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
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
	if body.Credits.Monthly != 50 || body.Credits.Addon != 10 {
		t.Fatalf("发放后余额: got %d/%d, want 50/10", body.Credits.Monthly, body.Credits.Addon)
	}
}

func TestAdminGrantUnknownEmailReturns404(t *testing.T) {
	r, db := setupRouterWithDB(t)
	adminToken := registerAndLogin(t, r, "admin2@example.com", "secret12345")
	if err := db.Model(&model.User{}).Where("email = ?", "admin2@example.com").
		Update("role", "admin").Error; err != nil {
		t.Fatalf("提权: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credits",
		strings.NewReader(`{"email":"nobody@example.com","monthly":10,"addon":0}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("未知邮箱应当 404: got %d; body=%s", w.Code, w.Body.String())
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./internal/server/ -run TestAdminGrant -v
```

期望：FAIL（404，路由不存在）。

- [ ] **Step 3: 实现 admin 中间件**

`internal/middleware/admin.go`：

```go
package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

// RequireAdmin 必须挂在 Auth 之后——它依赖 Auth 放进 context 的 userID。
//
// 每次请求查一次库而不是把 role 塞进 JWT：role 变更（封禁、降权）要能立即生效，
// 而 JWT 有 7 天有效期，塞进去就意味着降权后还有 7 天的窗口。
func RequireAdmin(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetUint(CtxUserIDKey)
		var user model.User
		if err := db.First(&user, userID).Error; err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"code": 40300, "message": "forbidden"})
			return
		}
		if user.Role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"code": 40300, "message": "forbidden"})
			return
		}
		c.Next()
	}
}
```

- [ ] **Step 4: 实现 handler**

`internal/handler/admin.go`：

```go
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/credit"
	"image-backend/internal/model"
)

type AdminHandler struct {
	DB *gorm.DB
}

type grantRequest struct {
	Email   string `json:"email" binding:"required,email"`
	Monthly int    `json:"monthly"`
	Addon   int    `json:"addon"`
}

// GrantCredits 给指定邮箱的用户发放次数。内测期间替代手工 SQL——手工改库不留流水。
func (h *AdminHandler) GrantCredits(c *gin.Context) {
	var req grantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "invalid request body"})
		return
	}
	var user model.User
	if err := h.DB.Where("email = ?", req.Email).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": 40400, "message": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	if err := credit.Grant(h.DB, user.ID, req.Monthly, req.Addon, "admin grant"); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": err.Error()})
		return
	}
	bal, err := credit.Balance(h.DB, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"email": user.Email,
		"credits": gin.H{
			"monthly": bal.MonthlyCredits,
			"addon":   bal.AddonCredits,
		},
	})
}
```

- [ ] **Step 5: 注册路由**

在 `internal/server/router.go` 里 `authed` 组之后加：

```go
	adminHandler := &handler.AdminHandler{DB: db}
	admin := authed.Group("/admin", middleware.RequireAdmin(db))
	admin.POST("/credits", adminHandler.GrantCredits)
```

- [ ] **Step 6: 运行全部测试**

```bash
go build ./... && go vet ./... && go test ./... 2>&1 | tail -15
```

期望：全部包通过。

- [ ] **Step 7: 提交**

```bash
git add internal/middleware/admin.go internal/handler/admin.go internal/server
git commit -m "feat: 管理员发放次数接口，role 每请求查库以便降权立即生效"
```

---

## Task 7: 文档与手工验证

**Files:**
- Modify: `README.md`
- Modify: `.env.example`

- [ ] **Step 1: `.env.example` 补充**

追加：

```
# Postgres：内测起就应该用它。留空会退化成临时文件 SQLite，
# 进程一停账号与余额全没——测试期间反复重发次数会污染你对扣费正确性的判断。
DATABASE_URL=postgres://imageapp:imageapp@localhost:5432/imageapp?sslmode=disable

# 仅测试用：指向 Postgres 时才会运行并发扣费测试。
# 留空时该测试会 SKIP 并打印原因（SQLite 单连接会串行化并发，测不出竞争）。
TEST_DATABASE_URL=
```

- [ ] **Step 2: README 增加"次数账本"一节**

在 API 表之后插入：

```markdown
## 次数账本

双余额：`monthly_credits` 随订阅每月重置，`addon_credits` 一次性购买永不过期。
扣费**先扣月度、不足再扣加量包**。

**所有余额变动只能经过 `internal/credit`。** handler 不得直接写 `credit_accounts`——
绕过它就意味着漏流水，而漏了流水的余额无法对账，出问题时只能猜。

`credit_transactions` 把 `monthly_delta` 与 `addon_delta` **分开记**，不合并成一个总数：
退款必须按扣费时的拆分还回去。把加量包次数错还成月度次数，会在月底重置时凭空蒸发。

扣费的三重保险（缺一不可，见 `internal/credit/ledger.go` 注释）：
事务包裹 + `SELECT ... FOR UPDATE` 行锁 + 带条件的 `UPDATE` 并校验 `RowsAffected`。
**不要**改成"先 SELECT 判断、再无条件 UPDATE"——那中间有窗口。

### 并发测试需要 Postgres

`internal/database/database.go` 在 dev 模式下用临时 SQLite 且 `SetMaxOpenConns(1)`。
连接池会把并发请求**串行化**，所以在默认配置下并发扣费测试必然通过且什么都没证明。
真并发测试由 `TEST_DATABASE_URL` 门控，未设置时显式 SKIP 并打印原因：

```bash
docker compose up -d
TEST_DATABASE_URL="postgres://imageapp:imageapp@localhost:5432/imageapp?sslmode=disable" \
  go test ./internal/credit/ -run TestConcurrentSpend -v
```

### 给测试账号发次数

注册接口不会创建管理员，第一个 admin 只能直接改库：

```sql
UPDATE users SET role = 'admin' WHERE email = 'you@example.com';
```

之后用接口发放（会留流水，比手工 SQL 可追溯）：

```bash
curl -X POST localhost:8080/api/v1/admin/credits \
  -H "Authorization: Bearer <admin-token>" -H 'Content-Type: application/json' \
  -d '{"email":"tester@example.com","monthly":50,"addon":0}'
```
```

- [ ] **Step 3: 手工端到端验证**

```bash
docker compose up -d && sleep 5
DATABASE_URL="postgres://imageapp:imageapp@localhost:5432/imageapp?sslmode=disable" \
  JWT_SECRET="local-test-secret-not-the-default" PORT=8080 go run ./cmd/server &
sleep 3
curl -s localhost:8080/api/v1/models
curl -s -X POST localhost:8080/api/v1/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"m3a@example.com","password":"secret12345"}'
TOKEN=$(curl -s -X POST localhost:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"m3a@example.com","password":"secret12345"}' | sed -E 's/.*"token":"([^"]+)".*/\1/')
curl -s localhost:8080/api/v1/me -H "Authorization: Bearer $TOKEN"
```

期望依次：模型列表含 `flux-2-max` / `Flux 2 Max` / `credits:1`；注册 201；`/me` 返回 `"credits":{"monthly":0,"addon":0}`。

然后提权并发放，确认 `/me` 余额变化。**记得最后停掉这个进程**。

- [ ] **Step 4: 提交**

```bash
git add README.md .env.example
git commit -m "docs: 次数账本说明、并发测试需 Postgres 的原因与发放流程"
```

---

## 不在本计划范围内（属于 M3b）

- adapter 接口、stub adapter、Flux adapter
- `generations` 表与 `POST /api/v1/generations`
- 启动兜底扫描卡住的 `processing` 行
- 前端四个 Route Handler 切换、删 `lib/fixtures.ts`
- 前端 e2e 适配（关键词触发要挪到后端 stub adapter）

## 已知缺口

- 第一个管理员只能靠改库提权，没有引导流程
- 发放接口无审计日志（只有 `credit_transactions` 的 note 字段）
- 月度重置尚无实现——那要等 Stripe 的 `invoice.paid` 事件
- 登录接口无速率限制（M1 遗留），公网部署前必须补
- **并发扣费未在真实 Postgres 上验证**（开发机无 Docker/Postgres）。`TestConcurrentSpendNeverOversells` 会 Skip。**上线前必须在装有 Postgres 的机器上跑通这条测试**——它覆盖的是扣成负数/超卖，是全系统后果最贵的失效模式
- 内测若继续用临时 SQLite，进程一停账号与余额全没；正式内测前需要 Docker Desktop 或本机 Postgres
