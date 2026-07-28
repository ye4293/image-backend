package model

import "time"

// ImageModel 对应 **image_models** 表——GORM 由类型名复数化而来，不是 models。
// 类型名不叫 Model 是为了避免与包名 model 读起来像 model.Model；不用 TableName()
// 把表名覆盖回 models，因为 image_models 本身更自描述，而 GORM 的表名覆盖是后
// 人要去翻代码才能发现的隐式魔法。
//
// Provider 决定运行时用哪个 adapter。不同 provider 的上游接口路径、请求体与响应
// 格式**完全不同**（产品要求兼容各家官方功能），差异全部关在各自 adapter 里，
// 本表只存"选哪个 adapter"和"上游模型名"。
type ImageModel struct {
	ID                   string `gorm:"primaryKey;size:64"`
	DisplayName          string `gorm:"size:100;not null"`
	Provider             string `gorm:"size:32;not null"`
	UpstreamModel        string `gorm:"size:100;not null"`
	Credits              int    `gorm:"not null"`
	SupportsImageToImage bool   `gorm:"not null;default:false"`
	Enabled              bool   `gorm:"not null;default:true"`
	SortOrder            int    `gorm:"not null;default:0"`
	CreatedAt            time.Time
	UpdatedAt            time.Time
}
