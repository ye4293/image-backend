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

// 流水类型。addon_purchase 留给加量包里程碑（M4b）。
const (
	TxGenerationCost    = "generation_cost"
	TxGenerationRefund  = "generation_refund"
	TxAdminGrant        = "admin_grant"
	TxSubscriptionGrant = "subscription_grant"
	// TxSignupGrant 注册赠送的体验额度。
	//
	// 与 TxAdminGrant 分开一个类型而不是只靠 Note 区分：对账时要能一眼聚合出"送出去
	// 多少体验额度"——那是一笔真实的上游成本，而 Note 是自由文本、没法可靠聚合。
	// 它同时是幂等键的一半：(ExternalID, Type) 唯一索引里的 Type。
	TxSignupGrant = "signup_grant"
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

	// 两个复合唯一索引各管一件事，且都是**唯一权威**，不能靠"先 Count 再 INSERT"
	// 替代：那两步之间在 READ COMMITTED 下有窗口——两个并发退款都数到 0，然后都
	// 插进去，退两次款。唯一键冲突没有这个窗口。
	//
	//   (GenerationID, Type) → 退款幂等与"一次生成只扣一次"
	//   (ExternalID, Type)   → 同一个 Stripe 事件只发一次额度
	//
	// ExternalID 存 Stripe 事件 id，除了兜底幂等还是对账线索：光看到一行
	// "+800 月度"，运营需要知道是哪张发票造成的。stripe_events 表已经保证了一次
	// 幂等，但运维"删掉那行让它重投"是真实会发生的操作，有这个索引就不会因此重发。
	//
	// 两者都是 *string 而不是 string：绝大多数流水只有其中一个（生成流水没有外部
	// id，订阅流水没有 generation id），另一个必须存 NULL。存 '' 的话这些行会在
	// 唯一索引上互相冲突。SQLite 与 Postgres 都把 NULL 视为互不相等。
	ExternalID   *string `gorm:"uniqueIndex:idx_credit_tx_ext_type,priority:1;size:128"`
	GenerationID *string `gorm:"uniqueIndex:idx_credit_tx_gen_type,priority:1;size:64"`
	Type         string  `gorm:"uniqueIndex:idx_credit_tx_gen_type,priority:2;uniqueIndex:idx_credit_tx_ext_type,priority:2;size:32;not null"`

	Note      string `gorm:"size:255"`
	CreatedAt time.Time
}
