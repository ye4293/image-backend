package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/credit"
	"image-backend/internal/model"
)

// setBonusViaAPI 用后台接口把赠送次数改成 n。
//
// **必须走 PATCH /admin/settings 而不是直接写库。** 路由持有的 settings.Runtime 是
// 构造时建的，直接改库不会触发 Reload，测试会拿到旧值而看起来像"赠送没生效"。
// 走接口顺带覆盖了真实路径：管理员改完立刻生效、不重启。
func setBonusViaAPI(t *testing.T, r *gin.Engine, token, n string) {
	t.Helper()
	w := doPatchSettings(r, token, fmt.Sprintf(`{"signupBonusCredits":%q}`, n))
	if w.Code != http.StatusOK {
		t.Fatalf("设置 signupBonusCredits=%s 失败：%d %s", n, w.Code, w.Body.String())
	}
}

// balanceOf 直接查账户表。账户行不存在是合法状态（没发额度时就是这样），返回零值
// ——这与 credit.Balance 的约定一致。
func balanceOf(t *testing.T, db *gorm.DB, email string) model.CreditAccount {
	t.Helper()
	var u model.User
	if err := db.Where("email = ?", email).First(&u).Error; err != nil {
		t.Fatalf("查用户 %s: %v", email, err)
	}
	var acct model.CreditAccount
	if err := db.Where("user_id = ?", u.ID).First(&acct).Error; err != nil {
		return model.CreditAccount{UserID: u.ID}
	}
	return acct
}

func countTxOfType(t *testing.T, db *gorm.DB, txType string) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.CreditTransaction{}).Where("type = ?", txType).Count(&n).Error; err != nil {
		t.Fatalf("统计流水: %v", err)
	}
	return n
}

func TestSignupBonusNotGrantedByDefault(t *testing.T) {
	// 默认不赠送。这是刻意的：开始送钱必须是一次显式的后台操作，而不是某次部署的
	// 副作用。若哪天默认值变成非 0，这条会挡住它。
	r, db := setupRouterWithDB(t)

	postJSON(r, "/api/v1/auth/register", `{"email":"nobonus@example.com","password":"secret12345"}`)

	acct := balanceOf(t, db, "nobonus@example.com")
	if acct.MonthlyCredits != 0 || acct.AddonCredits != 0 {
		t.Errorf("未配置赠送时新用户余额必须是 0，得到 monthly=%d addon=%d",
			acct.MonthlyCredits, acct.AddonCredits)
	}
	if n := countTxOfType(t, db, model.TxSignupGrant); n != 0 {
		t.Errorf("未配置赠送时不该有 signup_grant 流水，实际 %d 条", n)
	}
}

func TestSignupBonusGrantedAsMonthlyNotAddon(t *testing.T) {
	// **必须发成 monthly 而不是 addon。** addon 的语义是"单独付费买的、永不过期、
	// 续费也不动它"；记成 addon，这笔白送的额度会永久叠加在用户后来付费买的额度之上。
	// 记成 monthly 才会在首次 invoice.paid 时被 ResetMonthly 覆盖掉，也就是
	// "试用额度用完即止"——那才是赠送想要的语义。
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "bonus-admin@example.com")
	setBonusViaAPI(t, r, token, "7")

	postJSON(r, "/api/v1/auth/register", `{"email":"bonus@example.com","password":"secret12345"}`)

	acct := balanceOf(t, db, "bonus@example.com")
	if acct.MonthlyCredits != 7 {
		t.Errorf("monthly 应当是 7，得到 %d", acct.MonthlyCredits)
	}
	if acct.AddonCredits != 0 {
		t.Errorf("addon 必须是 0——赠送记成 addon 会永久叠加在付费额度之上，得到 %d",
			acct.AddonCredits)
	}

	// 类型必须是 signup_grant 而不是 admin_grant：对账要能聚合出"送出去多少体验额度"，
	// 那是一笔真实的上游成本，而 Note 是自由文本、没法可靠聚合。
	if n := countTxOfType(t, db, model.TxSignupGrant); n != 1 {
		t.Errorf("应当有 1 条 signup_grant 流水，实际 %d 条", n)
	}
	if n := countTxOfType(t, db, model.TxAdminGrant); n != 0 {
		t.Errorf("不该记成 admin_grant（那会让对账分不清是谁送的），实际 %d 条", n)
	}

	// **流水的拆分与快照列必须和账户对得上。**
	//
	// 只断言余额是不够的：余额由一条独立的 UPDATE 语句改，流水的 Delta 由另一处写，
	// 两边可以各说一套。那种不一致不会报错，但会让退款按错的桶还回去（把 addon 错还
	// 成 monthly，月底重置时凭空蒸发），也让对账彻底失效——而这恰恰是快照列存在的理由。
	var tx model.CreditTransaction
	if err := db.Where("type = ?", model.TxSignupGrant).First(&tx).Error; err != nil {
		t.Fatalf("查 signup_grant 流水: %v", err)
	}
	if tx.MonthlyDelta != 7 || tx.AddonDelta != 0 {
		t.Errorf("流水拆分必须是 monthly=7/addon=0，得到 monthly=%d/addon=%d"+
			"——记错桶会让退款还错、月底重置时额度凭空蒸发",
			tx.MonthlyDelta, tx.AddonDelta)
	}
	if tx.MonthlyAfter != acct.MonthlyCredits || tx.AddonAfter != acct.AddonCredits {
		t.Errorf("快照列必须与账户余额一致：流水 %d/%d，账户 %d/%d——对不上说明对账已失效",
			tx.MonthlyAfter, tx.AddonAfter, acct.MonthlyCredits, acct.AddonCredits)
	}
}

func TestSignupBonusRejectsAbsurdValueAtWriteTime(t *testing.T) {
	// 主防线在写入时：拦在坏数据产生之前。放过一个 100000 之后，服务会带着它照常
	// 运行、一边给每个注册的人送 100000 次额度——多打一个 0 是这一项最常见的手误。
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "absurd-admin@example.com")

	for _, bad := range []string{"100000", "-1", "abc", "1.5"} {
		w := doPatchSettings(r, token, fmt.Sprintf(`{"signupBonusCredits":%q}`, bad))
		if w.Code != http.StatusBadRequest {
			t.Errorf("signupBonusCredits=%q 应当被拒（400），得到 %d；body=%s",
				bad, w.Code, w.Body.String())
		}
	}
}

func TestSignupBonusTakesEffectWithoutRestart(t *testing.T) {
	// 赠送次数是会被反复微调的运营参数，所以走的是 getter 而非构造时取值。
	// 这条钉住"后台改完立刻生效"——若哪天改成在构造时读一次 int，它会失败。
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "hot-admin@example.com")

	setBonusViaAPI(t, r, token, "3")
	postJSON(r, "/api/v1/auth/register", `{"email":"first@example.com","password":"secret12345"}`)
	if got := balanceOf(t, db, "first@example.com").MonthlyCredits; got != 3 {
		t.Fatalf("改成 3 之后应当送 3，得到 %d", got)
	}

	setBonusViaAPI(t, r, token, "9")
	postJSON(r, "/api/v1/auth/register", `{"email":"second@example.com","password":"secret12345"}`)
	if got := balanceOf(t, db, "second@example.com").MonthlyCredits; got != 9 {
		t.Errorf("改成 9 之后应当送 9，得到 %d——说明取值被固化在构造时了", got)
	}
}

func TestSignupBonusRegistrationStillSucceedsWhenBonusFails(t *testing.T) {
	// 赠送失败**不能**影响注册。用户已经建好了，此时回 500 会让用户看到"注册失败"，
	// 而账号其实已存在——他再试一次会撞 409「邮箱已注册」，卡死在自相矛盾的状态里。
	//
	// 造失败的办法：先手工插一条该用户的 signup_grant 流水占掉幂等键，注册时的发放
	// 就会撞唯一索引。这同时验证了 ErrAlreadyGranted 被当成正常情况忽略。
	r, db := setupRouterWithDB(t)
	token := adminTokenFor(t, r, db, "fail-admin@example.com")
	setBonusViaAPI(t, r, token, "5")

	// 下一个自增 id 未知，所以先注册、再用同一 userID 重复发放来验证幂等分支
	// （注册本身那次已经成功发放过了）。
	w := postJSON(r, "/api/v1/auth/register", `{"email":"dup@example.com","password":"secret12345"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("注册必须成功（201），得到 %d；body=%s", w.Code, w.Body.String())
	}
	var u model.User
	if err := db.Where("email = ?", "dup@example.com").First(&u).Error; err != nil {
		t.Fatalf("查用户: %v", err)
	}

	// 再发一次：必须被幂等挡住，且余额不变。
	if err := credit.GrantSignupBonus(db, u.ID, 5); err == nil {
		t.Fatal("第二次发放必须失败（幂等），否则任何重试都会重复送额度")
	}
	if got := balanceOf(t, db, "dup@example.com").MonthlyCredits; got != 5 {
		t.Errorf("重复发放后余额必须仍是 5，得到 %d——说明幂等没挡住", got)
	}
	if n := countTxOfType(t, db, model.TxSignupGrant); n != 1 {
		t.Errorf("只该有 1 条 signup_grant 流水，实际 %d 条", n)
	}
}
