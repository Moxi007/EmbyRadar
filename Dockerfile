# 阶段一：构建可执行文件
FROM golang:1.21-alpine AS builder

WORKDIR /app

# 启用 Go Modules 并设置代理加速下载（可选，但在国内环境通常很有用）
ENV GO111MODULE=on \
    GOPROXY=https://goproxy.cn,direct

# 缓存依赖
COPY go.mod go.sum ./
RUN go mod download

# 复制其它代码并编译
COPY . .
# CGO_ENABLED=0 确保静态链接编译，方便放在没有任何库的基础 alpine 镜像中跑
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o embyradar

# 阶段二：运行
FROM alpine:latest

# 安装 tzdata 以便在运行时能根据需要配置时区
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# 将构建好的二进制文件拷贝过来
COPY --from=builder /app/embyradar .

# 注意：配置文件 config.json 我们不打包进去，而是通过外部挂载提供
# 设置时区变量默认值为 Asia/Shanghai
ENV TZ=Asia/Shanghai

CMD ["./embyradar"]
