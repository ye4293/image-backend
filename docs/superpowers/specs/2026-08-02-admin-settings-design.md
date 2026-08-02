# 运营配置搬进后台设置页 — 设计文档

日期：2026-08-02
前置：生成历史与 R2 转存（已完成并合并，HEAD `0782fa2`）

---

## 1. 目标与非目标

**目标：** 让运营在后台改上游 key、换 R2 桶、改前端跳转地址，不必改 `.env` 也不必重启容器。

**非目标（明确不做）：**

- **Stripe 的两个 secret 不搬。** 它们一年动不了一次，而那是唯一能直接动钱的凭据。放进库换来的灵活性接近于零，风险却最高（见 §3）。
- `DATABASE_URL` / `JWT_SECRET` / `PORT` 不搬。鸡生蛋：管理员要登录才能改设置，而登录本身依赖这两项。
- 不做配置变更审计日志、不做多环境配置集、不做配置版本回滚。YAGNI。

**搬与不搬的完整清单：**

| 配置项 | 归属 | 理由 |
|---|---|---|
| `FLUX_API_KEY` | **后台**（加密） | 换 key、轮换、接第二个网关都是常规运营动作 |
| `EZLINKAI_BASE_URL` | **后台** | 换网关地址 |
| `R2_ENDPOINT` / `R2_ACCESS_KEY_ID` / `R2_SECRET_ACCESS_KEY` / `R2_BUCKET` / `R2_PUBLIC_BASE_URL` | **后台**（两个 secret 加密） | 换桶、换域名是运营动作；且三种错填都能在保存时拦住 |
| `APP_BASE_URL` | **后台** | 纯配置，无密性 |
| `STRIPE_SECRET_KEY` / `STRIPE_WEBHOOK_SECRET` | env | 见 §3 |
| `DATABASE_URL` / `JWT_SECRET` / `PORT` / `BOOTSTRAP_ADMIN_EMAIL` | env | 启动依赖 |
| **新增** `CONFIG_ENCRYPTION_KEY` | env | 加密后台里那些 secret 的主密钥 |

env 从 11 项降到 5 项，而所有真正会改的东西都在后台。

---

## 2. 核心决定

### 2.1 secret 必须加密存储，且主密钥只能来自 env

明文写库意味着它们会出现在 `pg_dump` 备份、任何有库读权限的人手里、被拖库时、以及排查问题时随手 `SELECT *` 的终端历史里。

所以：AES-256-GCM 加密，密钥来自 `CONFIG_ENCRYPTION_KEY`（base64 编码的 32 字节）。

**要明确承认的一点：`.env` 消不掉。** 这次交换是把「6 个 secret 在 env」换成「1 个加密主密钥 + 2 个 Stripe secret 在 env」。收益是运营自助与轮换便利，不是「不用填 env 了」。

### 2.2 API 永不回传 secret 明文

管理接口读取时，secret 一律返回**掩码 + 是否已配置**两个信息：

```json
{"fluxApiKey": {"configured": true, "masked": "sk-••••••••cd12"}}
```

理由：一旦回传明文，设置页就成了一个 secret 泄露端点——任何 XSS 或被盗的管理员会话都能一次性把所有凭据读走。掩码保留「看得出是不是那一把」的能力，够用。

改 secret 只能整体覆写，不能读出来改一半。

### 2.3 运行时热替换，不需要重启

三个构造点全在 `internal/server/router.go`：`billing.New`（只有 `AppBaseURL` 变）、`buildStorage`、`buildFluxAdapter`。

引入 `settings.Runtime`：持有当前生效的配置值与**已构造好的客户端**，管理接口写入成功后触发 `Reload()`，用 `atomic.Pointer` 原子换掉整个快照。读方每次请求取一次快照。

**为什么用原子快照而不是每次请求重建：** 重建会为每个请求新建 HTTP client 与 S3 client，连接池全部失效。而加锁读会让生成这种长请求持锁数十秒。原子快照读是无锁的，写极少发生。

**为什么不选「保存后需重启生效」：** 那只交付了一半的灵活性，而重启会掐断正在进行的生成（`stop_grace_period` 是 5 分钟，等于每次改配置最坏要等 5 分钟）。

### 2.4 首次启动从 env 播种，之后库是权威

升级后第一次启动时，若设置表为空，就把当前 env 里的 `FLUX_API_KEY` / `R2_*` / `APP_BASE_URL` 写进去（secret 加密）。之后这些 env 变量不再被读取。

理由：这给了一条零改动的迁移路径——现有 `.env` 照原样部署，配置自动进库，然后就能在后台改。若改成「库里没有就回落到 env」，则「当前生效的到底是哪个值」会长期含糊，而那种含糊在排查配置问题时代价很高。

播种后在启动日志里打一行明确说明，并在 `.env.prod.example` 里注明这些项只在首次启动时被读取。

### 2.5 启动期拦截改成写入期校验 + 启动期告警

现在 `ValidateStorage` 是**拒绝启动**。配置可运行时修改之后，「启动时对、后来被改坏」变成可能，所以校验必须挪位置：

- **写入时校验并拒绝**（返回 400），复用现有 `ValidateStorage` 的规则：R2 公开域名不能为空 / 不能是 S3 API 域名 / 必须带 scheme。这是主防线，因为它在坏数据产生**之前**。
- **启动时仍然校验**，但从 `Fatal` 降为**告警日志**——库里的值可能是上一个管理员改坏的，此时拒绝启动等于让一次误操作把服务打死，而正确的行为是带着告警起来、让管理员能登录进去改回来。

`CONFIG_ENCRYPTION_KEY` 缺失或长度不对**仍然拒绝启动**：解不开的密文等于所有上游凭据失效，那时候起来也没用，且会以"上游认证失败"的形式误导排查。

---

## 3. 为什么 Stripe 的 secret 不搬

三条理由，任一条单独都够：

1. **收益接近于零。** Stripe 密钥一年动不了一次。为一个永不使用的便利，把唯一能直接动钱的凭据挪进备份文件里，不划算。
2. **webhook secret 无法校验。** 填错的唯一表现是所有 webhook 400、Stripe 无限重投、用户付了钱拿不到次数。而保存时**无法验证**它——只能等真事件来。一个改错了没有任何即时反馈、后果又是丢单的配置项，不该做成能随手改的。
3. **它已经有启动期拦截。** `ValidateStripe` 会拦住 `sk_live_` + localhost 这个组合。搬进库之后这个拦截只能降级成告警（同 §2.5），而它防的正是"真实扣款后跳到用户打不开的地址"。

`APP_BASE_URL` 例外——它无密性，且改前端域名是真实会发生的运营动作。但它参与 `ValidateStripe` 的判断，所以写入时要带上同样的 live-key + localhost 校验。

---

## 4. 数据模型

```go
// AppSetting 是一张 key/value 表，不是每个配置项一列。
//
// 选 key/value 而不是宽表：新增一项配置不用迁移。代价是没有列级类型约束——
// 由 §5 的白名单与写入校验补上，那比数据库类型更能表达"R2 公开域名不能是 S3
// 域名"这类规则。
type AppSetting struct {
	Key string `gorm:"primaryKey;size:64"`
	// Value 非 secret 项存明文；secret 项存 base64(nonce||ciphertext)。
	Value string `gorm:"type:text;not null"`
	// Encrypted 标记 Value 是否为密文。
	//
	// 显式存这一列而不是靠 Key 推断：轮换加密方式时需要能区分"这行还是旧格式"，
	// 而靠 key 名推断会让迁移期无法判断。
	Encrypted bool `gorm:"not null;default:false"`
	UpdatedAt time.Time
}
```

不加 `updated_by`：审计是非目标（§1）。

---

## 5. 配置项白名单

`internal/settings` 里用一张表声明每一项：key、是否 secret、默认值、校验函数。

```go
type Spec struct {
	Key      string
	Secret   bool
	Default  string
	// EnvVar 首次播种时从哪个环境变量取（§2.4）。
	EnvVar   string
}
```

**白名单是必须的**，不能让管理接口写任意 key：否则一个打错的 key 会被静默接受、看起来保存成功、而实际什么都没生效。未知 key 一律 400。

---

## 6. 接口

全部挂在已有的 `admin` 路由组下（`middleware.RequireAdmin`）。

`GET /api/v1/admin/settings` → 200

```json
{
  "settings": {
    "ezlinkaiBaseUrl": {"value": "https://api.ezlinkai.com"},
    "fluxApiKey": {"configured": true, "masked": "sk-••••••••cd12"},
    "r2Endpoint": {"value": "https://x.r2.cloudflarestorage.com"},
    "r2AccessKeyId": {"configured": true, "masked": "abc••••••••1234"},
    "r2SecretAccessKey": {"configured": false, "masked": ""},
    "r2Bucket": {"value": "images"},
    "r2PublicBaseUrl": {"value": "https://img.example.com"},
    "appBaseUrl": {"value": "https://app.example.com"}
  },
  "storageEnabled": true
}
```

非 secret 项给 `value`；secret 项只给 `configured` + `masked`（§2.2）。`storageEnabled` 让前端直接显示「转存是否生效」，不必自己拼五项判断。

`PATCH /api/v1/admin/settings` — 请求体是**部分更新**，只含要改的 key：

```json
{"r2Bucket": "images-v2", "r2SecretAccessKey": "新的密钥"}
```

- 未知 key → 400
- 校验失败 → 400，message 说明原因（复用 `ValidateStorage` 的三条规则）
- 成功 → 200 返回与 GET 同形状的结果，**并已触发 `Runtime.Reload()`**

secret 传空字符串表示**清空**（退化成未配置），而不是"不改"。不想改就不要带这个 key——这与 M4c 的 PATCH 指针语义一致。

---

## 7. 影响范围

| 文件 | 变更 |
|---|---|
| `internal/model/setting.go`（新） | `AppSetting` |
| `internal/settings/spec.go`（新） | 白名单与校验 |
| `internal/settings/crypto.go`（新） | AES-256-GCM 加解密 |
| `internal/settings/store.go`（新） | 读写、播种、掩码 |
| `internal/settings/runtime.go`（新） | 原子快照 + Reload + 构造客户端 |
| `internal/config/config.go`（改） | 加 `ConfigEncryptionKey`；R2/Flux/AppBaseURL 降级为播种源 |
| `internal/server/router.go`（改） | 三个构造点改为从 `Runtime` 取 |
| `internal/handler/admin_settings.go`（新） | GET / PATCH |
| `internal/handler/generations.go`（改） | `Adapters` 改为从 Runtime 取 |
| `internal/database/database.go`（改） | 迁移 `AppSetting` |
| `cmd/server/main.go`（改） | 建 Runtime、播种、校验降级为告警 |
| `.env.prod.example` / `.env.example`（改） | 新增 `CONFIG_ENCRYPTION_KEY`；注明播种项 |

前端 `/admin/settings` 页面另开一份计划（与历史页那轮同样分两份）。

---

## 8. 验证方式

**加密层：** 加密→解密往返；同一明文两次加密密文不同（nonce 随机）；密文被改一个字节则解密失败（GCM 完整性）；错误密钥解不开。

**播种：** 空表 + env 有值 → 播种进库且 secret 已加密；再次启动不覆盖已有值（**这条最重要**：覆盖会让后台改过的配置在每次重启后被 env 悄悄改回去）。

**掩码：** GET 永不返回明文（对每个 secret 项断言响应里不含原值）。

**热重载：** 改 `r2Bucket` 后，**不重启**，下一次生成上传到新桶（用假 storage 断言 key 前缀或 bucket 变化）。

**写入校验：** 三条 R2 规则各一条用例；未知 key 400；secret 传空 → `configured:false`。

**降级：** 库里存了非法的 R2 公开域名（模拟被改坏）→ 启动**不失败**，打告警，`storageEnabled=false`。

**人工：** 后台改 `FLUX_API_KEY` 为错误值 → 生成失败且日志是「上游认证失败」；改回 → 恢复。全程不重启。

---

## 9. 已知问题与后续

- `CONFIG_ENCRYPTION_KEY` 丢失 = 所有 secret 不可恢复，只能在后台重填。需要在部署文档里写清楚它和数据库备份要一起备份。
- 不做密钥轮换工具。`Encrypted` 列为将来留了余地（§4）。
- 配置改动没有审计记录，谁改了什么无从追溯（非目标）。
- Stripe 两个 secret 仍在 env，改它们仍需重启（刻意，§3）。
