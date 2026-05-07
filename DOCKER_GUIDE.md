# EmbyRadar Docker 使用指南

本指南详细介绍了如何使用 Docker 部署和运行 `EmbyRadar`。

## 1. 环境准备

确保您的系统中已安装以下软件：
- Docker
- Docker Compose

**目录结构准备：**
项目根目录下应包含一个 `config` 文件夹，其中存放 `config.json`：
```text
EmbyRadar/
├── config/
│   └── config.json
└── docker-compose.yml
```

## 2. 核心配置文件说明

- **Dockerfile**: 采用多阶段构建，确保镜像体积最小化并包含必要证书。
- **docker-compose.yml**: 将宿主机的 `./config` 目录挂载到容器的 `/app/config`。
- **.dockerignore**: 排除不必要的文件，加速镜像构建。

## 3. 部署步骤

### 本地构建并启动
在项目根目录下运行：
```bash
docker-compose up -d --build
```
该命令会：
1. 构建镜像并运行容器。
2. 自动在 `config/` 目录下生成 `message_id.json` 缓存文件。

### 查看日志
```bash
docker-compose logs -f
```

### 停止并移除容器
```bash
docker-compose down
```

## 4. 常见问题 (FAQ)

- **时区问题**: 默认时区已设置为 `Asia/Shanghai`。如果需要更改，请在 `docker-compose.yml` 的 `environment` 部分修改 `TZ` 变量。
- **配置未生效**: `config.json` 以只读方式挂载。修改宿主机上的文件后，通常需要重启容器：`docker-compose restart`。
- **消息重复发送**: 请确保 `message_id.json` 已正确挂载。如果该文件丢失，程序会重新发送一段置顶消息。
