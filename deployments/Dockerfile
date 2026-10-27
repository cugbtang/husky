# Husky API 镜像
#
# 关键点：internal/admin/admin.go 用 //go:embed static/* 把前端打进二进制，
# 所以镜像构建必须先有前端产物，否则 go build 直接报
# "pattern static/*: no matching files found"。
# 上游这份 Dockerfile 缺了这一步——Go 阶段直接 COPY . . 就 build，构建必失败。

# ---- 阶段 1：前端构建 ----
FROM node:20-alpine AS web

WORKDIR /web

# 先只拷依赖清单，让 npm ci 这一层可缓存
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY web/ ./
RUN npx vite build --mode embed

# ---- 阶段 2：Go 构建 ----
FROM golang:1.24-alpine AS builder

# 版本必须 >= go.mod 声明的 1.24.0，否则 go build 直接报错退出
WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# 把前端产物放进 embed 目录。注意 go:embed 会跳过 "_" 开头的文件，
# 所以必须走 prepare-admin-embed.sh 做重命名 + 引用替换
# （与 make web-build 共用同一份脚本，避免两处逻辑漂移）。
COPY --from=web /web/dist ./web/dist
RUN sh scripts/prepare-admin-embed.sh web/dist internal/admin/static

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o main ./cmd/api

# ---- 阶段 3：运行 ----
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata \
    && adduser -D -H -u 10001 husky

WORKDIR /app

COPY --from=builder /app/main .

# 非 root 运行。K8s 清单里 securityContext.runAsUser 必须与此一致（10001），
# 否则 Pod 会因权限不足写临时文件失败。
USER 10001

EXPOSE 8080

# 镜像内不带 config.yaml：启动时找不到配置文件会自动回退到环境变量
CMD ["./main"]
