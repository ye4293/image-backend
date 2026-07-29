package database

import (
	"log"
	"os"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

// IsEphemeral 判断该 databaseURL 是否对应"进程退出即弃"的库。
//
// cmd/seed-stripe 用它拒绝在一次性库上跑：那个命令会在真实 Stripe 账号里创建
// Product 与 Price（真钱对象、且 Price 金额不可变无法删改），把 ID 写回库里。
// 若库是临时文件，ID 随进程消失而服务端永远看不到，重跑只会再堆一批重复商品。
func IsEphemeral(databaseURL string) bool { return databaseURL == "" }

// Open 连接数据库。databaseURL 的取值决定驱动与持久性：
//
//	""                    → 临时文件 SQLite，进程退出即弃（测试、一次性试跑）
//	postgres://…
//	postgresql://…        → Postgres
//	其他任意值            → 当成 SQLite 文件路径，**数据持久**
//
// 第三种是本地开发该用的。没有持久选项的话每次重启都是一个新的空库：账号要重新
// 注册，订阅行与已发放的次数全部消失——而订阅联调（付款→发额度→续费→退订→重投
// 事件）跨越多次重启，在一次性库上根本走不完。设 DATABASE_URL=./local.db 即可。
//
// 注意：不能用 ":memory:" 内存库——glebarez/sqlite（modernc 纯 Go 驱动）不支持
// cache=shared，每个新连接都会得到一个独立的空库，连接池新开连接时会随机出现
// "no such table" 错误。文件库跨连接共享同一份数据，没有该问题。
func Open(databaseURL string) (*gorm.DB, error) {
	var dialector gorm.Dialector
	ephemeral := IsEphemeral(databaseURL)
	usingSQLite := true
	var sqlitePath string
	switch {
	case ephemeral:
		f, err := os.CreateTemp("", "image-backend-dev-*.db")
		if err != nil {
			return nil, err
		}
		sqlitePath = f.Name()
		if err := f.Close(); err != nil {
			return nil, err
		}
		dialector = sqlite.Open(sqlitePath)
	case strings.HasPrefix(databaseURL, "postgres://"),
		strings.HasPrefix(databaseURL, "postgresql://"):
		usingSQLite = false
		dialector = postgres.Open(databaseURL)
	default:
		sqlitePath = databaseURL
		dialector = sqlite.Open(sqlitePath)
	}
	db, err := gorm.Open(dialector, &gorm.Config{TranslateError: true})
	if err != nil {
		return nil, err
	}
	if usingSQLite {
		// SQLite 是单写者，限制为单连接避免并发写锁冲突。
		sqlDB, err := db.DB()
		if err != nil {
			return nil, err
		}
		sqlDB.SetMaxOpenConns(1)
		if ephemeral {
			log.Printf("database: 临时 SQLite %s，进程退出即弃（未配置 DATABASE_URL）；"+
				"本地要持久保存请设 DATABASE_URL=./local.db", sqlitePath)
		} else {
			log.Printf("database: SQLite 文件 %s", sqlitePath)
		}
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.ImageModel{},
		&model.CreditAccount{},
		&model.CreditTransaction{},
		&model.Generation{},
		&model.Plan{},
		&model.Subscription{},
		&model.StripeEvent{},
	); err != nil {
		return nil, err
	}
	if err := seedModels(db); err != nil {
		return nil, err
	}
	if err := seedPlans(db); err != nil {
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

// seedPlans 幂等地播种三个档位。
//
// 用 FirstOrCreate 而不是 Save：价格与次数是**运营可改**的，每次启动覆盖回
// 默认值会把线上调价悄悄抹掉（seedModels 同理）。StripePriceID 尤其不能被
// 覆盖成空——那会让 cmd/seed-stripe 重新建一批 Price，产生重复商品。
func seedPlans(db *gorm.DB) error {
	plans := []model.Plan{
		{ID: "starter", DisplayName: "Starter", PriceUSDCents: 990, MonthlyCredits: 200, Enabled: true, SortOrder: 10},
		{ID: "pro", DisplayName: "Pro", PriceUSDCents: 2990, MonthlyCredits: 800, Enabled: true, SortOrder: 20},
		{ID: "max", DisplayName: "Max", PriceUSDCents: 4990, MonthlyCredits: 3000, Enabled: true, SortOrder: 30},
	}
	for i := range plans {
		if err := db.Where(model.Plan{ID: plans[i].ID}).FirstOrCreate(&plans[i]).Error; err != nil {
			return err
		}
	}
	return nil
}
