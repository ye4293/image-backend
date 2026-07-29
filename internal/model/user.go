package model

import "time"

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
	UpdatedAt    time.Time
}
