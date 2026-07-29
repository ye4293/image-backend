// 一次性命令：为 plans 表里还没有 stripe_price_id 的档位创建 Stripe Product 与 Price，
// 并把 Price ID 写回。
//
// 为什么用代码建而不是在 Dashboard 手建：价格、次数、Price ID 三者必须一致。
// 手工抄 ID 一旦错位，表现是"用户付了 Pro 的钱、拿到 Starter 的次数"——而这种
// 错位在测试时很可能撞不到，因为你只会盯着自己刚建的那一档测。
//
// **幂等且绝不覆盖**：已有 stripe_price_id 的行直接跳过。Stripe 的 Price 金额
// 不可变，重跑若重建 Price 就会产生一批重复商品，而老订阅仍绑在旧 Price 上。
//
// 用法：go run ./cmd/seed-stripe
package main

import (
	"context"
	"log"

	stripe "github.com/stripe/stripe-go/v86"

	"image-backend/internal/config"
	"image-backend/internal/database"
	"image-backend/internal/model"
)

func main() {
	cfg := config.Load()
	if cfg.StripeSecretKey == "" {
		log.Fatal("STRIPE_SECRET_KEY 未配置")
	}
	db, err := database.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	sc := stripe.NewClient(cfg.StripeSecretKey)
	ctx := context.Background()

	var plans []model.Plan
	if err := db.Order("sort_order asc").Find(&plans).Error; err != nil {
		log.Fatal(err)
	}
	for _, p := range plans {
		if p.StripePriceID != "" {
			log.Printf("跳过 %s：已有 Price %s", p.ID, p.StripePriceID)
			continue
		}
		prod, err := sc.V1Products.Create(ctx, &stripe.ProductCreateParams{
			Name:     stripe.String(p.DisplayName),
			Metadata: map[string]string{"plan_id": p.ID},
		})
		if err != nil {
			log.Fatalf("创建 Product 失败（%s）：%v", p.ID, err)
		}
		price, err := sc.V1Prices.Create(ctx, &stripe.PriceCreateParams{
			Product:    stripe.String(prod.ID),
			Currency:   stripe.String("usd"),
			UnitAmount: stripe.Int64(int64(p.PriceUSDCents)),
			Recurring:  &stripe.PriceCreateRecurringParams{Interval: stripe.String("month")},
			Metadata:   map[string]string{"plan_id": p.ID},
		})
		if err != nil {
			log.Fatalf("创建 Price 失败（%s）：%v", p.ID, err)
		}
		if err := db.Model(&model.Plan{}).Where("id = ?", p.ID).
			Update("stripe_price_id", price.ID).Error; err != nil {
			// 这里失败很难受：Stripe 侧已经建好了，库里没记下。重跑会再建一个。
			// 所以把 Price ID 打出来，人工回填比重跑安全。
			log.Fatalf("回填失败！请手工把 %s 的 stripe_price_id 设为 %s：%v", p.ID, price.ID, err)
		}
		log.Printf("%s：Product %s / Price %s（$%.2f/月，%d 次）",
			p.ID, prod.ID, price.ID, float64(p.PriceUSDCents)/100, p.MonthlyCredits)
	}
}
