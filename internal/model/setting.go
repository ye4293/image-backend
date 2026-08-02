package model

import "time"

// AppSetting 是运营可在后台修改的配置项，key/value 一行一项。
//
// 选 key/value 而不是宽表：新增一项配置不用改表结构。代价是没有列级类型约束，
// 由 internal/settings 的白名单与写入校验补上——而那比数据库类型更能表达
// "R2 公开域名不能是 S3 API 域名"这类规则。
//
// **不是所有配置都在这里。** DATABASE_URL / JWT_SECRET / PORT 必须留在环境变量
// （管理员要登录才能改设置，而登录本身依赖它们），Stripe 的两个 secret 也刻意
// 留在环境变量（见设计文档 §3）。
type AppSetting struct {
	Key string `gorm:"primaryKey;size:64"`
	// Value 非 secret 项存明文；secret 项存 base64(nonce||ciphertext)。
	Value string `gorm:"type:text;not null"`
	// Encrypted 标记 Value 是否为密文。
	//
	// 显式存一列而不是靠 Key 推断：将来轮换加密方式时要能区分"这行还是旧格式"，
	// 靠 key 名推断会让迁移期无法判断。默认 false——写成 true 会让明文项被当成
	// 密文去解密，表现是启动时所有配置都解不开。
	Encrypted bool `gorm:"not null;default:false"`
	UpdatedAt time.Time
}
