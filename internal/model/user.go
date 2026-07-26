package model

import "time"

type User struct {
	ID           uint   `gorm:"primaryKey"`
	Email        string `gorm:"uniqueIndex;size:255;not null"`
	PasswordHash string `gorm:"size:255"`
	Role         string `gorm:"size:20;not null;default:user"`
	Status       string `gorm:"size:20;not null;default:active"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
