# M3：接通第一个真实模型（Flux）与次数账本 设计文档

日期：2026-07-28
仓库：`image-backend`（主要）+ `image-front`（仅替换四个假数据 Route Handler 的内部实现）
上游规格：`docs/superpowers/specs/2026-07-27-image-platform-design.md`
前置里程碑：M1（后端骨架 + 邮箱认证）、M2（前端工作台与定价页，假数据驱动）

---

## 1. 目标与范围

把生成链路从假数据换成真实上游，**只接一个模型**（`flux-2-max`），跑通"提交 → 扣费 → 出图 → 失败退款"的完整闭环，供内部测试。

**本轮做：**

| 交付物 | 说明 |
|---|---|
| `image_models` / `credit_accounts` / `credit_transactions` 三张表 | 次数账本 |
| 条件原子扣费 + 失败退款 | 计费正确性，必须有单测 |
| `GET /api/v1/models` | 供前端渲染模型选择器 |
| `POST /api/v1/generations` | 同步返回结果，内部含 `get_result` 兜底 |
| `GET /api/v1/me` 扩展 | 附带双余额 |
| Flux adapter | provider 差异的第一个实现 |
| 启动兜底扫描 | 回收卡在 `processing` 的行 |
| 管理员发放次数接口 | 给测试账号发次数，替代手工 SQL |
| 前端切换 | 四个 Route Handler 内部改调后端，删 `lib/fixtures.ts` |

**本轮不做：** Stripe、R2 转存、OAuth、`/history`、`/gallery`、管理后台 UI、第二个模型、参考图上传（`input_image`）。

**部署形态：** 本地/内网运行（选项 A）。公网部署单独作为后续步骤——先在本地把生成跑通，部署失败与生成失败才不会混在一起。**但数据库从一开始就用 Postgres**（`docker-compose up -d`）：临时 SQLite 一重启账号与余额全没，会让人反复重发次数，从而污染对扣费正确性的判断。

---

## 2. 已实测确认的上游契约

以下不是文档摘抄，是 2026-07-28 用真实 key 请求得到的结果。

### 2.1 提交

```
POST https://api.ezlinkai.com/flux/v1/flux-2-max
Header: Content-Type: application/json
Header: x-key: <API_KEY>
Body:   { "prompt": "...", "width": 1024, "height": 1024,
          "output_format": "jpeg", "safety_tolerance": 2,
          "seed": 42,                        // 可选
          "input_image": "<base64 或 URL>"   // 可选，本轮不用
        }
```

实测响应（**耗时 20.96 秒**）：

```json
{
  "cost": 7,
  "id": "y4c0jsw1vsrmt0czmwmv8s7f94",
  "input_mp": 0,
  "output_mp": 1,
  "polling_url": "https://replicate.delivery/xezq/…/tmp8utul5vq.png",
  "status": "Ready"
}
```

**关键结论：ezlinkai 是同步的。** 它在内部替调用方轮询 BFL，挂住连接直到出图，直接在提交响应里给出最终图片 URL。M2 设计文档 §2.1 关于"图像生成同步"的判断得到证实，无需 reconcile worker、无需 `upstream_task_id`、前端无需轮询。

**但 `polling_url` 这个字段名有误导性**——它装的是**最终图片 URL**，不是给你去轮询的地址。adapter 里读它时必须写注释说明，否则后人会拿它当轮询端点。

### 2.2 兜底查询

```
GET https://api.ezlinkai.com/flux/v1/get_result?id=<id>
Header: Authorization: Bearer <API_KEY>
```

```json
{ "id": "…", "result": { "sample": "https://replicate.delivery/…png" }, "status": "Ready" }
```

注意**两个端点的认证头不一致**：提交用 `x-key`，查询用 `Authorization: Bearer`。这不是笔误，是实测所见。adapter 要按端点分别设置，并注释说明这是上游的不一致而非我们写错。

**兜底必须实现，即使提交通常直接返回 Ready。** ezlinkai 内部也有超时；一旦它先返回非 `Ready` 状态，我们必须能用 `id` 去查询。不实现这条，那种情况就是扣了次数拿不到图，且无从补救。

### 2.3 计价

`cost: 7` 对应 `output_mp: 1`，即上游按**输出百万像素**计价。这意味着用户选的尺寸直接影响真实成本。本轮把三种画幅都固定成约 1MP，使成本可预测：

| 画幅 | 尺寸 | 约 |
|---|---|---|
| `1:1` | 1024 × 1024 | 1.05 MP |
| `16:9` | 1344 × 768 | 1.03 MP |
| `9:16` | 768 × 1344 | 1.03 MP |

### 2.4 图片 URL 会过期

上游返回的是 `replicate.delivery` 链接，响应头 `Cache-Control: public,max-age=3600`，Replicate 官方策略是**输出文件 1 小时后清理**。

产品方决定 **R2 转存由 ezlinkai 侧后续完善，本轮不做**。因此：

> **已知后果：生成的图约 1 小时后变成死链。** 内测阶段可接受，但意味着 `/history` 在转存完成前基本无意义——历史记录里全是 404。这条必须写进 README，否则会被当成 bug 反复排查。

---

## 3. Adapter 层：每个 provider 一套，不要强行抽通用参数

**产品方明确要求兼容各官方 API 的功能**，因此不同模型的路径、请求体、响应格式**完全不同**（Flux 走 `/flux/v1/flux-2-max`，xAI、image-2、nano 各不相同）。

设计上必须承认这一点，而不是造一个"通用参数结构"去套所有家——那种抽象在第三家一定漏。adapter 接口只约定**最小共同点**：

```go
// internal/generation/adapter.go

// GenerateRequest 只包含**我们自己的**领域概念。各 provider 如何把它翻译成
// 自家的请求体、又如何从自家的响应里挖出图片 URL，全部关在各自的 adapter 内。
type GenerateRequest struct {
    Prompt string
    Width  int
    Height int
    Seed   *int // nil 表示不指定
}

type GenerateResult struct {
    ImageURL     string
    UpstreamCost int // 上游报告的成本，落流水便于对账
}

type Adapter interface {
    // Generate 同步返回结果。实现内部负责重试与兜底查询。
    // ctx 由调用方控制超时，且**必须**是脱离 HTTP 请求生命周期的 context（见 §5.1）。
    Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error)
}
```

`image_models` 表存 `provider` 字段，运行时按它选 adapter。加一个模型 = 写一个 adapter + 插一行配置，**前端一行不改**。

---

## 4. 数据模型

沿用 GORM AutoMigrate（M1 既有做法）。

| 表 | 字段 |
|---|---|
| `image_models` | `id`(string PK, 如 `flux-2-max`)、`display_name`、`provider`、`upstream_model`、`credits`、`supports_image_to_image`、`enabled`、`sort_order` |
| `credit_accounts` | `user_id`(PK, FK users)、`monthly_credits`、`addon_credits`、`updated_at` |
| `credit_transactions` | `id`、`user_id`、`type`、`monthly_delta`、`addon_delta`、`monthly_after`、`addon_after`、`generation_id`(nullable)、`note`、`created_at`；**`(generation_id, type)` 上有复合唯一索引** |
| `generations` | `id`(uuid)、`user_id`、`model`、`prompt`、`aspect_ratio`、`width`、`height`、`status`、`image_url`、`credits_spent`、`upstream_id`、`upstream_cost`、`error`、`is_public`、`duration_ms`、`created_at` |

**`credit_transactions` 记 `monthly_delta` 与 `addon_delta` 两个字段而不是一个总数**，因为退款必须按扣费时的拆分还回去。把加量包次数错还成月度次数，会在月底重置时凭空蒸发。M2 前端的 `planSpend`/`applyRefund` 纯函数就是这个逻辑的原型（`image-front/lib/fixtures.ts`），其单测可直接作为后端实现的验收参照。

`type` 取值：`generation_cost`、`generation_refund`、`admin_grant`。（`subscription_grant`、`addon_purchase` 留给 Stripe 里程碑。）

表名注意：Go 类型是 `ImageModel`，GORM 复数化后表名是 **`image_models`**，不是 `models`。不用 `TableName()` 覆盖回去——`image_models` 更自描述，而表名覆盖是后人要去翻代码才能发现的隐式魔法。

`(generation_id, type)` 上的复合唯一索引是退款幂等与"一次生成只扣一次"的**唯一权威**。不能只靠"先 `COUNT` 再 `INSERT`"：那两步之间在 READ COMMITTED 下有窗口——两个并发退款各数到 0，然后都插进去，退两次款。因此 `generation_id` 必须可空（发放类流水存 NULL 而非 `''`，否则所有发放记录会在这个索引上互相冲突；SQLite 与 Postgres 都把 NULL 视为互不相等）。

退款函数**不接收 userID**：退给谁由扣费流水的 `user_id` 说了算。若由调用方传入，handler 拿别人的 generation ID 就能给自己造钱，还会留下"用户 A 的退款流水指向用户 B 的扣费流水"这种无法对账的脏数据。


---

## 5. 同步模式的两个风险与对策

M2 设计文档 §2.2 已论证过，这里给出后端落地方式。

### 5.1 客户端断开会导致扣了次数丢了图

**Go 中若用 `c.Request.Context()` 调上游，用户关闭标签页会取消请求 context，上游调用随之中断**——次数已扣，图片丢失。Flux 实测 21 秒、慢时更久，"中途离开"是常见情况而非边缘情况。

对策：

```go
// 刻意**不**继承 c.Request.Context()：客户端断开不应该取消已经付过费的生成。
// 服务端必须把活干完并落库，用户回来能在历史里找到。
upstreamCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()
```

### 5.2 进程崩溃或部署重启吃掉进行中的生成

对策：调上游**之前**先落 `generations` 行（`status = processing`），服务启动时扫描 `processing` 的行并退款。

退款必须幂等：以"该 generation 尚无 `generation_refund` 流水"为条件，在同一事务内完成。扫描跑两次不能退两次。

---

## 6. 扣费正确性

**这是整个系统最容易出错的地方，必须有单测。**

1. **条件原子更新**，杜绝并发扣成负数：

```sql
UPDATE credit_accounts
   SET monthly_credits = monthly_credits - ?, addon_credits = addon_credits - ?
 WHERE user_id = ? AND monthly_credits >= ? AND addon_credits >= ?
```

检查 `RowsAffected`，为 0 即余额不足（返回 `40001`）。**不要**先 SELECT 再判断再 UPDATE——那中间有窗口。

2. 拆分规则：**先扣月度，不足再扣加量包**。拆分在扣费时算出并写进流水。
3. 余额变动与流水写入必须在**同一事务**内。
4. 失败退款按原拆分还回，且仅对 `failed` 的 generation 退、仅退一次。

---

## 7. API 契约

前端 M2 已按下列形状写好，**后端必须匹配**，否则前端要返工。

### `GET /api/v1/models`
```json
{ "models": [ { "id": "flux-2-max", "name": "Flux 2 Max", "credits": 1, "supportsImageToImage": false } ] }
```

### `POST /api/v1/generations`
请求：`{ "prompt": "...", "model": "flux-2-max", "aspectRatio": "1:1", "isPublic": false }`

成功（HTTP 200）：
```json
{ "id": "...", "model": "flux-2-max", "prompt": "...", "aspectRatio": "1:1",
  "isPublic": false, "status": "succeeded", "imageUrl": "https://...",
  "creditsSpent": 1, "createdAt": "2026-07-28T..." }
```

上游失败（HTTP 200，业务失败）：`status: "failed"`、`error: "..."`、**`creditsSpent: 0`**（次数已退回；记成 1 会让用户对不上账）。

余额不足：HTTP 402 + `{ "code": 40001, "message": "not enough credits" }`
模型不存在：HTTP 400 + `{ "code": 40000 }`；模型被禁用：HTTP 400 + `{ "code": 40003 }`

### `GET /api/v1/me` 扩展
在现有 `{id, email, role}` 基础上增加 `"credits": { "monthly": 12, "addon": 3 }`。

### `POST /api/v1/admin/credits`（`role=admin` 限定）
`{ "email": "tester@example.com", "monthly": 50, "addon": 0 }` → 事务内调整余额 + 写 `admin_grant` 流水。

存在理由：内测要反复给测试账号发次数，手工 SQL 既易错又不留流水。

---

## 8. 前端改动

只改四个 Route Handler 的内部实现，**组件、类型、e2e 测试全不动**——这是 M2 选择 Route Handler 假数据方案的全部意义。

| 文件 | 改法 |
|---|---|
| `app/api/models/route.ts` | 改调后端 `GET /models` |
| `app/api/credits/route.ts` | 改读后端 `GET /me` 的 `credits` |
| `app/api/generations/route.ts` | 改调后端 `POST /generations`，转发 JWT |
| `app/api/plans/route.ts` | **暂不变**（Stripe 未接，仍用假数据） |
| `app/api/credits/reset/route.ts` | **删除**，连同 `lib/fixtures.ts` |
| `e2e/global-setup.ts` | 改为通过管理员接口给测试账号发次数 |

`lib/backend.ts` 增加对应函数，沿用既有 `Result<T>` 错误形状。

**注意 e2e 的 prompt 关键词机制会失效**：`fail`/`slow`/`quick` 是假数据的确定性触发器，接真上游后普通 prompt 会真的调用 Flux（花钱且慢 21 秒）。因此 e2e 需要一个后端 mock 模式：`FLUX_API_KEY` 未配置时使用返回占位图的 stub adapter，保留关键词触发逻辑。上游规格 §10 也提到过"ezlinkai 无 key 时后端 mock 模式返回占位图"。

---

## 9. 密钥处理

`FLUX_API_KEY` 只存在于 `image-backend/.env`（已 gitignore）。`.env.example` 里只放占位符。

> 本轮使用的 key 曾在对话中明文出现，**正式上线前必须轮换**。

---

## 10. 验证方式

**单元测试（必须）：**
1. 并发扣费：并发提交，断言余额永不为负、流水与余额对账一致
2. 拆分正确：月度足够只扣月度；月度不足先扣光再扣加量包；恰好等于通过；差一拒绝
3. 退款按原拆分还回，不把加量包错还成月度
4. 退款幂等：重复调用不二次入账
5. 仅对 `failed` 退款
6. 启动扫描：卡住的 `processing` 行被退款且只退一次

**集成测试：** stub adapter 下走通成功、上游失败退款、余额不足 402 三条路径。

**真实上游联调（手工，各一次）：** 真实生成成功并出图；余额正确扣减；流水正确落库；关闭客户端连接后服务端仍完成并落库（验证 §5.1）。

---

## 11. 已知缺口（本轮不解决，需写进 README）

- **生成的图约 1 小时后变死链**（R2 转存待 ezlinkai 侧完善）
- 登录接口无速率限制（前后端皆无）——公网部署前必须补
- 无 Stripe，次数只能由管理员发放
- 无 `/history`，而同步模式 + 21 秒等待让它比原计划更重要
- 参考图（`input_image`）未接
- 套餐文案未本地化（`plans` 表需按语言列）
- 前端仅 `/api/*` 无鉴权的问题在本轮消失（改为转发 JWT 到后端），但后端必须在 `planSpend` **之前**解析身份，且鉴权检查要在调上游**之前**——否则 `slow` 路径变成免认证 DoS
