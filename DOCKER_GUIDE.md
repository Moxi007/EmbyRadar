# EmbyRadar Docker 使用指南

本项目现在是双进程架构：
- `embyradar`：Go 主程序，负责 Telegram、Emby、TMDB、求片流转
- `cognition`：Node/TS sidecar，负责记忆、人格、线程、工具调用与主动行为

如果只启动 `embyradar`，主程序会在启动时探活 `cognition`，探活失败就直接退出。

## 1. 环境准备

确保您的系统中已安装：
- Docker
- Docker Compose

建议目录结构：

```text
EmbyRadar/
├── config/
│   └── config.json
├── cognition_data/
├── logs/
├── qdrant_data/
└── docker-compose.yml
```

## 2. 配置要点

`config/config.json` 中至少需要包含以下新字段：

```json
{
  "cognition_enabled": true,
  "cognition_base_url": "http://cognition:3400",
  "cognition_db_path": "config/cognition.db",
  "autonomy_enabled": true,
  "autonomy_tick_seconds": 300,
  "quiet_hours": [0, 1, 2, 3, 4, 5, 6],
  "proactive_cooldown_minutes": 720,
  "persona_seed": "记住群内长期关系变化，按群人设稳定说话，优先直接、克制、像常驻群成员。",
  "relationship_decay_days": 14
}
```

关键点：
- 在 Docker Compose 网络里，`cognition_base_url` 必须写成 `http://cognition:3400`
- 不要写 `http://127.0.0.1:3400`
- `127.0.0.1` 在容器里指向当前容器自己，不会指向 `cognition` 服务

## 3. 启动方式

在项目根目录运行：

```bash
docker compose up -d --build
```

该命令会：
1. 构建 `embyradar` 主服务镜像
2. 构建 `cognition` sidecar 镜像
3. 启动 `qdrant`
4. 将 sidecar 的 SQLite 数据持久化到 `./cognition_data`

## 4. 查看日志

查看全部服务：

```bash
docker compose logs -f
```

只看 cognition：

```bash
docker compose logs -f cognition
```

只看主程序：

```bash
docker compose logs -f embyradar
```

## 5. 停止与清理

停止服务：

```bash
docker compose down
```

如果只想重启某个服务：

```bash
docker compose restart cognition
docker compose restart embyradar
```

## 6. 常见问题

- 报错 `connect: connection refused` 到 `127.0.0.1:3400`
  说明你把 `cognition_base_url` 写成了容器内回环地址，或者根本没启动 `cognition` 服务。

- 只拉了 `lfy1680/embyradar:beta` 这种单镜像
  这个项目现在不是单容器架构，必须同时部署 sidecar。仅主程序镜像不足以运行完整 AI 链路。

- 修改了 `config.json` 但没生效
  重启 `embyradar` 即可：

```bash
docker compose restart embyradar
```
