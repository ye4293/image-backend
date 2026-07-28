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

### 4. 幂等靠数据库唯一约束，不靠"先 Count 再 INSERT"

`(generation_id, type)` 上的复合唯一索引是"退款只退一次"和"一次生成只扣一次"的**唯一权威**。

"先 `Count` 判断、再 `INSERT`" 在 READ COMMITTED 下有窗口：`Count` 不加锁、只看已提交行，两个并发退款会各数到 0，然后都插进去，**退两次款**。在守卫之后才拿的行锁保护不了守卫本身。串行的测试在结构上看不见这个缺陷。

因此 `generation_id` 必须是 `*string`（可空）：发放类流水存 `NULL` 而不是 `''`，否则所有发放记录会在这个唯一索引上互相冲突。SQLite 与 Postgres 都把 NULL 视为互不相等。

### 5. `Refund` 不接收 userID

退给谁由扣费流水的 `user_id` 说了算，不由调用方说了算。若签名是 `Refund(db, userID, generationID)`，handler 只要把 JWT 里的 userID 和请求里的 generationID 一拼，拿别人的 generation ID 就能给自己造钱，还会留下"用户 A 的退款流水指向用户 B 的扣费流水"这种无法对账的脏数据。**把参数删掉比校验它更彻底——错误变得不可表达。**

---

## File Structure

```
internal/model/
  model_config.go        image_models 表（GORM 由 ImageModel 复数化而来，不是 models）
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

- [x] **Step 1: 写 models 表模型**

`internal/model/model_config.go`：

```go
package model

import "time"

// ImageModel 对应 **image_models** 表——GORM 由类型名复数化而来，不是 models。
// 类型名不叫 Model 是为了避免与包名 model 读起来像 model.Model；不用 TableName()
// 把表名覆盖回 models，因为 image_models 本身更自描述，而 GORM 的表名覆盖是后
// 人要去翻代码才能发现的隐式魔法。
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

- [x] **Step 2: 写余额与流水模型**

`internal/model/credit.go`：

```go
package model

import "time"

// CreditAccount 是用户的双余额。monthly 随订阅每月重置，addon 一次性购买永不过期。
//
// UserID 显式关掉自增：GORM 默认会给任何单整型主键加自增，那在 Postgres 上会
// 生成一个毫无意义的 bigserial 序列，还会让 UserID = 0 无法插入。这一列的值
// 永远来自 users.id，不该由本表自己生成。
type CreditAccount struct {
	UserID         uint `gorm:"primaryKey;autoIncrement:false"`
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
	ID           uint `gorm:"primaryKey"`
	UserID       uint `gorm:"index;not null"`
	MonthlyDelta int  `gorm:"not null"`
	AddonDelta   int  `gorm:"not null"`
	MonthlyAfter int  `gorm:"not null"`
	AddonAfter   int  `gorm:"not null"`

	// (GenerationID, Type) 上的复合唯一索引是退款幂等与"一次生成只扣一次"的
	// **唯一权威**。不能只靠"先 Count 再 INSERT"：那两步之间在 READ COMMITTED
	// 下有窗口——两个并发退款都数到 0，然后都插进去，退两次款。唯一键冲突没有
	// 这个窗口。
	//
	// GenerationID 是 *string 而不是 string：发放类流水没有关联生成任务，必须
	// 存 NULL。存 '' 的话所有发放记录会在这个唯一索引上互相冲突。SQLite 与
	// Postgres 都把 NULL 视为互不相等，所以 nil 之间不冲突。
	GenerationID *string `gorm:"uniqueIndex:idx_credit_tx_gen_type,priority:1;size:64"`
	Type         string  `gorm:"uniqueIndex:idx_credit_tx_gen_type,priority:2;size:32;not null"`

	Note      string `gorm:"size:255"`
	CreatedAt time.Time
}
```

- [x] **Step 3: 迁移并播种模型配置**

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

- [x] **Step 4: 验证迁移与播种**

```bash
go build ./... && go vet ./...
```

期望：无输出。

- [x] **Step 5: 提交**

```bash
git add internal/model internal/database
git commit -m "feat: models 与双余额、流水表及模型配置播种"
```

---

## Task 2: 扣费与退款仓储层（TDD，本计划核心）

**Files:**
- Create: `internal/credit/ledger.go`
- Test: `internal/credit/ledger_test.go`

- [x] **Step 1: 写失败的测试**

`internal/credit/ledger_test.go`：

```go
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
```

- [x] **Step 2: 运行测试确认失败**

```bash
go test ./internal/credit/ 2>&1 | head -20
```

期望：编译失败，报 `undefined: credit.Spend` 之类。

- [x] **Step 3: 实现仓储层**

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

// ErrAlreadySpent 同一个 generation 已经扣过费。由 (generation_id, type) 上的
// 唯一索引兜住——重复扣费属于调用方 bug，必须报错而不是静默吞掉。
var ErrAlreadySpent = errors.New("generation already charged")

// errAlreadyRefunded 是内部哨兵：退款流水插入撞唯一键时用它把事务**回滚**掉，
// 再在事务外转成 nil 返回。不能在闭包里直接 return nil——那样上面已经加过钱的
// 那次 UPDATE 会被提交，变成一次没有流水的退款（正是唯一索引要防的双退）。
var errAlreadyRefunded = errors.New("already refunded")

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
//
// 本设计针对 **READ COMMITTED**（Postgres 默认）：FOR UPDATE 阻塞解除后
// Postgres 会 EvalPlanQual 重新求值，所以读到的 acct 是前一个事务提交后的新
// 值而非陈旧快照。在 REPEATABLE READ 下同样的交错会抛序列化失败，需要调用方
// 重试——本包不实现重试。
//
// 同一个 generationID 重复调用会撞 (generation_id, type) 唯一索引并返回
// ErrAlreadySpent，不会扣两次。
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
			GenerationID: genIDPtr(generationID),
		}
		if err := tx.Create(&txRow).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrAlreadySpent
			}
			return err
		}
		split = s
		return nil
	})
	if err != nil {
		return Split{}, err
	}
	return split, nil
}

// Refund 按原扣费拆分退回次数。
//
// **不接收 userID 参数**：退给谁由扣费流水说了算，不由调用方说了算。早先的
// 签名是 Refund(db, userID, generationID)，那样 handler 把 JWT 里的 userID 和
// 请求里的 generationID 一拼，拿别人的 generation ID 就能给自己造钱，还会留下
// "用户 A 的退款流水指向用户 B 的扣费流水"这种无法对账的脏数据。把参数删掉比
// 校验它更彻底——错误变得不可表达。
//
// 幂等靠 (generation_id, type) 唯一索引，不靠"先 Count 再 INSERT"：后者在
// READ COMMITTED 下有窗口，两个并发退款会各数到 0 然后都插进去，退两次。
// Count 只作为省一趟写的快速路径保留。
//
// 没有对应扣费流水时静默返回——启动兜底扫描会对"扣费失败但落了 processing
// 行"的情况调用本函数，那时本就没有要退的东西。
func Refund(db *gorm.DB, generationID string) error {
	err := db.Transaction(func(tx *gorm.DB) error {
		var refunded int64
		if err := tx.Model(&model.CreditTransaction{}).
			Where("generation_id = ? AND type = ?", generationID, model.TxGenerationRefund).
			Count(&refunded).Error; err != nil {
			return err
		}
		if refunded > 0 {
			return errAlreadyRefunded // 快速路径：已退过
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

		// 退给谁由流水说了算。
		userID := cost.UserID
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
		err = tx.Create(&model.CreditTransaction{
			UserID:       userID,
			Type:         model.TxGenerationRefund,
			MonthlyDelta: monthly,
			AddonDelta:   addon,
			MonthlyAfter: acct.MonthlyCredits + monthly,
			AddonAfter:   acct.AddonCredits + addon,
			GenerationID: genIDPtr(generationID),
			Note:         "生成失败退回",
		}).Error
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			// 并发的另一笔退款抢先提交了。回滚本次（含上面那次加钱），
			// 由事务外转成成功——退款本就该是幂等的。
			return errAlreadyRefunded
		}
		return err
	})
	if errors.Is(err, errAlreadyRefunded) {
		return nil
	}
	return err
}

// Grant 发放次数（管理员操作）。账户不存在时创建。
//
// **拒绝负数**：本包声称自己是余额变动的唯一安全路径，而 Grant 走的是不带
// WHERE 守卫、不校验 RowsAffected 的相对 UPDATE。放负数进来等于开了一条零防护
// 的扣款通道，能把余额直接扣成负数。管理员冲正如果将来要做，得走一条和 Spend
// 同样三层防护的独立路径，不能挂在这里。
func Grant(db *gorm.DB, userID uint, monthly, addon int, note string) error {
	if monthly < 0 || addon < 0 {
		return fmt.Errorf("发放数量不能为负（monthly=%d addon=%d）；扣减必须走 Spend", monthly, addon)
	}
	if monthly == 0 && addon == 0 {
		return fmt.Errorf("发放数量不能全为 0")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		// 先确保账户行存在。用 OnConflict DoNothing 而不是 FirstOrCreate：
		// 两个并发发放同时插同一主键时，后插的那个会以唯一键冲突**中止整个
		// 事务**，那个错误会原样抛给调用方。DoNothing 让它退化成一次空操作。
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&model.CreditAccount{UserID: userID}).Error; err != nil {
			return err
		}
		// 加锁重读。快照列必须基于**锁内**读到的值：不加锁的话两个并发 +10
		// 打在 100 上，余额会正确地变成 120（相对 UPDATE 本身是安全的），但两
		// 条流水都会写 MonthlyAfter=110——账本说 110/110、账户说 120，正是快照
		// 列本来要防的那种对账失败。
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
			Type:         model.TxAdminGrant,
			MonthlyDelta: monthly,
			AddonDelta:   addon,
			MonthlyAfter: acct.MonthlyCredits + monthly,
			AddonAfter:   acct.AddonCredits + addon,
			Note:         note,
			// GenerationID 留 nil：发放不关联生成任务，而 NULL 之间互不相等，
			// 多条发放流水不会在 (generation_id, type) 唯一索引上互相打架。
		}).Error
	})
}

// genIDPtr 把空串转成 nil。空串会在 (generation_id, type) 唯一索引上和其他空串
// 冲突，NULL 不会。
func genIDPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
```

- [x] **Step 4: 运行测试确认通过**

```bash
go test ./internal/credit/ -v 2>&1 | tail -30
```

期望：17 个测试通过，`TestConcurrentSpendNeverOversells` 与 `TestConcurrentRefundRefundsOnce` 两条显示 `SKIP` 并打印跳过原因。

**顺手 dump 一次 DDL，确认唯一索引真的建出来了**——只看代码写对了不算数，GORM 的索引标签写错了是静默失效的：

```
CREATE UNIQUE INDEX `idx_credit_tx_gen_type` ON `credit_transactions`(`generation_id`,`type`)
```

同时确认 `credit_accounts.user_id` **没有** `AUTOINCREMENT`（GORM 默认会给单整型主键加上，靠 `autoIncrement:false` 关掉）。

- [x] **Step 5: 在 Postgres 上跑一次并发测试**

```bash
docker compose up -d
sleep 5
TEST_DATABASE_URL="postgres://imageapp:imageapp@localhost:5432/imageapp?sslmode=disable" \
  go test ./internal/credit/ -run TestConcurrentSpendNeverOversells -v
```

期望：PASS（不再是 SKIP）。若失败，说明行锁或条件更新没起作用——**不要放宽断言**，那是真 bug。

并发退款测试同理：

```bash
TEST_DATABASE_URL="postgres://imageapp:imageapp@localhost:5432/imageapp?sslmode=disable"   go test ./internal/credit/ -run TestConcurrentRefund -v
```

- [x] **Step 6: 提交**

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

- [x] **Step 1: 加两个辅助函数**

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

- [x] **Step 2: 确认现有测试仍通过**

```bash
go test ./internal/server/ -v 2>&1 | tail -15
```

期望：原有测试全部通过（`setupRouter` 行为未变）。

- [x] **Step 3: 提交**

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

- [x] **Step 1: 写失败的测试**

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


- [x] **Step 2: 运行确认失败**

```bash
go test ./internal/server/ -run TestListModels -v
```

期望：FAIL（404，路由不存在）。

- [x] **Step 3: 实现 handler**

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

- [x] **Step 4: 注册路由**

在 `internal/server/router.go` 中，`api.POST("/auth/login", ...)` 之后加：

```go
	modelsHandler := &handler.ModelsHandler{DB: db}
	api.GET("/models", modelsHandler.Get)
```

- [x] **Step 5: 运行确认通过**

```bash
go test ./internal/server/ -run TestListModels -v
```

期望：PASS。

- [x] **Step 6: 提交**

```bash
git add internal/handler/models.go internal/server
git commit -m "feat: GET /models 返回启用的模型配置"
```

---

## Task 5: /me 附带余额

**Files:**
- Modify: `internal/handler/me.go`
- Test: `internal/server/me_test.go`

- [x] **Step 1: 追加失败的测试**

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

- [x] **Step 2: 运行确认失败**

```bash
go test ./internal/server/ -run TestMeIncludesCredits -v
```

期望：FAIL（响应里没有 `credits` 字段，读到 0/0 而期望 7/3）。

- [x] **Step 3: 改 handler**

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

- [x] **Step 4: 运行确认通过**

```bash
go test ./internal/server/ -v 2>&1 | tail -20
```

期望：全部通过，含两个新测试。

- [x] **Step 5: 提交**

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

- [x] **Step 1: 写失败的测试**

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

- [x] **Step 2: 运行确认失败**

```bash
go test ./internal/server/ -run TestAdminGrant -v
```

期望：FAIL（404，路由不存在）。

- [x] **Step 3: 实现 admin 中间件**

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

- [x] **Step 4: 实现 handler**

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

- [x] **Step 5: 注册路由**

在 `internal/server/router.go` 里 `authed` 组之后加：

```go
	adminHandler := &handler.AdminHandler{DB: db}
	admin := authed.Group("/admin", middleware.RequireAdmin(db))
	admin.POST("/credits", adminHandler.GrantCredits)
```

- [x] **Step 6: 运行全部测试**

```bash
go build ./... && go vet ./... && go test ./... 2>&1 | tail -15
```

期望：全部包通过。

- [x] **Step 7: 提交**

```bash
git add internal/middleware/admin.go internal/handler/admin.go internal/server
git commit -m "feat: 管理员发放次数接口，role 每请求查库以便降权立即生效"
```

---

## Task 7: 文档与手工验证

**Files:**
- Modify: `README.md`
- Modify: `.env.example`

- [x] **Step 1: `.env.example` 补充**

追加（已完成）：

```
# Postgres：内测起就应该用它。留空会退化成临时文件 SQLite，
# 进程一停账号与余额全没——测试期间反复重发次数会污染你对扣费正确性的判断。
DATABASE_URL=postgres://imageapp:imageapp@localhost:5432/imageapp?sslmode=disable

# 仅测试用：指向 Postgres 时才会运行并发扣费测试。
# 留空时该测试会 SKIP 并打印原因（SQLite 单连接会串行化并发，测不出竞争）。
# TestConcurrentRefundRefundsOnce 是唯一覆盖 Refund 重复键回滚分支的测试——
# 该路径在 CI 和本地从未执行过，上线前必须跑通。
TEST_DATABASE_URL=
```

- [x] **Step 2: README 增加"次数账本"一节**

在 API 表之后插入（已完成）。注意以下几点与原计划文字不同：

- 表名是 `image_models`，不是 `models`（GORM 由 `ImageModel` 复数化）。
- `credit.Refund(db, generationID)` 不接收 `userID`——退给谁由扣费流水说了算。
- `(generation_id, type)` 复合唯一索引是幂等保证的唯一权威，不是 `Count` 检查。
- `generation_id` 是 `*string`（可空），发放流水存 `NULL` 使多条发放不冲突。
- `Grant` 拒绝负数；没有管理员冲正路径。
- `TestConcurrentRefundRefundsOnce` 是唯一覆盖 `Refund` 重复键回滚分支的测试。
- `Spend` 针对 READ COMMITTED；REPEATABLE READ 下并发扣费会抛序列化失败，本包不重试。
- 并发测试跑通是**上线前硬性前置条件**，不是可选项。
- `docker compose up -d` 命令已移除——开发机无 Docker，手工验证在 SQLite dev 模式下进行。

- [x] **Step 3: 手工端到端验证**

> **注意：本机无 Docker、无 Postgres**（2026-07-28 实测），手工验证在默认 SQLite dev 模式下进行，
> 不使用 `docker compose up -d`。DB 路径由启动日志打印。

```bash
# 无 DATABASE_URL → SQLite dev 模式
JWT_SECRET="local-test-secret-not-the-default" PORT=8080 go run ./cmd/server &
```

验证步骤（需先从启动日志取 SQLite 路径用于提权）：

1. `GET /api/v1/models` → 含 `flux-2-max` / `Flux 2 Max` / `credits: 1`
2. 注册新邮箱 → 201
3. 登录 → token
4. `GET /api/v1/me` → `credits: {monthly: 0, addon: 0}`（新用户无账户行，0/0 不是 500）
5. 从启动日志找 SQLite 路径，提权为 admin；再通过 API 发放次数
6. `GET /api/v1/me` → 余额反映发放
7. 非 admin 调用 `POST /api/v1/admin/credits` → 403

已在 2026-07-28 完成，真实输出记录在 Task 7 提交说明中。

- [x] **Step 4: 提交**

```bash
git add README.md .env.example docs/superpowers/plans/2026-07-28-m3a-credit-ledger.md
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
- `Spend` 里 `RowsAffected != 1` 把"余额不足"和"账户行在锁外被删了"混为一谈，都返回 `ErrInsufficientCredits`。现在拆开会让错误处理变复杂而收益很小，暂不做
- `Grant` 只拒绝负数，没有管理员冲正通道。冲正若要做，必须走一条和 `Spend` 同样三层防护（事务 + 行锁 + 带条件 UPDATE 校验 `RowsAffected`）的独立路径，**不能挂在 `Grant` 上**
- `database.Open` 在 dev 模式下每次调用都新建一个临时 SQLite 文件且从不删除，跑一轮测试会留下几十个文件。是 M1 既有问题，不在本轮范围
- 扣费/退款针对 **READ COMMITTED**（Postgres 默认）设计。若有人把隔离级别调到 REPEATABLE READ，同样的并发交错会抛序列化失败，需要调用方重试——本包不实现重试
