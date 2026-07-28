// Package bootstrap 收拢"服务开始接流量之前要做一次"的启动动作。
package bootstrap

import (
	"errors"
	"log"

	"gorm.io/gorm"

	"image-backend/internal/model"
)

// PromoteAdmin 把 email 对应的用户提权为管理员，返回是否真的提权了。
//
// 为什么需要它：此前拿到第一个管理员的**唯一**办法是手工执行
// `UPDATE users SET role='admin'`。那既是部署缺口（新环境起来后没有任何人能调
// /api/v1/admin/*），也让端到端测试无法自动准备数据。
//
// 边界划得很紧，它不是后门：
//   - email 为空（默认值）时什么都不做；
//   - **不创建用户**——凭据仍然只能由注册接口产生，密码哈希不经此处；
//   - 不绕过任何认证：被提权的账号仍要正常登录拿 JWT；
//   - 用户不存在不是错误（运维大概还没注册），只记日志并继续启动。
//
// 每次启动都会跑，所以必须幂等——已经是 admin 时 Update 是空操作。
func PromoteAdmin(db *gorm.DB, email string) (bool, error) {
	if email == "" {
		return false, nil
	}
	var user model.User
	if err := db.Where("email = ?", email).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 刻意不返回错误、也不建用户：最常见的情况就是运维刚部署完还没注册。
			// 但必须喊清楚，否则表现为"配了却没生效"，没人知道该去注册。
			log.Printf("bootstrap: BOOTSTRAP_ADMIN_EMAIL=%q 对应的用户不存在，未提权——"+
				"先用该邮箱注册，然后重启服务", email)
			return false, nil
		}
		return false, err
	}
	if user.Role == "admin" {
		log.Printf("bootstrap: %q 已经是管理员，无需提权", email)
		return false, nil
	}
	if err := db.Model(&model.User{}).Where("id = ?", user.ID).
		Update("role", "admin").Error; err != nil {
		return false, err
	}
	log.Printf("bootstrap: 已把 %q（用户 #%d）提权为管理员（BOOTSTRAP_ADMIN_EMAIL）", email, user.ID)
	return true, nil
}
