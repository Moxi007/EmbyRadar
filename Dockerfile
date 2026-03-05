# 阶段一：构建可执行文件
FROM golang:1.21-alpine AS builder

WORKDIR /app

# 启用 Go Modules 并设置代理加速下载
ENV GO111MODULE=on \
    GOPROXY=https://goproxy.cn,direct

# 缓存依赖
COPY go.mod go.sum ./
RUN go mod download

# 复制其它代码并编译
COPY . .
# CGO_ENABLED=0 确保静态链接编译
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o embyradar

# 阶段二：运行
FROM alpine:latest

# 安装必要证书并设置时区
RUN apk --no-cache add ca-certificates tzdata && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/localtime

WORKDIR /app

# 将构建好的二进制文件拷贝过来
COPY --from=builder /app/embyradar .

# 默认环境变量
ENV TZ=Asia/Shanghai

CMD ["./embyradar"]
