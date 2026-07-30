# 生成历史与 R2 转存 — 设计文档

日期：2026-07-30
前置里程碑：M3b（Flux 接通）、M4a（Stripe 订阅）、M4c（后台配置）
配套仓库：`../image-front`（前端已在 `e91c750` 完成真实后端对接，无假数据层）

---

## 1. 背景与目标

`POST /generations` 是同步接口，响应体就是成品图。但整条读路径不存在：

- `GenerationsHandler` 只有 `Create`。`generations` 表写入了 `user_id` / `created_at` / `image_url`，**没有任何 HTTP 接口能读出来**。
- 客户端一旦丢掉那个 POST 响应（关标签页、断网、刷新），图片对该用户**永久不可达**——而次数已经扣了。
- `image_url` 指向上游 CDN，约一小时后失效。即使现在就加一个历史页，页面上也全是死链。

这两件事必须**捆在一起**做：只做历史页会交付一个满是坏图的页面，比没有更糟；只做转存则用户仍然只能在当次会话里看到图。

**目标：** 用户能在 `/history` 里找回自己付费生成过的每一张图，且链接永久有效。

**本轮不做：** 删除、收藏、搜索、按模型筛选（都不服务于"拿回自己的图"这个目标）；公开画廊（`is_public` 继续是只写字段）；图生图与参考图上传；M4b 加量包。

---

## 2. 核心决定

### 2.1 同步转存，不引入后台 worker

上游返回图片 URL 后，**在同一个生成请求内**下载、传 R2、写永久 URL 入库，再返回给用户。

取舍：请求时长 +1~3 秒。Flux 实测约 21 秒，占比很小，不改变 UX 量级。备选的异步 worker 方案需要队列、重试、状态机——这个项目目前完全没有这一层基础设施，而它换来的只是那几秒，且会让 `image_url` 出现"先临时后永久"的中间态，历史页在转存完成前依然是死链。

### 2.2 转存失败 → 降级，不退款，不判失败 ⚠️

**这条违反直觉，所以必须显式记下来，否则以后一定有人把它"修"掉。**

任何一步转存失败（未配置、下载失败、类型不合法、超限、上传失败），行为一律是：告警日志 + 保留上游临时 URL + `stored = false` + **生成照常算成功、不退款**。

理由：图已经出了，钱已经花在上游了。因为我们自己的存储抖动就判定失败并退款，等于把一次成功且已付费的上游调用白扔，用户还得重新排队等 21 秒。降级的最坏后果只是这一张图一小时后失效——比彻底没有强得多。

### 2.3 公开桶 + 自定义域，不用预签名 URL

`image_url` 存完整永久 URL（`https://<公开域>/g/<uuid>.<ext>`）。访问控制依赖 key 不可猜（UUID）。

理由：预签名 URL **会把我们正在修的问题重新引入一遍**——它本质上还是过期链接，只是过期时间由我们控制。用户把图发给朋友、或者第二天回来看历史，链接又失效了，只不过这次是我们自己造的。而且签名参数会让 CDN 缓存命中率趋近于零。

**必须知道的代价：** 此时"私有生成"只意味着"不出现在公开画廊里"，**不意味着别人拿到 URL 打不开**。如果产品定位要求"私有"是真正的鉴权隔离，需要另做一轮。

**方向性说明：** 这个决定是**可以后续收紧的**（把桶转私有 + 加签名层），反过来"从公开改成签名"才是不可逆的泄露。所以先选公开是安全的一侧。

### 2.4 历史页只读

`GET /generations` + 前端网格。没有删除接口——删除要额外决定"删数据库行还是删 R2 对象"（只删行会留孤儿文件持续付费，连 R2 一起删则操作可能只成功一半），而它不服务于本轮目标。

---

## 3. 数据模型

### 3.1 `generations` 加一列

```go
// Stored 图片是否已转存到我们自己的存储。
//
// false 有两种来源：R2 未配置（本地开发），或转存失败后降级（见 §2.2）。
// 两种情况下 ImageURL 都是上游的临时链接，约一小时后失效——历史接口把这一列
// 透出去，前端才能诚实地提示"链接可能已失效"，而不是让用户对着坏图猜。
Stored bool `gorm:"not null;default:false"`
```

**不加 `storage_key` 列。** key 从 `id` 确定性推导（`g/<id>.<ext>`），再存一份就是两份可能不一致的真相。档 1 没有删除功能；真要删时按 URL 反推即可。

### 3.2 换成复合索引

现状 `UserID` 上是普通单列索引，只能过滤，排序要额外 sort。历史查询是：

```sql
WHERE user_id = ? AND status != 'processing' ORDER BY created_at DESC, id DESC LIMIT ?
```

改为：

```go
UserID    uint      `gorm:"index:idx_gen_user_created,priority:1;not null"`
CreatedAt time.Time `gorm:"index:idx_gen_user_created,priority:2"`
```

现在几十行数据看不出差别；等单用户攒到几千张时这是"翻页 200ms"与"翻页 3 秒"的差别，而那时候加索引要锁表。

### 3.3 存量数据

库里已有记录的 `image_url` 全是失效的上游链接，但**都是测试数据**，不写迁移脚本，直接清表。

注意 `stored` 列的用途**不是**为了这些老数据——降级路径会持续产出同样的东西（§2.2），所以这个区分是长期需要的。

---

## 4. 配置

五个环境变量，全部无默认值：

| 变量 | 说明 |
|---|---|
| `R2_ENDPOINT` | `https://<account_id>.r2.cloudflarestorage.com`。**存完整 endpoint 而不是 account id**，这样测试能指向本地 minio 或 `httptest.Server` |
| `R2_ACCESS_KEY_ID` | |
| `R2_SECRET_ACCESS_KEY` | |
| `R2_BUCKET` | |
| `R2_PUBLIC_BASE_URL` | 自定义域，如 `https://img.example.com`。拼最终 URL 用 |

```go
// StorageEnabled 五项齐全才算配置好。缺任何一项都退化成 NoopStorage——
// 与 FluxAPIKey 为空退化成 stub、StripeSecretKey 为空禁用计费是同一个约定。
func (c *Config) StorageEnabled() bool
```

### 一条启动期误配拦截

```go
// ValidateStorage：配了 R2 凭证但没配 R2_PUBLIC_BASE_URL → 拒绝启动。
func (c *Config) ValidateStorage() error
```

与 `ValidateStripe` 拦 `sk_live_` + localhost 是同一类：这个组合**不报错，只静默产出坏数据**。少了公开域名，只能拿 S3 endpoint 拼 URL，而那个地址不允许匿名读——上传全部成功、`stored=true`、每张图在浏览器里 401。等发现时已经攒了一批 URL 全错的记录，而它们指向的对象是好的，得写脚本回头改。启动时拦住，成本是一个 if。

- 两处 env 示例文件（`.env.example`、`.env.prod.example`）同步五个变量。
- 前置条件（人工）：Cloudflare R2 开通、建桶、绑自定义域并开启公开访问。

---

## 5. 存储层 `internal/storage/`

```go
// storage.go
type Storage interface {
    // Put 上传并返回可公开访问的永久 URL。
    Put(ctx context.Context, key, contentType string, body []byte) (string, error)
}

// ErrNotConfigured NoopStorage 的固定返回，调用方据此走降级路径。
var ErrNotConfigured = errors.New("storage is not configured")
```

**签名收 `[]byte` 而不是 `io.Reader`：** aws-sdk-go-v2 的 `PutObject` 需要可重放的 body 才能签名和重试，给它一个不可 seek 的 reader 会导致 SDK 自己先缓冲一遍——同一份数据在内存里两份。反正上游就是一张图、我们本来就要限大小，直接收字节更诚实。

- `r2.go` — `s3.NewFromConfig`，`BaseEndpoint` = `R2_ENDPOINT`，`Region: "auto"`（R2 的要求），`UsePathStyle: true`。返回 `R2_PUBLIC_BASE_URL + "/" + key`。
- `noop.go` — `NoopStorage.Put` 直接返回 `ErrNotConfigured`。

**Noop 返回错误而不是返回原 URL**，是为了让"未配置"和"配置了但失败"在装饰器里走**同一条**代码路径。否则降级分支只在生产才会被走到，而那是最不该第一次运行的地方。

依赖选型：`aws-sdk-go-v2`。排除手写 SigV4——签错的表现是 403 且错误信息不指向出错的那一步，调它能耗掉一整天，换来的只是几 MB 二进制体积，对容器里的服务毫无意义。排除 `minio-go`——R2 的官方兼容性声明与可查资料都是针对 AWS SDK 的。

---

## 6. 转存放在装饰器层 `internal/generation/storing.go`

```go
type StoringAdapter struct {
    inner  Adapter
    store  storage.Storage
    client *http.Client
}
```

`GenerateResult` 加 `Stored bool` 字段；handler 里 `gen.Stored = res.Stored`。

**为什么是装饰器而不是写在 handler 里：** 它顺着这个项目已有的结构长——`Registry` + `Adapter` + `StubAdapter` 已经建立了"provider 行为可替换、可注入假实现"的模式。新增 provider 会自动获得转存，不依赖谁记得加代码。而塞进 handler 会让"转存"只能靠跑完整生成流程来测，而完整流程测试正是这个项目一直刻意避免的东西（stub adapter 存在的全部理由就是这个）。

`Generate` 流程，**每一步失败都降级、返回 nil 错误**（§2.2）：

1. `inner.Generate` 失败 → 原样返回，不碰存储。
2. `ImageURL` 为空 → 原样返回。
3. 下载图片，**独立 60 秒超时，不继承上游那 5 分钟**。上游花了 4 分 50 秒的话，共用 ctx 只剩 10 秒给转存、必然降级——而这时候本来是可以再等一会儿的。
4. **限制 20 MiB**（`io.LimitReader`，读满即判超限）。无上限地下载进内存是内存耗尽向量：并发几十个请求 + 上游返回一个巨大或永不结束的响应，就能把服务打死。
5. **用 `http.DetectContentType` 嗅探前 512 字节判类型，不信上游的 `Content-Type` 头**；只允许 `image/png` / `image/jpeg` / `image/webp`，扩展名从嗅探结果推导。这不是洁癖：我们要把这个字节流挂到**自己的域名**下，上游若返回 HTML，我们就在自己的 origin 上托管了一个别人可控的 HTML 文件——那是 XSS。
6. `store.Put(ctx, "g/<genID>.<ext>", contentType, body)` → 成功则替换 `ImageURL`、`Stored = true`；失败则告警日志 + 保留原 URL + `Stored = false` + 返回 nil。

`BuildAdapters` 包一层：

```go
func BuildAdapters(cfg *config.Config) generation.Registry {
    store := buildStorage(cfg) // 未配置 → NoopStorage，并 log 一行
    return generation.Registry{
        "flux": generation.NewStoringAdapter(buildFluxAdapter(cfg), store),
    }
}
```

---

## 7. 历史接口

`GET /api/v1/generations` — JWT（挂在已有的 `authed` 组下）。

| 参数 | 说明 |
|---|---|
| `cursor` | 不透明串：base64 of `<createdAt RFC3339Nano>\|<id>`。不透明是为了以后能换实现而不破坏客户端 |
| `limit` | 1–50，默认 20。越界**钳制而不报错** |

**200：**

```json
{
  "generations": [ /* toGenerationResponse 的结果，加 stored 字段 */ ],
  "nextCursor": "..."
}
```

- 复用已有的 `toGenerationResponse(gen)`，与 `POST /generations` 的响应形状**不可能分叉**。
- `nextCursor` 无下一页时为 `null`。
- 游标条件写成 `created_at < ? OR (created_at = ? AND id < ?)`，**不用行值比较元组**——SQLite 与 Postgres 对行值比较的支持不一致，而本项目两边都要跑。
- **不返回 `processing` 状态的行。** 它要么很快转终态，要么会被启动兜底扫描回收；露出来只会让用户看到一个永远转圈的格子。
- **失败的记录要返回**，带 `status:"failed"` 与 `error`。用户看到"我明明生成过一张"却在历史里找不到，会怀疑是不是被吞了钱；而失败记录恰恰能证明没扣钱（`creditsSpent: 0`）。

**错误：**

| 条件 | 状态 | 响应 |
|---|---|---|
| cursor 无法解码 | 400 | `{"code":40000,"message":"invalid cursor"}` |
| DB 失败 | 500 | `{"code":50000,"message":"internal error"}` |
| 未登录 / 非 active | 401 / 403 | 沿用中间件既有响应 |

**非法 cursor 不能静默当第一页处理**：那会让翻页在游标格式变更后无声地从头开始，用户以为图丢了。

`POST /generations` 的响应也加 `stored`，让前端出图当下就能提示。

---

## 8. 前端 `/history`

- `lib/backend.ts` 加 `listGenerations(token, cursor?, limit?)`，沿用现有 `Result<T>` 判别联合与错误码常量。
- `app/api/generations/route.ts` **加一个 GET handler**（同路径不同方法），自己 `getToken()`（`/api/*` 不在 proxy matcher 内），无 token → 401。只读，不需要 CSRF 检查。
- `app/[locale]/history/page.tsx` — RSC，与 `/account`、`/generate` 一致：`getToken()` + 401 兜底；首屏直接 RSC 打 Go，不绕自家 Route Handler（沿用既有约定）。
- `proxy.ts` 的 `PROTECTED` 正则加 `history`；顶栏加入口。
- 分页用**「加载更多」按钮**，不用无限滚动：无限滚动在 375 宽下会让页脚永远够不着，且 Playwright 里难以确定性断言"翻到了第二页"。
- 卡片三态：
  - 成功且 `stored` → 正常图；
  - 成功但 `!stored` → 图 + 「链接可能已失效」提示；
  - `failed` → 灰格子 + 错误提示 + 「未扣除次数」。
- `messages/{en,zh,ja,ko}.json` 加 `History` 命名空间，**四语齐全**，不硬编码任何用户可见文案。
- 遵循既有栈陷阱：Next 16 的 Proxy（非 Middleware）、shadcn v4 无 `asChild`（用 `<Link className={buttonVariants({...})}>`）。

---

## 9. 测试方式

**后端**

- `internal/storage`：`R2Storage` 指向 `httptest.Server`，断言 PUT 的 path 含 bucket 与 key、`Content-Type` 正确、**返回 URL 由公开域拼出而非 S3 endpoint 拼出**（正是 §4 启动拦截要防的那个错）。
- `internal/generation/storing_test.go`（假 inner + 假 storage，不碰网络、不碰数据库）：
  1. 成功 → 替换 URL 且 `Stored=true`；
  2. **storage 报错 → 保留原 URL、`Stored=false`、返回 nil 错误**（§2.2 降级契约，最容易被后人"修"掉的一条）；
  3. inner 失败 → 完全不调 storage；
  4. 上游返回 HTML → 拒绝并降级；
  5. 超过 20 MiB → 拒绝并降级。
- `internal/server/generations_list_test.go`：
  1. **建两个用户交叉验证只能看到自己的记录**（最容易写错、后果最严重的一条）；
  2. `processing` 不返回；
  3. 插 5 条用 `limit=2` 翻三页，断言不重不漏；
  4. 非法 cursor → 400；
  5. `limit=0` / `limit=999` 被钳制。
- `internal/config`：五项齐全才 `StorageEnabled`；配了凭证但缺 `R2_PUBLIC_BASE_URL` → `ValidateStorage` 报错。

**前端**

- vitest：`listGenerations` 的成功 / 401 / 不可达映射；cursor 与 limit 确实进了 query string。
- playwright：未登录访问 `/history` 跳登录；生成一张后历史页能看到它；「加载更多」翻页；**375×667 视口下的网格布局与无横向溢出**。

**人工**

- 真实 R2 凭证跑一次生成，确认对象进桶、自定义域能匿名打开、一小时后仍可访问。
- 故意填错 `R2_BUCKET`，确认生成仍然成功、`stored=false`、日志有告警、次数没被退。

---

## 10. 影响范围

| 文件 | 变更 |
|---|---|
| `internal/model/generation.go` | 加 `Stored`，换复合索引 |
| `internal/config/config.go` | 五个 R2 配置项、`StorageEnabled`、`ValidateStorage` |
| `internal/storage/{storage,r2,noop}.go` | 新建 |
| `internal/generation/adapter.go` | `GenerateResult` 加 `Stored` |
| `internal/generation/storing.go` | 新建装饰器 |
| `internal/handler/generations.go` | `gen.Stored = res.Stored`；`toGenerationResponse` 加 `stored`；新增 `List` 方法 |
| `internal/server/router.go` | `BuildAdapters` 包装饰器；注册 `GET /generations` |
| `cmd/server/main.go` | 调用 `ValidateStorage` |
| `.env.example` / `.env.prod.example` | 五个变量 |
| `go.mod` | `aws-sdk-go-v2` |
| 前端 | `lib/backend.ts`、`app/api/generations/route.ts`、`app/[locale]/history/page.tsx`、`proxy.ts`、`components/site-header.tsx`、`messages/*.json` ×4 |

---

## 11. 已知问题与后续

- **"私有"不是鉴权隔离**（§2.3）。若产品需要真正的隔离，需另做一轮。
- **ja/ko 译文未经母语审校**——本轮沿用现状，不假装已完成。
- 后端错误 `message` 仍会以英文穿透到四种语言的界面。前端已刻意拒绝按字符串匹配翻译，改为按 `code` 映射是独立的一轮工作。
- 图生图与参考图上传仍是假的（`param-panel.tsx` 只读文件名做本地预览，后端 `POST /generations` 也没有图片字段），但 `GET /models` 却在对外声明 `supportsImageToImage`。
- 登录无限流，上线前必修。
- 公开画廊（`is_public` 的读路径）未做。
- M4b 加量包一次性支付未做。
