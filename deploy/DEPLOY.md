# 部署

前端在 Vercel（`https://moloom.ai`），后端在自己的服务器（`api.moloom.ai`），
数据库用外部托管实例（阿里云 PolarDB）。后端镜像由 GitHub Actions 构建并推到
Docker Hub，服务器上只拉镜像、不编译。

## 一次性准备

### 1. GitHub 仓库 secrets

CI 要推 Docker Hub，需要在 **Settings → Secrets and variables → Actions** 加两项：

| Secret | 值 |
|---|---|
| `DOCKERHUB_USERNAME` | `ye4293xx7` |
| `DOCKERHUB_TOKEN` | Docker Hub → Account Settings → Personal access tokens 里创建，权限选 **Read & Write** |

**用 access token 而不是账号密码**：token 可以单独吊销，且不会因为改密码而让 CI 全线中断。

推 GHCR 用的是 `GITHUB_TOKEN`，自动注入，不用配。

### 2. 服务器

```bash
# Docker 与 compose 插件
sudo apt update && sudo apt install -y docker.io docker-compose-v2

# 拉代码（只为了拿 compose 文件与 nginx 示例，镜像是从 Docker Hub 拉的）
git clone https://github.com/ye4293/image-backend.git
cd image-backend
```

`.env.prod` **不在仓库里**（`.gitignore` 挡着），要单独传：

```bash
# 在你本机执行
scp .env.prod 用户@服务器:~/image-backend/.env.prod
```

```bash
# 回到服务器
chmod 600 .env.prod   # 里面有主密钥和 sk_live_，别让同机其他用户读到
```

> ⚠️ **`.env.prod` 必须是 LF 行尾。** 在 Windows 上编辑过的话极可能变成 CRLF，
> 那时每个值末尾会多一个 `\r`。后果全是静默的：`DATABASE_URL` 解析失败、
> `CORS_ALLOWED_ORIGINS` 永远匹配不上（浏览器全挂而 curl 全通）、
> `STRIPE_WEBHOOK_SECRET` 验签永远失败（用户付款拿不到额度）。
> 服务器上检查与修复：
> ```bash
> file .env.prod                      # 不该出现 "with CRLF line terminators"
> sed -i 's/\r$//' .env.prod          # 若有则这样修
> ```

### 3. 数据库白名单

PolarDB 控制台把白名单收窄到服务器的公网 IP。**先部署再收窄**，否则会卡在启动阶段连不上库。

### 4. nginx 与证书

照 `deploy/nginx.conf.example` 配 `api.moloom.ai`。三个不能漏的点：

- `proxy_read_timeout 300s` —— 默认 60 秒，而慢模型生成最长 3 分钟。不调会在**快出图时**给用户 504，而后端用的是脱离请求的 context，图照样出完、次数照样已扣。
- **不要在 nginx 里加 `Access-Control-*` 头** —— CORS 由后端产生，重复的 `Access-Control-Allow-Origin` 会让浏览器拒绝整个响应，而报错却指向"后端没配 CORS"。
- **不要拦 `OPTIONS`** —— nginx 自己回的 204 不带白名单校验，等于把跨域对全网敞开。

### 5. DNS

`api.moloom.ai` 那条记录用 **灰云（DNS only）**。Cloudflare 免费版橙云代理有 **100 秒**超时上限，而慢生成最长 3 分钟 —— 会变成 524。

`image.moloom.ai`（R2 图片域名）开橙云没问题，静态文件秒级返回。

## 首次启动

```bash
docker compose -f docker-compose.external-db.yml --env-file .env.prod pull
docker compose -f docker-compose.external-db.yml --env-file .env.prod up -d
docker compose -f docker-compose.external-db.yml --env-file .env.prod logs -f backend
```

启动日志要确认：

**应该看到**
- `settings: 从环境变量播种了 N 项配置` —— 首次播种成功，**今后以数据库为准**，改 `.env.prod` 里的 `FLUX_API_KEY` / `R2_*` / `APP_BASE_URL` 不再生效，要去后台设置页改
- `listening on :8080`

**不该看到**
| 日志 | 含义 |
|---|---|
| `config: ` 开头的 Fatal | 配置被启动期校验拦下，照错误信息改 |
| `database: 临时 SQLite` | `DATABASE_URL` 没进容器 —— 服务能正常跑但**重启即丢全部数据** |
| `cors: CORS_ALLOWED_ORIGINS 未配置` | 浏览器会拦掉所有请求，而 curl 测起来一切正常 |
| `settings: R2 未完整配置` | 图片存的是上游临时链接，约一小时后全变死链 |
| `billing: ... 未配齐` | 计费禁用，订阅接口一律 503 |

### 引导管理员

`BOOTSTRAP_ADMIN_EMAIL` **不创建用户**，只在该邮箱的用户已存在时改 role。所以顺序是：

1. 用该邮箱在站上注册
2. 重启容器 —— 这一步才会提权

```bash
docker compose -f docker-compose.external-db.yml --env-file .env.prod restart backend
```

直接起完就去后台会是 403，那不是配错，是还没走完这个顺序。

### Stripe 套餐

`plans` 表的 `stripe_price_id` 已在 2026-08-03 用 `cmd/seed-stripe` 填好（`starter` / `pro` / `max`
对应 $9.99 / $29.99 / $99.99 每月），**不需要再跑**。

若将来新增档位，跑一次 `go run ./cmd/seed-stripe`（幂等，已有 Price 的行会跳过）。注意
它需要 `DATABASE_URL` 与 `STRIPE_SECRET_KEY`，而 `.env.prod` 里的数据库密码含 `)` 和 `*`，
**不能用 `source` 或 `export $(xargs)` 加载**（shell 会报语法错）。用直接赋值：

```bash
DATABASE_URL='...' STRIPE_SECRET_KEY='...' go run ./cmd/seed-stripe
```

## 发布一版

镜像**只在推 tag 时构建**。日常 push 到 `main` 只跑测试（`ci.yml`），不产出镜像。

```bash
# 本机
git tag v1.0.0
git push origin v1.0.0
```

Actions 跑完（约 2~3 分钟）后镜像就在 Docker Hub 上了。然后在服务器上：

```bash
cd ~/image-backend
git pull                                                            # 同步 compose 等文件
docker compose -f docker-compose.external-db.yml --env-file .env.prod pull
docker compose -f docker-compose.external-db.yml --env-file .env.prod up -d
```

`stop_grace_period: 5m` 会等进行中的生成跑完再换容器，所以 `up -d` 期间不会把用户的图打断。

### 预发布 tag 不会动 latest

tag 名里含 `-alpha` / `-beta` / `-rc` 的构建**不打 `latest`**。服务器默认拉 `latest`，
所以若让 `v1.0.0-rc1` 覆盖 `latest`，等于把预发布版静默推上生产 ——
`pull && up -d` 一切正常，没有任何地方提示你装的是 rc。

要部署预发布版必须显式指定：

```bash
MOLOOM_TAG=v1.0.0-rc1 docker compose -f docker-compose.external-db.yml \
  --env-file .env.prod up -d
```

## 回滚

每个镜像都带三个 tag：版本号、短 sha、以及（非预发布时）`latest`。回滚不用改任何文件：

```bash
# 回到某个版本
MOLOOM_TAG=v1.0.0 docker compose -f docker-compose.external-db.yml \
  --env-file .env.prod up -d

# 或回到某次具体提交
MOLOOM_TAG=a1b2c3d docker compose -f docker-compose.external-db.yml \
  --env-file .env.prod up -d
```

可用 tag 见 <https://hub.docker.com/r/ye4293xx7/moloom/tags>。

**CI 只保留最新 5 个 tag**（`latest` 与 `buildcache` 永不删）。所以超过 5 个版本之前的
镜像会消失 —— 需要长期保留某一版就别只依赖镜像，git tag 还在，随时可以重新触发构建
（Actions → Publish Docker image → Run workflow）。

## CI 是怎么构建的

两个 workflow：

**`.github/workflows/ci.yml`** —— push 到 `main`、PR、手动触发时跑
`go build` / `go vet` / `go test -race` / `gofmt` 检查。不碰 Docker。

之所以要单独一个：镜像改成 tag 触发之后，若测试只挂在镜像 workflow 里，坏提交会一直
躺在 `main` 上，直到打 tag 准备发布的那一刻才暴露 —— 而那是最不想被打断的时候。

**`.github/workflows/docker-image.yml`** —— 推 tag 或手动触发时：

跑测试 → buildx 构建 `linux/amd64,linux/arm64` → 推 Docker Hub 与 GHCR →
拉镜像跑冒烟测试（起容器打 `/api/v1/health`）→ 清理旧 tag 保留 5 个

冒烟测试那一步是刻意的：**构建成功不等于镜像能用**。交叉编译配错、基础镜像缺证书、
ENTRYPOINT 路径写错，都只在启动那一刻才暴露，而 CI 到那之前全是绿的。

两个架构在同一个原生 amd64 runner 上构建，**不用 QEMU**：`Dockerfile` 用
`--platform=$BUILDPLATFORM` 固定构建阶段架构，再靠 `GOARCH=$TARGETARCH` 交叉编译。
这对本项目成立是因为 `CGO_ENABLED=0`（两个数据库驱动都是纯 Go）。
**若将来引入 cgo 依赖，这个写法会失效**，必须在 workflow 里加 `docker/setup-qemu-action`
并去掉 `--platform=$BUILDPLATFORM`。

Dockerfile 里有一步校验产物架构与目标一致。交叉编译配错（例如漏了 `ARG TARGETARCH`）时
产物会静默变成构建机架构，而镜像 manifest 照样标着目标架构 —— 那种镜像只在真机启动时
以 `exec format error` 暴露，CI 全绿。那一步让它在构建期就失败。

## 未偿技术债

- **数据库走公网且未加密**：`.env.prod` 里 `sslmode=disable`。PolarDB 出厂 `ssl=off`，
  所以现在只能这样。控制台开启 SSL 后把它改回 `require`（pgx 的 `require` 只加密不校验
  证书，**不需要**往容器里塞阿里云 CA）。
- **数据库白名单曾是 `0.0.0.0/0`**：确认已收窄到服务器单 IP。
- **`CONFIG_ENCRYPTION_KEY` 需要离线备份**：它只在 `.env.prod` 里，而那个文件不进 git。
  丢了它，库里加密存的上游凭据全部无法恢复，而故障表现是「上游认证失败」，
  会把排查方向完全带偏。存进密码管理器，并与数据库备份一起管理。
