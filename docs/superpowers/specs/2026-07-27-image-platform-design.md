# AI 图像生成订阅平台 — 设计文档

日期：2026-07-27
状态：已经用户逐节确认，待最终审阅
涉及仓库：`image-front`（Next.js 前端）、`image-backend`（Go 后端）

## 1. 背景与目标

构建一个面向海外用户的按月订阅 AI 图像生成平台：

- 用户通过 Stripe 购买不同档位的月度订阅套餐，获得每月生成次数额度（**每月重置**）；
- 额度不足可购买**不过期的加量包**（一次性支付）；
- 生成时按模型扣不同次数（Flux / image-2 / Nanobanana 等），上游统一走现有的 **ezlinkai 网关**；
- 首版功能：生成工作台（文生图 + 图生图/参考图）、个人历史记录、公开画廊、管理后台。

### 调研结论（参考项目精读）

精读了 `virgoone/next-money` 与 `SamurAIGPT/ai-saas-starter`（源码在 `Desktop/image-refs/`）：

- 两者均为"一次性买积分包"模式，**订阅链路都未实现**，需自行设计；
- 值得借鉴：三表复式积分账本（余额 + 带余额快照的流水 + 账单）、"先落库 Pending 订单再开 Checkout、metadata 带订单 ID"的支付防重模式、事务扣费、失败退款幂等、按模型定价常量表、任务 ID 唯一索引 + 多路状态更新前判终态；
- 必须避开的坑：并发扣费竞态（先查后扣可扣成负数）、Stripe webhook 无幂等（重投重复入账）、副作用塞进 GET 读接口、serverless 内联阻塞轮询。

## 2. 技术选型

| 层 | 选型 | 理由 |
|---|---|---|
| 前端 `image-front` | Next.js (App Router) + Tailwind + shadcn/ui | 参考项目页面结构可大量借鉴，SEO 友好 |
| 后端 `image-backend` | Go：Gin + GORM + PostgreSQL | 复用 ezlinkai 技术栈经验；计费强一致靠事务；长驻 worker 做任务对账 |
| 支付 | Stripe（subscription + 一次性 payment） | 面向海外标配 |
| 图片存储 | Cloudflare R2（S3 兼容） | 无出站流量费；生成结果转存，不依赖上游临时 URL |
| 上游 | ezlinkai 网关 | 统一封装 Flux / image-2 / Nanobanana 等模型 |
| 部署 | 后端 Docker + Postgres + R2；前端 Vercel 或同机 Docker | 首版不引入 Redis/消息队列 |

### 备选方案与取舍

- 方案 B（Next.js 全栈 + Prisma + NextAuth）：上线最快、可大段抄参考项目，但 serverless 做长任务对账别扭，且与用户 Go 技术栈错位 → 否决；
- 方案 C（Next.js + NestJS）：需学新框架，优势不明显 → 否决。

## 3. 整体架构

```
用户浏览器
   │
   ▼
image-front (Next.js, 纯前端)          ← 落地页/定价页/工作台/画廊/后台 UI
   │  REST API (JWT)
   ▼
image-backend (Go: Gin + GORM + PostgreSQL)
   ├─ auth 模块        Google/GitHub OAuth + 邮箱密码，签发 JWT
   ├─ billing 模块     Stripe Checkout/Portal/Webhook，订阅+加量包，次数账本
   ├─ generation 模块  提交生成任务 → 调 ezlinkai → 扣次数
   ├─ reconcile worker 长驻 goroutine：轮询 processing 任务、兜底对账、失败退次数
   └─ admin 模块       用户/订阅/订单/套餐/模型/统计
   │
   ▼
ezlinkai 网关 ──→ Flux / image-2 / Nanobanana 上游
```

## 4. 数据模型

| 表 | 作用 | 关键字段 |
|---|---|---|
| `users` | 用户 | email、password_hash（可空）、oauth 绑定（provider + provider_user_id）、role、status |
| `plans` | 订阅套餐 | 名称、月价、stripe_price_id、每月次数额度、可用模型、是否上架 |
| `subscriptions` | 用户订阅状态 | user_id、plan_id、stripe_subscription_id、status（active/past_due/canceled）、current_period_start/end、cancel_at_period_end |
| `credit_accounts` | 双余额 | monthly_credits（月度，续费重置）、addon_credits（加量包，不过期）；扣费先扣 monthly 再扣 addon |
| `credit_transactions` | 不可变流水 | type（subscription_grant/addon_purchase/generation_cost/generation_refund/admin_adjust）、变动数、变动后两项余额快照、关联 order_id / generation_id |
| `orders` | 加量包一次性订单 | user_id、amount、credits、phase（pending/paid/failed/canceled）、stripe session/payment_intent id |
| `stripe_events` | webhook 幂等去重 | event_id 主键、type、处理时间 |
| `generations` | 生成任务 | user_id、model、prompt、参考图 URL、upstream_task_id（唯一索引）、status（pending/processing/succeeded/failed）、结果 R2 URL、消耗次数、is_public、error、耗时 |
| `models` | 模型配置 | 展示名、映射 ezlinkai 模型名、每次消耗次数、是否支持图生图、是否启用 |

### 一致性防御（针对参考项目的坑）

1. 扣次数用**条件原子更新**：`UPDATE credit_accounts SET ... WHERE user_id=? AND monthly_credits+addon_credits >= ?`，检查影响行数，杜绝并发扣成负数；
2. 所有"余额变动 + 流水"写在同一数据库事务；
3. Stripe webhook 处理前先 `INSERT INTO stripe_events ... ON CONFLICT DO NOTHING`，冲突即跳过（防重投重复入账）；
4. 失败退款以"该 generation 无 refund 流水"为幂等条件，且仅对 failed 任务退；
5. 多路状态更新（worker 轮询/兜底对账）写前先判断任务是否已终态，防覆盖。

## 5. 计费流程

### 订阅

1. 定价页选套餐 → `POST /billing/subscribe` → Stripe Checkout Session（`mode: subscription`，用 `plans.stripe_price_id`，metadata 带 user_id/plan_id）；
2. Webhook 事件处理：

| 事件 | 处理 |
|---|---|
| `checkout.session.completed` | 绑定 stripe_customer_id 到用户（不发额度） |
| `invoice.paid` | **发放额度唯一入口**（首付与每月续费均触发）：事务内 upsert `subscriptions` + `monthly_credits` 重置为套餐额度 + 写 subscription_grant 流水 |
| `invoice.payment_failed` | status → past_due，前端提示 |
| `customer.subscription.updated` | 同步升降级、取消预约 |
| `customer.subscription.deleted` | status → canceled，monthly_credits 清零，addon 保留 |

3. 升级走 Stripe proration 立即生效；降级 period end 生效；换卡/取消统一跳 Stripe Billing Portal。

### 加量包

1. 先落库 `orders`（pending）→ Checkout（`mode: payment`，动态 price_data，metadata 带 order_id）；
2. `payment_intent.succeeded` → 校验订单 pending → 事务：置 paid + `addon_credits` 累加 + 写流水；
3. 仅限有效订阅用户购买。

## 6. 生成任务流程

```
POST /api/v1/generations
  ① 校验：订阅有效 + 模型启用 + 余额足够
  ② 建 generations（status=pending）
  ③ 条件原子扣次数 + 写流水（同一事务；失败返回 40001 次数不足）
  ④ 调 ezlinkai 提交 → 记 upstream_task_id，status → processing
     （提交失败 → 事务退次数 + status=failed）
  ⑤ 返回 generation_id
前端每 2~3 秒轮询 GET /generations/:id 至终态
```

结果回收双通道（Go 长驻 worker，主动拉取，不依赖上游 webhook）：

- **主通道**：每 3 秒扫一批 processing 任务，并发查 ezlinkai 状态；成功 → 下载图片转存 R2、置 succeeded；失败 → 幂等退次数、记 error；
- **兜底**：每 5 分钟扫超 10 分钟仍 processing 的任务，强制对账或标记失败退款，防永久卡住；
- 可选优化：提交接口内联等待 5 秒，快模型直接返回结果，超时转轮询。

## 7. API 设计（前缀 `/api/v1`）

认证：
- `POST /auth/register`、`POST /auth/login`（邮箱，注册发验证邮件）
- `GET /auth/oauth/:provider`、`GET /auth/oauth/:provider/callback`（google/github）
- `GET /me`（用户信息 + 双余额 + 订阅状态）

计费：
- `GET /plans`（公开）
- `POST /billing/subscribe`、`POST /billing/addon-checkout`、`POST /billing/portal`
- `GET /billing/transactions`（流水分页）
- `POST /webhooks/stripe`（验签 + 幂等）

生成：
- `GET /models`
- `POST /generations`、`GET /generations/:id`、`GET /generations`（历史分页/筛选）、`DELETE /generations/:id`
- `POST /uploads`（参考图上传 R2）
- `GET /gallery`（公开画廊分页）

管理后台（role=admin）：
- `GET/PUT /admin/users`（列表/封禁/手动调整次数）
- `GET /admin/subscriptions`、`GET /admin/orders`
- CRUD `/admin/plans`、`/admin/models`
- `GET /admin/stats`（MRR、日生成量、模型用量分布）

统一错误格式 `{code, message}`：`40001` 次数不足（前端弹升级窗）、`40002` 订阅过期、`40003` 模型不可用。

## 8. 前端页面（image-front）

| 路由 | 页面 | 要点 |
|---|---|---|
| `/` | 落地页 | 示例图墙 + CTA，SSR 利于 SEO |
| `/pricing` | 定价页 | 套餐卡 + 加量包，未登录先跳登录 |
| `/login` `/register` | 认证 | Google / GitHub / 邮箱密码 |
| `/generate` | 核心工作台 | 左参数右结果；模型选择显示消耗次数；参考图上传；轮询出图；余额不足弹升级窗 |
| `/history` | 个人历史 | 瀑布流 + 筛选；复用 prompt / 下载 / 公开私密切换 / 删除 |
| `/gallery` | 公开画廊 | 瀑布流 + 大图详情（含 prompt） |
| `/account` | 账户 | 双余额分开展示、流水、订阅管理跳 Portal |
| `/admin/*` | 管理后台 | 表格 CRUD + 统计图表 |

交互细节：生成按钮标注本次消耗、图生图 before/after 对比滑块、未提交 prompt 存 sessionStorage、下载走后端代理防 CORS。首版仅英文界面。

## 9. 影响范围

全新项目，无存量数据与迁移。依赖外部服务：Stripe 账号（需建订阅 Price）、Cloudflare R2 桶、ezlinkai 网关 key、Google/GitHub OAuth 应用、邮件服务（注册验证，可用 Resend）。

## 10. 验证方式

计费相关逻辑必须有单元测试：

1. **并发扣费**：并发提交生成请求，断言余额永不为负、流水与余额对账一致；
2. **webhook 幂等**：同一 event 投递两次仅入账一次；
3. **续费重置**：`invoice.paid` 后 monthly 重置、addon 不变；
4. **失败退款**：仅 failed 任务退、且仅退一次；
5. **端到端**：Stripe CLI 转发 webhook 本地跑通"订阅 → 生成 → 扣费 → 失败退款"全链路；ezlinkai 无 key 时后端 mock 模式返回占位图。
