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
	ID string `gorm:"primaryKey;size:64"`
	// UserID 与 CreatedAt 组成复合索引 idx_gen_user_created，专门服务历史查询
	// （WHERE user_id = ? ORDER BY created_at DESC）。只留单列索引的话排序要落到
	// 额外的 sort。
	UserID      uint   `gorm:"index:idx_gen_user_created,priority:1;not null"`
	Model       string `gorm:"size:64;not null"`
	Prompt      string `gorm:"type:text;not null"`
	AspectRatio string `gorm:"size:16;not null"`
	Width       int    `gorm:"not null"`
	Height      int    `gorm:"not null"`
	// Status 见上面三个常量。索引是给启动扫描用的——它要按 status 找卡住的行。
	Status       string `gorm:"index;size:16;not null"`
	ImageURL     string `gorm:"type:text"`
	CreditsSpent int    `gorm:"not null;default:0"`
	// Stored 图片是否已转存到我们自己的存储。
	//
	// false 有两种来源：R2 未配置（本地开发），或转存失败后降级。两种情况下
	// ImageURL 都是上游的临时链接，约一小时后失效——历史接口把这一列透出去，
	// 前端才能诚实地提示"链接可能已失效"，而不是让用户对着坏图猜。
	//
	// **不配套加 storage_key 列**：key 从 ID 确定性推导（g/<id>.<ext>），再存
	// 一份就是两份可能不一致的真相。
	Stored bool `gorm:"not null;default:false"`
	// UpstreamID 是上游返回的任务 id，出问题时凭它去上游对账。
	UpstreamID   string    `gorm:"size:128"`
	UpstreamCost int       `gorm:"not null;default:0"`
	Error        string    `gorm:"type:text"`
	IsPublic     bool      `gorm:"not null;default:false"`
	DurationMs   int64     `gorm:"not null;default:0"`
	CreatedAt    time.Time `gorm:"index:idx_gen_user_created,priority:2"`
	UpdatedAt    time.Time
}
