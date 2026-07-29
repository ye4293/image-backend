# M4a Stripe 订阅 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用户能真的订阅并按月拿到次数，续费自动重置，退订清零月度但保留加量包。

**Architecture:** 新增 `plans` / `subscriptions` / `stripe_events` 三表与 `users.stripe_customer_id`。Checkout Session 由后端创建；发放额度的唯一入口是 `invoice.paid` webhook，验签→幂等→业务在同一事务内完成。发多少次数按订阅当前的 Price ID 反查 `plans` 表，不按 metadata。

**Tech Stack:** Go 1.25 / Gin / GORM，`github.com/stripe/stripe-go/v86 v86.1.1`。

**规格：** `docs/superpowers/specs/2026-07-29-m4a-stripe-subscription-design.md`（**含附录：v86 的 API 形状，与网上示例不同，已用编译探针验证**）

---

## 关键前提：stripe-go v86 的 API 与你记忆中的不同

训练数据里的 stripe-go 示例几乎全是旧写法，照抄**一定编不过**。已验证的正确形状：

```go
sc := stripe.NewClient(apiKey)                          // 不是 stripe.Key = apiKey
sess, err := sc.V1CheckoutSessions.Create(ctx, &stripe.CheckoutSessionCreateParams{...})
                                                        // 不是 session.New(params)
                                                        // 参数类型都在**根包** stripe，不在子包
```

| 你可能会写 | 实际 |
|---|---|
| `inv.Subscription` | `inv.Parent.SubscriptionDetails.Subscription.ID` |
| `sub.CurrentPeriodEnd` | `sub.Items.Data[0].CurrentPeriodEnd` |
| `sub.Plan.ID` | `sub.Items.Data[0].Price.ID` |

`inv.Parent`、`inv.Customer`、`sub.Items` 都是指针且可能为 nil。**判空**——这里 panic 掉的是 webhook，后果是 Stripe 无限重投。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/model/billing.go`（新） | `Plan`、`Subscription`、`StripeEvent` |
| `internal/model/user.go`（改） | 加 `StripeCustomerID *string` |
| `internal/model/credit.go`（改） | 加 `ExternalID` 列与 `TxSubscriptionGrant` |
| `internal/credit/ledger.go`（改） | 加 `ResetMonthly` |
| `internal/database/database.go`（改） | 迁移新表 + `seedPlans` |
| `internal/config/config.go`（改） | Stripe 三个变量 + 校验 |
| `internal/billing/client.go`（新） | Stripe 客户端封装 |
| `internal/billing/checkout.go`（新） | Checkout / Portal Session 创建 |
| `internal/billing/events.go`（新） | 五个事件的业务处理 |
| `internal/handler/plans.go`（新） | `GET /plans` |
| `internal/handler/billing.go`（新） | subscribe / portal |
| `internal/handler/stripe_webhook.go`（新） | 验签 + 幂等 + 派发 |
| `cmd/seed-stripe/main.go`（新） | 建 Stripe Product/Price 并回填 |

---

## Task 1：三张表与套餐播种

**Files:**
- Create: `internal/model/billing.go`
- Modify: `internal/model/user.go`、`internal/model/credit.go`
- Modify: `internal/database/database.go`
- Test: `internal/database/database_test.go`

- [ ] **Step 1：写 `internal/model/billing.go`**

```go
package model

import "time"

// Plan 是订阅档位。价格与次数**由库里的行说了算**，代码不得硬编码金额——
// 运营调价改这张表，改代码要重新发版。
//
// StripePriceID 由 cmd/seed-stripe 回填。Stripe 的 Price 对象金额不可变，
// 所以调价必须新建 Price 再迁移订阅，不能改这一列指向的对象。
type Plan struct {
	ID             string `gorm:"primaryKey;size:32"` // starter / pro / max
	DisplayName    string `gorm:"size:64;not null"`
	PriceUSDCents  int    `gorm:"not null"`
	MonthlyCredits int    `gorm:"not null"`
	// 播种前为空。webhook 靠这一列反查档位，所以要有索引。
	StripePriceID string `gorm:"size:64;index"`
	Enabled       bool   `gorm:"not null;default:true"`
	SortOrder     int    `gorm:"not null;default:0"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Subscription 是用户当前的订阅。一个用户同时只有一个，所以 UserID 直接做主键。
//
// StripeCustomerID **不在这里**，在 users 上：customer 是"人"的属性，用户可能
// 先有 customer（进过 Checkout 但没付成）而还没有订阅。放这里就存不下那个状态。
type Subscription struct {
	UserID uint   `gorm:"primaryKey;autoIncrement:false"`
	PlanID string `gorm:"size:32;not null"`
	// webhook 收到的是 Stripe 的订阅 id，靠它反查我们的行，必须唯一。
	StripeSubscriptionID string `gorm:"size:64;uniqueIndex;not null"`
	// 直接沿用 Stripe 的词汇：active / past_due / canceled / incomplete。
	// 不自造映射——自造就得两边同步，且对不上时没人知道以谁为准。
	Status             string `gorm:"size:32;not null"`
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	CancelAtPeriodEnd  bool `gorm:"not null;default:false"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// StripeEvent 是 webhook 幂等表。主键就是 Stripe 的事件 id。
//
// 靠主键冲突去重，而不是"先查再插"：Stripe 会重投事件，两个并发重投在
// READ COMMITTED 下会各查到 0 然后都插进去，于是发两次额度。
type StripeEvent struct {
	ID          string `gorm:"primaryKey;size:64"`
	Type        string `gorm:"size:64;not null"`
	ProcessedAt time.Time `gorm:"not null"`
}
```

- [ ] **Step 2：`internal/model/user.go` 加一列**

```go
	// StripeCustomerID 是 *string 而不是 string：绝大多数用户没有 customer，
	// 存 '' 的话所有这些用户会在唯一索引上互相冲突。NULL 之间互不相等。
	StripeCustomerID *string `gorm:"uniqueIndex;size:64"`
```

- [ ] **Step 3：`internal/model/credit.go` 加流水类型与外部关联列**

在类型常量里加：

```go
	TxSubscriptionGrant = "subscription_grant"
```

在 `CreditTransaction` 里，把 `Type` 同时挂到第二个唯一索引上，并新增 `ExternalID`：

```go
	// ExternalID 存 Stripe 事件 id。它有两个作用：
	//  1. 对账——光看到一行 "+800 月度"，运营需要知道是哪张发票造成的；
	//  2. 兜底幂等——stripe_events 表已经保证了一次，但运维"删掉那行让它重投"
	//     是真实会发生的操作，有这个唯一索引就不会因此重复发放。
	ExternalID *string `gorm:"uniqueIndex:idx_credit_tx_ext_type,priority:1;size:128"`
	GenerationID *string `gorm:"uniqueIndex:idx_credit_tx_gen_type,priority:1;size:64"`
	Type         string  `gorm:"uniqueIndex:idx_credit_tx_gen_type,priority:2;uniqueIndex:idx_credit_tx_ext_type,priority:2;size:32;not null"`
```

**同一列挂两个 uniqueIndex 的 GORM 标签写法必须实测**——Step 5 会 dump DDL 确认两个索引都真的建出来了，不要只看代码就认为成立。

- [ ] **Step 4：`internal/database/database.go` 迁移与播种**

`AutoMigrate` 参数里加 `&model.Plan{}, &model.Subscription{}, &model.StripeEvent{}`，并在 `seedModels(db)` 后加 `seedPlans(db)`：

```go
// seedPlans 幂等地播种三个档位。
//
// 用 FirstOrCreate 而不是 Save：价格与次数是**运营可改**的，每次启动覆盖回
// 默认值会把线上调价悄悄抹掉（seedModels 同理）。StripePriceID 尤其不能被
// 覆盖成空——那会让 cmd/seed-stripe 重新建一批 Price，产生重复商品。
func seedPlans(db *gorm.DB) error {
	plans := []model.Plan{
		{ID: "starter", DisplayName: "Starter", PriceUSDCents: 990, MonthlyCredits: 200, Enabled: true, SortOrder: 10},
		{ID: "pro", DisplayName: "Pro", PriceUSDCents: 2990, MonthlyCredits: 800, Enabled: true, SortOrder: 20},
		{ID: "max", DisplayName: "Max", PriceUSDCents: 4990, MonthlyCredits: 3000, Enabled: true, SortOrder: 30},
	}
	for i := range plans {
		if err := db.Where(model.Plan{ID: plans[i].ID}).FirstOrCreate(&plans[i]).Error; err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 5：写测试，确认索引真的建出来了**

在 `internal/database/database_test.go` 里加：

```go
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
```

- [ ] **Step 6：跑测试**

Run: `go test ./internal/database/... -run 'TestCreditTx|TestSeedPlans' -v`
Expected: PASS。若 `idx_credit_tx_ext_type` 缺失，说明双 uniqueIndex 标签写法不成立，改成在 `Open` 里显式 `db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS ...")`，**不要**改成非唯一索引蒙混过关。

- [ ] **Step 7：确认存量测试没被打破**

Run: `go test ./...`
Expected: 全绿。

- [ ] **Step 8：提交**

```bash
git add internal/model internal/database
git commit -m "feat: plans/subscriptions/stripe_events 三表与套餐播种"
```

---

## Task 2：`credit.ResetMonthly`

**Files:**
- Modify: `internal/credit/ledger.go`
- Test: `internal/credit/ledger_test.go`

- [ ] **Step 1：先写失败的测试**

```go
func TestResetMonthlySetsNotAdds(t *testing.T) {
	db := newTestDB(t)
	mustGrant(t, db, 1, 50, 30) // 月度 50，加量包 30
	err := db.Transaction(func(tx *gorm.DB) error {
		return credit.ResetMonthly(tx, 1, 800, "evt_1", "订阅续费")
	})
	if err != nil {
		t.Fatal(err)
	}
	bal, _ := credit.Balance(db, 1)
	if bal.MonthlyCredits != 800 {
		t.Errorf("月度应被**设置**为 800（而非累加成 850），得到 %d", bal.MonthlyCredits)
	}
	if bal.AddonCredits != 30 {
		t.Errorf("加量包不该被动，期望 30，得到 %d", bal.AddonCredits)
	}
}

func TestResetMonthlyCanLowerBalanceOnDowngrade(t *testing.T) {
	db := newTestDB(t)
	mustGrant(t, db, 1, 3000, 0)
	err := db.Transaction(func(tx *gorm.DB) error {
		return credit.ResetMonthly(tx, 1, 200, "evt_2", "降档")
	})
	if err != nil {
		t.Fatal(err)
	}
	bal, _ := credit.Balance(db, 1)
	if bal.MonthlyCredits != 200 {
		t.Errorf("降档应下调到 200，得到 %d", bal.MonthlyCredits)
	}
	// 快照与账户必须对得上，否则对账时无从判断哪笔开始错的
	var last model.CreditTransaction
	db.Order("id desc").First(&last)
	if last.MonthlyDelta != -2800 || last.MonthlyAfter != 200 {
		t.Errorf("流水应记 delta=-2800 after=200，得到 delta=%d after=%d", last.MonthlyDelta, last.MonthlyAfter)
	}
}

func TestResetMonthlyRejectsCallOutsideTransaction(t *testing.T) {
	db := newTestDB(t)
	// 直接传裸 db。webhook 要求"幂等记录与发放同生共死"，脱离事务调用会让
	// stripe_events 与发放分属两个事务，中间崩溃就永久漏发一次。
	err := credit.ResetMonthly(db, 1, 800, "evt_3", "x")
	if err == nil {
		t.Fatal("脱离事务调用必须报错，否则原子性保证形同虚设")
	}
}

func TestResetMonthlyRejectsDuplicateExternalID(t *testing.T) {
	db := newTestDB(t)
	mustGrant(t, db, 1, 0, 0)
	run := func() error {
		return db.Transaction(func(tx *gorm.DB) error {
			return credit.ResetMonthly(tx, 1, 800, "evt_dup", "x")
		})
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if err := run(); !errors.Is(err, credit.ErrAlreadyGranted) {
		t.Fatalf("同一事件重复发放应返回 ErrAlreadyGranted，得到 %v", err)
	}
	bal, _ := credit.Balance(db, 1)
	if bal.MonthlyCredits != 800 {
		t.Errorf("第二次不该改变余额，得到 %d", bal.MonthlyCredits)
	}
}
```

`newTestDB` / `mustGrant` 若 `ledger_test.go` 里已有同等辅助函数，**复用现有的**，不要新造一套。

- [ ] **Step 2：跑测试确认失败**

Run: `go test ./internal/credit/ -run TestResetMonthly -v`
Expected: 编译失败 `undefined: credit.ResetMonthly`。

- [ ] **Step 3：实现**

在 `internal/credit/ledger.go` 加：

```go
// ErrAlreadyGranted 同一个外部事件已经发放过。
var ErrAlreadyGranted = errors.New("credits already granted for this event")

// errNotInTransaction 调用方没有提供事务。
var errNotInTransaction = errors.New("credit.ResetMonthly 必须在调用方的事务内调用")

// ResetMonthly 把月度次数**设置**为 amount，加量包次数不动。
//
// 与 Grant 的区别是"设置"而非"累加"。续费若累加，用不完的次数会攒起来，
// 与定价页承诺的"月度次数不累积到下月"直接矛盾。
//
// 允许结果低于当前余额（高档降到低档），这与 Grant 拒绝负数不冲突：这里是把
// 余额**设**到一个由套餐决定的非负值，不存在扣成负数的路径。
//
// **必须由调用方提供事务。** webhook 要求"幂等记录与发放同生共死"：若分属两个
// 事务，进程在两步之间崩溃会留下"记了已处理但没发放"——那是永久漏发一次，
// 比重复发放更难发现（重复发放至少余额对不上，漏发看起来一切正常）。
func ResetMonthly(tx *gorm.DB, userID uint, amount int, externalID, note string) error {
	if amount < 0 {
		return fmt.Errorf("%w：月度次数不能为负，得到 %d", ErrInvalidGrantAmount, amount)
	}
	if externalID == "" {
		return errors.New("externalID 必填——它既是对账线索也是兜底幂等键")
	}
	if tx.Statement == nil {
		return errNotInTransaction
	}
	if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return errNotInTransaction
	}

	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&model.CreditAccount{UserID: userID}).Error; err != nil {
		return err
	}
	// 加锁重读：快照列必须基于锁内读到的值，理由同 Grant 的注释。
	var acct model.CreditAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ?", userID).First(&acct).Error; err != nil {
		return err
	}
	if err := tx.Model(&model.CreditAccount{}).Where("user_id = ?", userID).
		Update("monthly_credits", amount).Error; err != nil {
		return err
	}
	err := tx.Create(&model.CreditTransaction{
		UserID:       userID,
		Type:         model.TxSubscriptionGrant,
		MonthlyDelta: amount - acct.MonthlyCredits,
		AddonDelta:   0,
		MonthlyAfter: amount,
		AddonAfter:   acct.AddonCredits,
		ExternalID:   &externalID,
		Note:         note,
	}).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrAlreadyGranted
	}
	return err
}
```

- [ ] **Step 4：跑测试**

Run: `go test ./internal/credit/ -v`
Expected: 全部 PASS（含存量的 Spend / Refund / Grant 测试）。

- [ ] **Step 5：提交**

```bash
git add internal/credit
git commit -m "feat: credit.ResetMonthly——月度次数按订阅设置而非累加"
```

---

## Task 3：配置与 test/live 校验

**Files:**
- Modify: `internal/config/config.go`
- Modify: `.env.example`、`.env.prod.example`
- Test: `internal/config/config_test.go`（新建）

- [ ] **Step 1：先写失败的测试**

```go
func TestValidateStripeRejectsLiveKeyWithLocalhost(t *testing.T) {
	cfg := &Config{StripeSecretKey: "sk_live_x", AppBaseURL: "http://localhost:3000"}
	if err := cfg.ValidateStripe(); err == nil {
		t.Fatal("live key 配 localhost 必须拒绝启动：真实扣款后会跳到用户打不开的地址")
	}
}

func TestValidateStripeAllowsTestKeyWithLocalhost(t *testing.T) {
	cfg := &Config{StripeSecretKey: "sk_test_x", AppBaseURL: "http://localhost:3000"}
	if err := cfg.ValidateStripe(); err != nil {
		t.Fatalf("本地开发的常规组合不该被拒：%v", err)
	}
}

func TestValidateStripeAllowsEmptyKey(t *testing.T) {
	cfg := &Config{}
	if err := cfg.ValidateStripe(); err != nil {
		t.Fatalf("未配置 Stripe 时应放行（计费功能禁用，其余功能照常）：%v", err)
	}
}

func TestBillingEnabledRequiresBothSecrets(t *testing.T) {
	if (&Config{StripeSecretKey: "sk_test_x"}).BillingEnabled() {
		t.Error("只有 secret key、没有 webhook secret 时不算启用——收得到钱但发不出额度，比整个关掉更糟")
	}
	if !(&Config{StripeSecretKey: "sk_test_x", StripeWebhookSecret: "whsec_x"}).BillingEnabled() {
		t.Error("两个都有时应当启用")
	}
}
```

- [ ] **Step 2：跑测试确认失败**

Run: `go test ./internal/config/ -v`
Expected: 编译失败，字段与方法未定义。

- [ ] **Step 3：实现**

`Config` 加三个字段并在 `Load()` 里读取：

```go
	// StripeSecretKey 为空时计费功能整体禁用，相关接口返回明确的"未配置"错误
	// 而不是 500——让没配 Stripe 的本地开发仍能跑其余功能。
	StripeSecretKey string
	// StripeWebhookSecret 由 `stripe listen` 或 Dashboard 的 endpoint 提供。
	// **本地与生产是两个不同的值**（按 endpoint 生成），混用的表现是验签一直失败。
	StripeWebhookSecret string
	// AppBaseURL 前端地址，用于拼 Checkout 的 success_url / cancel_url。
	AppBaseURL string
```

```go
		StripeSecretKey:     getEnv("STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret: getEnv("STRIPE_WEBHOOK_SECRET", ""),
		AppBaseURL:          getEnv("APP_BASE_URL", "http://localhost:3000"),
```

```go
// BillingEnabled 计费功能是否可用。
//
// **两个 secret 都要有。** 只有 secret key 意味着能创建 Checkout、收得到钱，
// 却因为没有 webhook secret 而无法发放额度——用户付了钱拿不到东西，
// 这比整个功能关掉严重得多。
func (c *Config) BillingEnabled() bool {
	return c.StripeSecretKey != "" && c.StripeWebhookSecret != ""
}

// ValidateStripe 启动时的误配拦截。
func (c *Config) ValidateStripe() error {
	if c.StripeSecretKey == "" {
		return nil
	}
	if strings.HasPrefix(c.StripeSecretKey, "sk_live_") {
		u, err := url.Parse(c.AppBaseURL)
		if err != nil {
			return fmt.Errorf("APP_BASE_URL 解析失败：%w", err)
		}
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "" {
			// 这个组合几乎必然是误配，而后果是真实扣款后跳到用户打不开的地址。
			return fmt.Errorf(
				"检测到 live 模式密钥但 APP_BASE_URL 是 %q——真实扣款后用户会跳到打不开的地址；"+
					"本地开发请用 sk_test_ 开头的密钥", c.AppBaseURL)
		}
	}
	return nil
}
```

- [ ] **Step 4：在 `cmd/server/main.go` 里调用校验**

在 `config.Load()` 之后、开始监听之前：

```go
	if err := cfg.ValidateStripe(); err != nil {
		log.Fatalf("config: %v", err)
	}
	if !cfg.BillingEnabled() {
		log.Println("billing: STRIPE_SECRET_KEY / STRIPE_WEBHOOK_SECRET 未配齐，计费功能已禁用")
	}
```

- [ ] **Step 5：更新两个 env 示例文件**

`.env.example` 与 `.env.prod.example` 各加一段（`.env.prod.example` 的 `APP_BASE_URL` 填真实前端域名）：

```
# ---- Stripe ----
# 测试用 sk_test_ 开头。live 密钥配合 localhost 的 APP_BASE_URL 会拒绝启动。
STRIPE_SECRET_KEY=
# 本地：stripe listen --forward-to localhost:8080/api/v1/webhooks/stripe 打印的值
# 生产：Dashboard → Webhooks → 对应 endpoint 的 Signing secret
# **两者是不同的值**，混用会导致验签一直失败。
STRIPE_WEBHOOK_SECRET=
# 前端地址，用于拼 Checkout 成功/取消的跳转。
APP_BASE_URL=http://localhost:3000
```

- [ ] **Step 6：跑测试并提交**

Run: `go test ./...`
Expected: 全绿。

```bash
git add internal/config cmd/server .env.example .env.prod.example
git commit -m "feat: Stripe 配置项与 live/test 误配拦截"
```

---

## Task 4：`GET /plans` 与 `/me` 扩展

**Files:**
- Create: `internal/handler/plans.go`
- Modify: `internal/handler/me.go`、`internal/server/router.go`
- Test: `internal/server/plans_test.go`（新）、`internal/server/me_test.go`

- [ ] **Step 1：先写失败的测试**

```go
func TestGetPlansReturnsEnabledOnlyAndHidesPriceID(t *testing.T) {
	db := newTestDB(t)
	db.Model(&model.Plan{}).Where("id = ?", "max").Update("enabled", false)
	db.Model(&model.Plan{}).Where("id = ?", "pro").Update("stripe_price_id", "price_secret")

	w := doRequest(t, db, "GET", "/api/v1/plans", nil, "")
	if w.Code != 200 {
		t.Fatalf("期望 200，得到 %d：%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `"max"`) {
		t.Error("禁用的档位不该出现在公开列表里")
	}
	if strings.Contains(body, "price_secret") {
		t.Error("stripe_price_id 是服务端细节，不该出现在响应里")
	}
	if !strings.Contains(body, "starter") || !strings.Contains(body, "pro") {
		t.Errorf("启用的档位应当返回，得到 %s", body)
	}
}

func TestMeIncludesSubscriptionNullWhenNone(t *testing.T) {
	// 未订阅用户的 subscription 字段必须是 null 而不是缺失或零值对象，
	// 前端靠它区分"没订阅"和"订阅了但状态未知"。
}

func TestMeIncludesSubscriptionWhenPresent(t *testing.T) {
	// 建一行 subscription，断言 planId / status / currentPeriodEnd / cancelAtPeriodEnd 都在
}

func TestGetPlansIsPublic(t *testing.T) {
	// 不带 token 也应当 200——定价页在未登录时就要能看
}
```

测试辅助函数复用 `internal/server` 包里现有的（`newTestDB`、`doRequest` 或等价物；**先读 `internal/server/models_test.go` 看现有写法**，不要另起炉灶）。

- [ ] **Step 2：跑测试确认失败**

Run: `go test ./internal/server/ -run 'TestGetPlans|TestMeIncludes' -v`

- [ ] **Step 3：实现 `internal/handler/plans.go`**

```go
package handler

// PlansHandler 返回公开的套餐列表。
//
// **不返回 stripe_price_id**：那是服务端细节，前端下单只传 planId，
// 由后端查表拿 Price——把 Price ID 交给前端等于让客户端指定价格。
type PlansHandler struct{ DB *gorm.DB }

func (h *PlansHandler) List(c *gin.Context) {
	var plans []model.Plan
	if err := h.DB.Where("enabled = ?", true).Order("sort_order asc").Find(&plans).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	out := make([]gin.H, 0, len(plans))
	for _, p := range plans {
		out = append(out, gin.H{
			"id":             p.ID,
			"displayName":    p.DisplayName,
			"priceUsdCents":  p.PriceUSDCents,
			"monthlyCredits": p.MonthlyCredits,
		})
	}
	c.JSON(http.StatusOK, gin.H{"plans": out})
}
```

- [ ] **Step 4：`/me` 加 subscription 字段**

在 `MeHandler.Get` 里，取完余额后：

```go
	// 未订阅时是 nil → JSON 里是 null。前端靠 null 区分"没订阅"与"订阅状态未知"，
	// 所以不要退化成空对象。
	var subOut any
	var sub model.Subscription
	err = h.DB.Where("user_id = ?", userID).First(&sub).Error
	if err == nil {
		subOut = gin.H{
			"planId":            sub.PlanID,
			"status":            sub.Status,
			"currentPeriodEnd":  sub.CurrentPeriodEnd,
			"cancelAtPeriodEnd": sub.CancelAtPeriodEnd,
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		// 查不到是正常的（没订阅），查报错不是——不能把 DB 故障伪装成"没订阅"，
		// 那会让付过费的用户在故障期间看到未订阅界面。
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
```

并在响应体里加 `"subscription": subOut`。

- [ ] **Step 5：注册路由**

`internal/server/router.go` 里，在 `api.GET("/models", ...)` 附近（公开区）：

```go
	plansHandler := &handler.PlansHandler{DB: db}
	api.GET("/plans", plansHandler.List)
```

- [ ] **Step 6：跑测试并提交**

Run: `go test ./...`

```bash
git add internal/handler internal/server
git commit -m "feat: GET /plans 与 /me 返回订阅状态"
```

---

## Task 5：Stripe 客户端与 Price 播种命令

**Files:**
- Create: `internal/billing/client.go`
- Create: `cmd/seed-stripe/main.go`

- [ ] **Step 1：`internal/billing/client.go`**

```go
// Package billing 封装 Stripe 调用。
//
// handler 不直接碰 stripe-go：把 SDK 关在这一层里，换版本或加重试时只改一处。
package billing

import (
	"errors"

	stripe "github.com/stripe/stripe-go/v86"
)

// ErrNotConfigured 未配置 Stripe。handler 据此返回明确的"计费未启用"而不是 500。
var ErrNotConfigured = errors.New("billing is not configured")

type Client struct {
	sc         *stripe.Client
	appBaseURL string
}

// New 返回 nil 表示未配置——调用方必须判 nil。
func New(secretKey, appBaseURL string) *Client {
	if secretKey == "" {
		return nil
	}
	return &Client{sc: stripe.NewClient(secretKey), appBaseURL: appBaseURL}
}
```

- [ ] **Step 2：`cmd/seed-stripe/main.go`**

```go
// 一次性命令：为 plans 表里还没有 stripe_price_id 的档位创建 Stripe Product 与 Price，
// 并把 Price ID 写回。
//
// 为什么用代码建而不是在 Dashboard 手建：价格、次数、Price ID 三者必须一致。
// 手工抄 ID 一旦错位，表现是"用户付了 Pro 的钱、拿到 Starter 的次数"——而这种
// 错位在测试时很可能撞不到，因为你只会盯着自己刚建的那一档测。
//
// **幂等且绝不覆盖**：已有 stripe_price_id 的行直接跳过。Stripe 的 Price 金额
// 不可变，重跑若重建 Price 就会产生一批重复商品，而老订阅仍绑在旧 Price 上。
//
// 用法：go run ./cmd/seed-stripe
package main

func main() {
	cfg := config.Load()
	if cfg.StripeSecretKey == "" {
		log.Fatal("STRIPE_SECRET_KEY 未配置")
	}
	db, err := database.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	sc := stripe.NewClient(cfg.StripeSecretKey)
	ctx := context.Background()

	var plans []model.Plan
	if err := db.Order("sort_order asc").Find(&plans).Error; err != nil {
		log.Fatal(err)
	}
	for _, p := range plans {
		if p.StripePriceID != "" {
			log.Printf("跳过 %s：已有 Price %s", p.ID, p.StripePriceID)
			continue
		}
		prod, err := sc.V1Products.Create(ctx, &stripe.ProductCreateParams{
			Name: stripe.String(p.DisplayName),
			Metadata: map[string]string{"plan_id": p.ID},
		})
		if err != nil {
			log.Fatalf("创建 Product 失败（%s）：%v", p.ID, err)
		}
		price, err := sc.V1Prices.Create(ctx, &stripe.PriceCreateParams{
			Product:    stripe.String(prod.ID),
			Currency:   stripe.String("usd"),
			UnitAmount: stripe.Int64(int64(p.PriceUSDCents)),
			Recurring:  &stripe.PriceCreateRecurringParams{Interval: stripe.String("month")},
			Metadata:   map[string]string{"plan_id": p.ID},
		})
		if err != nil {
			log.Fatalf("创建 Price 失败（%s）：%v", p.ID, err)
		}
		if err := db.Model(&model.Plan{}).Where("id = ?", p.ID).
			Update("stripe_price_id", price.ID).Error; err != nil {
			// 这里失败很难受：Stripe 侧已经建好了，库里没记下。重跑会再建一个。
			// 所以把 Price ID 打出来，人工回填比重跑安全。
			log.Fatalf("回填失败！请手工把 %s 的 stripe_price_id 设为 %s：%v", p.ID, price.ID, err)
		}
		log.Printf("%s：Product %s / Price %s（$%.2f/月，%d 次）",
			p.ID, prod.ID, price.ID, float64(p.PriceUSDCents)/100, p.MonthlyCredits)
	}
}
```

- [ ] **Step 3：编译验证**

Run: `go build ./... && go vet ./...`
Expected: 无错误。

**不要在本任务里真的执行播种命令**——那会在 Stripe 账号里建真实对象。执行由人工在 Task 9 之后决定。

- [ ] **Step 4：提交**

```bash
git add internal/billing cmd/seed-stripe go.mod go.sum
git commit -m "feat: Stripe 客户端封装与 Price 播种命令"
```

---

## Task 6：Checkout 与 Billing Portal

**Files:**
- Create: `internal/billing/checkout.go`、`internal/handler/billing.go`
- Modify: `internal/server/router.go`
- Test: `internal/server/billing_test.go`（新）

- [ ] **Step 1：`internal/billing/checkout.go`**

```go
// CreateCheckoutSession 为用户创建订阅结账会话。
//
// customerID 为空时传 nil 让 Stripe 新建——复用已有 customer 很重要，
// 否则同一用户会产生多个 customer，Billing Portal 里只能看到其中一部分发票。
func (c *Client) CreateCheckoutSession(ctx context.Context, userID uint, planID, priceID, customerID string) (string, error) {
	params := &stripe.CheckoutSessionCreateParams{
		Mode:       stripe.String("subscription"),
		SuccessURL: stripe.String(c.appBaseURL + "/account?checkout=success"),
		CancelURL:  stripe.String(c.appBaseURL + "/pricing?checkout=cancel"),
		ClientReferenceID: stripe.String(strconv.FormatUint(uint64(userID), 10)),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{Price: stripe.String(priceID), Quantity: stripe.Int64(1)},
		},
		// metadata 挂在 subscription 上而非 session 上：invoice.paid 事件拿到的是
		// 订阅，session 的 metadata 在那时已经够不着了。
		SubscriptionData: &stripe.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: map[string]string{
				"user_id": strconv.FormatUint(uint64(userID), 10),
				"plan_id": planID,
			},
		},
	}
	if customerID != "" {
		params.Customer = stripe.String(customerID)
	}
	sess, err := c.sc.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}

// CreatePortalSession 换卡/取消/看发票统一跳 Stripe 自己的页面——
// 我们不做这些 UI，自己做是纯浪费且要处理 PCI 相关问题。
func (c *Client) CreatePortalSession(ctx context.Context, customerID string) (string, error) {
	sess, err := c.sc.V1BillingPortalSessions.Create(ctx, &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(c.appBaseURL + "/account"),
	})
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}
```

- [ ] **Step 2：`internal/handler/billing.go`**

```go
type BillingHandler struct {
	DB      *gorm.DB
	Billing *billing.Client // nil 表示未配置
}

func (h *BillingHandler) Subscribe(c *gin.Context) {
	if h.Billing == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 50300, "message": "billing is not configured"})
		return
	}
	userID := c.GetUint(middleware.CtxUserIDKey)
	var req struct{ PlanID string `json:"planId"` }
	if err := c.ShouldBindJSON(&req); err != nil || req.PlanID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "planId is required"})
		return
	}
	// 价格由**服务端**查表决定。前端只传 planId——让前端传 priceId 等于
	// 让客户端指定自己付多少钱。
	var plan model.Plan
	if err := h.DB.Where("id = ? AND enabled = ?", req.PlanID, true).First(&plan).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "unknown plan"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "internal error"})
		return
	}
	if plan.StripePriceID == "" {
		// 播种命令还没跑。这不是用户的错，别回 400。
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 50301, "message": "plan is not available for purchase yet"})
		return
	}
	var user model.User
	if err := h.DB.First(&user, userID).Error; err != nil { /* 500 */ }
	customerID := ""
	if user.StripeCustomerID != nil {
		customerID = *user.StripeCustomerID
	}
	url, err := h.Billing.CreateCheckoutSession(c.Request.Context(), userID, plan.ID, plan.StripePriceID, customerID)
	if err != nil {
		// 上游错误详情只进日志，不回给客户端。
		log.Printf("billing: 创建 Checkout 失败 user=%d plan=%s: %v", userID, plan.ID, err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 50200, "message": "payment provider unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"checkoutUrl": url})
}

// Portal 没有 customer 时回 400 而不是造一个：没进过结账流程的用户，
// Portal 里什么都没有，跳过去只会看到空页面。
func (h *BillingHandler) Portal(c *gin.Context) { /* 同上结构 */ }
```

- [ ] **Step 3：注册路由**

```go
	billingHandler := &handler.BillingHandler{DB: db, Billing: billing.New(cfg.StripeSecretKey, cfg.AppBaseURL)}
	authed.POST("/billing/subscribe", billingHandler.Subscribe)
	authed.POST("/billing/portal", billingHandler.Portal)
```

- [ ] **Step 4：测试**

只测**不需要真调 Stripe** 的分支（`Billing` 传 nil 或 plan 数据造出来）：

```go
func TestSubscribeRejectsUnknownPlan(t *testing.T)        // 400
func TestSubscribeRejectsDisabledPlan(t *testing.T)       // 400，禁用的档位不能下单
func TestSubscribeWhenPriceNotSeededReturns503(t *testing.T) // 不是 400——不是用户的错
func TestSubscribeRequiresAuth(t *testing.T)              // 401
func TestSubscribeWhenBillingUnconfiguredReturns503(t *testing.T)
func TestPortalWithoutCustomerReturns400(t *testing.T)
```

Run: `go test ./internal/server/ -run 'TestSubscribe|TestPortal' -v`

- [ ] **Step 5：提交**

```bash
git add internal/billing internal/handler internal/server
git commit -m "feat: Checkout Session 与 Billing Portal 接口"
```

---

## Task 7：Webhook 验签与幂等

**Files:**
- Create: `internal/handler/stripe_webhook.go`
- Modify: `internal/server/router.go`
- Test: `internal/server/stripe_webhook_test.go`（新）

**本任务不需要真实的 `whsec_`**：测试自己用任意密钥构造签名。真实密钥只在 Task 9 之后的人工联调用到。

- [ ] **Step 1：先写失败的测试**

测试需要自己造 Stripe 签名头。格式是 `t=<unix>,v1=<hex hmac-sha256 of "<t>.<payload>">`：

```go
func signPayload(t *testing.T, payload []byte, secret string) string {
	t.Helper()
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", ts, payload)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	// 用错误的密钥签名 → 400，且**不得**在 stripe_events 里留下记录
	// （留了记录就等于把伪造事件的 id 占掉，真事件到达时会被当成重复而丢弃）
}

func TestWebhookRejectsMissingSignature(t *testing.T) {
	// 无 Stripe-Signature 头 → 400
}

func TestWebhookIsIdempotent(t *testing.T) {
	// 同一 event id 投递两次 → 都 200，但 stripe_events 只有一行，业务只执行一次
}

func TestWebhookReturns200ForUnhandledType(t *testing.T) {
	// 我们不关心的事件类型 → 200。返回 5xx 会让 Stripe 一直重投毫无意义的事件。
}

func TestWebhookDoesNotRequireAuth(t *testing.T) {
	// 不带 cookie → 不是 401。Stripe 不带我们的 cookie，安全性由验签保证。
}
```

- [ ] **Step 2：跑测试确认失败**

Run: `go test ./internal/server/ -run TestWebhook -v`

- [ ] **Step 3：实现**

```go
// maxWebhookBody 限制读入的字节数。不设上限的话，一个超大 body 就能吃光内存。
const maxWebhookBody = 1 << 20 // 1 MiB

type StripeWebhookHandler struct {
	DB      *gorm.DB
	Secret  string
	Billing *billing.Client
}

// errAlreadyProcessed 内部哨兵：撞幂等主键时用它把事务回滚掉，
// 再在事务外转成 200。不能在闭包里直接 return nil——那会把已经跑了一半的
// 业务变更提交掉。
var errAlreadyProcessed = errors.New("event already processed")

func (h *StripeWebhookHandler) Handle(c *gin.Context) {
	if h.Secret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 50300, "message": "billing is not configured"})
		return
	}
	// **必须用原始字节验签。** 任何 ShouldBindJSON 再序列化都会改变字节，
	// 导致签名对不上。这是这个 handler 最容易写错的一行。
	payload, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "cannot read body"})
		return
	}
	ev, err := webhook.ConstructEvent(payload, c.GetHeader("Stripe-Signature"), h.Secret)
	if err != nil {
		// 不验签的后果不是"可能被伪造"，是任何人 POST 一个 invoice.paid
		// 就能给自己发额度。验签失败一律拒绝，且**不留幂等记录**。
		log.Printf("stripe webhook: 验签失败: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"code": 40100, "message": "invalid signature"})
		return
	}

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		// 幂等记录与业务处理必须**同一事务**：分开的话，进程在两步之间崩溃
		// 会留下"记了已处理但没处理"，那是永久漏发一次，比重复发放更难发现。
		if err := tx.Create(&model.StripeEvent{
			ID: ev.ID, Type: string(ev.Type), ProcessedAt: time.Now(),
		}).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return errAlreadyProcessed
			}
			return err
		}
		return h.dispatch(c.Request.Context(), tx, ev)
	})
	switch {
	case errors.Is(err, errAlreadyProcessed):
		c.JSON(http.StatusOK, gin.H{"received": true, "duplicate": true})
	case err != nil:
		// 回 5xx 让 Stripe 重投——业务处理失败是唯一该重投的情况。
		log.Printf("stripe webhook: 处理 %s(%s) 失败: %v", ev.Type, ev.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 50000, "message": "processing failed"})
	default:
		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}

// dispatch 本任务先只留骨架：未知类型返回 nil（→200）。具体处理在 Task 8。
func (h *StripeWebhookHandler) dispatch(ctx context.Context, tx *gorm.DB, ev stripe.Event) error {
	log.Printf("stripe webhook: 收到 %s(%s)，暂未处理", ev.Type, ev.ID)
	return nil
}
```

路由（**公开区，不过认证中间件**）：

```go
	api.POST("/webhooks/stripe", (&handler.StripeWebhookHandler{
		DB: db, Secret: cfg.StripeWebhookSecret, Billing: billingClient,
	}).Handle)
```

- [ ] **Step 4：跑测试并提交**

Run: `go test ./...`

```bash
git add internal/handler internal/server
git commit -m "feat: Stripe webhook 验签与幂等骨架"
```

---

## Task 8：五个事件的业务处理

**Files:**
- Create: `internal/billing/events.go`
- Modify: `internal/handler/stripe_webhook.go`
- Test: `internal/server/stripe_webhook_test.go`

- [ ] **Step 1：先写失败的测试**

用真实形状的事件 JSON（**注意 v86 的字段路径**）：

```go
func TestInvoicePaidGrantsAndResetsMonthly(t *testing.T) {
	// 用户已有月度 50 / 加量包 30，收到 pro 档的 invoice.paid
	// → 月度**设置**为 800（不是 850），加量包仍是 30
}

func TestCheckoutCompletedBindsCustomerButGrantsNothing(t *testing.T) {
	// 这是最重要的一条：checkout.session.completed 只绑 customer id。
	// 它和 invoice.paid 都发的话就是双倍到账，而这个 bug 只在真实付款时暴露。
	// 断言：users.stripe_customer_id 被写入，且 credit_transactions 没有新增行。
}

func TestPaymentFailedSetsPastDueButKeepsCredits(t *testing.T) {
	// status → past_due，次数**不变**。用户可能只是卡过期，几天内会补款。
}

func TestSubscriptionDeletedZerosMonthlyKeepsAddon(t *testing.T) {
	// 月度 → 0，加量包保留（那是用户单独花钱买的）
}

func TestInvoicePaidResolvesPlanByPriceIDNotMetadata(t *testing.T) {
	// **关键**：metadata 写 starter，但订阅 item 的 Price 是 pro 的。
	// 必须按 Price 发 800 次（用户实际被计费的档位），不是 metadata 的 200。
	// 这条覆盖"用户在 Billing Portal 里升级、Stripe 换 Price 但不改 metadata"。
}

func TestInvoicePaidWithUnknownPriceGrantsNothing(t *testing.T) {
	// Price 在 plans 里查不到（Dashboard 手工建的订阅）→ 不发放、记日志。
	// 宁可漏发等人工，也不要瞎猜一个档位。
}

func TestSubscriptionUpdatedDoesNotTouchCredits(t *testing.T) {
	// 只同步 status / cancelAtPeriodEnd / 周期，额度变化交给随之而来的 invoice.paid
}
```

- [ ] **Step 2：跑测试确认失败**

- [ ] **Step 3：实现 `dispatch`**

```go
func (h *StripeWebhookHandler) dispatch(ctx context.Context, tx *gorm.DB, ev stripe.Event) error {
	switch ev.Type {
	case "checkout.session.completed":
		return billing.HandleCheckoutCompleted(tx, ev)
	case "invoice.paid":
		return billing.HandleInvoicePaid(ctx, tx, h.Billing, ev)
	case "invoice.payment_failed":
		return billing.HandlePaymentFailed(tx, ev)
	case "customer.subscription.updated":
		return billing.HandleSubscriptionUpdated(tx, ev)
	case "customer.subscription.deleted":
		return billing.HandleSubscriptionDeleted(tx, ev)
	default:
		// 未知类型也返回 nil → 200。回 5xx 会让 Stripe 一直重投我们根本不处理的事件。
		return nil
	}
}
```

- [ ] **Step 4：实现 `internal/billing/events.go`**

要点（**每一条都有对应测试**）：

1. **`HandleInvoicePaid` 是发放额度的唯一入口。**
   - 解析 `ev.Data.Raw` 到 `stripe.Invoice`。
   - **判空**：`inv.Parent == nil || inv.Parent.SubscriptionDetails == nil` → 非订阅发票，返回 nil（加量包走 M4b）。
   - 取 `subID := inv.Parent.SubscriptionDetails.Subscription.ID`。
   - 用 `h.Billing` 拉取订阅拿周期与 Price：`sc.V1Subscriptions.Retrieve(ctx, subID, nil)`；`sub.Items == nil || len(sub.Items.Data) == 0` → 返回错误（让 Stripe 重投）。
   - **按 `sub.Items.Data[0].Price.ID` 反查 `plans`**，不是按 metadata 的 `plan_id`。查不到 → 记日志、返回 nil（不发放，也不让 Stripe 白重投）。
   - `user_id` 从 `inv.Parent.SubscriptionDetails.Metadata["user_id"]` 取；同时按 `stripe_customer_id` 反查用户。**两者都存在且不一致时返回错误并告警**——数据串了，猜哪个都可能发错人。
   - upsert `subscriptions` 行（`user_id` 主键，`clause.OnConflict{Columns: user_id, UpdateAll: true}`）。
   - `credit.ResetMonthly(tx, userID, plan.MonthlyCredits, ev.ID, "订阅发放："+plan.ID)`；返回 `credit.ErrAlreadyGranted` 时记日志并返回 nil（不是错误——说明有人删过 stripe_events 行）。

2. **`HandleCheckoutCompleted` 只写 `users.stripe_customer_id`，绝不碰次数。**

3. **`HandlePaymentFailed` 只把 status 改成 `past_due`，不动次数。**

4. **`HandleSubscriptionUpdated` 同步 `plan_id`（按 Price 反查）/ `status` / `cancel_at_period_end` / 周期，不动次数。**

5. **`HandleSubscriptionDeleted`：status → `canceled`，`credit.ResetMonthly(tx, userID, 0, ev.ID, "订阅取消")`。** 加量包不动——`ResetMonthly` 本来就只改月度。

- [ ] **Step 5：跑全部测试**

Run: `go test ./... -v`
Expected: 全绿。

- [ ] **Step 6：提交**

```bash
git add internal/billing internal/handler internal/server
git commit -m "feat: Stripe 事件处理——invoice.paid 为唯一发放入口，按 Price 反查档位"
```

---

## Task 9：前端接真

**Files:**（仓库 `~/Desktop/image-front`）
- Create: `app/api/plans/route.ts`、`app/api/billing/subscribe/route.ts`、`app/api/billing/portal/route.ts`
- Modify: `lib/backend.ts`、`app/[locale]/pricing/*`、`app/[locale]/account/*`、`messages/{en,zh,ja,ko}.json`
- Delete: `lib/plans.ts`

- [ ] **Step 1：BFF 路由**

三个 Route Handler，照 `app/api/generations/route.ts` 的既有写法：`checkSameOrigin` → `getToken` → 转发 → `toClientError`。`/api/plans` 是公开的，不需要 token。

- [ ] **Step 2：定价页接真**

- 套餐从 `GET /api/plans` 取，**删掉 `lib/plans.ts`**（最后一处假数据）。
- 按钮从 `disabled` 改成调 `POST /api/billing/subscribe` 并 `window.location.href = checkoutUrl`。
- **未登录时先跳登录**，带 `?next=/pricing`。不要在未登录状态弹 Checkout——用户付完款我们不知道该给谁。
- **删掉三张卡上编造的功能差异点**（"优先排队 / 私密生成 / 商用授权 / 最高并发"）。一样都没实现，留着就是虚假宣传。改成只列次数。

- [ ] **Step 3：账户页显示订阅**

`/me` 的 `subscription` 为 null 时显示"未订阅 + 去订阅"；否则显示档位、状态、续费日期，以及"管理订阅"按钮（调 `/api/billing/portal` 后跳转）。`cancelAtPeriodEnd` 为真时明确提示"将于 X 日到期后停止续费"。

- [ ] **Step 4：四语文案**

`messages/{en,zh,ja,ko}.json` 各补齐同一批 key。**四个文件的 key 集合必须完全一致**，缺一个就是线上某语言下的空白文案。

- [ ] **Step 5：PC 与手机端都验证**

`npm run build && npm start`，用 Playwright 在 **1280×800 与 375×667** 两个尺寸各截一次定价页与账户页，**看截图**。移动端要确认套餐卡片纵向堆叠、按钮没有被挤出视口。

- [ ] **Step 6：提交**

```bash
git add -A && git commit -m "feat: 定价页与账户页接真实订阅接口"
```

---

## 人工联调（代码完成后，需要真实 whsec_）

**0. 先解决 API 版本，否则第 3 步起每个事件都会失败。**

本账号默认 API 版本实测是 `2023-10-16`，而 SDK（stripe-go v86.1.1）要求 `2026-06-24.dahlia`。
`stripe listen` **默认按账号默认版本转发**，所以：

- 本地必须加 `--latest`：`stripe listen --latest --forward-to localhost:8080/api/v1/webhooks/stripe`
- 生产在 Dashboard 建 endpoint 时把 `api_version` 显式设为 `2026-06-24.dahlia`
  （账号里已有一个别的项目的 endpoint 钉在 `2024-06-20`，所以**不要**去改账号默认版本，那会打断它）

版本不匹配时后端回 `500 / 50302` 并在日志里写清怎么改，**不会**伪装成"验签失败"。
若 `--latest` 拿到的比 `dahlia` 还新，就得升 stripe-go 或在 Dashboard 显式钉版本。

1. `stripe listen --latest --forward-to localhost:8080/api/v1/webhooks/stripe`，把 `whsec_` 填进 `.env`
2. **`.env` 里必须有持久库**（如 `DATABASE_URL=./local.db`）。留空是一次性 SQLite，
   而联调跨越多次重启；`go run ./cmd/seed-stripe` 也会直接拒绝在一次性库上运行
3. `go run ./cmd/seed-stripe` 建三个 Price
4. 测试卡 `4242 4242 4242 4242` 订阅 → 月度次数到账、`subscriptions` 行正确
5. 首次订阅后**确认能进 Billing Portal**——`users.stripe_customer_id` 要由
   `checkout.session.completed` 回填，没回填的话 Portal 接口会回 400
6. `stripe trigger invoice.payment_failed` → `past_due` 且次数**未**被清零
7. Portal 里取消 → 月度清零、加量包保留
8. **`stripe events resend <id>` 重投同一事件 → 只入账一次**
9. **3DS 卡 `4000 0025 0000 3155` → `incomplete` 状态不会误发额度**

第 8、9 条最容易被跳过，也最容易在真实付款中出事。

---

## 已知缺口

- 加量包（M4b）
- 调价迁移：Price 金额不可变，调价需新建 Price 并迁移已有订阅
- 三档毛利率未核算——缺"上游 7 个单位折合多少钱"
- Postgres 并发测试仍未跑过（本机无 Docker/Postgres），`ResetMonthly` 的行锁在 SQLite 下是空操作
