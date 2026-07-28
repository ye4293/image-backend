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

// ErrAlreadySpent 同一个 generation 已经扣过费。由 (generation_id, type) 上的
// 唯一索引兜住——重复扣费属于调用方 bug，必须报错而不是静默吞掉。
var ErrAlreadySpent = errors.New("generation already charged")

// errAlreadyRefunded 是内部哨兵：退款流水插入撞唯一键时用它把事务**回滚**掉，
// 再在事务外转成 nil 返回。不能在闭包里直接 return nil——那样上面已经加过钱的
// 那次 UPDATE 会被提交，变成一次没有流水的退款（正是唯一索引要防的双退）。
var errAlreadyRefunded = errors.New("already refunded")

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
//
// 本设计针对 **READ COMMITTED**（Postgres 默认）：FOR UPDATE 阻塞解除后
// Postgres 会 EvalPlanQual 重新求值，所以读到的 acct 是前一个事务提交后的新
// 值而非陈旧快照。在 REPEATABLE READ 下同样的交错会抛序列化失败，需要调用方
// 重试——本包不实现重试。
//
// 同一个 generationID 重复调用会撞 (generation_id, type) 唯一索引并返回
// ErrAlreadySpent，不会扣两次。
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
			GenerationID: genIDPtr(generationID),
		}
		if err := tx.Create(&txRow).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrAlreadySpent
			}
			return err
		}
		split = s
		return nil
	})
	if err != nil {
		return Split{}, err
	}
	return split, nil
}

// Refund 按原扣费拆分退回次数。
//
// **不接收 userID 参数**：退给谁由扣费流水说了算，不由调用方说了算。早先的
// 签名是 Refund(db, userID, generationID)，那样 handler 把 JWT 里的 userID 和
// 请求里的 generationID 一拼，拿别人的 generation ID 就能给自己造钱，还会留下
// "用户 A 的退款流水指向用户 B 的扣费流水"这种无法对账的脏数据。把参数删掉比
// 校验它更彻底——错误变得不可表达。
//
// 幂等靠 (generation_id, type) 唯一索引，不靠"先 Count 再 INSERT"：后者在
// READ COMMITTED 下有窗口，两个并发退款会各数到 0 然后都插进去，退两次。
// Count 只作为省一趟写的快速路径保留。
//
// 没有对应扣费流水时静默返回——启动兜底扫描会对"扣费失败但落了 processing
// 行"的情况调用本函数，那时本就没有要退的东西。
func Refund(db *gorm.DB, generationID string) error {
	err := db.Transaction(func(tx *gorm.DB) error {
		var refunded int64
		if err := tx.Model(&model.CreditTransaction{}).
			Where("generation_id = ? AND type = ?", generationID, model.TxGenerationRefund).
			Count(&refunded).Error; err != nil {
			return err
		}
		if refunded > 0 {
			return errAlreadyRefunded // 快速路径：已退过
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

		// 退给谁由流水说了算。
		userID := cost.UserID
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
		err = tx.Create(&model.CreditTransaction{
			UserID:       userID,
			Type:         model.TxGenerationRefund,
			MonthlyDelta: monthly,
			AddonDelta:   addon,
			MonthlyAfter: acct.MonthlyCredits + monthly,
			AddonAfter:   acct.AddonCredits + addon,
			GenerationID: genIDPtr(generationID),
			Note:         "生成失败退回",
		}).Error
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			// 并发的另一笔退款抢先提交了。回滚本次（含上面那次加钱），
			// 由事务外转成成功——退款本就该是幂等的。
			return errAlreadyRefunded
		}
		return err
	})
	if errors.Is(err, errAlreadyRefunded) {
		return nil
	}
	return err
}

// Grant 发放次数（管理员操作）。账户不存在时创建。
//
// **拒绝负数**：本包声称自己是余额变动的唯一安全路径，而 Grant 走的是不带
// WHERE 守卫、不校验 RowsAffected 的相对 UPDATE。放负数进来等于开了一条零防护
// 的扣款通道，能把余额直接扣成负数。管理员冲正如果将来要做，得走一条和 Spend
// 同样三层防护的独立路径，不能挂在这里。
func Grant(db *gorm.DB, userID uint, monthly, addon int, note string) error {
	if monthly < 0 || addon < 0 {
		return fmt.Errorf("发放数量不能为负（monthly=%d addon=%d）；扣减必须走 Spend", monthly, addon)
	}
	if monthly == 0 && addon == 0 {
		return fmt.Errorf("发放数量不能全为 0")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		// 先确保账户行存在。用 OnConflict DoNothing 而不是 FirstOrCreate：
		// 两个并发发放同时插同一主键时，后插的那个会以唯一键冲突**中止整个
		// 事务**，那个错误会原样抛给调用方。DoNothing 让它退化成一次空操作。
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&model.CreditAccount{UserID: userID}).Error; err != nil {
			return err
		}
		// 加锁重读。快照列必须基于**锁内**读到的值：不加锁的话两个并发 +10
		// 打在 100 上，余额会正确地变成 120（相对 UPDATE 本身是安全的），但两
		// 条流水都会写 MonthlyAfter=110——账本说 110/110、账户说 120，正是快照
		// 列本来要防的那种对账失败。
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
			Type:         model.TxAdminGrant,
			MonthlyDelta: monthly,
			AddonDelta:   addon,
			MonthlyAfter: acct.MonthlyCredits + monthly,
			AddonAfter:   acct.AddonCredits + addon,
			Note:         note,
			// GenerationID 留 nil：发放不关联生成任务，而 NULL 之间互不相等，
			// 多条发放流水不会在 (generation_id, type) 唯一索引上互相打架。
		}).Error
	})
}

// genIDPtr 把空串转成 nil。空串会在 (generation_id, type) 唯一索引上和其他空串
// 冲突，NULL 不会。
func genIDPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
