package database

import (
	"log"
	"os"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

// Open 连接数据库。databaseURL 为空时使用临时文件 SQLite（本地开发/测试，dev 模式）。
//
// 注意：不能用 ":memory:" 内存库——glebarez/sqlite（modernc 纯 Go 驱动）不支持
// cache=shared，每个新连接都会得到一个独立的空库，连接池新开连接时会随机出现
// "no such table" 错误。临时文件库跨连接共享同一份数据，没有该问题。
func Open(databaseURL string) (*gorm.DB, error) {
	var dialector gorm.Dialector
	devMode := databaseURL == ""
	var devPath string
	if devMode {
		f, err := os.CreateTemp("", "image-backend-dev-*.db")
		if err != nil {
			return nil, err
		}
		devPath = f.Name()
		if err := f.Close(); err != nil {
			return nil, err
		}
		dialector = sqlite.Open(devPath)
	} else {
		dialector = postgres.Open(databaseURL)
	}
	db, err := gorm.Open(dialector, &gorm.Config{TranslateError: true})
	if err != nil {
		return nil, err
	}
	if devMode {
		// SQLite 是单写者，限制为单连接避免并发写锁冲突。
		sqlDB, err := db.DB()
		if err != nil {
			return nil, err
		}
		sqlDB.SetMaxOpenConns(1)
		log.Printf("database: using temporary SQLite %s (dev mode), no DATABASE_URL configured", devPath)
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.ImageModel{},
		&model.CreditAccount{},
		&model.CreditTransaction{},
	); err != nil {
		return nil, err
	}
	if err := seedModels(db); err != nil {
		return nil, err
	}
	return db, nil
}

// seedModels 幂等地播种内置模型配置。
//
// 用 FirstOrCreate 而不是 Save：Credits 等字段是**运营可改**的（后台调价），
// 每次启动覆盖回默认值会把线上调整悄悄抹掉。
func seedModels(db *gorm.DB) error {
	flux := model.ImageModel{
		ID:                   "flux-2-max",
		DisplayName:          "Flux 2 Max",
		Provider:             "flux",
		UpstreamModel:        "flux-2-max",
		Credits:              1,
		SupportsImageToImage: false,
		Enabled:              true,
		SortOrder:            10,
	}
	return db.Where(model.ImageModel{ID: flux.ID}).FirstOrCreate(&flux).Error
}
