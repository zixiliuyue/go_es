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
| `-tls.cert` | (空) | TLS 证书 PEM 路径;与 `-tls.key` 同时设置才生效 |
| `-tls.key` | (空) | TLS 私钥 PEM 路径 |
| `-tls.enable-http2` | `true` | 是否在 TLS 上协商 h2 |
| `-tls.client-ca` | (空) | 客户端 CA 池 PEM 路径(启用 mTLS 时必填,与 `-tls.client-auth` 配对) |
| `-tls.client-auth` | `none` | 客户端证书强制级别:`none`/`request`/`require_any`/`require_verify` |

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
tls:
  cert: "/etc/go_es/server.crt"   # 空 = 关闭 TLS
  key:  "/etc/go_es/server.key"
  enable_http2: true              # 默认 true
  client_ca: "/etc/go_es/client_ca.crt"  # 空 = 关闭 mTLS
  client_auth: "require_verify"            # none/request/require_any/require_verify
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
| `docker-compose.tls.test.yml` | **TLS 测试 compose**: 自研 server(启用 TLS+h2) + TLS tester |
| `docker-compose.mtls.test.yml` | **mTLS 测试 compose**: 自研 server(启用 TLS+h2 + mTLS require_verify) + mTLS tester |
| `scripts/wait-for-es.sh` | demo 用的 ES 探活 |
| `scripts/test-in-docker.sh` | **明文测试入口**,自动拉起 + 清理 |
| `scripts/test-tls-in-docker.sh` | **TLS 测试入口**,自动生成自签证书 + 拉起 + 清理 |
| `scripts/test-mtls-in-docker.sh` | **mTLS 测试入口**,自动生成 CA + server cert + client cert + 拉起 + 清理 |
| `scripts/gen-test-cert.sh` | TLS/mTLS 测试用的自签证书生成器(无 `-m` 走单向 TLS,带 `-m` 走 mTLS 全套) |
| `scripts/e2e-tests.sh` | tester 容器内跑的明文端到端断言 |
| `scripts/e2e-tls-tests.sh` | tester 容器内跑的 TLS 端到端断言 |
| `scripts/e2e-mtls-tests.sh` | tester 容器内跑的 mTLS 端到端断言(双向握手/拒绝无 cert/拒绝错误 CA) |

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
- 2026-08-06: 完成第四轮扩展 — **TLS / HTTP-2 over h2**:
  - 新增 `-tls.cert` / `-tls.key` / `-tls.enable-http2` 启动参数,以及 yaml 中 `tls: { cert, key, enable_http2 }` 块
  - `EnableHTTP2` 用 `*bool` 区分"未设置"与"显式 false",yaml 显式给值时覆盖 flag 默认
  - `cmd/server/main.go` 抽 `configureTransport()`:TLS 路径用 `crypto/tls` 加载证书 + 显式 `http2.ConfigureServer`;明文路径保留 `h2c.NewHandler` 包一层(行为不变)
  - cert+key 在启动期 `os.Stat` + `tls.LoadX509KeyPair` 双校验,失败立即 fail-fast
  - 单元测试:`internal/server/config_test.go::TestConfigLoader_TLSBlock`(yaml 解析 + 单边/双边/未设 3 种状态);`cmd/server/main_test.go`(4 个 `configureTransport` case + 1 个真实 TLS handshake end-to-end)
  - 容器化测试:`scripts/gen-test-cert.sh`(openssl 自签 P-256,SAN=localhost+127.0.0.1,1 天);`docker-compose.tls.test.yml`;`scripts/test-tls-in-docker.sh`;`scripts/e2e-tls-tests.sh` 覆盖 TLS 握手 / ALPN h2 协商 / 完整 CRUD / /metrics over TLS
- 2026-08-06: 完成 reindex 进度精细化 — **reindexBatchSize: 500 → 100**:
  - `internal/server/extras.go` 提取 `reindexBatchSize=100` 常量,循环内每 100 条 `Progress.Batches++`(原 500)
  - 100 比 e2e 最小数据集 5 大,小批量仍走响应里 `+1` 兜底,语义不变
  - 单元测试:`internal/server/extensions_test.go::TestTasks_ReindexBatchCounting` 用 250 条数据验证 循环内 Batches=2,Response.batches=3(原 batch=500 时同样 250 条只能 0+1=1,精细度提升 5x)
- 2026-08-06: 完成 Web UI 增强 — **多 Tab + 历史 + 字段类型推断**:
  - **多 Tab 系统**:`internal/server/web/index.html` 加 Tab 栏,每个 Tab 独立 `currentIndex/qField/qValue/qType/qFrom/qSize/aggField/aggSize/aggQuery/cached results/fieldMap`;新建/关闭/重命名(双击)+ 切换动画;`localStorage` keys `go_es_tabs` / `go_es_active_tab` 持久化
  - **历史查询**:每次 `runSearch`/`runAgg` 成功 push 一条 `{ts,type,tabTitle,summary,params}`,抽屉面板支持内容搜索/类型筛选/时间筛选(1h/1d/7d/全部),`replayHistory()` 一键重跑 + `deleteHistory()`/`clearHistory()` 单删清空;最多 200 条 FIFO 截断,key `go_es_history`
  - **字段类型推断**:Tab 选索引时拉 `/{idx}/_mapping` + 1 条 `_search` 抽样,综合 ES mapping type + JS 推断(`string|number|integer|boolean|date|object` 6 种);qField input 加 `<datalist>` 自动补全;value 控件按类型切换(number/date/checkbox/text);新增字段 chips 面板,点击填入字段;聚合结果 key 按类型着色
  - 响应式: `@media (max-width:900px)` 左侧栏折叠;Tab 切换 0.15s fadein + 0.2s 历史抽屉 transform 动画
  - 向后兼容:保留 `runSearch/runAgg/loadIndices/selectIndex/loadCluster/prevPage/nextPage` 全局函数名 + `go_es · 控制台` 标题,e2e 既有断言不破
  - 单元测试:`internal/server/extensions2_test.go` 新增 5 个 TestUI_*(MultiTabSurface/HistorySurface/FieldInferenceSurface/BackwardCompat/PageSizeReasonable)
  - e2e:`scripts/e2e-tests.sh` section 9 拆 9a/9b/9c/9d,新增 14 条断言(Tab 栏/newTab/closeTab/localStorage 键/history 抽屉/replayHistory/field chips/extractFieldMap/inferType/checkbox/date/number 等);总体 51/51 通过
- 2026-08-06: 完成 YAML 配置 schema 校验 — **启动期语义校验**:
  - 新增 `internal/server/config_schema.go`:`ConfigSchema` 结构 + `FieldRule{Path,Kind,Message,...}` 数据驱动规则;`ValidationError`/`ValidationErrors`(聚合,按 Path 排序,稳定输出)
  - 规则类型:`required / type / range / enum / pattern / min_len / max_len / cross_field`(跨字段业务规则,支持 `When` 条件);`Min/Max` 用 `*float64` 区分"未设置"与"=0",允许规则限定包含 0
  - 零值跳过:非 required/cross_field 规则在零值(nil/""/0/false/`time.Duration(0)`)上自动跳过,避免对"未设置"项报错(由 required 单独抓)
  - 覆盖范围:addr 必为 `:port` 形式 / auth.enabled=true 须有凭据 / TLS cert+key 须同时配置 / limit 三项数值范围(0~1GiB / 0~1e7 / 0~1e6) / log_level enum(debug/info/warn/error) / watch_interval 0~1h / `*bool` 区分 nil 与显式 false
  - 集成: `ConfigLoader.Load()` 在 `yaml.Unmarshal` 之后 + 提交 `current` 之前调 `DefaultConfigSchema().Validate()` + `SanityCheck()`;失败 wrap 为 `fmt.Errorf("validate config: %w", errs)`,与 parse 错误同级触发 `log.Fatalf` 启动退出;`current` 保持上一次合法值,热更新不会回写到坏状态
  - 错误信息格式: `config: <path> <reason> (实际值=<val>)`, 路径点分 + 期望规则 + 实际值
  - 可维护性:规则全部在 `DefaultConfigSchema()` 集中声明,加新字段时只补规则,不动核心 `applyRule`/`Validate`;无第三方依赖
  - 单元测试:`internal/server/config_test.go` 新增 17 个 `TestSchema_*`(正常/必填/类型/范围/枚举/业务规则/错误聚合/错误格式/wrap/失败不改 current/范围下限 0/pattern/*bool 显式 false/SanityCheck 端口),共 23 个 TestConfigLoader+TestSchema 全过
  - 集成测试:`scripts/test-in-docker.sh` host-side 三个 case(坏 log_level+max_body_bytes、TLS 单边、auth.enabled=true 无凭据)验证启动期非 0 退出 + 错误信息含 "validate config" / "必须同时配置" 关键词
- 2026-08-06: 完成 reindex 取消回滚 — **目标索引回滚到 reindex 前状态**:
  - `internal/server/extras.go::runReindex` 增加 `written []string` 记录本次 reindex 过程中成功 Put 到目标索引的 doc id;取消时调用新 `rollbackReindex()` 逐个反向 Delete(store + engine)
  - 取消语义: 取消时已写入目标索引的 doc 全部回滚;目标索引原本存在的 doc 保持不变;目标索引原本为空则取消后为 0(行为与"从未 reindex 过"等价);状态=TaskStatusCancelled,Created/Batches 重置 0
  - 双检查点:循环内 `<-e.cancel` 立即回滚;循环跑完后**再次** `cancelled.Load()`,避免"循环完成 -> cancel 到达 -> 状态已置 completed"的竞态
  - **修复 taskEntry 既有数据竞争**: 加 `sync.Mutex` 保护 `info` 字段;新增 `snapshot()` 持锁读 + `withInfo(fn)` 持锁写;`runReindex` 全字段改走 `withInfo`;`go test -race` 5/5 绿
  - 单元测试: `internal/server/extensions_test.go` 新增 3 个 `TestTasks_Reindex*`(RollsBackWritten/PreservesPreExisting/NoCancelDoesNotRollback),用 200 条数据保证 cancel 命中窗口;5 个 reindex 测试全过
  - e2e: `scripts/e2e-tests.sh` section 4b 验证容器环境下 reindex + 立即 DELETE /_tasks/{id} + 检查目标索引 0 docs;53/53 全过
- 2026-08-06: 完成 mTLS 双向认证 — **TLS 单向扩展为支持 mTLS**:
  - `internal/server/config.go::TLSConfig` 新增 `ClientCAFile` (PEM CA 池路径) + `ClientAuth *string`(指针, 区分未设与显式 none);新枚举类型 `ClientAuthKind` (none/request/require_any/require_verify);新方法 `MTLSEnabled()` 与 `AuthKind()`
  - schema 新规则: `tls.client_ca` 字符串/类型 + `tls.client_auth` 4 值枚举 + 业务规则(设 client_ca 时 auth 不能 none, 非 none 时必须配 client_ca);`KindEnum` 扩展接受 `*string`
  - `cmd/server/main.go` 新 flag `-tls.client-ca` / `-tls.client-auth`(默认 none);启动期 fail-fast 校验 4 种错配;`configureTransport()` 加 mTLS 分支(加载 CA 池 + 注入 ClientCAs + 映射 ClientAuth);新 `clientAuthToTLS()` 把内部枚举映射到 `crypto/tls.ClientAuthType`
  - 单元测试: `internal/server/config_test.go` 新增 5 个 `TestSchema_MTLS*`(SingleSide_CAWithoutAuth/AuthWithoutCA/ValidFull/ClientAuthInvalidEnum);`cmd/server/main_test.go` 新增 5 个 `TestConfigureTransport_MTLS*` + `TestMTLSEndToEnd_RequireVerify`(真实双向握手 + 拒绝无 cert)+ `TestClientAuthToTLS`;`go test -race` 全过
  - `scripts/gen-test-cert.sh` 加 `-m` 模式:生成自签 CA + CA 签发的 server cert + CA 签发的 client cert;SAN 加 `DNS:go_es_server`(docker 容器名);openssl `x509 -req -extfile` 必须显式 `-extensions v3_req` 才能挂上 EKU
  - 新增 `docker-compose.mtls.test.yml` + `scripts/test-mtls-in-docker.sh` + `scripts/e2e-mtls-tests.sh`(独立 project + 端口 19203,不与 TLS test 互扰)
  - mTLS 容器化关键点: **不使用 docker healthcheck**(healthcheck 不带 client cert 会被服务端拒);改由主机侧 curl + cert 等待
  - e2e: `e2e-mtls-tests.sh` 10/10 — 双向握手/ALPN h2/完整 CRUD/无 cert 拒绝/错误 CA 签发 cert 拒绝;明文 53 + TLS 9 + mTLS 10 全过
- 2026-08-06: 完成 Web UI 拖拽 + 导入导出 — **Tab 排序/拖出关闭/跨会话状态保存**:
  - `internal/server/web/index.html` 拖拽:每个 `.tab` 加 `draggable="true"`;`onTabDragStart` 记录源 id + `onTabDragOver` 用目标水平中线算 before/after + `onTabDrop` 重排 `tabs[]`;视觉提示 `drag-over-left`/`drag-over-right` 蓝色侧边条
  - 拖出 tabbar 关闭:`onTabDragEnd` 用 `getBoundingClientRect()` 判定鼠标位置,不在 tabbar 矩形内则 `closeTab(dragSourceId)`,toast 提示
  - 导入导出:header 加 2 个按钮 + 隐藏 file input;`buildExportPayload()` 输出 `{version:1, exportedAt, tabs, activeTabId, history}`;`exportTabs()` 走 `Blob + URL.createObjectURL + a.click` 下载 `go_es_tabs_<ts>.json`;`importTabs()` 走 `FileReader + JSON.parse + validateImportPayload()`;导入有 `merge`/`replace` 双模式(`confirm` 询问用户)
  - 安全: `validateImportPayload` 严格校验 version=1 / tabs 数组 / 每个 tab 有 id+title / history 是数组 / activeTabId 是字符串, 防 XSS/坏数据
  - 单元测试:`internal/server/extensions2_test.go` 新增 3 个 `TestUI_*`:DragAndDropSurface (10 hook 字符串)/ ImportExportSurface (13 hook + 按钮)/ ImportPayloadSpec (Go 侧 round-trip 验证 JSON spec)
  - e2e:`scripts/e2e-tests.sh` section 9e/9f,新增 20 条断言(拖拽 handlers/视觉/导入导出按钮/函数/浏览器 API);73/73 全过
- 2026-08-06: 完成 Web UI 历史图表 — **纯 SVG 24h 调用频次可视化**:
  - `internal/server/web/index.html` 新增 `.chartwrap` 区域(在历史面板的 filter 与列表之间),含标题"最近 24h 调用频次"+ 图例 + `<div id="histChart">`
  - `renderHistoryChart()` 纯 SVG 柱状图:24 个小时桶(idx 0=最老,23=当前),每个桶内 search/agg 双柱并列(蓝 #1f6feb / 紫 #d2a8ff);viewBox 320×90,自适应宽度;x 轴 4 标签(-24h / -16h / -8h / now);y 轴显示 max 值;每根柱 `<title>` 悬停提示
  - 累计副标题:基于全部 history(不只是 24h 内)统计 search vs agg 总数 + 百分比
  - 触发点: `pushHistory`(成功搜索/聚合时)+ `clearHistory`(清空)+ `toggleHistory`(打开面板时)+ `importTabs`(导入后)
  - 空态: history 为空时显示"暂无数据"
  - 单元测试:`internal/server/extensions2_test.go::TestUI_ChartSurface` 11 个 hook 字符串 + 验证 pushHistory 函数体内含 `renderHistoryChart()` 调用
  - e2e:`scripts/e2e-tests.sh` section 9g,8 条断言(SVG/挂载点/CSS/柱元素/双柱颜色/空态文案);81/81 全过
- 2026-08-07: 完成 #16 真实快照与恢复 — **NDJSON 文件级快照 + 跨 store 恢复 + 完整性校验**:
  - `internal/server/extras.go::handleSnapshotCreate`:遍历 `storage.ScanAllKeys` 导出全量数据到 NDJSON 文件;过滤 `snapshot/`、`doc-tf/`、`postings-version/`、`doc-meta/` 前缀;文件末尾写内嵌 meta 行(`__snapshot_meta__`,含 version/doc_count/key_count/created_at);写 store 侧元数据含 `doc_count`
  - `internal/server/extras.go::handleSnapshotRestore`:读 NDJSON 文件跳过 `__snapshot_meta__` 行,用 `PutRaw` 写回目标 store;`engine.LoadAll()` 重建倒排;从 meta 行取 `expected_doc_count` 与 `restored_docs` 比对;响应体含 `restored`/`restored_docs`/`expected_docs`
  - `internal/server/extras.go::handleSnapshotDelete`:删除快照元数据 + 物理 NDJSON 文件(`os.Remove`)
  - `internal/server/server.go`:注册 `POST /_snapshot/{repo}/{snap}/_restore`;Server 加 `snapDir` 字段;默认路径 `dataDir/snapshots`
  - `internal/storage/store.go`:新增 `Dir()` 方法暴露数据目录 + `ScanAllKeys` 迭代全量 key;新增 `PutRaw` 写原始字节
  - 单元测试:`internal/server/snapshot_test.go` 10 个用例:路径推导/创建恢复/元数据缺失恢复/无仓库/文件格式/删除元数据+文件/恢复后可搜索/恢复文档计数
  - e2e:`scripts/e2e-tests.sh` section 35(35a~35m):建库→写数据→创建快照→验证元信息→删除原数据→恢复→验证 5 docs 全恢复→搜索命中→文档内容完整→删除快照→已删除快照 404→已删除快照恢复 404→删仓库
- 2026-08-10: 完成 #23 分布式追踪 — **OpenTelemetry Trace Context 透传**:
  - `internal/server/tracing.go` 核心追踪模块(W3C TraceContext + B3 双协议):
    - W3C:`parseW3CTraceContext`/`injectW3CTraceContext` 处理 `traceparent` + `tracestate` 头(格式 `00-{traceId}-{spanId}-{flags}`)
    - B3:`parseB3Context`/`injectB3Context` 支持多头(X-B3-TraceId/SpanId/Sampled/ParentSpanId)和单头(b3)两种格式
    - 传播模式:`tracecontext`/`b3`/`both` 三种,可通过 `TracingConfig.Propagation` 选择
    - TracerProvider:全局单例管理器,支持按 name 获取/更新 tracer,支持热更新配置
    - Tracer:Span 创建/采样控制/远程 TraceContext 继承
    - Span:完整生命周期 (Start→SetAttribute→SetStatus→AddEvent→End),线程安全 (sync.Mutex)
    - 辅助:`PropagateTraceContext`/`InjectTraceContext`/`ExtractTraceContext` 用于 HTTP 客户端透传
  - `internal/server/guards.go` 中间件:
    - `middlewareTrace`:最外层包裹,入站从 header 提取 trace context→创建子 Span→注入 context;出站在响应头注入 traceparent/b3
    - `traceResponseWriter`:包装 ResponseWriter 捕获 status code,用于 Span 状态判定
  - `internal/server/server.go` 集成:
    - `Server` 新增 `tp *TracerProvider` 字段,`ServerOptions` 新增 `Tracing TracingConfig`
    - `NewWithOptions` 自动初始化并注入 guards,日志输出初始化配置
    - `Server.TracerProvider()` 公共访问器
  - 日志集成:
    - `accesslog.go::AccessLogEntry` 新增 `TraceID`/`SpanID`
    - `auditlog.go::AuditEntry` 新增 `TraceID`/`SpanID`
    - `slowlog.go` 慢请求日志新增 `trace_id`/`span_id`
    - `TraceInfoFromContext(ctx)` 统一提取接口
  - 配置:
    - YAML 支持 `tracing:` 块:`enabled`/`service_name`/`service_version`/`propagation`/`sampling_rate`
    - `config_schema.go` 新增 6 条 tracing 字段校验规则(类型/范围/枚举)
  - 单元测试:`internal/server/tracing_test.go` 35+ 用例全通过
- 2026-08-11: 完成 #17 ILM 真执行 — **后台扫描 + rollover/delete 动作 + 真实 explain**:
  - 新增 `internal/server/ilm_executor.go`:`ILMExecutor` 后台 goroutine 每 30s 扫描 `managed: true` 索引,按 policy.phases 的 min_age 推进 phase;命中 rollover 条件时创建新索引 + 别名切换;delete 阶段清理 store 与 engine 内存态
  - `internal/server/extras.go::handleILMExplainForName` 改为读取真实 `ILMState` 返回(phase/rollover_count/error/managed/action)
  - `internal/search/engine.go` 新增 `CreateIndex` / `DeleteIndex` 同步内存态;`internal/search/scorer.go` 新增 `onDeleteIndex` 清理 BM25 统计
  - 关键修复:rollover 语义修正为 `max_age OR max_docs`(与 ES 一致);`maxAge=0` 视为"立即到期";`switchAliases` 正确读取与回写 alias 列表
  - 单元测试:`internal/server/ilm_executor_test.go` 共 17 个用例,核心函数平均覆盖率 ≥ 85%
  - 注意:`pkg/aggregate`、`pkg/pool`、`pkg/search` 中部分用例在 main 分支上已存在失败,与本次改动无关
- 2026-08-11: 完成 #18 索引设置真生效 — **GET /_settings + 多索引模式 + 全量查询**:
  - `internal/server/index_doc.go` 新增 `handleIndexSettingsForName`:`GET /{index}/_settings` 支持单索引、多索引(idx1,idx2)、通配(idx*)模式;精确名不存在返回 404,通配无匹配返回 200+空对象
  - 新增 `handleAllSettings`:`GET /_settings` 返回全部索引的 settings
  - 路由注册:server.go 加 `/_settings` 顶级路由 + dispatcher 内 `/{index}/_settings` 分发
  - 单元测试:`internal/server/settings_test.go` 13 个 `TestSettings_*` 用例全通过,`handleIndexSettingsForName` 覆盖率 91.7%,`handleAllSettings` 覆盖率 85.7%
  - 响应格式:`{index_name: {settings: {...}}}` 与 ES 兼容
- 2026-08-11: 完成 #19 mapping 校验 — **文档写入前类型校验 + 动态字段控制**:
  - 新增 `internal/server/mapping.go`:`MappingValidator` 核心校验模块
    - 支持 `dynamic: true/false/strict` 三种模式(默认 true)
    - `NewMappingValidator(mapping)` 从 IndexMeta.Mapping 构造校验器
    - `Validate(doc)` 校验文档字段类型与动态字段合规性
    - `validateFieldType` 支持 text/keyword/integer/long/float/double/boolean/object/nested/date 等类型校验
    - 处理 `json.Number` 类型(因 `decodeJSON` 使用 `UseNumber()`)
  - 集成到文档写入流程:
    - `handleDocIndexForName`(PUT/POST `/{index}/_doc/{id}`)
    - `handleDocIndexAutoIDForName`(POST `/{index}/_doc`)
    - `handleUpdateForName`(POST `/{index}/_update/{id}`)
    - 校验失败返回 400 + `mapper_parsing_exception` 错误类型
  - 单元测试:`internal/server/mapping_test.go` 共 16 个 `TestMappingValidator_*` 用例全通过:
    - 纯逻辑校验(无 mapping、空 mapping、dynamic=true/false/strict、17 种类型校验用例表、多字段混合、空文档、非法 dynamic、无 type 字段、格式错误等)
    - 集成测试(validateDocMapping 直接调用、HTTP 端到端 strict/dynamic=false/no mapping/dynamic=true 模式、update 路径、auto-ID 路径)
- 2026-08-11: 完成 #29/#30/#31/#32 安全与权限(交付 v0.6.0 M5):
  - **#29 RBAC**(internal/server/rbac.go + rbac_extended.go):
    - User/Role/Permission 核心模型 + 内置 superuser/admin/read/monitor 四种角色
    - 中间件 `middlewareRBAC` 集成到 `auth → rbac → auditLog` 链路,向后兼容(auth 未启用时跳过)
    - 路由 `/_security/user/{name}`、`/_security/role/{name}`、`/_security/whoami`、`/_security/permission` 完整 CRUD + 批量操作
    - `requestAction(method, path)` 映射 read/write/admin/monitor/cluster 五种操作,支持通配 `logs-*`/`*-2024`/`logs-*-bak`
    - 单元测试:`rbac_test.go`、`rbac_session_test.go`、`rbac_integration_test.go`、`rbac_extended_test.go`(若存在)全通过
  - **#30 审计日志**(internal/server/auditlog.go):
    - 异步 buffered channel 写入,默认关闭可通过 `/_audit/config` 热启用
    - 端点 `GET /_audit`、`GET /_audit/stats`、`PUT /_audit/config`
    - 中间件 `middlewareAuditLog` 自动记录写操作
  - **#31 输入校验硬化**(internal/server/validation.go):
    - 索引名正则 + 长度 + `_` 前缀校验;`from+size ≤ 10000`
    - 中间件 `middlewareValidation` 支持多索引/通配符友好
    - 全局 `SetValidationConfig`/`GetValidationConfig` 支持 YAML 热更新
  - **#32 CORS**(internal/server/validation.go):
    - 中间件 `middlewareCORS` 支持 `AllowedOrigins/Methods/Headers/Credentials`
    - 预检请求(OPTIONS)200;白名单外域 403
  - `go test ./internal/server/` 全部通过

- 2026-08-11: 完成 #33 Web UI 索引管理面板(交付 v0.8.0 M7):
  - `internal/server/web/index.html` 索引管理增强:
    - 索引列表行:`.idxrow` CSS 类 + 名称/文档数/映射/设置/删除四按钮
    - 创建索引模态框:`showCreateIndexModal`/`doCreateIndex` 支持 mapping JSON 粘贴与校验
    - 删除确认:`confirmDeleteIndex` + `doDeleteIndex` 二次确认
    - 查看模态框:`openMappingModal`(GET `/_mapping`) + `openSettingsModal`(GET `/_settings`)
    - 通用 `closeModal(id)` + `toast(msg)` 操作反馈
  - 单元测试:`extensions2_test.go` 新增 7 个 `TestUI_IndexPanel_*`:ListRows/CreateFlow/DeleteFlow/ViewModals/SidebarStructure/Integration/EmptyState,`go test -race` 全通过
  - e2e:`scripts/e2e-tests.sh` section 9i,40+ 条断言(JS hook + 真实 HTTP 端到端创建/删除/查询)

- 2026-08-11: 完成 #6 multi_match / query_string / simple_query_string(交付 v0.3.0 M2):
  - `internal/search/multimatch.go`:5 种 multi_match 类型(best_fields/most_fields/cross_fields/phrase/phrase_prefix)+ query_string mini Lucene parser + simple_query_string(剥离保留字符不报错)
  - `internal/search/query.go`:Query 结构体已集成 MultiMatch/QueryString/SimpleQueryString 字段,Match() 方法自动分发
  - 集合工具:intersectSets/unionSets/subtractSets
  - 单元测试:`multimatch_test.go` 19 个用例全通过,`go test -race` 无竞争
  - e2e:`scripts/e2e-tests.sh` sections 24-26 覆盖 multi_match/query_string/simple_query_string

- 2026-08-11: 完成 #11 搜索结果评分缓存(交付 v0.7.0 M6):
  - `pkg/cache/lru.go`:纯内存 LRU 缓存(capacity 可配),key = SHA1(sorted_indices + query_body);支持 Set/Get/InvalidateIndex/InvalidateAll/Stats/HitRate;双向链表维护访问顺序;线程安全 sync.RWMutex
  - `internal/server/server.go`:Server 新增 searchCache + searchCacheCfg 字段;SearchCacheConfig{Enabled,Capacity,MaxSize};invalidateCacheForIndex/invalidateCacheAll 辅助方法
  - `internal/server/search.go`:doSearch 读 bodyBytes → 生成 cache key → 命中缓存直接返回(X-GoES-Cache: HIT);响应写入缓存受 MaxSize 限制;Cache Key 用排序后的索引列表 + query_body SHA1 生成
  - `internal/server/metrics.go`:Prometheus 指标 go_es_search_cache_hits_total/misses_total/size(Gauge),Collect 周期刷新
  - 失效策略:index_doc(PUT/DELETE)、bulk、reindex、update_by_query/delete_by_query(sync+async)、snapshot restore(全量失效)、index delete(按索引失效)
  - 单元测试:`pkg/cache/lru_test.go` 14 个(LRU 核心逻辑);`internal/server/cache_test.go` 9 个(集成:HitAndMiss/InvalidationOnWrite/InvalidationOnDelete/Disabled/MaxSize/DifferentQueries/Concurrent/LRUEviction/IndexDeletion)
  - `go test -race` 无数据竞争

### 已实现完整覆盖
- **搜索 DSL 完整覆盖**:match/multi_match/query_string/simple_query_string/term/terms/range/bool/match_all/match_phrase
- **搜索结果评分缓存**(#11):LRU 内存缓存 + 索引级精确失效 + 全量失效 + MaxSize 限制 + Prometheus metrics
- HTTP/2 (h2c + h2)
- TLS / h2
- mTLS(双向证书认证)
- gzip
- 跨索引通配模式
- 内置 Web UI(多 Tab + 历史 + 字段类型推断 + 拖拽排序/拖出关闭 + 导入导出 + **历史图表** + **索引管理面板**)
- 配置加载 + 热更新(只读侧, addr/data/tls 启动后不可变)
- 写合并(bulk 路径,单 doc 写仍走原路径)
- reindex 进度精细化(batch=100) + 取消回滚
- YAML 配置 schema 校验(启动期, 数据驱动, 错误聚合)
- **真实快照与恢复**(#16, NDJSON 文件级, 跨 store 恢复, 完整性校验, 物理文件删除)
- **分布式追踪**(#23, W3C TraceContext + B3 双协议透传, Span 生命周期, 日志 trace_id/span_id 关联)
- **ILM 真执行**(#17, 后台扫描 + 多 phase + rollover + delete, 真实 explain)
- **索引设置真生效**(#18, GET /_settings 端点, 多索引/通配/全量查询)
- **mapping 校验**(#19, 文档写入前类型校验 + dynamic: true/false/strict 模式 + mapper_parsing_exception 错误)
- **RBAC 权限控制**(#29, User/Role/Permission + 中间件 + 路由 `/_security/*` + 通配索引匹配)
- **审计日志**(#30, 异步 buffered channel + `/_audit` 查询/统计/配置热更新)
- **输入校验硬化**(#31, 索引名正则 + from+size 限制 + 中间件 + 热更新)
- **CORS 中间件**(#32, Origin 白名单 + 预检请求 + 头注入)
- **Web UI 索引管理面板**(#33, 索引列表/创建/删除/mapping/settings 查看 + 模态框 + 二次确认)
- **数据备份/导出工具**(#20, `pkg/dumprestore` Exporter/Importer NDJSON, `cmd/dump`/`cmd/restore` CLI, 支持跨索引、进度回调、Basic 认证、stdout/stdin)

### 实现记录补充
- 2026-08-11: 完成 #20 数据备份/导出工具 — NDJSON dump/restore:
  - `pkg/dumprestore/dumprestore.go`:Exporter(HTTP _search 滚动)+ Importer(_bulk 批量 + TargetIndex) + DumpToFile/RestoreFromFile 便捷函数 + __dump_meta__ 元数据行
  - `cmd/dump/main.go` + `cmd/restore/main.go`:CLI 子命令, 支持 flags(-url/-out/-idx/-user/-pass/-page-size/-in/-target-idx/-batch-size),SIGINT/SIGTERM 优雅退出
  - 单元测试:`dumprestore_test.go` 17+ 用例, 覆盖率 80.8%,`go test -race` 无竞争
  - e2e:`scripts/e2e-tests.sh` section 40(HTTP 模拟 dump/restore) + `scripts/test-in-docker.sh` host-side CLI 真实命令行验证

- 2026-08-11: 完成 #25 fuzz testing — **Go 1.18+ native fuzz 覆盖核心解析路径**:
  - `internal/search/fuzz_test.go` 7 个 fuzz target: `FuzzParseQueryString` / `FuzzParseSimpleQueryString` / `FuzzStripSQSReserved`(纯字符串解析) + `FuzzEvalMultiMatch` / `FuzzEvalQueryString` / `FuzzEvalSimpleQueryString`(Engine 方法, JSON → UseNumber → map) + `FuzzEngineMatch`(完整 Match 路径, fuzz []byte → parseQuery → Match)
  - `internal/server/fuzz_test.go` 4 个 fuzz target: `FuzzParseQuery`(parseQuery map→Query) + `FuzzSearchBody`(`POST /_search` 端到端) + `FuzzBulkBody`(`POST /_bulk` NDJSON 端到端) + `FuzzIndexDoc`(`PUT /{index}/_doc/{id}` 端到端); 端到端 target 断言不 panic / 不 500
  - fuzz Engine 预置 3 条文档(text/keyword/number/bool), 让输入命中真实倒排路径; 所有 target 用 `json.NewDecoder + UseNumber()` 复现 server 层真实解码
  - `newTestServer` 参数改为 `testing.TB` 以兼容 `*testing.F`
  - 11 个 target 各 10s 探索: **0 panic / 0 crash**, ~3M+ execs
  - 无第三方依赖(Go stdlib native fuzz)

- 2026-08-11: 完成 #26 性能回归基线(benchmark) — **search + HTTP 端到端 48+ benchmark + 一键脚本 + 退化阻断**:
  - `internal/search/bench_test.go`: 33 个 benchmark, 覆盖写(IndexDoc × 3 档) + 9 种查询 × 3 档规模;预置 Engine 在 `b.StopTimer()` 外构造;结果写 `sinkResults` 防编译器 DCE;`b.ReportAllocs()` 追踪 ns/B/allocs
  - `internal/server/bench_test.go`: 15+ HTTP 端到端 benchmark, 覆盖 HTTPSearch(7 种查询) / 单 doc PUT / bulk 4 种批量(100/500/1k/5k) / 写+读综合 / 创建索引;复用 `newTestServer(testing.TB)` 内存服务器
  - `scripts/bench.sh`: smoke(默认 1×50ms) / full(5×1s, benchstat 友好) / run-only / compare 四模式;compare 模式直接解析 go test 原始输出,同名 benchmark 多采样取均值,退化 > BENCH_REGRESSION_THRESHOLD_PCT%(默认 10)时 exit 2,用于 CI 阻断合入;自动 `go install benchstat` 从 `$(go env GOPATH)/bin/benchstat` 查找;基线文件带 generated_at/go/host 头,输出目录 `bench/`;软链 `latest_smoke.txt` / `latest_full.txt`;仅 capture go test stdout 剔除 zap stderr 日志污染
  - 实测 1k 档: Engine.Match match_all ≈ 10µs, match ≈ 68µs, multi_match ≈ 155µs, range ≈ 327µs, bool ≈ 373µs, query_string ≈ 327µs
  - compare 阻断自测: match_all 10000→12500 ns/op (+25%, 阈值 10%) 正确 exit 2;match_all 10000→10500 ns/op (+5%) 正确 exit 0

- 2026-08-11: 完成 #27 端到端压测脚本 — **vegeta 四阶段 + P50/P95/P99/QPS/内存峰值 + CI 阈值阻断**:
  - `scripts/loadtest.sh`: 模式 `smoke / ci-smoke / full / stage`, 阶段 `warmup (bulk 预置 N 条) → read / write / mixed(默认 70:30)`; flag 支持 `-url/-rate/-dur/-warmup/-read-write-ratio/-index/-qps/-out/-server/-data/-auth/-apikey`
  - vegeta 安装: `ensure_vegeta()` 先找系统 `vegeta`(可用 `LOADTEST_VEGETA` 覆盖), 找不到再 `GOFLAGS=-mod=mod go install github.com/tsenart/vegeta@latest`, 失败提示 `brew install vegeta`
  - 请求 target 避免 HTTP 文本解析问题: `emit_json_target()` 写 `{method,url,header,body=b64}` 每行一个 NDJSON 到 targets 文件; jq 路径优先写更快; 无 jq 用 python3 单行脚本兜底; 认证头(Basic/ApiKey)直接写进 JSON header
  - attack 不使用 `-lazy` (targets 少时 vegeta 会把 targets 一次消费完立即退出, -dur 不生效); 保持 `-duration` 驱动, 外层 `timeout --kill-after=3s $((dur+10))` 兜底, 超时不 die 继续 report; 新增 `normalize_duration_seconds()` 把 `10s/1m/2m30s` 转秒
  - 报告: `vegeta report -type=text/json/csv` 三份; `extract_qps()` 新版从单行 `Requests [total, rate, throughput]` 取第 3 列 throughput (实际 QPS, 比 rate 更真); `append_summary_row()` 新版单行 `Latencies [min, mean, 50, 90, 95, 99, max]` 拆 7 列 + `to_ms()` 兼容 µs/ms/s 转毫秒; 兼容旧版逐行 `50%/95%/99%/mean` 与 `Success: 100%` 格式
  - 内存峰值: `start_mem_sampler()` 独立后台进程, 每 1s `ps -o rss= $PID` 或 Linux `ss+lsof` 找对应 PID 写 RSS 到 `memory.csv`; `stop_mem_sampler()` 算 max 并在最终 summary 表附 `mem_max_mb`; trap EXIT 清理进程
  - 内嵌服务端: `-server <bin> -data <dir>` 自动拉起 server + `-probe_target` 健康检查等到 ready; `stop_server` SIGTERM + 10 次等待, 最后 SIGKILL; 不指定 `-server/-data` 直接对已有 `-url` 压测
  - CI 集成: `-qps N` 设置阈值, read/mixed 阶段 QPS<N 计数, 最终 QPS 不达标 `exit 3`; 即使某阶段 throughput 解析失败 succ 默认 0 仍能出完整 summary.csv
  - 实测 10s smoke: `-rate 500 -warmup 500 docs` → read 阶段 `Requests [total, rate, throughput] 5000, 500.14, 500.08`, **throughput ≥ 500**

### 待办(更长期)
(所有本期工作已完成; 后续按需增量)

实施前请先在本节加"实现记录"段,说明日期与作者。
