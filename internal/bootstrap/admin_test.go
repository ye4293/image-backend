package bootstrap_test

import (
	"testing"

	"image-backend/internal/bootstrap"
	"image-backend/internal/database"
	"image-backend/internal/model"
)

func TestPromoteAdminPromotesExistingUser(t *testing.T) {
	db, err := database.Open("")
	if err != nil {
		t.Fatal(err)
	}
	u := model.User{Email: "boss@example.com", PasswordHash: "x", Role: "user", Status: "active"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}

	promoted, err := bootstrap.PromoteAdmin(db, "boss@example.com")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !promoted {
		t.Fatal("应当报告已提权")
	}
	var after model.User
	if err := db.First(&after, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Role != "admin" {
		t.Fatalf("role 应当变成 admin: got %q", after.Role)
	}
}

// 未配置时必须什么都不做——这是默认状态，任何副作用都是安全事故。
func TestPromoteAdminDoesNothingWhenUnset(t *testing.T) {
	db, err := database.Open("")
	if err != nil {
		t.Fatal(err)
	}
	u := model.User{Email: "nobody@example.com", PasswordHash: "x", Role: "user", Status: "active"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}

	promoted, err := bootstrap.PromoteAdmin(db, "")
	if err != nil {
		t.Fatalf("未配置时不该报错: %v", err)
	}
	if promoted {
		t.Fatal("未配置时不该提权任何人")
	}
	var after model.User
	_ = db.First(&after, u.ID).Error
	if after.Role != "user" {
		t.Fatalf("未配置时 role 不该变: got %q", after.Role)
	}
}

// 用户不存在不是错误：运维大概还没注册。必须继续启动，且**不能创建用户**——
// 它只提权，凭据仍然只能由注册接口产生。
func TestPromoteAdminMissingUserIsNotAnError(t *testing.T) {
	db, err := database.Open("")
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := bootstrap.PromoteAdmin(db, "ghost@example.com")
	if err != nil {
		t.Fatalf("用户不存在不该报错: %v", err)
	}
	if promoted {
		t.Fatal("不存在的用户不该被报告为已提权")
	}
	var n int64
	db.Model(&model.User{}).Count(&n)
	if n != 0 {
		t.Fatalf("不该创建任何用户: got %d", n)
	}
}

// 每次启动都会跑，所以重跑必须安全。
func TestPromoteAdminIsIdempotent(t *testing.T) {
	db, err := database.Open("")
	if err != nil {
		t.Fatal(err)
	}
	u := model.User{Email: "boss2@example.com", PasswordHash: "x", Role: "admin", Status: "active"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := bootstrap.PromoteAdmin(db, "boss2@example.com"); err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
	}
	var after model.User
	_ = db.First(&after, u.ID).Error
	if after.Role != "admin" {
		t.Fatalf("role 应当仍是 admin: got %q", after.Role)
	}
}
