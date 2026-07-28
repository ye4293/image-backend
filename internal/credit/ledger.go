// Package credit 是余额变动的唯一入口。
//
// handler 不得直接写 credit_accounts——绕过本包就意味着漏流水，而漏了流水
// 的余额是无法对账的（出问题时只能猜）。
package credit

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"image-backend/internal/model"
)

// ErrInsufficientCredits 余额不足。调用方据此返回 40001 / HTTP 402。
var ErrInsufficientCredits = errors.New("insufficient credits")

// Split 是一次扣费在两种余额上的分配。
//
// 之所以要把拆分返回并落库，是因为**退款必须按同样的拆分还回去**。
type Split struct {
	Monthly int
	Addon   int
}

// Balance 读余额。账户不存在时返回零值而非报错——新注册用户在拿到第一笔
// 发放前没有账户行，那不是异常。
func Balance(db *gorm.DB, userID uint) (model.CreditAccount, error) {
	var acct model.CreditAccount
	err := db.Where("user_id = ?", userID).First(&acct).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.CreditAccount{UserID: userID}, nil
	}
	if err != nil {
		return model.CreditAccount{}, err
	}
	return acct, nil
}

// planSpend 先扣月度、不足再扣加量包。余额不够返回 false。
func planSpend(acct model.CreditAccount, cost int) (Split, bool) {
	if acct.MonthlyCredits+acct.AddonCredits < cost {
		return Split{}, false
	}
	monthly := cost
	if acct.MonthlyCredits < cost {
		monthly = acct.MonthlyCredits
	}
	return Split{Monthly: monthly, Addon: cost - monthly}, true
}

// Spend 扣费并写流水，返回实际拆分。
//
// 三重保险，缺一不可：
//  1. 整个过程在一个事务里——余额变动与流水必须同生共死；
//  2. SELECT 加 FOR UPDATE 行锁（Postgres 生效；SQLite 忽略，靠单连接串行化）；
//  3. UPDATE 带 WHERE 条件并校验 RowsAffected==1——即使前两层被绕过（比如
//     有人把隔离级别调低），带条件的更新也不会扣成负数。
//
// **不要**改成"先 SELECT 判断、再无条件 UPDATE"：那中间有窗口。
func Spend(db *gorm.DB, userID uint, cost int, generationID string) (Split, error) {
	if cost <= 0 {
		return Split{}, fmt.Errorf("cost 必须为正整数，得到 %d", cost)
	}
	var split Split
	err := db.Transaction(func(tx *gorm.DB) error {
		var acct model.CreditAccount
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ?", userID).First(&acct).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrInsufficientCredits // 没有账户等于没有余额
		}
		if err != nil {
			return err
		}

		s, ok := planSpend(acct, cost)
		if !ok {
			return ErrInsufficientCredits
		}

		res := tx.Model(&model.CreditAccount{}).
			Where("user_id = ? AND monthly_credits >= ? AND addon_credits >= ?",
				userID, s.Monthly, s.Addon).
			Updates(map[string]any{
				"monthly_credits": gorm.Expr("monthly_credits - ?", s.Monthly),
				"addon_credits":   gorm.Expr("addon_credits - ?", s.Addon),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			// 条件不成立：余额在锁之外被改过，或隔离级别不足。
			return ErrInsufficientCredits
		}

		txRow := model.CreditTransaction{
			UserID:       userID,
			Type:         model.TxGenerationCost,
			MonthlyDelta: -s.Monthly,
			AddonDelta:   -s.Addon,
			MonthlyAfter: acct.MonthlyCredits - s.Monthly,
			AddonAfter:   acct.AddonCredits - s.Addon,
			GenerationID: generationID,
		}
		if err := tx.Create(&txRow).Error; err != nil {
			return err
		}
		split = s
		return nil
	})
	return split, err
}

// Refund 按原扣费拆分退回次数。
//
// 幂等条件：该 generation 已有 generation_refund 流水则直接返回。没有对应
// 扣费流水时也静默返回——启动兜底扫描会对"扣费失败但落了 processing 行"的
// 情况调用本函数，那时本就没有要退的东西。
func Refund(db *gorm.DB, userID uint, generationID string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var refunded int64
		if err := tx.Model(&model.CreditTransaction{}).
			Where("generation_id = ? AND type = ?", generationID, model.TxGenerationRefund).
			Count(&refunded).Error; err != nil {
			return err
		}
		if refunded > 0 {
			return nil // 已退过
		}

		var cost model.CreditTransaction
		err := tx.Where("generation_id = ? AND type = ?", generationID, model.TxGenerationCost).
			First(&cost).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // 没扣过，无需退
		}
		if err != nil {
			return err
		}

		// 按原拆分还回：cost 的 delta 是负数，取反即为要加回的数量。
		monthly, addon := -cost.MonthlyDelta, -cost.AddonDelta

		var acct model.CreditAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ?", userID).First(&acct).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.CreditAccount{}).Where("user_id = ?", userID).
			Updates(map[string]any{
				"monthly_credits": gorm.Expr("monthly_credits + ?", monthly),
				"addon_credits":   gorm.Expr("addon_credits + ?", addon),
			}).Error; err != nil {
			return err
		}
		return tx.Create(&model.CreditTransaction{
			UserID:       userID,
			Type:         model.TxGenerationRefund,
			MonthlyDelta: monthly,
			AddonDelta:   addon,
			MonthlyAfter: acct.MonthlyCredits + monthly,
			AddonAfter:   acct.AddonCredits + addon,
			GenerationID: generationID,
			Note:         "生成失败退回",
		}).Error
	})
}

// Grant 发放次数（管理员操作）。账户不存在时创建。
func Grant(db *gorm.DB, userID uint, monthly, addon int, note string) error {
	if monthly == 0 && addon == 0 {
		return fmt.Errorf("发放数量不能全为 0")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		acct := model.CreditAccount{UserID: userID}
		if err := tx.Where("user_id = ?", userID).
			FirstOrCreate(&acct, model.CreditAccount{UserID: userID}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.CreditAccount{}).Where("user_id = ?", userID).
			Updates(map[string]any{
				"monthly_credits": gorm.Expr("monthly_credits + ?", monthly),
				"addon_credits":   gorm.Expr("addon_credits + ?", addon),
			}).Error; err != nil {
			return err
		}
		return tx.Create(&model.CreditTransaction{
			UserID:       userID,
			Type:         model.TxAdminGrant,
			MonthlyDelta: monthly,
			AddonDelta:   addon,
			MonthlyAfter: acct.MonthlyCredits + monthly,
			AddonAfter:   acct.AddonCredits + addon,
			Note:         note,
		}).Error
	})
}
