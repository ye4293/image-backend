package database

import (
	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

// Open 连接数据库。databaseURL 为空时使用 SQLite 内存库（本地开发/测试）。
func Open(databaseURL string) (*gorm.DB, error) {
	var dialector gorm.Dialector
	if databaseURL == "" {
		dialector = sqlite.Open(":memory:")
	} else {
		dialector = postgres.Open(databaseURL)
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&model.User{}); err != nil {
		return nil, err
	}
	return db, nil
}
