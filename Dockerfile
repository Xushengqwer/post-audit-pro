# post_audit/Dockerfile (最终修正版)

# =================================================================
# Stage 1: The Builder Stage
# =================================================================
FROM golang:1.23-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o /app/post_audit_service ./main.go


# =================================================================
# Stage 2: The Final Stage
# =================================================================
FROM alpine:latest

# 安装 ca-certificates 对需要访问外部HTTPS服务的应用（如阿里云）至关重要
RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /app/post_audit_service /app/post_audit_service

# 【关键修正】确保目标目录存在，并将开发配置文件复制并重命名
RUN mkdir -p /app/config
COPY internal/config/config.development.yaml /app/config/config.yaml

# 【关键修正】由于没有HTTP服务，移除所有 EXPOSE 和 HEALTHCHECK 指令

# 定义容器的启动命令
ENTRYPOINT ["/app/post_audit_service"]
CMD ["--config=/app/config/config.yaml"]