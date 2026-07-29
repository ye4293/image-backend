# ---- 构建阶段 ----
FROM golang:1.25-alpine AS build

WORKDIR /src

# 先只拷依赖清单，让 go mod download 能被 Docker 层缓存住——
# 改业务代码时不会重新拉一遍依赖。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 能编出静态二进制，所以运行阶段可以用最小镜像。
# 本项目的两个数据库驱动都是纯 Go 实现（glebarez/sqlite 走 modernc，
# gorm.io/driver/postgres 走 pgx），不依赖 libsqlite3 或 libpq。
# -trimpath 去掉构建机的绝对路径，-s -w 去掉调试符号，镜像小一半。
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/server ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.21

# ca-certificates 是必需的：服务要用 HTTPS 调 ezlinkai，
# 没有根证书会报 x509: certificate signed by unknown authority。
# tzdata 让容器内的时间戳不是 UTC 之外的乱值（日志与 created_at 都靠它）。
RUN apk add --no-cache ca-certificates tzdata

# 不用 root 跑。65532 是 distroless 约定的 nonroot uid，这里手动建同名用户。
RUN adduser -D -u 65532 app
USER app

COPY --from=build /out/server /usr/local/bin/server

EXPOSE 8080

# 直接 exec 二进制，不套 shell——这样容器收到 SIGTERM 时信号能直达进程，
# 优雅停机才有意义（否则 shell 吃掉信号，docker stop 只能等超时后强杀，
# 而强杀会留下卡在 processing 的生成行）。
ENTRYPOINT ["/usr/local/bin/server"]
