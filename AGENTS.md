# AGENTS.md — go_es 工程约定

本文件面向在本仓库工作的 AI agent(以及任何后续维护者),固化已经过容器化测试验证的工程约定、命令模板、不可触碰的边界。

> 一切涉及"如何测试 / 怎么跑 / 哪些是默认 / 哪些是禁区"的问题,**先看本文件**。

---

## 1. 项目概览

- **语言/版本**: Go 1.25
- **目标**: 自研一个最小可运行的 Elasticsearch 8 兼容服务端(数据用 BadgerDB 持久化),使 `examples/main.go` 与单元测试能脱离真实 ES 完成端到端验证
- **入口**: `cmd/server/main.go`(`go_es_server` 二进制)
- **关键目录**:
  - `internal/server/` — HTTP 服务端(路由、中间件、handlers、metrics、tasks、auth)
  - `internal/search/` — 内嵌倒排查询引擎
  - `internal/storage/` — BadgerDB 持久化封装
  - `pkg/*` — 客户端 SDK + 业务封装(client / index / document / search / bulkio / alias / ilm / template / reindex / ingest / cluster / metrics / pool / aggregate / suggest / errors)
  - `scripts/` — 容器化测试脚本
  - `examples/main.go` — 全功能 demo

---

## 2. 测试方式(强制)

### 2.1 单元测试(快速)
```bash
go test -count=1 ./...
```
注: 仓库内 `pkg/pool` 等个别测试需要真实 ES,本地没起 ES 时会失败,**这是预期行为**,不是我改坏的。

### 2.2 端到端容器化测试(权威)

**默认就使用这个方式**,不要用"我手动 curl 一下"代替:

```bash
bash scripts/test-in-docker.sh
```

它会自动:
1. 创建独立 compose project `go_es_test_<ts>`(与日常 dev 隔离)
2. 拉起 ES 8.13.4(端口 19200) + 自研 `go_es_server`(端口 19201)
3. 等待两边 healthcheck 通过
4. 在 `tester` 容器内跑 `scripts/e2e-tests.sh`,覆盖 6 项新能力 + SDK 客户端冒烟
5. 退出后自动清理容器与 volume(除非传 `-k`)

**常用参数**:
- `-k` / `--keep` — 保留容器,用于手动调试
- `-s` / `--skip-build` — 跳过镜像重建(用现有镜像)

**手动调试流程**:
```bash
bash scripts/test-in-docker.sh -k
# 容器保留, 本机端口:
#   19200 -> ES, 19201 -> 自研 server
# 想看 tester 内部结果:
docker exec <proj>_tester sh /e2e-tests.sh
# 清理:
docker compose -p <proj> -f docker-compose.test.yml down -v
```

### 2.3 测试边界(重要)

容器化测试断言的指标都已在 `scripts/e2e-tests.sh` 中明确,**不要**为了让测试"过"而改:
- `task created >= 5`(reindex 必须真把 5 条文档搬过去)
- `metrics counter >= 1`(每次业务请求必须能查到 `go_es_http_requests_total` 自增)
- `liveness/readiness/startup` 三个端点

**禁止**:
- 把断言条件改宽(如 `>= 5` 改 `>= 0`)
- 用 `|| true` 吞掉失败
- 删除测试用例

---

## 3. 路由与扩展

### 3.1 现有路由(部分)
精确路由在 `internal/server/server.go::buildRouter` 注册,新增路由**必须**也在这里注册。

```
GET    /
GET    /_cluster/health
POST   /_aliases
GET    /_alias/{name}
HEAD   /_alias/{name}
PUT    /_index_template/{name}
GET    /_index_template/{name}
DELETE /_index_template/{name}
POST   /_index_template/_simulate/{name}
PUT    /_component_template/{name}
DELETE /_component_template/{name}
PUT    /_ilm/policy/{name}
GET    /_ilm/policy/{name}
DELETE /_ilm/policy/{name}
PUT    /_ingest/pipeline/{name}
GET    /_ingest/pipeline/{name}
DELETE /_ingest/pipeline/{name}
POST   /_ingest/pipeline/_simulate
POST   /_reindex
POST   /_bulk, PUT /_bulk
POST   /_search
PUT    /_snapshot/{repo}
DELETE /_snapshot/{repo}
PUT    /_snapshot/{repo}/{snap}
GET    /_snapshot/{repo}/{snap}
DELETE /_snapshot/{repo}/{snap}
GET    /_cat/nodes
GET    /_cat/indices
GET    /_health/liveness
GET    /_health/readiness
GET    /_health/startup
GET    /metrics
GET    /_ui
GET    /_ui/index.html
GET    /_tasks
GET    /_tasks/{id}
DELETE /_tasks/{id}
```

### 3.2 中间件链(guards.go::chainMiddleware)

从外到内:
1. `middlewareRecover` — panic → 500
2. `middlewareShutdown` — 关闭期间拒绝新连接(健康端点除外)
3. `middlewareMetrics` — 计数 / 耗时 / inflight,**兼 gzip 压缩**(见 `gzip.go::compressingWriter`)
4. `middlewareRequestID` — 注入 `X-Request-Id`
5. `middlewareAuth` — Basic / ApiKey(健康端点 + `/metrics` + `/_ui` 白名单)
6. `middlewareRateLimit` — IP 令牌桶
7. `middlewareBodyLimit` — `http.MaxBytesReader`
8. router 业务分发

**新加中间件**请按职责选最合适的位置;切勿在 router 里写权限/限速。

### 3.3 路由模板与 metrics

router 会把"路由模板"(如 `/{index}/_doc/{id}`)注入到 `r.Context()`,key 为 `ctxKeyRoute`。`middlewareMetrics` 用它做 Prometheus 打标,**禁止**直接把 `r.URL.Path` 当 label,会撑爆 Prometheus。

### 3.4 异步任务

异步任务走 `globalTaskManager.Submit(action, runner)`,runner 必须:
- 周期性 `select` 检查 `<-e.cancel`,被取消时设置 `e.info.Status = TaskStatusCancelled` 并 return
- 结束时设置 `e.info.Status` 为 `TaskStatusCompleted/Failed/Cancelled`
- 进度字段写到 `e.info.Progress.{Total,Created,Updated,Deleted,Failures,Batches}`

**禁止**:在 runner 里调 `panic`、阻塞 IO 超时不设上限、忽略 cancel 信号。

---

## 4. 服务端可调参数

启动参数(全部可选,都有合理默认):

| Flag | 默认 | 说明 |
|---|---|---|
| `-addr` | `:9200` | HTTP 监听地址 |
| `-data` | `./data` | BadgerDB 数据目录(空 = 内存) |
| `-auth.user` | (空) | 启用 Basic 认证的用户名 |
| `-auth.password` | (空) | Basic 认证密码 |
| `apikey=<token>` | (无) | 任意次传,启用 API Key 认证(从 `flag.Args()` 解析) |
| `-max-body` | `100` | 单请求体最大 MiB |
| `-rate` | `0`(不限) | 单 IP 每秒允许的请求数 |
| `-config` | (空) | YAML 配置文件路径;支持 mtime 热更新(默认 5s 轮询) |

**生产部署前必开**:`-auth.user` + `-auth.password`(或 `apikey=...`)。

### 4.1 配置文件格式
```yaml
addr: ":9200"               # 启动时生效, 修改需重启
data: "./data"              # 启动时生效, 修改需重启
auth:
  enabled: true             # 改 true/false 后热更新生效
  basic:
    admin: "secret"
  api_keys:
    - "token1"
limit:
  max_body_bytes: 104857600 # 100 MiB
  rate_per_second: 1000
  burst: 1000
log_level: "info"
watch_interval: 5s          # 轮询间隔(go duration)
```

---

## 5. 与客户端 SDK 的兼容约定

`go-elasticsearch/v8` 客户端在建立连接时会做"产品嗅探",要求响应带 `X-Elastic-Product: Elasticsearch` 头。
**这个头由 router 在 `ServeHTTP` 第一行设置**,业务 handler 不要覆盖它。

---

## 6. 容器化文件清单

| 文件 | 角色 |
|---|---|
| `Dockerfile` | demo 镜像(跑 `examples/main.go` 演示) |
| `Dockerfile.server` | **自研服务端镜像**(跑 `cmd/server`) |
| `docker-compose.yml` | demo compose: ES + demo 容器 |
| `docker-compose.test.yml` | **测试 compose**: ES + 自研 server + tester |
| `scripts/wait-for-es.sh` | demo 用的 ES 探活 |
| `scripts/test-in-docker.sh` | **测试入口**,自动拉起 + 清理 |
| `scripts/e2e-tests.sh` | tester 容器内跑的端到端断言 |

**禁止**:在测试容器内手动改 data volume;测试脚本必须幂等,索引名带时间戳后缀。

---

## 7. 实施准则(给 AI 代理)

1. **先看,后改** — 改任何文件前先 `Read`,不要凭印象编辑
2. **小步走** — 单次 commit/PR 围绕一个能力;不要"顺手"重构
3. **测试驱动** — 任何新能力必须同时:
   - Go 单元测试在 `internal/server/extensions_test.go` 加 case
   - e2e 断言在 `scripts/e2e-tests.sh` 加 case
4. **不破坏向后兼容** — `New(...)` 必须保持原签名,新能力走 `NewWithOptions(...)`
5. **不要造轮子** — BadgerDB / zap / prometheus client / golang.org/x/time/rate 已有依赖直接用,不要引入新库
6. **代码自包含** — 函数/文件职责清晰,减少跨文件耦合
7. **禁止覆盖锁** — 任何新加锁请说明: 临界区 / 是否可重入 / 顺序
8. **不要提交大文件** — 容器数据目录(`/data`、`/tmp/go_es_data*`)必须在 `.gitignore`,不进入 git

---

## 8. 已知约束与未来工作

### 实现记录
- 2026-08-05: 完成第一轮扩展(6 项 P0/P1 能力 — metrics/tasks/auth/health/shutdown/limits)
- 2026-08-05: 完成第二轮扩展:
  - 跨索引通配模式(建议 #11) — `POST /idx*/_search`、`POST /a,b,-c*/_search` 等
  - HTTP/2 (h2c) — 通过 `golang.org/x/net/http2/h2c`,无需 TLS
  - gzip 协商头(建议 #9 Vary 部分) — 中间件只加 `Vary: Accept-Encoding`
  - 内置 Web UI(建议 #13) — `GET /_ui`,纯 HTML+JS,`go:embed` 嵌入
- 2026-08-05: 完成第三轮扩展(完成 AGENTS.md 全部 P2/P3 优先级):
  - **gzip 实际压缩**(建议 #9 全文) — 合并 statusWriter 与 gzipWriter 为 `compressingWriter`,在 metrics 中间件层统一处理;只对 application/json 响应且 ≥512B 启动压缩;4xx 跳过;`/metrics` 跳过
  - **range 查询倒排加速** — 新增 `internal/search/sorted_index.go`,对每个 (index, field) 维护内存排序倒排;IndexDoc/DeleteDoc 增量更新;evalRange 走二分定位 O(logN+K);复杂类型自动回退到全表扫描
  - **bulk 写合并** — `internal/server/bulk_batch.go` 提供 `bulkWriter` 封装 `badger.WriteBatch`;`internal/server/bulk.go` 改为先累积再 flush
  - **Web UI 增强** — `/_ui` 新增分页 (←/→)、聚合面板(terms aggregation 表格 + 进度条)、query type 选择(match/term/range/match_all)、从分筛选 JSON 表达式
  - **配置热加载**(建议 #15) — `internal/server/config.go` + `gopkg.in/yaml.v3`;`ConfigLoader` mtime 轮询(默认 5s);`cmd/server/main.go` 加 `-config` 启动参数;只对 auth/limit 生效,addr/data 需重启

### 已实现完整覆盖
- 倒排索引查询引擎(range 已倒排化, term/match 原本就是倒排)
- HTTP/2 + gzip
- 跨索引通配模式
- 内置 Web UI(含分页/聚合)
- 配置加载 + 热更新(只读侧, addr/data 启动后不可变)
- 写合并(bulk 路径,单 doc 写仍走原路径)

### 待办(更长期)
- 增量 reindex 进度(目前 batch 计数每 500 跳一次, 改为 100)
- Web UI: 多索引 Tab 切换 / 历史查询 / 字段类型推断
- 配置文件 schema 校验(目前 yaml 解析失败会启动报错)
- TLS 支持(HTTP/2 over h2, 当前仅 h2c)

实施前请先在本节加"实现记录"段,说明日期与作者。
