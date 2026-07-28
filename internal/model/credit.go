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
