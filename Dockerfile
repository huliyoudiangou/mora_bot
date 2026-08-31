# syntax=docker/dockerfile:1

# ============ 构建阶段：纯 Go 静态编译（无 CGO） ============
FROM golang:1.23-alpine AS builder
WORKDIR /src

# 先只拷依赖清单，充分利用层缓存
COPY go.mod go.sum ./
RUN go mod download

# 拷源码（docs 参考、data 等已被 .dockerignore 排除）
COPY cmd ./cmd
COPY internal ./internal

# 只构建入口及其依赖包；CGO 关闭后可产出纯静态二进制
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mora ./cmd/mora

# ============ 运行阶段：轻量 alpine ============
FROM alpine:3.20
# ca-certificates：Telegram/Jellyfin HTTPS 必需；tzdata：本地时区
# adduser 创建非 root 运行用户
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S mora && adduser -S mora -G mora

WORKDIR /app
COPY --from=builder /out/mora /app/mora

RUN mkdir -p /app/data && chown mora:mora /app/data

USER mora
# main.go 已用 signal.NotifyContext 处理 SIGTERM/SIGINT，优雅退出
STOPSIGNAL SIGTERM

CMD ["/app/mora"]
