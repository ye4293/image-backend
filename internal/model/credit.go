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
