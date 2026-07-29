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
	ID          string    `gorm:"primaryKey;size:64"`
	Type        string    `gorm:"size:64;not null"`
	ProcessedAt time.Time `gorm:"not null"`
}
