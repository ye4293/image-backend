# ---- 构建阶段 ----
#
# --platform=$BUILDPLATFORM 让构建阶段**始终跑在 runner 的原生架构上**（amd64），
# 而不是跟着目标平台被 QEMU 模拟。配合下面的 GOARCH=$TARGETARCH，多架构镜像靠 Go
# 自己的交叉编译产出。
#
# 这一点对本项目成立是因为 CGO_ENABLED=0：两个数据库驱动都是纯 Go 实现，没有 C
# 依赖，所以交叉编译和原生编译产物等价。若哪天引入了 cgo 依赖，这个写法会失效，
# 必须退回 QEMU（在 workflow 里加 docker/setup-qemu-action）。
#
# 不这么做的代价是实打实的：arm64 在 QEMU 下编译 Go 通常慢 5~10 倍，而 CI 是
# 每次 push main 都跑的，那个时间会一直付下去。
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

WORKDIR /src

# 先只拷依赖清单，让 go mod download 能被 Docker 层缓存住——
# 改业务代码时不会重新拉一遍依赖。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# TARGETARCH / TARGETOS 由 buildx 自动注入（amd64、arm64 等）。
# **必须声明 ARG 才拿得到**，漏了的话它们是空串，GOARCH="" 会让 go build 回退到
# 构建机架构——于是 arm64 那份镜像里装的是 amd64 二进制，在 arm 机器上启动直接
# 报 exec format error。而镜像 manifest 仍然标着 arm64，看不出任何异常。
ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 能编出静态二进制，所以运行阶段可以用最小镜像。
# 本项目的两个数据库驱动都是纯 Go 实现（glebarez/sqlite 走 modernc，
# gorm.io/driver/postgres 走 pgx），不依赖 libsqlite3 或 libpq。
# -trimpath 去掉构建机的绝对路径，-s -w 去掉调试符号，镜像小一半。
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -trimpath -ldflags="-s -w" \
    -o /out/server ./cmd/server

# 校验产物架构与目标一致。交叉编译配错时（例如漏了上面的 ARG）产物会静默变成
# 构建机架构，而镜像 manifest 照样标着目标架构——那种镜像只在真机上启动时才以
# "exec format error" 暴露，且 CI 全绿。这一行让它在构建期就失败。
RUN set -eu; \
    got=$(go version -m /out/server | sed -n 's/^[[:space:]]*build[[:space:]]*GOARCH=//p' | head -1); \
    want="${TARGETARCH}"; \
    if [ -n "$want" ] && [ "$got" != "$want" ]; then \
        echo "架构不符：产物是 '$got'，目标是 '$want'" >&2; exit 1; \
    fi; \
    echo "产物架构校验通过：$got"

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
