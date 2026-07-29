# M4a：Stripe 订阅 设计文档

日期：2026-07-29
仓库：`image-backend`（主要）+ `image-front`（定价页按钮接真、账户页显示订阅状态）
上游规格：`docs/superpowers/specs/2026-07-27-image-platform-design.md` 第 5 节
前置里程碑：M3a（次数账本）、M3b（Flux 接通）

---

## 1. 目标与范围

让用户能真的订阅并按月拿到次数。

**本轮做：** `plans` 扩展、`subscriptions`、`stripe_events` 三张表；Checkout Session 创建；webhook 验签 + 幂等 + 事件处理；月度次数发放与重置；Billing Portal 跳转；Price 播种命令；定价页按钮接真。

**本轮不做（M4b）：** 加量包一次性支付（`orders` 表、`payment_intent.succeeded`）。订阅是主要收入路径，加量包是它的补充（规格里也写了"仅限有效订阅用户购买"），所以先做订阅。

**本轮不做（更后）：** 发票邮件、税务、多币种、优惠券、试用期。

### 已确认的前提

- Stripe 账号 `acct_1OhrwEI7MQJnDN9r`，币种 **USD**，`charges_enabled: true`（生产模式也能收款——所以配置里必须有 test/live key 的校验，见 §7）。
- 三个档位，**除了次数没有任何差别**。产品方 2026-07-29 明确决定。定价页上原先写的"优先排队 / 私密生成 / 商用授权 / 最高并发"是占位文案，一样都没实现，**本轮全部删掉**——留着就是虚假宣传。
- 价格与次数**暂不定稿**，从 `plans` 表读。代码不得硬编码任何金额。当前占位：$9.9 / 200、$29.9 / 800、$49.9 / 3000。
- 单张图上游报 `cost: 7`（1MP）。**7 个单位折合多少钱尚未知**，因此三档毛利率未经核算。顶档折合 $0.0166/次，是入门档的三分之一——可能是健康的量价梯度，也可能倒挂。定稿前必须补上这个数。

---

## 2. 数据模型

| 表 | 字段 |
|---|---|
| `plans`（新） | `id`(string PK, 如 `pro`)、`display_name`、`price_usd_cents`、`monthly_credits`、`stripe_price_id`、`enabled`、`sort_order` |
| `subscriptions`（新） | `user_id`(PK)、`plan_id`、`stripe_customer_id`、`stripe_subscription_id`(uniqueIndex)、`status`、`current_period_start/end`、`cancel_at_period_end`、`created_at`、`updated_at` |
| `stripe_events`（新） | `id`(string PK = Stripe event id)、`type`、`processed_at` |

`status` 取值沿用 Stripe 的词汇：`active`、`past_due`、`canceled`、`incomplete`。直接用上游词汇而不自造映射——自造一层映射意味着每次 Stripe 加状态都要改两处，且对不上的时候没人知道以谁为准。

`subscriptions` 以 `user_id` 为主键（一个用户同时只有一个订阅），并在 `stripe_subscription_id` 上加唯一索引——webhook 是按后者反查的。

**`plans` 表的价格字段用 `price_usd_cents`（整数分）而不是浮点。** 金额用浮点存迟早出现 `29.900000000000002`，而对账时那种差异极难解释。Stripe 的 API 本身也全用最小货币单位。

---

## 3. 发放额度的唯一入口：`invoice.paid`

**只有 `invoice.paid` 会发放/重置月度次数。** 首次订阅与每月续费都触发它，所以认这一个事件就够。

**`checkout.session.completed` 只做一件事：把 `stripe_customer_id` 绑到用户上。它不发额度。** 这是参考项目踩过的坑——两个事件都发就会双倍到账，而且这种 bug 只在真实付款时才暴露。

其余事件的处理：

| 事件 | 处理 |
|---|---|
| `checkout.session.completed` | 绑定 `stripe_customer_id`，**不发额度** |
| `invoice.paid` | **唯一发放入口**：同一事务内 upsert `subscriptions` + 月度次数**重置**为套餐额度 + 写 `subscription_grant` 流水 |
| `invoice.payment_failed` | `status` → `past_due`。**不清零次数**——用户可能只是卡过期，几天内会补款，清零会让他在补款前完全不能用 |
| `customer.subscription.updated` | 同步 `plan_id`、`status`、`cancel_at_period_end`、周期时间。**不碰次数**——升降级的额度变化由随之而来的 `invoice.paid` 负责 |
| `customer.subscription.deleted` | `status` → `canceled`，**月度次数清零，加量包次数保留** |

**取消时保留加量包次数**：那是用户单独花钱买的，不能因为退订就没收。

### 发多少次数：按 Price ID 反查，不按 metadata

发放数量取自 `plans` 行，而**定位哪一行必须用订阅当前的 `stripe_price_id` 反查**，不能用 Checkout 时写进 `subscription_data.metadata` 的 `plan_id`。

metadata 是我们**下单时请求的**东西，Price 是用户**实际被计费的**东西。用户在 Billing Portal 里升级档位时，Stripe 换掉 Price 但**不会**改订阅的 metadata——此时 metadata 还写着旧档。照 metadata 发就是"付了 Pro 的钱、拿到 Starter 的次数"，而这条路径在只测新订阅时撞不到。

metadata 里的 `user_id` 仍然要用（Price 反查不出人），并与 `stripe_customer_id` 映射互为交叉校验：两者都存在且不一致时**拒绝发放并告警**，因为那说明数据已经串了，猜哪个对都可能发错人。

Price ID 在 `plans` 里查不到时**不发放并告警**（在 Dashboard 里手工建的订阅会走到这里）。宁可漏发等人工处理，也不要瞎猜一个档位。

---

## 4. 月度重置是"设置"而不是"累加"

这需要 `internal/credit` 增加一个新操作。现有的 `Grant` 是**累加**（`monthly_credits + n`），而续费必须**重置**（`monthly_credits = n`）——否则用不完的次数会累积，用户攒三个月就有三倍额度，与"月度次数不累积到下月"的产品承诺相反（定价页上写着这句）。

```go
// ResetMonthly 把月度次数**设置**为 amount，加量包次数不动。
//
// 与 Grant 的区别是"设置"而非"累加"：续费若累加，用不完的次数会攒起来，
// 与定价页承诺的"月度次数不累积到下月"矛盾。
//
// 允许 delta 为负（高档降到低档时余额会下调），这与 Grant 拒绝负数不冲突：
// 这里是把余额**设**到一个由套餐决定的非负值，不存在扣成负数的路径。
func ResetMonthly(db *gorm.DB, userID uint, amount int, eventID string) error
```

幂等：以 `stripe_events` 表的事件 id 为准（见 §5），而不是在 `credit` 层再造一套。

---

## 5. Webhook：先验签，再幂等

顺序不能换。

**1. 验签。** 用 `STRIPE_WEBHOOK_SECRET` 校验 `Stripe-Signature` 头。**必须用原始请求体**——任何 JSON 反序列化再序列化都会改变字节从而验签失败。所以要在 gin 里读 `c.Request.Body` 的原始字节，不能用 `ShouldBindJSON`。

不验签的后果不是"可能被伪造"，是"任何人 POST 一个 `invoice.paid` 就能给自己发额度"。

**2. 幂等。** `INSERT INTO stripe_events (id, type, processed_at)`，主键冲突即说明处理过，直接返回 200。

**Stripe 会重投事件**（我们返回非 2xx、超时、或它自己的重试策略），不去重就是重复入账。用主键冲突而不是"先查再插"——后者在并发重投下有窗口，这个教训 M3a 的退款幂等已经付过一次学费。

**3. 处理。** 幂等记录与业务处理必须在**同一事务**里：先插 `stripe_events` 再做业务，两者一起提交。若分开，进程在两步之间崩溃会导致"记了已处理但没处理"——那是永久丢失一次发放，比重复发放更难发现。

**4. 返回 200。** 只要事件被接受（无论是否为我们关心的类型）就返回 200。返回 5xx 会让 Stripe 重投，而重投一个我们根本不处理的事件类型毫无意义。业务处理失败才返回 5xx（让它重投）。

---

## 6. API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/plans` | 公开。返回启用的套餐（含 `id`、`name`、`priceUsdCents`、`monthlyCredits`）。**不返回 `stripe_price_id`**——那是服务端细节，前端不需要，暴露它没有收益 |
| POST | `/api/v1/billing/subscribe` | 需登录。body `{planId}`，返回 `{checkoutUrl}` |
| POST | `/api/v1/billing/portal` | 需登录。返回 `{portalUrl}`，换卡/取消/看发票统一跳 Stripe Billing Portal |
| POST | `/api/v1/webhooks/stripe` | **公开且不过 `RequireActiveUser`**——Stripe 不带我们的 cookie。安全性由验签保证 |
| GET | `/api/v1/me` | 扩展：增加 `subscription: {planId, status, currentPeriodEnd, cancelAtPeriodEnd} \| null` |

**Checkout Session 的关键参数：**

- `mode: "subscription"`、`line_items: [{price: plan.StripePriceID, quantity: 1}]`
- `client_reference_id: <user_id>` 与 `subscription_data.metadata: {user_id, plan_id}`。**两处都放**：`invoice.paid` 事件里拿到的是 subscription，要能反查用户；只依赖 customer 映射的话，用户在 Dashboard 里被手工改动就会断链
- `success_url` / `cancel_url` 指向前端 `/account?checkout=success|cancel`
- `customer`：已有 `stripe_customer_id` 就复用，否则让 Stripe 新建（避免同一用户产生多个 customer，那会让 Portal 里只看到部分发票）

**Billing Portal 不需要我们做任何 UI**——换卡、取消、看发票、更新账单地址全在 Stripe 那边。自己做这些是纯浪费。

---

## 7. 配置与 test/live 校验

```
STRIPE_SECRET_KEY=          # sk_test_ 或 sk_live_
STRIPE_WEBHOOK_SECRET=      # whsec_，由 `stripe listen` 或 Dashboard 提供
APP_BASE_URL=               # 前端地址，用于拼 success_url / cancel_url
```

**必须有 test/live 校验。** 该账号 `charges_enabled: true`，意味着 live key 会真的扣钱。规则：

- `STRIPE_SECRET_KEY` 为空 → 计费功能整体禁用，相关接口返回明确的"未配置"错误（而不是 500）。让没配 Stripe 的本地开发环境仍能跑其余功能。
- 连接真实数据库（`DATABASE_URL` 非空）却用 `sk_test_` → 启动时**警告**但允许（测试环境常这么用）。
- 用 `sk_live_` 但 `APP_BASE_URL` 还是 localhost → **拒绝启动**。这个组合几乎必然是误配，而它的后果是真实扣款后跳到一个用户打不开的地址。

---

## 8. Price 播种

一个一次性命令 `cmd/seed-stripe`：读 `plans` 表里还没有 `stripe_price_id` 的行，为每行创建 Stripe Product + Price，把 ID 写回。

**为什么用代码建而不是在 Dashboard 手建：** 价格、次数、Price ID 三者必须一致。手工抄 ID 一旦错位，表现是"用户付了 Pro 的钱、拿到 Starter 的次数"——而这种错位在测试时很可能撞不到，因为你会盯着自己刚建的那一档测。

**Price 不可改价。** Stripe 的 Price 对象金额不可变，调价只能新建 Price 再迁移订阅。所以播种命令必须**幂等且不覆盖**：已有 `stripe_price_id` 的行跳过，绝不重建。调价是后续单独的迁移流程，不在本轮。

---

## 9. 前端改动

- `/pricing` 的按钮从 `disabled` 变成真的调 `POST /api/billing/subscribe` 并跳转 Checkout。**未登录时先跳登录**，登录后回到定价页（不要在未登录状态弹 Checkout，用户付完款我们不知道该给谁）。
- 删掉三张卡上编造的功能差异点，改成只列次数。
- `/account` 显示订阅状态与"管理订阅"按钮（跳 Billing Portal）。
- `plans` 从后端 `GET /plans` 取，删掉 `lib/plans.ts`——那是最后一处假数据。

---

## 10. 验证方式

**单元测试：** `ResetMonthly` 的设置语义（含降档时 delta 为负）、流水快照与账户一致；webhook 幂等（同一事件 id 两次只处理一次）；验签失败被拒绝。

**集成测试：** 用 Stripe 官方的测试事件构造签名，走完整 handler；断言 `invoice.paid` 发额度、`checkout.session.completed` **不**发额度、`subscription.deleted` 清零月度但保留加量包。

**真实联调（Stripe CLI + 测试卡）：**

1. `stripe listen --forward-to localhost:8080/api/v1/webhooks/stripe`
2. 前端点订阅 → 用测试卡 `4242 4242 4242 4242` 付款 → 月度次数到账、`subscriptions` 行正确
3. `stripe trigger invoice.payment_failed` → status 变 `past_due` 且次数未被清零
4. Portal 里取消订阅 → 月度清零、加量包保留
5. **同一事件重投两次**（`stripe events resend <id>`）→ 只入账一次
6. 用需要 3DS 的测试卡 `4000 0025 0000 3155` → 确认 `incomplete` 状态不会误发额度

第 5 和第 6 条最容易被跳过，也最容易在真实付款中出事。

---

## 11. 已知缺口（本轮不解决）

- 加量包（M4b）
- 调价迁移流程：Price 不可改价，调价需新建并迁移已有订阅
- 无试用期、无优惠券、无多币种（账号是 USD）
- 三档毛利率未核算——缺"上游 7 个单位折合多少钱"这个数
- 限流仍未做。它与档位差异挂钩，而本轮确定三档**只差次数**，因此限流现在可以按统一标准做，不必等档位差异
- webhook 失败后的人工补偿路径：目前靠 Stripe 自身重投，没有管理端"重放某个事件"的入口

---

## 附录：stripe-go v86 的 API 形状（已用编译探针验证）

依赖 `github.com/stripe/stripe-go/v86 v86.1.1`。**下面这些路径与训练数据/网上多数示例不一致**，全部经 `go build` 正反对照验证过，照抄旧写法一定编不过：

| 旧写法（到处都是） | v86 实际写法 |
|---|---|
| `session.New(params)`（子包级函数） | `sc := stripe.NewClient(key)` → `sc.V1CheckoutSessions.Create(ctx, params)` |
| `checkout.SessionParams` | `stripe.CheckoutSessionCreateParams`（**参数类型都在根包**） |
| `inv.Subscription` | `inv.Parent.SubscriptionDetails.Subscription.ID` |
| — | `inv.Parent.SubscriptionDetails.Metadata`（承载 `subscription_data.metadata`） |
| `sub.CurrentPeriodEnd` | `sub.Items.Data[0].CurrentPeriodEnd`（周期时间搬到 item 上了） |
| `sub.Plan.ID` | `sub.Items.Data[0].Price.ID` |

`webhook.ConstructEvent(payload, sigHeader, secret) (stripe.Event, error)` 未变；事件载荷取 `ev.Data.Raw`。

**`inv.Parent`、`inv.Customer`、`sub.Items` 都是指针且可能为 nil**（非订阅发票就没有 Parent）。取值前必须判空——这里 panic 掉的是 webhook handler，后果是 Stripe 一直重投。
