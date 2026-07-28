package model

import "time"

// 生成任务状态。processing 是**落库时的初始状态**——调上游之前就要落行，
// 这样进程崩溃后启动扫描才能找到它并退款。
const (
	GenStatusProcessing = "processing"
	GenStatusSucceeded  = "succeeded"
	GenStatusFailed     = "failed"
)

type Generation struct {
	ID          string `gorm:"primaryKey;size:64"`
	UserID      uint   `gorm:"index;not null"`
	Model       string `gorm:"size:64;not null"`
	Prompt      string `gorm:"type:text;not null"`
	AspectRatio string `gorm:"size:16;not null"`
	Width       int    `gorm:"not null"`
	Height      int    `gorm:"not null"`
	// Status 见上面三个常量。索引是给启动扫描用的——它要按 status 找卡住的行。
	Status       string `gorm:"index;size:16;not null"`
	ImageURL     string `gorm:"type:text"`
	CreditsSpent int    `gorm:"not null;default:0"`
	// UpstreamID 是上游返回的任务 id，出问题时凭它去上游对账。
	UpstreamID   string `gorm:"size:128"`
	UpstreamCost int    `gorm:"not null;default:0"`
	Error        string `gorm:"type:text"`
	IsPublic     bool   `gorm:"not null;default:false"`
	DurationMs   int64  `gorm:"not null;default:0"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
