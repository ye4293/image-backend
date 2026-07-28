# image-backend

AI 图像生成订阅平台后端（Go + Gin + GORM + PostgreSQL）。

设计文档：`docs/superpowers/specs/2026-07-27-image-platform-design.md`

## 本地运行

```
docker compose up -d          # 启动 Postgres
cp .env.example .env          # 按需修改
go run ./cmd/server           # 启动服务，默认 :8080
```

不配置 DATABASE_URL 时使用临时文件 SQLite（dev 模式），零配置即可启动。

## API（M1 + M3a）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/v1/health | 健康检查 |
| POST | /api/v1/auth/register | 邮箱注册 {email, password} |
| POST | /api/v1/auth/login | 登录，返回 {token} |
| GET | /api/v1/me | 当前用户（Bearer token），含 `credits.monthly/addon` |
| GET | /api/v1/models | 启用的模型列表（公开），含 `credits` 字段 |
| POST | /api/v1/admin/credits | 管理员给指定邮箱发放次数（`role=admin`） |

## 次数账本

双余额：`monthly_credits` 随订阅每月重置，`addon_credits` 一次性购买永不过期。
扣费**先扣月度、不足再扣加量包**。

**所有余额变动只能经过 `internal/credit`。** handler 不得直接写 `credit_accounts`——
绕过它就意味着漏流水，而漏了流水的余额无法对账，出问题时只能猜。

### 数据表

- `image_models`：模型配置，GORM 由 `ImageModel` 复数化而来（不是 `models`）。启动时幂等播种 `flux-2-max`，运营可改 `credits` 等字段——用 `FirstOrCreate` 而非 `Save`，每次启动不会覆盖线上调整。
- `credit_accounts`：双余额，`user_id` 为主键（无自增）。
- `credit_transactions`：不可变流水。`monthly_delta` 与 `addon_delta` 分开记，不合并成总数：退款必须按扣费时的拆分还回去，把加量包次数错还成月度次数会在月底重置时凭空蒸发。

### 幂等与安全性

`credit_transactions` 的 `(generation_id, type)` 有复合唯一索引 `idx_credit_tx_gen_type`，
这是退款幂等与"一次生成只扣一次"的**唯一权威**，不靠"先 Count 再 INSERT"：
COUNT 在 READ COMMITTED 下不加锁，两个并发退款会各数到 0 然后都插进去——退两次。
唯一键冲突没有这个窗口。

`generation_id` 是 `*string`（可空）：发放类流水存 `NULL` 而非空串，
因为 SQLite 与 Postgres 都把 NULL 视为互不相等——所有发放记录不会在唯一索引上互相冲突。

`credit.Refund(db, generationID)` **不接收 `userID` 参数**：退给谁由扣费流水说了算。
若接收 `userID`，handler 把 JWT 里的 `userID` 和请求里的 `generationID` 一拼，
拿别人的 generation ID 就能给自己造钱，还会留下无法对账的脏数据。

`credit.Grant` 拒绝负数：它走的是不带 WHERE 守卫的相对 UPDATE，放负数进来能把余额扣成负数。
目前没有管理员冲正/扣减路径——冲正若要做，必须走一条和 `Spend` 同样三层防护的独立路径。

### 扣费的三重保险（缺一不可，见 `internal/credit/ledger.go` 注释）

1. **事务**：余额变动与流水同生共死。
2. **`SELECT ... FOR UPDATE` 行锁**（Postgres 生效；SQLite 靠单连接串行化）。
3. **带条件的 `UPDATE` 并校验 `RowsAffected == 1`**：即使前两层被绕过也不会扣成负数。

`Spend` 针对 **READ COMMITTED**（Postgres 默认）设计。在 REPEATABLE READ 下，
同样的并发交错会抛序列化失败而非重新读取，且本包不实现重试。

### 并发测试需要 Postgres——上线前的硬性门控

`database.Open` dev 模式用临时 SQLite 且 `SetMaxOpenConns(1)`，连接池**串行化**所有并发请求。
在默认配置下并发扣费测试必然通过且什么都没证明。

有两条测试由 `TEST_DATABASE_URL` 门控，环境变量未设置时显式 SKIP 并打印原因（不静默跳过）：

- `TestConcurrentSpendNeverOversells`：验证 30 并发扣费中恰好 10 次成功、余额归零。
- `TestConcurrentRefundRefundsOnce`：**这是唯一覆盖 `Refund` 里重复键回滚分支的测试。**
  该代码路径在 CI 和本地从未执行过。

**在装有 Postgres 的机器上跑通这两条测试是上线前的硬性前置条件，不是可选项。**
它们覆盖的是扣成负数 / 超卖 / 双退——全系统后果最贵的失效模式，且代码路径从未在真实数据库上运行。

```bash
# 需要 Postgres（本机 dev 模式无法运行）
TEST_DATABASE_URL="postgres://imageapp:imageapp@localhost:5432/imageapp?sslmode=disable" \
  go test ./internal/credit/ -run TestConcurrent -v
```

### 给测试账号发次数

注册接口不会创建管理员，第一个 admin 只能直接改库。
dev 模式下 DB 路径在启动日志里打印（`database: using temporary SQLite /tmp/image-backend-dev-*.db`）：

```bash
# sqlite3 或任意 SQLite 客户端
sqlite3 /tmp/image-backend-dev-XXXX.db \
  "UPDATE users SET role = 'admin' WHERE email = 'you@example.com';"
```

Postgres 模式：

```sql
UPDATE users SET role = 'admin' WHERE email = 'you@example.com';
```

之后用接口发放（会留流水，比手工 SQL 可追溯）：

```bash
curl -X POST localhost:8080/api/v1/admin/credits \
  -H "Authorization: Bearer <admin-token>" \
  -H 'Content-Type: application/json' \
  -d '{"email":"tester@example.com","monthly":50,"addon":0}'
```

### 已知缺口

- `Spend` 里 `RowsAffected != 1` 把"余额不足"与"账户行在锁外被删了"混为一谈，都返回 `ErrInsufficientCredits`。
- `database.Open` dev 模式每次调用都新建临时 SQLite 文件且从不清理（M1 遗留）。
- 没有管理员冲正 / 扣减路径——`Grant` 只拒绝负数，冲正需独立实现。
- 月度重置无实现，等 Stripe `invoice.paid` 事件（M4）。
- 登录接口无速率限制（M1 遗留），公网部署前必须补。
- `TestConcurrentRefundRefundsOnce` 覆盖的 `Refund` 重复键回滚分支从未在真实数据库执行。
- 内测若继续用临时 SQLite，进程一停账号与余额全没；正式内测前需要 Docker Desktop 或本机 Postgres。

## 开发命令

```
go build ./... && go vet ./...   # 提交前必跑
go test ./...                    # 运行测试（并发测试需 TEST_DATABASE_URL）
```
