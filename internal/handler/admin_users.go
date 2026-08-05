package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"image-backend/internal/credit"
	"image-backend/internal/middleware"
	"image-backend/internal/model"
)

// AdminUsersHandler 后台用户管理。
//
// **只做查与改，不做删。** User 没有软删除也没有级联，硬删会留下孤儿
// credit_accounts / generations / subscriptions，而 Stripe 那边的 customer 还在
// 继续扣款——那是个没有回头路的操作。封禁（status=banned）已经能让
// middleware.RequireActiveUser 立即拦下该用户的所有请求，够用且可逆。
type AdminUsersHandler struct {
	DB *gorm.DB
}

type adminUserResponse struct {
	ID        uint      `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	// 余额随行返回，省掉前端为每一行再发一次请求。
	MonthlyCredits int `json:"monthlyCredits"`
	AddonCredits   int `json:"addonCredits"`
}

// encodeUserCursor 与 generations_list.go 的 encodeCursor 同一格式，只是行类型不同。
//
// 复用同包的 decodeCursor 解码（它已经处理了 base64 非法、结构不对、时间戳不合法
// 三种情况），这里只需要把 uint 主键格式化成字符串。
func encodeUserCursor(u model.User) string {
	raw := u.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatUint(uint64(u.ID), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// List 分页列出用户，支持按邮箱搜索与按角色/状态过滤。
//
// 游标分页而不是 limit/offset：后台边看边封人时，offset 翻页会因为行序变化而漏看或
// 重复看——而"漏看"在这个场景里意味着漏掉一个该处理的账号。排序键固定
// created_at DESC, id DESC，与 generations 列表同一套。
func (h *AdminUsersHandler) List(c *gin.Context) {
	limit := defaultListLimit
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "limit 必须是整数"})
			return
		}
		// 越界钳制而不报错，与 generations 列表一致：limit=1000 是"想要尽可能多"，
		// 按上限给他就是了，没必要让他猜上限是多少。
		limit = min(max(n, 1), maxListLimit)
	}

	q := h.DB.Model(&model.User{})

	// 邮箱模糊搜索。**转小写再比**：注册时邮箱已经 ToLower 入库，而运营在搜索框里
	// 大概率会按原样输入（比如从工单里粘贴的 Foo@Bar.com），不折叠会搜不到。
	if kw := strings.TrimSpace(c.Query("q")); kw != "" {
		q = q.Where("email LIKE ? ESCAPE '\\'", "%"+escapeLike(strings.ToLower(kw))+"%")
	}
	// 角色与状态过滤。**只接受已知取值**：静默接受未知值会让 role=admins（多个 s）
	// 这类打错字返回空列表，而运营会以为"真的没有管理员"。
	if v := c.Query("role"); v != "" {
		if v != model.RoleUser && v != model.RoleAdmin {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    errCodeBadRequest,
				"message": "role 只能是 user 或 admin",
			})
			return
		}
		q = q.Where("role = ?", v)
	}
	if v := c.Query("status"); v != "" {
		if v != model.StatusActive && v != model.StatusBanned {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    errCodeBadRequest,
				"message": "status 只能是 active 或 banned",
			})
			return
		}
		q = q.Where("status = ?", v)
	}

	if cur := c.Query("cursor"); cur != "" {
		ts, idStr, err := decodeCursor(cur)
		if err != nil {
			// **非法 cursor 必须报错，不能静默当第一页**：静默的后果是翻页翻着翻着
			// 悄悄回到开头，而使用者以为自己看完了全部。
			c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "cursor 不合法"})
			return
		}
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "cursor 不合法"})
			return
		}
		// 展开写而不用行值元组 (created_at, id) < (?, ?)：SQLite 与 Postgres 对它的
		// 支持不一致，与 generations 列表保持同一写法。
		q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", ts, ts, uint(id))
	}

	// 多取一行判断有无下一页，取完再切掉——比额外发一次 COUNT 便宜，也不会因为
	// 两次查询之间有人注册而给出前后矛盾的结果。
	var users []model.User
	if err := q.Order("created_at desc, id desc").Limit(limit + 1).Find(&users).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	var nextCursor any
	if len(users) > limit {
		nextCursor = encodeUserCursor(users[limit-1])
		users = users[:limit]
	}

	// 批量取余额，避免每行一次查询。走 credit.BalancesFor 而不是自己 join：
	// "账户行不存在 = 零余额"这个语义只该有一份实现（见该函数注释）。
	ids := make([]uint, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	balances, err := credit.BalancesFor(h.DB, ids)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	out := make([]adminUserResponse, 0, len(users))
	for _, u := range users {
		acct := balances[u.ID] // 缺失时是零值，正是"还没有账户行"的正确表示
		out = append(out, adminUserResponse{
			ID:             u.ID,
			Email:          u.Email,
			Role:           u.Role,
			Status:         u.Status,
			CreatedAt:      u.CreatedAt,
			MonthlyCredits: acct.MonthlyCredits,
			AddonCredits:   acct.AddonCredits,
		})
	}
	c.JSON(http.StatusOK, gin.H{"users": out, "nextCursor": nextCursor})
}

// escapeLike 把用户输入里的 LIKE 元字符转义成字面量。
//
// 参数是绑定的，所以这**不是**注入问题——是结果错的问题，而错的方向最坏：
// 搜 `%` 或 `_` 会命中**全部**用户（实测 5/5），搜 `a_c` 会连 `abc` 一起捞上来。
// 这个列表正是运营决定封谁的依据，多出来的行意味着照着搜索结果去封会封错人。
// 邮箱里 `_` 和 `%` 都合法（`100%off@…`、`a_c@…`），所以这不是理论输入。
//
// 反斜杠必须**第一个**替换，否则后面插入的那些反斜杠会被自己再转义一遍。
// 配套的 `ESCAPE '\'` 不能省：Postgres 默认就把反斜杠当转义符，但 **SQLite 默认
// 没有任何转义符**，不写 ESCAPE 的话转义在测试库里完全不生效——而测试跑在 SQLite 上，
// 那正是这个 bug 最可能被"测试通过"掩盖过去的方式。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// patchUserRequest 全指针：非指针分不清"没传"与"传了空串"。
type patchUserRequest struct {
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

// Patch 改用户的角色或状态。
//
// 两条**防自锁**守卫是这个接口最容易出事的地方，它们防的都是"改完之后再也进不来"：
//
//  1. 不能改自己的 role/status —— 手滑把自己封了或降权了，后台就再也登不进去，
//     只能连数据库改回来。
//  2. 不能把最后一个 admin 降权 —— 同上，而且这个状态连"用另一个管理员账号救回来"
//     的可能性都没有。
//
// 两条都返回 400 并说明原因，而不是静默忽略：静默忽略会让人以为改成功了。
func (h *AdminUsersHandler) Patch(c *gin.Context) {
	targetID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "id 必须是整数"})
		return
	}
	actorID := c.GetUint(middleware.CtxUserIDKey)

	var req patchUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "invalid request body"})
		return
	}

	var target model.User
	if err := h.DB.First(&target, uint(targetID)).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": 40400, "message": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	// 守卫 1：不能动自己。
	if target.ID == actorID {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": errCodeBadRequest,
			"message": "不能修改自己的角色或状态：把自己封禁或降权之后就再也进不了后台，" +
				"只能直连数据库恢复",
		})
		return
	}

	// 用 map 而不是结构体收集：Updates 传结构体会跳过零值。这里两个字段都是非空
	// 字符串所以暂时不受影响，但保持与 admin_models/admin_plans 同一个模式——
	// 下一个人加一个 bool 字段时就不会踩那个坑。
	updates := map[string]any{}

	if req.Role != nil {
		if *req.Role != model.RoleUser && *req.Role != model.RoleAdmin {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    errCodeBadRequest,
				"message": "role 只能是 user 或 admin",
			})
			return
		}
		updates["role"] = *req.Role
	}

	if req.Status != nil {
		if *req.Status != model.StatusActive && *req.Status != model.StatusBanned {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    errCodeBadRequest,
				"message": "status 只能是 active 或 banned",
			})
			return
		}
		updates["status"] = *req.Status
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "没有可修改的字段"})
		return
	}

	// 守卫 2：这次修改不能让系统里一个**可用**管理员都不剩。
	//
	// 三处曾经写错、现在都在这里一起解决：
	//
	//  1. **降权与封禁必须同等对待。** 原先只检查 role，而封禁一个管理员的效果与降权
	//     等价甚至更彻底（RequireActiveUser 会立刻拦下他）。实测两个管理员互相封禁
	//     可以 100% 把系统清零。
	//  2. **计数必须要求 status=active。** 只数 role=admin 会把已封禁的管理员算成
	//     "还有人能进后台"，于是把唯一可用的那个降权/封禁掉时守卫不响。
	//  3. **Count 与 UPDATE 必须在同一个事务里。** 原先是事务外的 Count-then-Update，
	//     实测两个管理员并发互降 5/5 次都把管理员清零——两个请求都读到 2 才各自写。
	//     这与 credit 包里"幂等靠数据库约束、不靠先 Count 再 INSERT"是同一条教训。
	//
	// 写在事务里的代价是这段逻辑不能提前 return，所以用哨兵错误把「业务拒绝」与
	// 「基础设施故障」分开。
	loseAdmin := (req.Role != nil && target.Role == model.RoleAdmin && *req.Role != model.RoleAdmin) ||
		(req.Status != nil && target.Role == model.RoleAdmin && *req.Status != model.StatusActive)

	errLastAdmin := errors.New("last admin")
	txErr := h.DB.Transaction(func(tx *gorm.DB) error {
		if loseAdmin {
			var admins int64
			// 锁住这次统计涉及的行，避免两个并发请求都读到 2。SQLite 忽略 FOR UPDATE
			// 但它本来就是串行写，Postgres 上这一句是必需的。
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Model(&model.User{}).
				Where("role = ? AND status = ?", model.RoleAdmin, model.StatusActive).
				Count(&admins).Error; err != nil {
				return err
			}
			if admins <= 1 {
				return errLastAdmin
			}
		}
		return tx.Model(&model.User{}).Where("id = ?", target.ID).Updates(updates).Error
	})
	if errors.Is(txErr, errLastAdmin) {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": errCodeBadRequest,
			"message": "这是系统里最后一个可用的管理员，不能降权或封禁：改完之后没有任何人能" +
				"访问后台，只能直连数据库恢复。请先提权另一个账号",
		})
		return
	}
	if txErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	// 重读而不是拿内存里那份回传，与 admin_models/admin_plans 一致：让响应反映库里
	// 真正的状态，避免"以为改了但没落库"。
	if err := h.DB.First(&target, target.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	acct, err := credit.Balance(h.DB, target.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, adminUserResponse{
		ID:             target.ID,
		Email:          target.Email,
		Role:           target.Role,
		Status:         target.Status,
		CreatedAt:      target.CreatedAt,
		MonthlyCredits: acct.MonthlyCredits,
		AddonCredits:   acct.AddonCredits,
	})
}
