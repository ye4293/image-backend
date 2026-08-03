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

// ErrInvalidGrantAmount 发放数量非法（负数或全为 0）。
//
// 单独一个哨兵，是为了让 handler 能把"调用方参数写错"（400）和"数据库炸了"
// （500）区分开。此前两者都被当成 400 并原样回传 err.Error()，既泄露内部信息，
// 又把基础设施故障伪装成用户的参数错误。
var ErrInvalidGrantAmount = errors.New("invalid grant amount")

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
		return fmt.Errorf("%w：发放数量不能为负（monthly=%d addon=%d）；扣减必须走 Spend",
			ErrInvalidGrantAmount, monthly, addon)
	}
	if monthly == 0 && addon == 0 {
		return fmt.Errorf("%w：发放数量不能全为 0", ErrInvalidGrantAmount)
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

// ErrAlreadyGranted 同一个外部事件已经发放过。
var ErrAlreadyGranted = errors.New("credits already granted for this event")

// errNotInTransaction 调用方没有提供事务。
var errNotInTransaction = errors.New("credit.ResetMonthly 必须在调用方的事务内调用")

// ResetMonthly 把月度次数**设置**为 amount，加量包次数不动。
//
// 与 Grant 的区别是"设置"而非"累加"。续费若累加，用不完的次数会攒起来，
// 与定价页承诺的"月度次数不累积到下月"直接矛盾。
//
// 允许结果低于当前余额（高档降到低档），这与 Grant 拒绝负数不冲突：这里是把
// 余额**设**到一个由套餐决定的非负值，不存在扣成负数的路径。
//
// **必须由调用方提供事务。** webhook 要求"幂等记录与发放同生共死"：若分属两个
// 事务，进程在两步之间崩溃会留下"记了已处理但没发放"——那是永久漏发一次，
// 比重复发放更难发现（重复发放至少余额对不上，漏发看起来一切正常）。
func ResetMonthly(tx *gorm.DB, userID uint, amount int, externalID, note string) error {
	if amount < 0 {
		return fmt.Errorf("%w：月度次数不能为负，得到 %d", ErrInvalidGrantAmount, amount)
	}
	if externalID == "" {
		return errors.New("externalID 必填——它既是对账线索也是兜底幂等键")
	}
	// 先判 Statement 再取 ConnPool：裸 db 上 Statement 可能为 nil，顺序反了会 panic。
	if tx.Statement == nil {
		return errNotInTransaction
	}
	if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return errNotInTransaction
	}

	// 建账户行用 OnConflict DoNothing 而不是 FirstOrCreate，理由同 Grant 的注释：
	// 并发插同一主键时唯一键冲突会中止整个事务。
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&model.CreditAccount{UserID: userID}).Error; err != nil {
		return err
	}
	// 加锁重读：快照列必须基于锁内读到的值，理由同 Grant 的注释。
	var acct model.CreditAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ?", userID).First(&acct).Error; err != nil {
		return err
	}
	if err := tx.Model(&model.CreditAccount{}).Where("user_id = ?", userID).
		Update("monthly_credits", amount).Error; err != nil {
		return err
	}
	err := tx.Create(&model.CreditTransaction{
		UserID:       userID,
		Type:         model.TxSubscriptionGrant,
		MonthlyDelta: amount - acct.MonthlyCredits,
		AddonDelta:   0, // 加量包是单独付费买的，续费绝不能动它
		MonthlyAfter: amount,
		AddonAfter:   acct.AddonCredits,
		ExternalID:   &externalID,
		Note:         note,
	}).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrAlreadyGranted
	}
	return err
}

// genIDPtr 把空串转成 nil。空串会在 (generation_id, type) 唯一索引上和其他空串
// 冲突，NULL 不会。
func genIDPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GrantSignupBonus 发放注册赠送的体验额度，**同一个用户只会成功一次**。
//
// 与 Grant 的区别只有幂等：Grant 把 ExternalID 留 nil，而 NULL 之间互不相等，
// 所以调两次就加两次（本包测试里有一条专门钉住那个行为）。这里填
// ExternalID = "signup:<userID>"，靠 (external_id, type) 唯一索引挡住重复发放，
// 重复时返回 ErrAlreadyGranted——调用方可以当成正常情况忽略。
//
// 为什么幂等在这里是必需的：注册流程里"建用户"与"发额度"是两步，中间任何一次重试
// （客户端重发、部署期请求重放、将来加了邮箱验证后的补发）都可能让同一个 userID
// 第二次走到这里。而这个函数发的是真金白银，每一次多发都是实打实的上游成本。
//
// **发成 monthly 而不是 addon** 是刻意的：addon 的语义是"单独付费买的、永不过期、
// 续费也不动它"。记成 addon，这笔白送的额度会永久叠加在用户后来付费买的额度之上；
// 记成 monthly 则会在首次 invoice.paid 时被 ResetMonthly 自然覆盖——也就是
// "试用额度用完即止"，那正是赠送想要的语义。
func GrantSignupBonus(db *gorm.DB, userID uint, amount int) error {
	if amount <= 0 {
		return fmt.Errorf("%w：注册赠送数量必须为正（amount=%d）", ErrInvalidGrantAmount, amount)
	}
	externalID := fmt.Sprintf("signup:%d", userID)

	return db.Transaction(func(tx *gorm.DB) error {
		// 建账户行。注册路径上它必然还不存在，但仍用 OnConflict DoNothing 而不是裸
		// Create：理由同 Grant 的注释，并发插同一主键会中止整个事务。
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&model.CreditAccount{UserID: userID}).Error; err != nil {
			return err
		}
		// 加锁重读，快照列必须基于锁内读到的值（理由同 Grant）。
		var acct model.CreditAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ?", userID).First(&acct).Error; err != nil {
			return err
		}

		// **先写流水再改余额。** 反过来的话重复调用会先把余额加上、再在唯一索引上
		// 失败，虽然事务会回滚，但那让幂等依赖"回滚生效"而不是依赖约束本身。
		// 先写流水则冲突立刻发生，余额那条 UPDATE 根本不会执行。
		if err := tx.Create(&model.CreditTransaction{
			UserID:       userID,
			Type:         model.TxSignupGrant,
			MonthlyDelta: amount,
			AddonDelta:   0,
			MonthlyAfter: acct.MonthlyCredits + amount,
			AddonAfter:   acct.AddonCredits,
			ExternalID:   &externalID,
			Note:         "signup bonus",
		}).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrAlreadyGranted
			}
			return err
		}

		return tx.Model(&model.CreditAccount{}).Where("user_id = ?", userID).
			Update("monthly_credits", gorm.Expr("monthly_credits + ?", amount)).Error
	})
}

// BalancesFor 批量取余额，用于后台用户列表。
//
// 单独一个函数而不是让 handler 自己查表：本包包头的约定是"余额的唯一入口"，读也走
// 同一个门。否则下一个人会顺手在别处再写一遍查询，而"账户行不存在 = 零余额"这个
// 语义就有了两份实现。
//
// **账户行不存在的用户不会出现在返回的 map 里**，调用方对缺失的 key 取零值即可。
// 这与 Balance 对不存在账户返回零值 CreditAccount 是同一个约定——刚注册且没拿到
// 赠送额度的用户正是这种情况。
func BalancesFor(db *gorm.DB, userIDs []uint) (map[uint]model.CreditAccount, error) {
	out := make(map[uint]model.CreditAccount, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}
	var accts []model.CreditAccount
	if err := db.Where("user_id IN ?", userIDs).Find(&accts).Error; err != nil {
		return nil, err
	}
	for _, a := range accts {
		out[a.UserID] = a
	}
	return out, nil
}
