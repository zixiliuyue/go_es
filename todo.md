# go_es 功能评估与待办清单

> 评估日期: 2026-08-07  
> 评估范围: `cmd/server`、`internal/server`、`internal/search`、`internal/storage`、`pkg/*`、`scripts/*`  
> 项目定位: 自研最小可运行的 Elasticsearch 8 兼容服务端(数据用 BadgerDB 持久化),客户端 + 服务端双端实现

---

## 评估摘要

**已实现亮点**(截至 v0.1.0):
- 完整 CRUD / Bulk / _search / _reindex(异步+回滚)/ _aliases / ILM / Ingest Pipeline / Index&Component Template / Snapshot / Tasks / Cat / 健康探针
- HTTP/2(h2c + h2)/ TLS / mTLS / gzip / 跨索引通配 / Basic+ApiKey 认证 / IP 限速 / 优雅关闭 / Prometheus metrics
- 倒排索引引擎 + 范围查询 O(logN+K) 排序倒排
- 内置 Web UI(多 Tab + 历史 + 字段类型推断 + 拖拽 + 导入导出 + 图表)
- YAML 配置热更新 + schema 校验

**主要短板**(按影响排序):
1. **服务端完全没有实现聚合分析**(客户端 SDK 支持 terms/histogram/avg/sum/... 10+ 种,但服务端 `doSearch` 忽略 `aggs` 字段,直接 0 命中返回) — 这是最大功能缺失
2. **没有 relevance scoring**: 所有 match query 退化为"是否匹配",没有 TF/BM25,UI 上 `_score` 总是 0,排序体验差
3. **没有 highlight / source filtering / track_total_hits**: ES 核心能力缺失
4. **没有 delete_by_query / update_by_query**: 必须逐个 _doc 操作
5. **没有 suggest / completion / phrase 端点**: `pkg/suggest` 只是 SDK wrapper,服务端未实现
6. **没有 multi_match / query_string / simple_query_string**: 复杂文本查询场景不支持
7. **数据规模受限**: 倒排全部在内存,大数据集(> 100 万 doc)内存爆炸
8. **写入路径未走 batch**: 单 doc 走 `Put` 一笔事务,bulk 走 `WriteBatch` 但每个 item 仍可独立优化
9. **没有持久化 / 恢复机制**: snapshot 路径只存元信息,实际数据不导出
10. **运维能力薄弱**: 没有 structured logging / trace propagation / 索引模板渲染真模拟 / 运行时 profiling endpoint

---

## 一、核心功能缺失(高优先级)

### 1. 服务端聚合分析(aggregations)
- **价值**: 客户端 SDK 已有完整 10+ 种聚合 API,但服务端 `search.go::doSearch` 完全忽略请求体中的 `aggs` 字段,导致 `pkg/aggregate` 实际是死代码。补齐后,examples + UI 都能跑真实聚合;`count` 聚合还能直接替代 `_cat/indices` 的部分场景
- **优先级**: 🔴 高
- **工时**: M(4-6 天)
- **实现要点**:
  - 在 `internal/search/` 新增 `aggregator.go`,实现 `terms` / `histogram` / `date_histogram` / `range` / `value_count` / `avg` / `sum` / `min` / `max` / `stats` / `cardinality`
  - `doSearch` 改为命中后 `evalAggregations(indices, hits, aggs)` 二次计算
  - terms/histogram 走 docID 倒排,先取匹配 doc,再遍历源文档统计(无需再次全表扫)
  - cardinality 走 HLL(HyperLogLog)近似,内存友好
  - UI 已有聚合面板和 terms chart,服务端补齐后立刻可视化
  - 单元测试: `extensions_test.go` 加 11 个 `TestAggregator_*` (每种 agg 一个);e2e: `scripts/e2e-tests.sh` 加聚合断言

### 2. 相关性打分(relevance scoring)
- **价值**: 当前所有 query 退化为"是否匹配"二分结果,UI 上 `_score` 始终 0,sort 默认是 docID 字典序。引入打分后,BM25 排序的体验才像真 ES
- **优先级**: 🔴 高
- **工时**: M(3-4 天)
- **实现要点**:
  - 在 `engine` 中维护 `(index, field) -> token -> postings: [(docID, tf, docLen)]` 倒排,把 `map[docID]struct{}` 升级为带 TF/位置的结构
  - 加 `(index) -> {totalDocs, avgFieldLen}` 统计,O(1) 拿到 IDF
  - match / multi_match / match_phrase 走 BM25 公式计算 score,按 score 降序输出
  - 保留 `constant_score` 包装器选项,允许用户强制退回布尔匹配
  - 单测: 验证 BM25 数值正确(对照 Lucene 测试集);e2e: 验证 `_score` 非零、sort 默认按 _score desc

### 3. `_update_by_query` 与 `_delete_by_query`
- **价值**: 批量条件更新/删除是运维高频操作(如清空某状态字段、删某时间之前数据),目前必须 scan + loop,SDK 也没有对应 wrapper
- **优先级**: 🟠 中高
- **工时**: M(3 天)
- **实现要点**:
  - 走与 `_reindex` 相同的 TaskManager 异步框架(sync/async 都支持)
  - `_delete_by_query`: 命中后 engine 标记 + store 批量 delete
  - `_update_by_query`: 命中后用 painless-script-lite(JSON 路径表达式)修改 _source,再 reindex
  - 进度上报复用 reindex 的 Progress 字段(Total/Created/Updated/Deleted/Batches)
  - 取消语义: 已删除/已修改不可回滚(只回滚 _update_by_query 在 batch 边界的未提交修改)
  - 单测: `TestDeleteByQuery_*` / `TestUpdateByQuery_*`;e2e: `e2e-tests.sh` section 4c 验证

### 4. highlight + source filtering + track_total_hits
- **价值**: 三个最常用的 search response 增强,缺失会破坏 ES 客户端体验(SDK 假设这些字段存在)
- **优先级**: 🟠 中
- **工时**: S(2 天)
- **实现要点**:
  - `highlight`: match 命中时记录 token 在原 string 的位置,返回 `<em>...</em>` 包裹;UI 直接展示
  - `_source` 过滤: 请求 `{"_source": ["a","b"]}` 时,从存储的 doc 中只 pick 这些字段
  - `track_total_hits`: true 时即使 from+size 截断,total.value 仍是真实命中数(默认 10000 上限避免慢 count)
  - 单测: 4 个 `TestSearch_Highlight/SourceFilter/TrackTotalHits_*`;e2e: 验证响应体字段

### 5. suggest / completion / phrase 服务端实现
- **价值**: `pkg/suggest` SDK 存在但服务端无对应 `_search` 端点的 suggest 字段支持,SDK 调用会 404
- **优先级**: 🟠 中
- **工时**: M(2-3 天)
- **实现要点**:
  - 在 `engine` 加 completion field 类型(支持在 doc 内置一个内存 trie / FST)
  - `_search` body 加 `suggest` 字段解析: completion 走 trie 查前缀;term 走编辑距离(ED)对 token 表的差 1-2 邻居;phrase 走 n-gram + 直接替换候选
  - 单测: 3 个 `TestSuggest_*`;e2e: `e2e-tests.sh` 加 suggest 段

### 6. multi_match / query_string / simple_query_string
- **价值**: ES 文本查询的主入口,目前只有 `match`(单字段) + `term` + `bool`,复杂场景需要 bool+多 match 嵌套,易用性差
- **优先级**: 🟠 中
- **工时**: M(3 天)
- **实现要点**:
  - `multi_match`: 内部展开为多个 match 子句,再用 bool 合并;支持 `best_fields` / `most_fields` / `cross_fields` / `phrase` / `phrase_prefix` 等 type
  - `query_string` / `simple_query_string`: 写一个 mini Lucene query parser(支持 `+word -excluded "phrase" field:value`),仅解析不编译
  - 单测: 6 个 `TestQuery_*`;e2e: 客户端用 SDK 调一次,断言响应

---

## 二、性能与可扩展性(中高优先级)

### 7. 倒排索引的持久化与重建加速
- **价值**: 当前冷启动 `engine.LoadAll` 走全表 Scan,O(N) IO + 反序列化;10w+ 文档下启动 30s+。落盘后启动可秒级
- **优先级**: 🟠 中高
- **工时**: M(4 天)
- **实现要点**:
  - 新增 `internal/storage/inverted.go`,在每次 `engine.IndexDoc` 后,异步 flush `(index, field) -> entries` 快照到 `inv/<index>/<field>` key
  - 启动时优先 LoadInverted(),仅在缺失时退化为 Scan
  - 用 BadgerDB 的 MergeOperator 维护增量
  - 单测: 100w 条数据对比启动时间(< 1s);e2e: 重启 server 验证状态一致

### 8. 文档 `_seq_no` / `_primary_term` 乐观并发控制
- **价值**: ES 标配,用于 if_seq_no/if_primary_term 防止并发覆盖,缺失会导致静默丢更新
- **优先级**: 🟡 中
- **工时**: S(2 天)
- **实现要点**:
  - `doc/<index>/<id>` 改为 `(source, meta)`,meta 包含 seq_no(原子自增)+ primary_term
  - update 路径支持 `?if_seq_no=X&if_primary_term=Y` 头,不一致返回 409
  - 单测 + e2e: 并发更新验证只有一个成功

### 9. 写入路径的事务合并与回压
- **价值**: bulk 路径已走 WriteBatch,但单 doc 路径仍是每笔事务,高 QPS 下 fsync 抖动明显
- **优先级**: 🟡 中
- **工时**: M(3 天)
- **实现要点**:
  - 新增 `internal/server/writebuf.go`,提供异步 buffer(最大 1000 条 / 50ms flush)
  - 单 doc 写入走 buffer,`refresh=true` 时强制同步 flush
  - 写满时 backpressure(返回 429)
  - 单测: 模拟 1w qps 写入,验证 P99 < 50ms;e2e: 100w doc 批量灌入 benchmark

### 10. 倒排分段(Segment)与可释放内存
- **价值**: 当前 sorted index 全在内存且不能淘汰,数据量 > 100w doc 时内存爆。引入 segment 后可冷热分层
- **优先级**: 🟡 中
- **工时**: L(7-10 天)
- **实现要点**:
  - 借鉴 Lucene 的 segment 模型:每 N 个 doc flush 一个 segment(只读),后台 merge
  - 查询时跨 segment merge hits,带 segment 级的 bloom filter
  - 内存里只保留最近的 hot segment,冷 segment 走磁盘倒排
  - 单测 + e2e + benchmark: 500w doc 内存占用 < 4GB

### 11. 搜索结果评分缓存
- **价值**: 高频相同 query 重复打(尤其 UI 自动补全),用 query hash 做 LRU 缓存
- **优先级**: 🟢 低
- **工时**: S(1-2 天)
- **实现要点**:
  - 内存 LRU(`hashicorp/golang-lru` 或自实现),key = SHA1(query_body + index)
  - 写操作时按 (index) 失效缓存
  - 单测: 二次相同 query 命中缓存;e2e: 验证 /metrics 加 `go_es_search_cache_hits_total` 计数器

---

## 三、运维与可观测性(中优先级)

### 12. 结构化访问日志 + request trace
- **价值**: 当前用 zap 写 logger,但访问层日志缺(请求行、耗时、status、req_id),出问题时排查困难
- **优先级**: 🟠 中
- **工时**: S(2 天)
- **实现要点**:
  - 中间件链加 `middlewareAccessLog`,记录 method/path/status/dur/req_id/auth_user
  - zap production encoder(JSON)+ 字段统一命名
  - 配置文件 `log_level` 真生效(zap.NewProductionConfig().Level)
  - 单测 + e2e: 抓 100 个请求,验证日志行 schema

### 13. 健康端点深化(/_health/cluster, /_health/storage)
- **价值**: 现有 3 个 K8s 探针,但缺少业务级健康(分片级、存储级),运维定位弱
- **优先级**: 🟡 中
- **工时**: S(1 天)
- **实现要点**:
  - `/_health/storage`: 调 `s.store.DB().IsClosed()` + 写读往返
  - `/_health/cluster`: 扫描 meta/,报告每个 index 的 status(green/yellow/red)
  - 单测 + e2e: 故障注入(stop badger)返回 503

### 14. Prometheus 指标扩展
- **价值**: 现有 7 个指标偏 HTTP 侧,缺搜索/索引/任务维度
- **优先级**: 🟡 中
- **工时**: S(1-2 天)
- **实现要点**:
  - 新增 `go_es_search_query_duration_seconds{index,query_type}` 直方图
  - `go_es_index_doc_count{index}` 在 IndexDoc/DeleteDoc 时同步
  - `go_es_task_active_count` / `go_es_task_completed_total{action,status}`
  - `go_es_storage_size_bytes{index}`(Scan 时统计)
  - 单测: 各指标注册 + 触发后值正确

### 15. 慢请求与失败请求采样日志
- **价值**: 默认不打 P99 慢请求详情,出问题时无法复现
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - middlewareMetrics: dur > configurable 阈值(默认 500ms) 或 status >= 500 时,WARN 级别打印完整 req+res 摘要
  - 阈值可配 `slow_request_threshold_ms`

---

## 四、数据完整性与可靠性(中优先级)

### 16. 真实快照与恢复
- **价值**: 当前 snapshot 只存元信息,数据未真正导出/恢复,生产无意义
- **优先级**: 🟠 中
- **工时**: L(5-7 天)
- **实现要点**:
  - `PUT /_snapshot/{repo}/{snap}` 真正把所有 `doc/<index>/*` + `meta/<index>` + `alias/*` 打包到 repo 目录(用 tar 或裸文件 copy)
  - 支持 `fs` 仓库类型;`s3` 可选(用 minio-go SDK)
  - `POST /_snapshot/{repo}/{snap}/_restore` 反向恢复
  - 单测 + e2e: 写入 1000 doc → snapshot → delete index → restore → 验证 doc 一致

### 17. ILM 真执行
- **价值**: `/_ilm/policy` 存了策略但没执行器,`/{index}/_ilm/explain` 写死返回 hot 阶段,生产没意义
- **优先级**: 🟡 中
- **工时**: L(7 天)
- **实现要点**:
  - 新增 `internal/server/ilm_executor.go`,每 30s 扫所有 `managed: true` 索引,根据 policy.phases.{hot,warm,cold,delete} 的 min_age + actions 执行
  - rollover: 当满足 `max_age`/`max_size`/`max_docs` 时,创建新索引 + 别名切换
  - 真删除 cold/delete 阶段索引(可选配保留 N 天)
  - 单测 + e2e: 配 1s min_age,验证自动 rollover

### 18. 索引设置(settings)真生效
- **价值**: 创建索引接受 `settings.number_of_shards/replicas`,但实际不存也不生效
- **优先级**: 🟡 中
- **工时**: S(2 天)
- **实现要点**:
  - `meta/<index>` 已存 settings,但查询时返回
  - `number_of_shards` 实际影响写并发(可拆分为 N 个 internal index 或仅文档化)
  - 至少保证 settings 透传 + `GET /_settings` 端点存在
  - 单测 + e2e

### 19. mapping 校验
- **价值**: 写入完全无 schema 校验,任何字段都能进,索引设计无约束
- **优先级**: 🟡 中
- **工时**: M(3-4 天)
- **实现要点**:
  - 创建索引时记录 mapping,在 `IndexDoc` 时按 mapping 做类型校验(类型不匹配 → mapper_parsing_exception)
  - 支持 dynamic mapping(未声明字段,首次写入时推断)
  - 写入用 typed processor(field coercion: string → int 失败报错)
  - 单测 + e2e: 故意发错类型,验证 400 错误

### 20. 数据备份/导出工具
- **价值**: 运维侧需要"把 go_es 数据导到 jsonl/再倒回"做数据迁移
- **优先级**: 🟢 低
- **工时**: S(1-2 天)
- **实现要点**:
  - 新增 `cmd/dump` 子命令,遍历 `doc/*` 写 ndjson
  - `cmd/restore` 子命令反之
  - 用 SDK,跟 server 解耦

---

## 五、客户端 SDK 完善(中优先级)

### 21. pkg/suggest 服务端实现对齐
- **价值**: 同 #5
- **优先级**: 🟠 中(已在 #5 覆盖)

### 22. retry / circuit-breaker 中间件
- **价值**: 客户端容错,网络抖动时不暴露给上游
- **优先级**: 🟢 低
- **工时**: S(2 天)
- **实现要点**:
  - 用 `cenkalti/backoff.v4` 做指数退避重试(只对 5xx 和 connection error)
  - 用 `sony/gobreaker` 做熔断
  - 默认开启,可通过 client.Config 关闭

### 23. 上下文超时与链路透传
- **价值**: `ctx` 已经在用,但未贯通 transport;链路追踪需求日益增长
- **优先级**: 🟢 低
- **工时**: S(1-2 天)
- **实现要点**:
  - 在 transport 层加 `X-Request-Id` 自动从 ctx 提取
  - 注入 traceparent(W3C Trace Context)头
  - 默认超时从 ctx 拿

### 24. SDK 集成测试 fixture
- **价值**: `pkg/pool` 等测试需要真 ES,本地失败是预期。提供 in-process test container
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - 在 `pkg/client` 加 `NewTestServer(t)` helper,起内存 BadgerDB + server,自动 cleanup
  - 改 `pkg/pool` 等用 TestServer 代替 skip

---

## 六、测试与质量保障(中优先级)

### 25. fuzz testing
- **价值**: search / bulk / query parser 接受任意 JSON,fuzz 可挖出 panic / OOM
- **优先级**: 🟡 中
- **工时**: S(1-2 天)
- **实现要点**:
  - 在 `internal/search` 加 `FuzzQuery` / `FuzzSearch`(用 Go 1.18+ native fuzz)
  - CI 集成 `go test -fuzz=... -fuzztime=30s`
  - 找到的 crash 立刻加 regression test

### 26. 性能回归基线(benchmark)
- **价值**: 后续任何改动需要能感知到性能漂移
- **优先级**: 🟡 中
- **工时**: S(2 天)
- **实现要点**:
  - 在 `internal/search` 加 `BenchmarkMatch_*` / `BenchmarkRange_*` / `BenchmarkBool_*`
  - CI 跑 benchmark,产出 benchstat 数据存到 `bench/` 目录
  - 写 `scripts/bench.sh` 一键跑 + 对比

### 27. 端到端压测脚本
- **价值**: 当前 e2e 验证功能,不验证性能/容量
- **优先级**: 🟢 低
- **工时**: S(1-2 天)
- **实现要点**:
  - `scripts/loadtest.sh`: 用 `vegeta` 或 `wrk` 打 bulk + search
  - 报告: P50/P95/P99 延迟、QPS、内存峰值
  - CI 在 PR 上跑 1 分钟 smoke loadtest

### 28. 一致性测试框架
- **价值**: 验证 go_es 与 ES 行为一致(同一 query 两个服务返回应一致)
- **优先级**: 🟢 低
- **工时**: M(3-4 天)
- **实现要点**:
  - `scripts/consistency-test.sh`:同一份数据 + 同一组 query 打到 ES 和 go_es,对比响应(忽略 _score 差异)
  - 失败时打印 diff 供人工分析
  - 跑通后用真实 ES 数据集(logstash 样例)

---

## 七、安全与权限(中高优先级)

### 29. 索引级 + 操作级 RBAC
- **价值**: 当前只有全局 Basic 认证,生产多租户场景无法做权限隔离
- **优先级**: 🟠 中高
- **工时**: M(3-4 天)
- **实现要点**:
  - 配置加 `auth.roles: [{name, indices, privileges}]`,权限 = (index_pattern, action_pattern)
  - ApiKey 关联 role
  - 中间件鉴权时查表,失败返回 403
  - 单测 + e2e: 验证不同 user 看到不同 index 集合

### 30. 审计日志
- **价值**: 谁在什么时间访问/修改了什么数据,合规要求
- **优先级**: 🟡 中
- **工时**: M(3 天)
- **实现要点**:
  - 写操作异步 append 到 `audit/<yyyy-mm-dd>` key,定期 compact
  - 提供 `GET /_audit?since=...&user=...&index=...` 查询
  - 单测 + e2e: 验证登录、修改、删除都被记录

### 31. 输入校验硬化
- **价值**: URL path / query 参数过短,易被 DoS(超大 index 名、超大 from+size)
- **优先级**: 🟡 中
- **工时**: S(1-2 天)
- **实现要点**:
  - 限制 index 名长度 ≤ 255 字符、字符集
  - 限制 from + size ≤ 10000(参考 ES)
  - 限制 _search body 大小独立可配
  - 单测 + e2e

### 32. CORS 与 CSRF
- **价值**: Web UI 与第三方域共享时需要 CORS
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - 中间件 `middlewareCORS`,配置 `cors.allowed_origins: ["..."]`
  - 默认 `*`, 生产建议白名单
  - 写操作要求 `Origin` 与 `Host` 匹配防 CSRF(可选)

---

## 八、用户体验(Web UI 增强)(中优先级)

### 33. 索引管理面板
- **价值**: UI 只能查询,不能创建/删除索引、修改 mapping,运维需 curl
- **优先级**: 🟠 中
- **工时**: M(2-3 天)
- **实现要点**:
  - 新增 "索引" 标签页,列出所有索引 + 文档数 + 存储大小
  - 点开 → mapping 查看器 + settings 编辑器
  - 新建索引表单(支持粘贴 mapping JSON)
  - 删除确认对话框
  - 单测 + e2e(通过 e2e shell 调 UI 操作接口)

### 34. SQL / DSL 双向转换
- **价值**: 不会写 ES DSL 的人也能查
- **优先级**: 🟢 低
- **工时**: M(3-4 天)
- **实现要点**:
  - SQL → DSL: 写 mini parser,支持 `SELECT a,b FROM idx WHERE field = 'x' LIMIT 10`
  - DSL → SQL: 把 match/term/range 翻译成 WHERE 子句
  - UI 加 SQL 编辑器

### 35. 实时刷新(websocket / SSE)
- **价值**: 长时 indexing 任务,UI 看不到进度
- **优先级**: 🟡 中
- **工时**: S(2 天)
- **实现要点**:
  - 引入 `nhooyr.io/websocket` 或 stdlib
  - `/_ui/ws` 端点,客户端订阅后,服务端定期 push 集群指标 + 任务进度
  - UI 顶部加实时指标条

### 36. 暗/亮主题 + 国际化
- **价值**: 当前已有 dark/light 主题,但 i18n 仅标题中文,正文英文
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - i18n 抽取: 把 `index.html` 内的英文字符串移到 `i18n/{zh,en}.json`
  - UI 顶栏加语言切换

### 37. 移动端 / 平板适配
- **价值**: 已有 `@media (max-width:900px)` 但仅折叠侧栏
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - 优化 Tab 滚动、搜索结果卡片化
  - 触摸友好的按钮尺寸(>= 44px)

### 38. 多窗口对比查询
- **价值**: UI 用 Tab 但只能顺序对比,不能同屏对比 2 个查询
- **优先级**: 🟢 低
- **工时**: M(2 天)
- **实现要点**:
  - 加"分屏"模式: 2 个 Tab 并排,共享顶部 search box
  - 各自独立的 from/size

---

## 九、生态与分发(低优先级)

### 39. Helm chart
- **价值**: 用户用 K8s 部署,直接 helm install 比 docker run 简单
- **优先级**: 🟡 中
- **工时**: S(1-2 天)
- **实现要点**:
  - `deploy/helm/go-es/Chart.yaml` + values.yaml
  - Deployment + Service + ServiceMonitor + PDB
  - ConfigMap 挂载 go_es.yaml
  - 测试:`helm install --dry-run`

### 40. 镜像多架构构建
- **价值**: linux/amd64 + linux/arm64(vs Apple Silicon 开发者)
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - Dockerfile.server 改 multi-stage with buildx
  - GitHub Actions 加 arm64 runner

### 41. release 自动化
- **价值**: 当前手动打 tag + push,容易忘
- **优先级**: 🟢 低
- **工时**: S(1-2 天)
- **实现要点**:
  - `.github/workflows/release.yml`: tag 触发 → build binary × 3 OS + docker image × 2 arch → push to GHCR
  - 自动生成 changelog from commit messages

### 42. 文档站 / 用户手册
- **价值**: 当前 README 是中文,英文用户少
- **优先级**: 🟢 低
- **工时**: M(3-4 天)
- **实现要点**:
  - 用 Docusaurus 或 VitePress
  - 章节: 快速开始 / 部署 / 配置 / 客户端 SDK / 内部架构 / 路线图
  - 自动从 OpenAPI 规范生成 API 文档

### 43. 协议升级: 9.0/9.1 新特性预览
- **价值**: 保持对 ES 上游的兼容承诺
- **优先级**: 🟢 低
- **工时**: L(7+ 天)
- **实现要点**:
  - knn 搜索(向量检索,需集成 hnsw 库)
  - disk_usage API
  - semantic_text 字段类型

---

## 优先级与工时总览

| 类别 | 项数 | 总工时(人天) | 关键路径 |
|---|---|---|---|
| 🔴 一、核心功能缺失 | 6 | 17-23 | 聚合+打分(必须先做,UI 才能活) |
| 🟠 二、性能可扩展 | 5 | 17-27 | 倒排持久化、seq_no |
| 🟡 三、运维可观测 | 4 | 5-7 | 访问日志、指标扩展 |
| 🟠 四、数据完整性 | 5 | 19-26 | 真实 snapshot、ILM 执行 |
| 🟢 五、SDK 完善 | 4 | 5-8 | retry、ctx timeout |
| 🟡 六、测试质量 | 4 | 8-10 | fuzz、benchmark、consistency |
| 🟠 七、安全权限 | 4 | 8-11 | RBAC、审计 |
| 🟡 八、UI 增强 | 6 | 9-12 | 索引管理、实时刷新 |
| 🟢 九、生态分发 | 5 | 13-21 | Helm、release、文档 |

**推荐迭代路径**:
- **v0.2.0 (M1)**: #1 聚合 + #2 打分 + #4 highlight/source — 补齐 ES 核心能力,客户端 SDK 全部可用
- **v0.3.0 (M2)**: #3 update_by_query + #5 suggest + #6 multi_match — 完善查询体系
- **v0.4.0 (M3)**: #7 倒排持久化 + #9 写入合并 + #10 segment — 性能与可扩展性
- **v0.5.0 (M4)**: #16 真 snapshot + #17 ILM 执行 + #19 mapping 校验 — 可靠性
- **v0.6.0 (M5)**: #29 RBAC + #30 审计 + #13 健康深化 — 生产化

---

## 备注

- **已实现的能力**(见 AGENTS.md 8.0)不在本评估范围,不再重复列
- **工时**为单人估算,实际可能 ±50%
- **优先级**基于"对 ES 兼容性的影响 × 实现成本"加权
- 每个建议都遵循项目约定: 单元测试 + e2e 断言 + 中文注释 + 不破坏向后兼容
- 任何"破坏现有 API/语义"的方案不在本评估中(如 v2 client、proto over HTTP/2)
