# M4c 后台配置 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans。步骤用 `- [ ]` 勾选。

**Goal:** 让运营在后台改模型扣费、上下架模型、接新模型、调档位次数，全部不用改代码发版。

**Architecture:** 纯 API，挂在已有的 `admin` 路由组（`middleware.RequireAdmin`）下。不引入新表——`image_models` 与 `plans` 已经存在且已被扣费/发放路径读取。

**Tech Stack:** Go / Gin / GORM。

**前置：** M4a 全部完成（HEAD 7e5bedb）。`credit.Spend` 已按 `image_models.credits` 逐请求扣费并有 `credits=7` 的测试覆盖。

---

## 为什么需要这一轮

`image_models.credits` 这一列已经被扣费路径读取，`seedModels` 用 `FirstOrCreate` 所以运营改过的值不会被启动覆盖。但**没有任何写入口**：今天要把 flux-2-max 从 1 调成 7，只能手工改数据库。

手改库的问题不是麻烦，是**没有校验**：把 credits 改成 0 会让 `credit.Spend` 对每次生成返回错误（它拒绝 `cost <= 0`），表现为该模型 500，而没人会想到是那次手改造成的。

---

## 五个必须守住的点

**1. PATCH 的字段必须是指针。**

用非指针的 `struct{ Credits int; Enabled bool }` 绑定 PATCH，"没传"和"传了零值"无法区分。一个只想改 `credits` 的请求会因为 `enabled` 缺省为 `false` 而**把模型下架**。这是 PATCH 的经典坑，而这里的后果是线上模型突然消失。

**2. `credits` 必须 ≥ 1。**

`credit.Spend` 拒绝 `cost <= 0`。credits=0 不会变成"免费模型"，会变成"该模型每次生成都 500"。写入时校验，返回 400 并说明原因。

**3. `plans.price_usd_cents` 与 `plans.stripe_price_id` 不可通过 API 修改。**

Stripe 的 Price 金额**不可变**。改我们这边的数字只会让定价页显示 $29.90 而 Stripe 实际收 $49.90——用户看到的价格和被扣的钱不一致，是最难解释的一类投诉。`stripe_price_id` 同理：手填一个 ID 就是"付了 Pro 的钱、拿到 Starter 的次数"。调价必须走新建 Price + 迁移订阅的独立流程。

**4. 新增模型时必须校验 provider 已注册 adapter。**

`generation.Registry` 只认注册过的 provider。建一个 `provider: "midjourney"` 的模型，用户点了生成会拿到 500（`h.Adapters.Get` 失败），而模型在列表里看着是正常的。写入时就拒绝。

**5. 改 `plans.monthly_credits` 会在下次续费生效，不追溯。**

`credit.ResetMonthly` 在 `invoice.paid` 时读 plan 行。这是期望行为（调整对所有人一致生效），但要在注释里写明，免得有人以为改完立刻到账。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/handler/admin_models.go`（新） | 模型的列表 / 新增 / 修改 |
| `internal/handler/admin_plans.go`（新） | 档位的列表 / 修改 |
| `internal/server/router.go`（改） | 注册 5 条 admin 路由 |
| `internal/server/admin_models_test.go`（新） | 模型配置的测试 |
| `internal/server/admin_plans_test.go`（新） | 档位配置的测试 |

`internal/handler/admin.go` 里已有 `AdminHandler{DB}` 与 `GrantCredits`。新 handler 复用同一个结构体还是另开，由实施者按可读性决定——但**模型配置需要 `Adapters`**（校验 provider），所以大概要新结构体。

---

## Task 1：模型配置

**Files:**
- Create: `internal/handler/admin_models.go`、`internal/server/admin_models_test.go`
- Modify: `internal/server/router.go`

- [ ] **Step 1：先写失败的测试**

```go
func TestAdminListModelsIncludesDisabled(t *testing.T)
// 公开的 GET /models 只返回启用的；后台必须能看到已下架的，否则下架后就再也
// 找不到它、无法重新上架。

func TestAdminPatchModelCreditsOnly(t *testing.T)
// **本轮最重要的一条**：只传 {"credits":7}，断言 enabled 仍为 true、
// displayName 未变。非指针绑定会让这条变红（enabled 缺省 false → 模型被下架）。

func TestAdminPatchModelRejectsZeroCredits(t *testing.T)
// credits=0 → 400。理由：credit.Spend 拒绝 cost<=0，0 不是"免费"而是"每次 500"。

func TestAdminPatchModelRejectsNegativeCredits(t *testing.T)

func TestAdminPatchUnknownModelReturns404(t *testing.T)

func TestAdminCreateModelRejectsUnregisteredProvider(t *testing.T)
// provider="nope" → 400。否则模型建出来了，用户一点生成就 500。

func TestAdminCreateModelSucceedsAndIsSpendable(t *testing.T)
// 建一个 credits=3 的 flux 模型，然后**真的用它生成一次**，断言扣了 3。
// 这条把"配置"和"扣费"两端连起来——只测写库不测生效，等于没测。

func TestAdminModelRoutesRequireAdmin(t *testing.T)
// 普通用户 403。参考 internal/server/admin_test.go 现有写法。
```

- [ ] **Step 2：跑测试确认失败**

Run: `go test ./internal/server/ -run TestAdminListModels -run TestAdminPatchModel -run TestAdminCreateModel -v`
（或用一个正则一次跑完：`-run 'TestAdmin(List|Patch|Create)Model'`）

- [ ] **Step 3：实现**

```go
// AdminModelsHandler 让运营在后台改模型扣费与上下架，不必改代码发版。
//
// 需要 Adapters 是为了校验 provider：建一个没注册 adapter 的 provider，
// 模型在列表里看着正常，用户一点生成就 500（h.Adapters.Get 失败）。
type AdminModelsHandler struct {
	DB       *gorm.DB
	Adapters generation.Registry
}

// patchModelRequest 的字段**全是指针**。
//
// 非指针的话，"没传 enabled"和"传了 enabled:false"无法区分——一个只想把
// credits 从 1 改成 7 的请求会顺手把模型下架，而且没有任何报错。
type patchModelRequest struct {
	DisplayName *string `json:"displayName"`
	Credits     *int    `json:"credits"`
	Enabled     *bool   `json:"enabled"`
	SortOrder   *int    `json:"sortOrder"`
}
```

`PATCH` 用 `map[string]any` 收集实际要改的列再 `Updates(...)`——`Updates` 传结构体会跳过零值，正好会漏掉 `enabled:false` 和 `sortOrder:0`。

`credits` 校验：`*req.Credits < 1` → 400，message 说明"credits 必须 ≥ 1"。

新增模型的必填校验：`id` / `displayName` / `provider` / `upstreamModel` / `credits`。`id` 已存在 → 409（不是 500，也不是静默覆盖）。

- [ ] **Step 4：注册路由**

```go
	adminModels := &handler.AdminModelsHandler{DB: db, Adapters: adapters}
	admin.GET("/models", adminModels.List)
	admin.POST("/models", adminModels.Create)
	admin.PATCH("/models/:id", adminModels.Patch)
```

- [ ] **Step 5：跑测试 + 提交**

Run: `go test ./... -count=1`

```bash
git add internal/handler internal/server
git commit -m "feat: 后台配置模型扣费与上下架"
```

---

## Task 2：档位配置

**Files:**
- Create: `internal/handler/admin_plans.go`、`internal/server/admin_plans_test.go`
- Modify: `internal/server/router.go`

- [ ] **Step 1：先写失败的测试**

```go
func TestAdminListPlansIncludesDisabledAndPriceID(t *testing.T)
// 与公开的 GET /plans 相反：后台要能看到 stripe_price_id（用于确认播种是否
// 跑过）和已下架的档位。

func TestAdminPatchPlanMonthlyCredits(t *testing.T)
// 只传 monthlyCredits，断言 enabled / priceUsdCents / stripePriceID 都没变。

func TestAdminPatchPlanRejectsPriceChange(t *testing.T)
// 传 priceUsdCents → 400。Stripe Price 金额不可变，改我们这边的数字只会让
// 定价页显示的价格和实际扣款不一致。

func TestAdminPatchPlanRejectsStripePriceIDChange(t *testing.T)
// 传 stripePriceId → 400。手填 Price ID 就是"付了 Pro 的钱拿到 Starter 的次数"。

func TestAdminPatchPlanRejectsNegativeCredits(t *testing.T)
// 允许 0（等于该档暂时不发次数），拒绝负数。

func TestAdminPlanRoutesRequireAdmin(t *testing.T)
```

- [ ] **Step 2：跑测试确认失败**

- [ ] **Step 3：实现**

```go
// patchPlanRequest 只开放三个字段。
//
// **price_usd_cents 与 stripe_price_id 刻意不可改。** Stripe 的 Price 金额不可变，
// 调价只能新建 Price 再迁移订阅。改我们这边的数字不会改变 Stripe 实际收多少钱，
// 只会让定价页显示 $29.90 而用户被扣 $49.90——这是最难向用户解释的一类不一致。
type patchPlanRequest struct {
	DisplayName    *string `json:"displayName"`
	MonthlyCredits *int    `json:"monthlyCredits"`
	Enabled        *bool   `json:"enabled"`
	SortOrder      *int    `json:"sortOrder"`
}
```

**显式拒绝而不是静默忽略**：如果请求体里出现 `priceUsdCents` 或 `stripePriceId`，返回 400 并说明原因。静默忽略的话，运营会以为改成功了，直到有人对账才发现没生效。
实现方式：先把 body 解成 `map[string]any` 检查有没有这两个 key，再解成结构体。

改 `monthlyCredits` 的注释要写明：**下次 `invoice.paid` 才生效，不追溯**（`credit.ResetMonthly` 在续费时读 plan 行）。

- [ ] **Step 4：注册路由**

```go
	adminPlans := &handler.AdminPlansHandler{DB: db}
	admin.GET("/plans", adminPlans.List)
	admin.PATCH("/plans/:id", adminPlans.Patch)
```

- [ ] **Step 5：跑测试 + 提交**

---

## 验证方式

除了单测，要有一条把"配置"与"生效"连起来的测试（Task 1 的 `TestAdminCreateModelSucceedsAndIsSpendable`）：改完配置后**真的走一次生成**，断言扣的是新值。只断言"库里的值变了"是不够的——那不能证明扣费路径读到了它。

---

## 已知缺口（本轮不做）

- **没有管理端 UI。** 本轮只有 API，运营要用 curl 或 Postman。UI 是独立一轮。
- **改动不留审计流水。** `GrantCredits` 会在 `credit_transactions` 里留下"谁发的"，但改模型 credits 不会留痕。等有多个运营时需要一张 `admin_audit_log`。
- 调价流程（新建 Price + 迁移订阅）仍未实现。
- 删除模型未开放：历史 generations 行引用 `model` 字段，删了会让历史记录显示不出模型名。下架（`enabled=false`）足够。
