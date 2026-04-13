# EmbyRadar Docker 使用指南

本项目现在使用纯 Go 主程序，`cognition` 已内嵌到 `embyradar` 进程内，不再需要单独部署 Node sidecar 容器。

## 1. 环境准备

确保您的系统中已安装：
- Docker
- Docker Compose

建议目录结构：

```text
EmbyRadar/
├── config/
│   └── config.json
├── logs/
├── qdrant_data/
└── docker-compose.yml
```

## 2. 配置要点

`config/config.json` 中至少需要包含以下新字段：

```json
{
  "cognition_enabled": true,
  "cognition_base_url": "http://127.0.0.1:3400",
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
- `cognition_base_url` 现在指向主进程内部自启的 Go cognition 服务
- Docker 和本地运行都统一使用 `http://127.0.0.1:3400`
- 不再需要 `http://cognition:3400`

## 3. 镜像发布

Docker Hub 只需要一个镜像：
- `lfy1680/embyradar:beta`

示例：

```bash
docker build -t lfy1680/embyradar:beta .
docker push lfy1680/embyradar:beta
```

## 4. 启动方式

在部署机上运行：

```bash
docker compose up -d
```

如果你要临时覆盖镜像标签，也可以：

```bash
EMBYRADAR_IMAGE=lfy1680/embyradar:beta docker compose up -d
```

该命令会：
1. 拉取 `embyradar` 主服务镜像
2. 启动 `qdrant`
3. 将配置和日志目录挂载到容器内

## 5. 查看日志

查看全部服务：

```bash
docker compose logs -f
```

只看主程序：

```bash
docker compose logs -f embyradar
```

## 6. 停止与清理

停止服务：

```bash
docker compose down
```

重启主程序：

```bash
docker compose restart embyradar
```

## 7. 常见问题

- 报错 `cognition 服务不可用`
  说明主程序内部的 cognition 自启动失败，优先检查 `cognition_db_path` 是否可写，以及端口 `3400` 是否被容器内其他进程占用。

- 修改了 `config.json` 但没生效
  重启 `embyradar` 即可：

```bash
docker compose restart embyradar
```
