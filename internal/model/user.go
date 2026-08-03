package model

import "time"

// 角色与状态的取值。
//
// 补这几个常量是因为后台要开始**写** Status（封禁/解封）了，而判定它的地方在
// internal/middleware/active.go。此前两边都是裸字符串字面量，改一边忘另一边不会有
// 任何编译期提示——那个 bug 的表现是"点了封禁但对方照常能用"，或者反过来
// "所有人都被判成非活跃、全站 403"。
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

const (
	StatusActive = "active"
	StatusBanned = "banned"
)

type User struct {
	ID           uint   `gorm:"primaryKey"`
	Email        string `gorm:"uniqueIndex;size:255;not null"`
	PasswordHash string `gorm:"size:255"`
	Role         string `gorm:"size:20;not null;default:user"`
	Status       string `gorm:"size:20;not null;default:active"`
	// StripeCustomerID 是 *string 而不是 string：绝大多数用户没有 customer，
	// 存 '' 的话所有这些用户会在唯一索引上互相冲突。NULL 之间互不相等。
	StripeCustomerID *string `gorm:"uniqueIndex;size:64"`
	CreatedAt        time.Time
	UpdatedAt        time.Time
}
