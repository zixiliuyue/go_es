# go_es Docker Demo

一行命令起 ES + 跑示例,无需本地装 Go / ES。

## 使用

```bash
docker compose up --build
```

Compose 会:
1. 拉起 Elasticsearch 8.13.4 (单节点, 512M 堆, 关安全)
2. 等 ES healthcheck 通过后,构建并启动 demo 镜像
3. demo 镜像会再确认一次 ES 可达,然后执行 `examples/main.go` 跑完全部 6 个新模块演示
4. demo 退出后容器 `on-failure` 停止 (不会自动重启,方便查看日志)

## 单独跑 (已有 ES)

如果已有 ES,只跑 demo:

```bash
docker build -t go_es_demo .
docker run --rm -e ES_ADDR=http://host.docker.internal:9200 go_es_demo
```

## 配置

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `ES_ADDR` | `http://es:9200` | ES 地址 |
| `ES_WAIT_TIMEOUT` | `120` | 等待 ES 就绪的秒数 |

## 文件

- `Dockerfile` — 多阶段构建,Alpine 运行时
- `scripts/wait-for-es.sh` — 启动前探活 ES
- `docker-compose.yml` — ES + Demo 一体编排
