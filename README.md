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

## API（M1）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/v1/health | 健康检查 |
| POST | /api/v1/auth/register | 邮箱注册 {email, password} |
| POST | /api/v1/auth/login | 登录，返回 {token} |
| GET | /api/v1/me | 当前用户（Bearer token） |

## 开发命令

```
go build ./... && go vet ./...   # 提交前必跑
go test ./...                    # 运行测试
```
