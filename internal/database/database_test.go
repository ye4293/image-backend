package database

import (
	"testing"

	"image-backend/internal/model"
)

func TestOpenMigratesUserTable(t *testing.T) {
	db, err := Open("") // 空 DSN → dev 模式 SQLite（临时文件库）
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	u := model.User{Email: "a@b.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}
	// 验证 Role/Status 数据库默认值
	var got model.User
	if err := db.First(&got, u.ID).Error; err != nil {
		t.Fatalf("load user failed: %v", err)
	}
	if got.Role != "user" || got.Status != "active" {
		t.Fatalf("expected default role=user status=active, got role=%q status=%q", got.Role, got.Status)
	}
	dup := model.User{Email: "a@b.com", PasswordHash: "y"}
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("expected unique constraint error for duplicate email")
	}
}

// 回归测试：dev 模式数据库必须在连接池新开连接后仍可见同一份数据。
// 若用 ":memory:" 内存库（glebarez 驱动不支持跨连接共享），新连接会得到独立空库，
// 出现 "no such table: users"。SetMaxIdleConns(0) 强制每次操作新建连接来复现该场景。
func TestOpenDevModeSurvivesNewConnections(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB failed: %v", err)
	}
	sqlDB.SetMaxIdleConns(0) // 不保留空闲连接，强制后续操作走新连接
	u := model.User{Email: "fresh-conn@b.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user on fresh connection failed (in-memory db not shared across connections?): %v", err)
	}
}
