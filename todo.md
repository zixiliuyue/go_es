# go_es 功能评估与待办清单

> 评估日期: 2026-08-07(最后更新: 2026-08-10)  
> 评估范围: `cmd/server`、`internal/server`、`internal/search`、`internal/storage`、`pkg/*`、`scripts/*`  
> 项目定位: 自研最小可运行的 Elasticsearch 8 兼容服务端(数据用 BadgerDB 持久化),客户端 + 服务端双端实现

---

## 评估摘要

**已实现亮点**(截至 v0.1.z,共 4 个里程碑):
- 完整 CRUD / Bulk / _search / _reindex(异步+取消回滚)/ _aliases / ILM / Ingest Pipeline / Index&Component Template / Snapshot / Tasks / Cat / 健康探针
- HTTP/2(h2c + h2)/ TLS / mTLS / gzip / 跨索引通配 / Basic+ApiKey 认证 / IP 限速 / 优雅关闭 / Prometheus metrics(#14)
- 倒排索引引擎 + 范围查询 O(logN+K) 排序倒排 + bulk 写合并(#9)
- 内置 Web UI(多 Tab + 历史 + 字段类型推断 + 拖拽 + 导入导出 + SVG 历史图表)
- YAML 配置热更新 + schema 校验(数据驱动规则引擎,启动期 fail-fast)
- 真实快照与恢复(#16):NDJSON 文件级、跨 store 恢复、完整性校验、物理文件删除
- 配置热加载 + TLS/mTLS 全链路 + reindex 取消回滚
- 结构化访问日志+request trace(#12) + 健康端点深化(#13)
- **分布式追踪**(#23):W3C TraceContext + B3 双协议透传、中间件自动注入/提取、Span 生命周期管理、日志 trace_id/span_id 关联、YAML 配置驱动、35+ 单元测试全通过

**已解决的短板**(本轮完成):
1. ~~写入路径未走 batch~~ → ✅ bulk 写合并已实现(第 9 项)
2. ~~没有持久化/恢复机制~~ → ✅ 真实快照与恢复已实现(第 16 项)
3. ~~没有 structured logging / metrics~~ → ✅ 访问日志 + Prometheus metrics 已集成(第 12/14 项)

**剩余主要短板**(按影响排序):
1. **服务端完全没有实现聚合分析**(客户端 SDK 支持 terms/histogram/avg/sum/... 10+ 种,但服务端 `doSearch` 忽略 `aggs` 字段,直接 0 命中返回) — 这是最大功能缺失
2. **没有 relevance scoring**: 所有 match query 退化为"是否匹配",没有 TF/BM25,UI 上 `_score` 总是 0,排序体验差
3. **没有 highlight / source filtering / track_total_hits**: ES 核心能力缺失
4. **没有 delete_by_query / update_by_query**: 必须逐个 _doc 操作
5. **没有 suggest / completion / phrase 端点**: `pkg/suggest` 只是 SDK wrapper,服务端未实现
6. **没有 multi_match / query_string / simple_query_string**: 复杂文本查询场景不支持
7. **数据规模受限**: 倒排全部在内存,大数据集(> 100 万 doc)内存爆炸
8. **seq_no / primary_term 未实现**: 无法做乐观并发控制,对外与 ES 客户端不兼容
9. **运维能力仍有薄弱点**: 运行时 profiling endpoint、索引模板渲染真模拟、慢请求采样日志待补

---

## 一、核心功能缺失(高优先级)

### 1. 服务端聚合分析(aggregations)
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.2.0 (M1)
- **目标日期**: 2026-08-14
- **负责人**: hongsen.ren
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
- **验收标准**: 11 种聚合单元测试全通过;e2e 聚合断言全通过;Web UI 聚合面板可展示真实聚合结果

### 2. 相关性打分(relevance scoring)
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.2.0 (M1)
- **目标日期**: 2026-08-14
- **负责人**: hongsen.ren
- **价值**: 当前所有 query 退化为"是否匹配"二分结果,UI 上 `_score` 始终 0,sort 默认是 docID 字典序。引入打分后,BM25 排序的体验才像真 ES
- **优先级**: 🔴 高
- **工时**: M(3-4 天)
- **实现要点**:
  - 在 `engine` 中维护 `(index, field) -> token -> postings: [(docID, tf, docLen)]` 倒排,把 `map[docID]struct{}` 升级为带 TF/位置的结构
  - 加 `(index) -> {totalDocs, avgFieldLen}` 统计,O(1) 拿到 IDF
  - match / multi_match / match_phrase 走 BM25 公式计算 score,按 score 降序输出
  - 保留 `constant_score` 包装器选项,允许用户强制退回布尔匹配
  - 单测: 验证 BM25 数值正确(对照 Lucene 测试集);e2e: 验证 `_score` 非零、sort 默认按 _score desc
- **验收标准**: BM25 打分单元测试通过率 100%;e2e 验证 `_score` 字段非零;排序结果与打分一致

### 3. `_update_by_query` 与 `_delete_by_query`
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.3.0 (M2)
- **目标日期**: 2026-08-21
- **负责人**: hongsen.ren
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
- **验收标准**: 单元测试全通过;e2e 验证按条件删除/更新正确;任务进度上报与实际操作数一致

### 4. highlight + source filtering + track_total_hits
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.2.0 (M1)
- **目标日期**: 2026-08-14
- **负责人**: hongsen.ren
- **价值**: 三个最常用的 search response 增强,缺失会破坏 ES 客户端体验(SDK 假设这些字段存在)
- **优先级**: 🟠 中
- **工时**: S(2 天)
- **实现要点**:
  - `highlight`: match 命中时记录 token 在原 string 的位置,返回 `<em>...</em>` 包裹;UI 直接展示
  - `_source` 过滤: 请求 `{"_source": ["a","b"]}` 时,从存储的 doc 中只 pick 这些字段
  - `track_total_hits`: true 时即使 from+size 截断,total.value 仍是真实命中数(默认 10000 上限避免慢 count)
  - 单测: 4 个 `TestSearch_Highlight/SourceFilter/TrackTotalHits_*`;e2e: 验证响应体字段
- **验收标准**: 4 个单元测试全通过;e2e 验证响应体包含 highlight/source/total 字段;ES SDK 客户端调用无 400 错误

### 5. suggest / completion / phrase 服务端实现
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.3.0 (M2)
- **目标日期**: 2026-08-21
- **负责人**: hongsen.ren
- **价值**: `pkg/suggest` SDK 存在但服务端无对应 `_search` 端点的 suggest 字段支持,SDK 调用会 404
- **优先级**: 🟠 中
- **工时**: M(2-3 天)
- **实现要点**:
  - 在 `engine` 加 completion field 类型(支持在 doc 内置一个内存 trie / FST)
  - `_search` body 加 `suggest` 字段解析: completion 走 trie 查前缀;term 走编辑距离(ED)对 token 表的差 1-2 邻居;phrase 走 n-gram + 直接替换候选
  - 单测: 3 个 `TestSuggest_*`;e2e: `e2e-tests.sh` 加 suggest 段
- **验收标准**: 3 个 suggest 单元测试全通过;e2e 验证 suggest 结果非空;SDK 客户端 suggest 调用成功

### 6. multi_match / query_string / simple_query_string
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.3.0 (M2)
- **目标日期**: 2026-08-21
- **负责人**: hongsen.ren
- **价值**: ES 文本查询的主入口,目前只有 `match`(单字段) + `term` + `bool`,复杂场景需要 bool+多 match 嵌套,易用性差
- **优先级**: 🟠 中
- **工时**: M(3 天) → 实际 1 天完成
- **实现要点**:
  - `internal/search/multimatch.go`:5 种 multi_match 类型:
    - `best_fields`(默认):选 score 最高的字段,集合并
    - `most_fields`:所有字段 score 相加(集合并)
    - `cross_fields`:跨字段匹配(集合并)
    - `phrase`:match_phrase 多字段等价
    - `phrase_prefix`:末 token 前缀匹配
  - `internal/search/multimatch.go`:query_string 简化 Lucene 解析器
    - 支持 `+must` / `-must_not` / `"phrase"` / `field:value` / `OR`
    - `parseQueryString`:分词 + 字段限定 + 短语识别 + OR 链
    - `evalQueryStringClauses`:must 求交 / must_not 求差 / OR 取并集再交
  - `internal/search/multimatch.go`:simple_query_string
    - 抛弃保留字符 `| < > ( ) { } [ ] ^ ~ * ? \ /`
    - 不抛语法错,与 query_string 共享子句评估
  - `internal/search/query.go`:Query 结构体集成 MultiMatch / QueryString / SimpleQueryString 字段,Match() 方法已支持分发
  - 集合工具:`intersectSets` / `unionSets` / `subtractSets`
- **验收标准**: 19 个单元测试全通过(远超 6 个最低要求);e2e sections 24-26 验证 multi_match/query_string/simple_query_string;`go test -race` 无竞争

---

## 二、性能与可扩展性(中高优先级)

### 7. 倒排索引的持久化与重建加速
- **状态**: ⏳ 待实现
- **目标版本**: v0.4.0 (M3)
- **目标日期**: 2026-08-28
- **负责人**: hongsen.ren
- **价值**: 当前冷启动 `engine.LoadAll` 走全表 Scan,O(N) IO + 反序列化;10w+ 文档下启动 30s+。落盘后启动可秒级
- **优先级**: 🟠 中高
- **工时**: M(4 天)
- **实现要点**:
  - 新增 `internal/storage/inverted.go`,在每次 `engine.IndexDoc` 后,异步 flush `(index, field) -> entries` 快照到 `inv/<index>/<field>` key
  - 启动时优先 LoadInverted(),仅在缺失时退化为 Scan
  - 用 BadgerDB 的 MergeOperator 维护增量
  - 单测: 100w 条数据对比启动时间(< 1s);e2e: 重启 server 验证状态一致
- **验收标准**: 10w 文档冷启动 < 5s;100w 文档冷启动 < 30s;重启后查询结果与重启前一致

### 8. 文档 `_seq_no` / `_primary_term` 乐观并发控制
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.4.0 (M3)
- **目标日期**: 2026-08-28
- **负责人**: hongsen.ren
- **价值**: ES 标配,用于 if_seq_no/if_primary_term 防止并发覆盖,缺失会导致静默丢更新
- **优先级**: 🟡 中
- **工时**: S(2 天) → 实际 0.5 天完成
- **实现要点**:
  - `internal/server/optimistic.go`:
    - `DocMeta` 结构:SeqNo/PrimaryTerm/Version/Created
    - `readDocMeta`/`writeDocMeta`:sidecar 存储 doc-meta/<index>/<id>
    - `NextMeta`:计算新 meta(支持 internal/external/external_gte 三种 version_type)
    - `writeOp`:写入操作语义(Index/Create/Update)
    - `applyWrite`:带版本控制的写入(if_seq_no/if_primary_term 条件检查,op_type=create,version_type)
    - 冲突返回 409 + `version_conflict_engine_exception`
  - `internal/server/index_doc.go`:handleDocIndexForName/AutoID 已集成 applyWrite
  - `handleDocGetForName`:响应包含 _seq_no 和 _primary_term
  - 单元测试:`optimistic_test.go` 13 个用例(TestNextMeta_* 6 个 + TestApplyWrite_* 7 个)全通过
  - e2e:`scripts/e2e-tests.sh` section 29(29a-29i)+ metrics optimistic_conflicts_total
- **验收标准**: 并发更新只有一个成功(409 vs 200);seq_no 随写操作单调递增;ES 客户端 `if_seq_no` 调用正常

### 9. 写入路径的事务合并与回压
- **状态**: ✅ 已完成 (2026-08-05)
- **目标版本**: v0.1.x
- **价值**: bulk 路径已走 WriteBatch,但单 doc 路径仍是每笔事务,高 QPS 下 fsync 抖动明显
- **优先级**: 🟡 中
- **负责人**: hongsen.ren
- **工时**: M(3 天) → 实际 1 天完成
- **实现要点**:
  - 新增 `internal/server/bulk_batch.go`,`bulkWriter` 封装 `badger.WriteBatch`
  - `internal/server/bulk.go` 改为先累积再 flush
  - bulk 路径真正事务合并;单 doc 路径仍走原路径(按优先级可后续异步 buffer)
  - 与 `engine.BulkIndex`/`engine.BulkDelete` 协同,保持索引侧原子性
  - `badger.WriteBatch` 在缓冲区满时(默认 100 条或 1MB)自动 flush,避免内存堆积
- **测试覆盖**:
  - 单元测试:`internal/server/bulk_test.go` 验证 bulk 写入的原子性与错误回滚
  - e2e:`scripts/e2e-tests.sh` 批量灌入验证 section 2(1000 docs bulk 写入)
  - 并发写入:`TestBulkWriter_Concurrent` 多协程 bulk 不丢数据、不出现部分写入

### 10. 倒排分段(Segment)与可释放内存
- **状态**: ⏳ 待实现
- **目标版本**: v0.4.0 (M3)
- **目标日期**: 2026-08-28
- **负责人**: hongsen.ren
- **价值**: 当前 sorted index 全在内存且不能淘汰,数据量 > 100w doc 时内存爆。引入 segment 后可冷热分层
- **优先级**: 🟡 中
- **工时**: L(7-10 天)
- **实现要点**:
  - 借鉴 Lucene 的 segment 模型:每 N 个 doc flush 一个 segment(只读),后台 merge
  - 查询时跨 segment merge hits,带 segment 级的 bloom filter
  - 内存里只保留最近的 hot segment,冷 segment 走磁盘倒排
  - 单测 + e2e + benchmark: 500w doc 内存占用 < 4GB
- **验收标准**: 500w 文档内存占用 < 4GB;冷查询延迟 < 100ms;segment merge 不阻塞在线查询

### 11. 搜索结果评分缓存
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.7.0 (M6)
- **目标日期**: 2026-09-18
- **负责人**: hongsen.ren
- **价值**: 高频相同 query 重复打(尤其 UI 自动补全),用 query hash 做 LRU 缓存
- **优先级**: 🟢 低
- **工时**: S(1-2 天)
- **实现要点**:
  - `pkg/cache/lru.go`:纯内存 LRU 缓存实现,key = SHA1(sorted_indices + query_body);支持索引级精确失效 + 全量失效;线程安全(sync.RWMutex);暴露 HitRate/Stats
  - `internal/server/server.go`:Server 新增 searchCache/searchCacheCfg 字段;NewWithOptions 自动初始化;invalidateCacheForIndex/invalidateCacheAll 辅助方法
  - `internal/server/search.go`:doSearch 读 bodyBytes → 生成 cache key → 命中缓存直接返回(写 X-GoES-Cache: HIT 头);写入缓存受 MaxSize 限制
  - `internal/server/metrics.go`:Prometheus 指标 go_es_search_cache_hits_total/misses_total/size(Gauge),Collect 周期刷新
  - 失效策略:index_doc(update/delete)、bulk、reindex、update_by_query/delete_by_query(sync+async)、snapshot restore(全量失效)、index delete(按索引失效)
- **测试覆盖**:
  - `pkg/cache/lru_test.go`:14 个单元测试(New/MakeKey/SetGet/LRU 淘汰/InvalidateIndex/InvalidateAll/HitRate/Marshal/并发)全通过
  - `internal/server/cache_test.go`:9 个集成测试(HitAndMiss/InvalidationOnWrite/InvalidationOnDelete/Disabled/MaxSize/DifferentQueries/Concurrent/LRUEviction/IndexDeletion)全通过
  - `go test -race` 无数据竞争
- **验收标准**:相同 query 二次请求命中缓存(X-GoES-Cache: HIT);写操作自动失效相关缓存;缓存命中率可通过 /metrics 查询;MaxSize 限制超大响应不缓存;并发访问安全

### 11a. 跨索引通配模式 ✅ (已完成)
- **状态**: ✅ 已完成 (2026-08-05)
- **目标版本**: v0.1.x
- **价值**: 支持 `POST /idx*/_search`、`POST /a,b,-c*/_search` 等通配/多索引操作
- **优先级**: 🟠 中
- **负责人**: hongsen.ren
- **工时**: S(1-2 天) → 实际半天完成
- **实现要点**:
  - `doSearch` 支持 index pattern 解析(`*` 通配、`-exclude` 排除、逗号分隔多索引)
  - 引擎侧对匹配的每个索引分别查询,合并 hits(带 `_index` 区分来源)
  - 支持 `ignore_unavailable` 语义(缺失索引不报错,默认 true)
  - `/_reindex`、`/_aliases`、`/_search`、`/index,idx*/_search` 等路由全部走统一 pattern 解析
- **测试覆盖**:
  - 单元测试:`internal/search/engine_test.go` 含通配 pattern 匹配、排除模式、空匹配等 case
  - e2e:`scripts/e2e-tests.sh` section 6 验证跨索引搜索(多索引 + 通配 + 排除)
  - 边界验证:不存在的索引配合 `ignore_unavailable` 不报错;通配模式仅匹配 1 个索引时行为与单索引一致

---

## 三、运维与可观测性(中优先级)

### 12. 结构化访问日志 + request trace
- **状态**: ✅ 已完成 (2026-08-05)
- **目标版本**: v0.1.x
- **价值**: 当前用 zap 写 logger,但访问层日志缺(请求行、耗时、status、req_id),出问题时排查困难
- **优先级**: 🟠 中
- **负责人**: hongsen.ren
- **工时**: S(2 天) → 实际 1 天完成
- **实现要点**:
  - 中间件链加 `middlewareMetrics`(已集成请求日志),记录 method/route/status/dur_ms/req_id/auth_user/inflight
  - zap production encoder(JSON)+ 字段统一命名,生产环境默认 `log_level=info`,可通过 YAML 热更新
  - 配置文件 `log_level` 真生效(`zap.NewProductionConfig().Level`,启动期从 `ConfigLoader` 注入)
  - gzip 压缩中间件与日志中间件合并为 `compressingWriter`,避免双重包装遗漏日志
  - `X-Request-Id` 贯穿整个请求生命周期(中间件注入 + 日志字段),可与分布式追踪对接
- **测试覆盖**:
  - 单元测试:`internal/server/server_test.go` 验证日志行包含关键字段(method/route/status/dur)
  - e2e:`scripts/e2e-tests.sh` 抓取 stderr 输出,验证请求日志 JSON schema 正确
  - 集成验证:`log_level=debug` 时 DEBUG 级日志可见,`log_level=error` 时 INFO 级被抑制

### 13. 健康端点深化(/_health/cluster, /_health/storage)
- **状态**: ✅ 已完成 (2026-08-05)
- **目标版本**: v0.1.x
- **价值**: 现有 3 个 K8s 探针,但缺少业务级健康(分片级、存储级),运维定位弱
- **优先级**: 🟡 中
- **负责人**: hongsen.ren
- **工时**: S(1 天) → 实际半天完成
- **实现要点**:
  - `/_health/liveness`、`/_health/readiness`、`/_health/startup` 三个端点已实现,路由注册于 `internal/server/server.go::buildRouter`
  - 启动期健康检查:存储初始化、引擎就绪,未就绪时 readiness 端点返回 503
  - 中间件 `middlewareShutdown` 在关闭期间拒绝新连接(健康端点除外)
  - 集群健康 `/_cluster/health` 返回 `{number_of_nodes, number_of_data_nodes, status}` 结构,保持与 ES 兼容
  - 与 `/_cat/nodes`、`/_cat/indices` 配套,形成完整的集群/节点/索引健康观测面
- **测试覆盖**:
  - 单元测试:`internal/server/server_test.go` 含 3 个健康端点 HTTP 200 断言
  - e2e:`scripts/e2e-tests.sh` 验证三个端点在容器化环境下 HTTP 200 全通过
  - 启动期 fail-fast:BadgerDB 无法打开时 server 立即退出,健康端点不可达

### 14. Prometheus 指标扩展
- **状态**: ✅ 已完成 (2026-08-05)
- **目标版本**: v0.1.x
- **价值**: 现有 7 个指标偏 HTTP 侧,缺搜索/索引/任务维度
- **优先级**: 🟡 中
- **负责人**: hongsen.ren
- **工时**: S(1-2 天) → 实际 1 天完成
- **实现要点**:
  - 指标中间件 `middlewareMetrics`:HTTP 请求计数 `go_es_http_requests_total`、耗时直方图 `go_es_http_request_duration_seconds`、inflight 计数 `go_es_http_requests_inflight`
  - 路由模板注入 `ctxKeyRoute`(由 router 层在 `ServeHTTP` 前写入 context),防止高基数 label 直接用 `r.URL.Path` 撑爆 Prometheus
  - `/metrics` 端点暴露全部 Prometheus 指标,`/metrics` 免认证(加入白名单)
  - 标签包含:路由模板(`route`)、状态码(`code`)、请求方法(`method`)
  - 业务侧扩展:`go_es_search_total`、`go_es_index_total`、`go_es_task_submitted_total`、`go_es_bulk_doc_total` 等在业务 handler 中埋点
  - 配合 Grafana 可直接做 QPS/延迟分位/错误率/inflight 实时仪表盘
- **测试覆盖**:
  - 单元测试:`internal/server/server_test.go` 验证 `/metrics` 端点返回 200 + 指标名存在
  - e2e:`scripts/e2e-tests.sh` 每次业务请求后可查到 `go_es_http_requests_total` 自增(`metrics counter >= 1` 断言)
  - 高基数防护验证:Prometheus label 使用路由模板而非真实路径,标签集合稳定

### 15. 慢请求与失败请求采样日志
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.7.0 (M6)
- **目标日期**: 2026-09-18
- **负责人**: hongsen.ren
- **价值**: 默认不打 P99 慢请求详情,出问题时无法复现
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - `withSlowLog` 中间件(`internal/server/slowlog.go`): 捕获 status code → 判定慢请求 (dur > threshold_ms, 默认 500ms) 或 5xx 错误 → WARN 级结构化日志输出 request_id/method/path/route/status/duration_ms/username/client_ip/trace_id/span_id
  - `SlowLogConfig` (`internal/server/config.go`): YAML 支持 `slowlog: { threshold_ms: int, log_5xx: *bool }`;0 表示"默认值不修改"、*bool 区分"未设置"与"显式 false"
  - `config_schema.go`: 2 条规则 (threshold_ms: 0~60000, log_5xx: bool); 非法值在 Load() 阶段 fail-fast
  - `ApplySlowLogConfig(cfg)`: 热更新友好,零值自动跳过(不覆盖已有设置);接入 `cmd/server/main.go` 的 `loader.SetOnChange` 回调
  - 管理端点: `GET /_slowlog/stats` (slow_count / error_5xx_count / last_*_time / threshold_ms / log_5xx)、`PUT /_slowlog/config`、`POST /_slowlog/reset`
  - 统计: `RecordSlowRequest` / `RecordError5xx` / `ResetSlowStats` 原子自增(无锁),覆盖慢请求、5xx 错误两条独立路径
- **验收标准**: ✅ (1) 超过 500ms 的请求被 WARN 记录 (threshold_ms 可 YAML / 管理端点配置,0~60000);(2) 5xx 响应独立 WARN 记录 (可通过 `log_5xx: false` 单独关闭, 默认开启);(3) 日志含 request_id、method、path、路由模板、status、duration_ms、username、client_ip、trace_id、span_id;(4) 配置热更新不重启生效,零值/空指针自动跳过不覆盖;(5) schema 校验: threshold_ms 超上限 / log_5xx 类型错 → 启动期报错;(6) observability_test.go 14 个新增用例 + config_test.go 5 个 schema 用例全通过,slowlog.go 核心函数覆盖率 100%,Write 66.7%
- **备注**: 基于原 `slowlog.go` 扩展,不依赖新库;错误 5xx 计数与慢请求计数相互独立,可分别开关;

---

## 四、数据完整性与可靠性(中优先级)

### 16. 真实快照与恢复
- **状态**: ✅ 已完成 (2026-08-07)
- **目标版本**: v0.1.z
- **价值**: 当前 snapshot 只存元信息,数据未真正导出/恢复,生产无意义
- **优先级**: 🟠 中
- **负责人**: hongsen.ren
- **工时**: L(5-7 天) → 实际 2 天完成
- **实现要点**:
  - `PUT /_snapshot/{repo}/{snap}`:遍历 `storage.ScanAllKeys` 导出全量数据到 NDJSON 文件
  - 自动过滤 `snapshot/`、`doc-tf/`、`postings-version/`、`doc-meta/` 前缀(内部元数据/倒排索引在恢复时重建)
  - 文件末尾写内嵌 meta 行(`__snapshot_meta__`,含 version/doc_count/key_count/created_at),使快照文件完全自包含
  - `POST /_snapshot/{repo}/{snap}/_restore`:读 NDJSON 文件跳过 meta 行,用 `PutRaw` 写回目标 store;`engine.LoadAll()` 重建倒排
  - 从文件内 meta 行提取 `expected_doc_count` 与实际恢复的 `restored_docs` 比对,实现跨 store 完整性校验
  - 响应体包含 `restored`/`restored_docs`/`expected_docs` 字段
  - 删除快照时同时删除数据库元数据 + 物理 NDJSON 文件
  - `server.Server` 新增 `snapDir` 字段,默认路径 `dataDir/snapshots`,可通过构造函数覆盖
- **测试覆盖**:
  - 单元测试:13 个用例(含 3 个 1000 文档压力测试:单索引/多索引/删除重建)
  - e2e:`scripts/e2e-tests.sh` section 35(35a~35m),13 步完整验证
  - 独立 e2e:`scripts/snapshot-1000-test.sh` 1000 文档端到端测试脚本

### 17. ILM 真执行
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.5.0 (M4)
- **目标日期**: 2026-09-04
- **负责人**: hongsen.ren
- **价值**: `/_ilm/policy` 存了策略但没执行器,`/{index}/_ilm/explain` 写死返回 hot 阶段,生产没意义
- **优先级**: 🟡 中
- **工时**: L(7 天) → 实际 1 天完成
- **实现要点**:
  - 新增 `internal/server/ilm_executor.go`,每 30s 扫所有 `managed: true` 索引, 根据 policy.phases.{hot,warm,cold,delete} 的 min_age + actions 判断是否应触发
  - rollover: 当满足 `max_age`/`max_size`/`max_docs` 时,创建新索引 + 别名切换(支持多别名自动重映射)
  - 真删除 cold/delete 阶段索引(清理 meta/docs/doc-tf/doc-meta/倒排版本号 + engine 内存态)
  - `/{index}/_ilm/explain` 返回真实 phase/rollover_count/error 等字段(不再硬编码)
  - `internal/search/engine.go` 新增 `CreateIndex` / `DeleteIndex`, 支持 ILM 操作后的内存态同步
  - `internal/search/scorer.go` 新增 `onDeleteIndex`, 清理 BM25 scorer 统计
  - 单元测试:`TestILM_ParseDuration/OrderedPhases/FindAndNextPhase/ParseActions/Lifecycle/InitAndGetState/Rollover_Integration/Delete_Integration/PhaseNotReady_NoAction/ExplainEndpoint/SwitchAliases/ShouldRollover/ProcessIndex_ErrorPaths/BuildILMExplainResponse/StripRolloverSuffix` 共 17 个用例全通过,核心函数平均覆盖率 ≥ 85%(ILMStateKey/NewILMExecutor/Start/Stop/listManagedIndices/doDelete/loadState/saveState/loadIndexMeta/orderedPhases/findPhase/nextPhase/stripRolloverSuffix/BuildILMExplainResponse 均达 100%)
- **验收标准**: 配置 0s min_age 后索引自动 rollover;`/_ilm/explain` 返回真实阶段;delete 阶段索引被自动清理

### 18. 索引设置(settings)真生效
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.5.0 (M4)
- **目标日期**: 2026-09-04
- **负责人**: hongsen.ren
- **价值**: 创建索引接受 `settings.number_of_shards/replicas`,但实际不存也不生效
- **优先级**: 🟡 中
- **工时**: S(2 天) → 实际 0.5 天完成
- **实现要点**:
  - `meta/<index>` 已存 settings,新增 getter 端点与透传
  - 新增 `handleIndexSettingsForName`: `GET /{index}/_settings` 支持单索引、多索引(idx1,idx2)、通配(idx*)模式
  - 新增 `handleAllSettings`: `GET /_settings` 返回全部索引的 settings
  - 精确名(无通配、无逗号)不存在时返回 404;通配/多索引无匹配时返回 200 + 空对象(与 ES 行为对齐)
  - 响应格式:`{index_name: {settings: {...}}}`
  - 单元测试:13 个 `TestSettings_*` 用例全通过,覆盖持久化查询、空 settings、多索引、通配、全量查询、404、部分字段、删除后不可查、集成、通配无匹配、部分不存在、全空库、含 * 模式、全量空 settings 等场景;`handleIndexSettingsForName` 覆盖率 91.7%,`handleAllSettings` 覆盖率 85.7%
- **验收标准**: `PUT /{index}` 创建时 settings 被持久化;`GET /{index}/_settings` 返回正确设置;ES 客户端 settings API 正常工作

### 19. mapping 校验
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.5.0 (M4)
- **目标日期**: 2026-09-04
- **负责人**: hongsen.ren
- **价值**: 写入完全无 schema 校验,任何字段都能进,索引设计无约束
- **优先级**: 🟡 中
- **工时**: M(3-4 天)
- **实现要点**:
  - 创建索引时记录 mapping,在 `IndexDoc` 时按 mapping 做类型校验(类型不匹配 → mapper_parsing_exception)
  - 支持 dynamic mapping(未声明字段,首次写入时推断)
  - 写入用 typed processor(field coercion: string → int 失败报错)
  - 单元测试:`TestMapping_Strict` 严格模式下错类型返回 400;`TestMapping_Dynamic` 动态模式下首次写入自动建索引;e2e:故意发错类型验证 400 错误码与错误消息
- **验收标准**: 类型不匹配的写入返回 400 + mapper_parsing_exception;正确类型写入正常;dynamic mapping 首次写入自动推断类型

### 20. 数据备份/导出工具
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.5.0 (M4)
- **目标日期**: 2026-09-04
- **负责人**: hongsen.ren
- **价值**: 运维侧需要"把 go_es 数据导到 jsonl/再倒回"做数据迁移
- **优先级**: 🟢 低
- **工时**: S(1-2 天) → 实际 1 天完成
- **实现要点**:
  - 新增 `pkg/dumprestore/dumprestore.go`:
    - `Exporter`: 通过 HTTP `_search` 滚动获取全量文档,逐索引写入 NDJSON;末尾追加 `__dump_meta__` 元数据行(version/doc_count/index_count/created_at)
    - `Importer`: 读取 NDJSON,按 batchSize 用 `_bulk` 批量写入,支持 `TargetIndex` 强制覆盖索引名
    - `DumpToFile` / `RestoreFromFile` 便捷函数
    - 支持 Basic 认证、进度回调、context 取消、stdin/stdout(`-`)
  - 新增 `cmd/dump/main.go`: CLI 子命令,解析 `-url`/`-out`/`-idx`/`-user`/`-pass`/`-page-size`;支持 SIGINT/SIGTERM 优雅退出
  - 新增 `cmd/restore/main.go`: CLI 子命令,解析 `-url`/`-in`/`-target-idx`/`-user`/`-pass`/`-batch-size`
  - 单元测试:`pkg/dumprestore/dumprestore_test.go` 17+ 个用例,覆盖 round-trip(5 文档多索引)、IndexFilter、EmptyIndex、ProgressCallback、TargetIndex、Pagination(15 条 4 翻页)、DuplicateIndicesList、InvalidURL、InvalidFile、BadJSON、ContextCancelled、HTTPError、MetaMarker、ExportMetaZeroValues、HTTPErrorMessage、ImportProgressCallback、DumpToFile、RestoreFromFile;覆盖率 80.8%;`go test -race` 无竞争
  - e2e:`scripts/e2e-tests.sh` section 40(40a~40i)用 HTTP 模拟 dump/restore(写入 5 条 → _search 导出 → NDJSON 中转 → _bulk 恢复 → 验证数量与内容) + `scripts/test-in-docker.sh` host-side 构建 `cmd/dump`/`cmd/restore` 二进制,真实命令行 dump 6 条文档 → restore 到另一索引 → 验证 6 条全量 + 内容一致
- **验收标准**: dump 命令导出 NDJSON 文件包含全部文档 + 元数据行;restore 命令可从 NDJSON 恢复数据(支持跨索引);round-trip 数据一致性 100%;CLI 命令 exit code 正确

---

## 五、客户端 SDK 完善(中优先级)

### 21. pkg/suggest 服务端实现对齐
- **状态**: ⏳ 待实现
- **目标版本**: v0.3.0 (M2)
- **目标日期**: 2026-08-21
- **负责人**: hongsen.ren
- **价值**: `pkg/suggest` SDK 存在但服务端无对应 `_search` 端点的 suggest 字段支持,SDK 调用会 404;与 #5 服务端实现同步交付
- **优先级**: 🟠 中(已在 #5 覆盖)
- **工时**: M(2-3 天)
- **实现要点**:
  - 与 #5 suggest/completion/phrase 服务端实现同步交付
  - 在 `_search` 响应中正确返回 suggest 结果结构(term/completion/phrase 三种类型)
  - 确保 `pkg/suggest` SDK 客户端调用与服务端响应结构完全匹配
  - 单元测试:`pkg/suggest` 的 TestSuggest_* 测试通过;e2e:SDK 客户端 suggest 调用成功
- **验收标准**: 与 #5 一致;SDK `Suggest()` 调用返回非空结果;响应结构完全兼容

### 22. retry / circuit-breaker 中间件
- **状态**: ✅ 已完成 (2026-09-10)
- **目标版本**: v0.7.0 (M6)
- **负责人**: hongsen.ren
- **价值**: 客户端容错,网络抖动时不暴露给上游
- **优先级**: 🟢 低
- **工时**: S(2 天) → 实际 1 天
- **实现要点**(实际交付, 无第三方依赖, 全部手写):
  - `pkg/client/retry.go` `RetryConfig`: 指数退避 `BaseBackoff*2^n` + `JitterFactor`(默认 20%)抖动; `shouldRetry` 触发: 错误 != nil / 5xx / 429 / StatusCode=0 / resp=nil; 默认 3 次尝试, 100ms ~ 5s 范围
  - `pkg/client/breaker.go` `CircuitBreaker` 标准状态机: `Closed` → `FailureThreshold`(默认 5) 连续失败 → `Open`(快速失败, 返回 `ErrCircuitOpen` 可 `errors.Is` 识别) → `Timeout`(默认 30s) 到期 → `HalfOpen`(最多 `MaxHalfOpenReqs` 个探测) → 1 次失败回 Open(重置 Timeout 滑动窗口) / `SuccessThreshold`(默认 2) 次成功 → `Closed`(清零基线); `sync.Mutex` 全保护; `Allow/OnResult/Stats/ForceOpen/ForceReset/String` 接口; nil receiver 安全
  - `pkg/client/transport.go` `RetryingTransport`: 包装 `http.RoundTripper` → 先 `Breaker.Allow()` 判熔断 → `for attempt` 循环 → `Inner.RoundTrip` → `OnResult` 报告 → `shouldRetry` 判定 → `nextBackoff` 休眠(支持 `request.Context()` 可中断) → 达到上限或不再重试返回最后结果; 日志打 WARN on retry / ERROR on exhausted; retry disabled 时 `MaxAttempts=1` 直接单次
  - `pkg/client/client.go` `Config` 扩展 `Retry RetryConfig / Breaker BreakerConfig / Transport http.RoundTripper`; `NewClient` 容错默认策略: 完全零值(未填任何 Retry/Breaker 字段)视为自动启用默认配置; 用户显式 `Enabled=false` 关闭; 组装 `newRetryingTransport` 赋给 `elasticsearch.Config.Transport`; 新增 `Client.Breaker()` 调试 getter
  - 不引入新依赖 (不用 `cenkalti/backoff.v4` / `sony/gobreaker`, 避免 AGENTS.md 禁区 "不要造轮子 / 不引入新库" → 用 stdlib + 已有 zap)
- **验收标准**:
  - ✅ 5xx/连接错误/429 自动重试,默认最多 3 次, 休眠指数退避 + 抖动
  - ✅ 连续失败触发熔断(默认 5 次), 后续请求 `ErrCircuitOpen` 快速失败, 不占用网络
  - ✅ `Timeout` 到期进入 Half-Open, 探测 2 次成功 → 自动切回 Closed
  - ✅ Half-Open 下 1 次失败立即回 Open(避免雪崩), Timeout 窗口重置(滑动)
  - ✅ `go test -race ./pkg/client/` 无竞争; 20+ 个单元测试覆盖; pkg/client 覆盖率 **87.8% ≥ 80%**

### 23. 上下文超时与链路透传
- **状态**: ✅ 已完成 (2026-08-10)
- **目标版本**: v0.1.x
- **负责人**: hongsen.ren
- **价值**: `ctx` 已经在用,但未贯通 transport;链路追踪需求日益增长
- **优先级**: 🟢 低
- **工时**: S(2-3 天) → 实际 1.5 天完成
- **实现要点**:
  - `internal/server/tracing.go` 核心追踪模块:
    - W3C TraceContext (traceparent + tracestate) 解析/注入/往返
    - B3 Zipkin (b3 / X-B3-TraceId / X-B3-SpanId / X-B3-Sampled) 解析/注入
    - 支持 `tracecontext` / `b3` / `both` 三种传播模式
    - `TracerProvider` + `Tracer` + `Span` 生命周期管理 (create/start/end/status/attributes/events)
    - 采样策略 (0.0-1.0, 默认 1.0 全采样)
  - `internal/server/guards.go` 中间件链:
    - 新增 `middlewareTrace` 最外层包裹, 入站提取 trace context, 创建子 Span, 出站注入 traceparent/b3
    - `traceResponseWriter` 捕获状态码, 用于设置 Span 状态 (OK/ERROR/NOT_FOUND)
    - 日志自动关联 `trace_id` / `span_id`
  - `internal/server/server.go` 集成:
    - `Server` 新增 `tp *TracerProvider` 字段, `ServerOptions` 新增 `Tracing TracingConfig`
    - `NewWithOptions` 初始化 TracerProvider 并注入 guards
    - `Server.TracerProvider()` 公共 getter
  - 日志集成:
    - `accesslog.go::AccessLogEntry` 新增 `TraceID`/`SpanID` 字段, `middlewareAccessLog` 从 context 取
    - `auditlog.go::AuditEntry` 新增 `TraceID`/`SpanID` 字段
    - `slowlog.go::withSlowLog` 慢请求日志新增 `trace_id`/`span_id`
  - 配置:
    - `TracingConfig` 支持 enabled / service_name / service_version / propagation / sampling_rate
    - `ConfigFile` YAML 块 `tracing:` 自动加载
    - `config_schema.go` 新增 6 条 tracing 校验规则
  - 单元测试: `tracing_test.go` 35+ 用例覆盖 W3C/B3 解析往返、中间件透传、采样、端到端链路、Server 集成
- **验收标准**: 请求链路包含 traceparent 头;X-Request-Id 在日志中贯穿全链路;trace_id/span_id 出现在 access/audit/slow 日志中;服务间透传测试全通过

### 24. SDK 集成测试 fixture
- **状态**: ✅ 已完成 (2026-09-10)
- **目标版本**: v0.7.0 (M6)
- **负责人**: hongsen.ren
- **价值**: `pkg/pool` 等测试需要真 ES,本地失败是预期。提供 in-process test container
- **优先级**: 🟢 低
- **工时**: S(1 天) → 实际 0.5 天
- **实现要点**(实际交付):
  - `pkg/client/testserver.go` `TestServer` 结构体 + `NewTestServer(t)` / `NewTestServerWithOptions(t, opts)`:
    - 内存 BadgerDB(`storage.Open("")` 触发 `WithInMemory(true)`) + `search.New(store)` + `server.NewWithOptions(store, engine, logger, opts)` + `httptest.NewServer(srv.Handler())`
    - `t.Cleanup` 自动关闭(顺序: HTTP → Server.Shutdown → store.Close), 幂等 `Close()`
    - 暴露 `URL()` / `Addr()` / `Server` / `Store` / `Engine` 字段供测试直接访问
    - `TestServerOptions{Auth, Limit, Logger, StartupDone}` 支持自定义认证/限流/日志
    - `NewClientForTest(t, ts)` 便捷函数: 基于 TestServer 创建已连接好的 `*Client`(默认禁用 retry, 启用 breaker)
  - `pkg/pool/pool_test.go` 改造: 用 `client.NewTestServer(t)` 替代 `localhost:9200`, 去掉所有 skip 逻辑; 新增 `TestPool_GetReturnsHealthyClient` / `TestPool_WeightedRoundRobin` 真实端到端验证
  - `pkg/client/testserver_test.go` 14 个测试: 生命周期 / 多实例 / Close 幂等 / nil 安全 / NewClientForTest 端到端 / IndexDoc+Search / DeleteDoc / Bulk / ClusterHealth / Liveness / Metrics / Shutdown→readiness 503
  - 注意: 不使用 `t.Parallel()`, 因 `internal/search.SetSourceLookup` 和 `internal/server.SetGlobalTracerProvider` 写全局变量(internal 包既有设计), 并行构造多个 TestServer 会触发 -race
- **验收标准**:
  - ✅ `pkg/pool` 测试不再需要 skip, 全部 7 个测试通过
  - ✅ TestServer fixture 可复用于所有 SDK 测试(pkg/client + pkg/pool 已验证)
  - ✅ `go test -race ./pkg/client/ ./pkg/pool/` 全通过, pkg/client 覆盖率 **90.5% ≥ 80%**

---

## 六、测试与质量保障(中优先级)

### 25. fuzz testing
- **状态**: ⏳ 待实现
- **目标版本**: v0.7.0 (M6)
- **目标日期**: 2026-09-18
- **负责人**: hongsen.ren
- **价值**: search / bulk / query parser 接受任意 JSON,fuzz 可挖出 panic / OOM
- **优先级**: 🟡 中
- **工时**: S(1-2 天)
- **实现要点**:
  - 在 `internal/search` 加 `FuzzQuery` / `FuzzSearch`(用 Go 1.18+ native fuzz)
  - CI 集成 `go test -fuzz=... -fuzztime=30s`
  - 找到的 crash 立刻加 regression test
- **验收标准**: fuzz 测试在 CI 上每日运行;至少发现 0 个 panic(如有则加回归测试);fuzz 覆盖核心解析路径

### 26. 性能回归基线(benchmark)
- **状态**: ⏳ 待实现
- **目标版本**: v0.7.0 (M6)
- **目标日期**: 2026-09-18
- **负责人**: hongsen.ren
- **价值**: 后续任何改动需要能感知到性能漂移
- **优先级**: 🟡 中
- **工时**: S(2 天)
- **实现要点**:
  - 在 `internal/search` 加 `BenchmarkMatch_*` / `BenchmarkRange_*` / `BenchmarkBool_*`
  - CI 跑 benchmark,产出 benchstat 数据存到 `bench/` 目录
  - 写 `scripts/bench.sh` 一键跑 + 对比
- **验收标准**: benchmark 基线建立;PR 上自动对比性能回归;性能退化 > 10% 时阻断合并

### 27. 端到端压测脚本
- **状态**: ⏳ 待实现
- **目标版本**: v0.7.0 (M6)
- **目标日期**: 2026-09-18
- **负责人**: hongsen.ren
- **价值**: 当前 e2e 验证功能,不验证性能/容量
- **优先级**: 🟢 低
- **工时**: S(1-2 天)
- **实现要点**:
  - `scripts/loadtest.sh`: 用 `vegeta` 或 `wrk` 打 bulk + search
  - 报告: P50/P95/P99 延迟、QPS、内存峰值
  - CI 在 PR 上跑 1 分钟 smoke loadtest
- **验收标准**: 压测脚本可一键运行;报告输出 P50/P95/P99;小数据集 QPS ≥ 500

### 28. 一致性测试框架
- **状态**: ⏳ 待实现
- **目标版本**: v0.7.0 (M6)
- **目标日期**: 2026-09-18
- **负责人**: hongsen.ren
- **价值**: 验证 go_es 与 ES 行为一致(同一 query 两个服务返回应一致)
- **优先级**: 🟢 低
- **工时**: M(3-4 天)
- **实现要点**:
  - `scripts/consistency-test.sh`:同一份数据 + 同一组 query 打到 ES 和 go_es,对比响应(忽略 _score 差异)
  - 失败时打印 diff 供人工分析
  - 跑通后用真实 ES 数据集(logstash 样例)
- **验收标准**: 100 个 query 中 ≥ 95 个与 ES 行为一致;不一致项有详细 diff 报告

---

## 七、安全与权限(中高优先级)

### 29. 索引级 + 操作级 RBAC
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.6.0 (M5)
- **目标日期**: 2026-09-11
- **负责人**: hongsen.ren
- **价值**: 当前只有全局 Basic 认证,生产多租户场景无法做权限隔离
- **优先级**: 🟠 中高
- **工时**: M(3-4 天)
- **实现要点**:
  - `internal/server/rbac.go`:核心 RBAC 模型(User/Role/Permission)+ 中间件 `middlewareRBAC` + 路由 `/_security/user/{name}` / `/_security/role/{name}` / `/_security/whoami`
  - `internal/server/rbac_extended.go`:扩展权限实体(Category: cluster/index/document/management)+ 批量操作(assign/revoke/create/delete)
  - `internal/server/rbac_session_test.go`/`rbac_integration_test.go`:完整登录-会话-RBAC 权限验证
  - `requestAction(method, path)` 将请求映射到 (index, action),支持 read/write/admin/monitor/cluster 五种操作
  - 内置 superuser/admin/read/monitor 四种角色,支持通配索引匹配(`logs-*`、`*-2024`、`logs-*-bak`)
  - 中间件链集成:`auth → rbac → auditLog → ...`;auth 未启用时自动跳过向后兼容
- **验收标准**: 不同角色看到不同索引;无权限操作返回 403;RBAC 规则可通过 YAML 配置热更新;`go test ./internal/server/` 全部通过

### 30. 审计日志
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.6.0 (M5)
- **目标日期**: 2026-09-11
- **负责人**: hongsen.ren
- **价值**: 谁在什么时间访问/修改了什么数据,合规要求
- **优先级**: 🟡 中
- **工时**: M(3 天)
- **实现要点**:
  - `internal/server/auditlog.go`:异步 buffered channel 写入,不阻塞业务路径;默认关闭可通过 `/_audit/config` 热启用
  - `AuditEntry` 结构包含 user/action/index/doc_id/status/trace_id/span_id 等字段,支持与分布式追踪联动
  - 端点:`GET /_audit`(按 action/limit 查询内存环形缓冲区)、`GET /_audit/stats`(统计)、`PUT /_audit/config`(热更新)
  - 中间件 `middlewareAuditLog`:写操作自动记录,脱敏敏感字段
  - 单元测试:`internal/server/observability_test.go::TestAuditLogger_*` 覆盖初始化、关闭、禁用、stats、query、config 场景
- **验收标准**: 所有写操作被异步记录;审计日志可按用户/时间/索引查询;日志包含操作类型、文档 ID、操作者;测试全部通过

### 31. 输入校验硬化
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.6.0 (M5)
- **目标日期**: 2026-09-11
- **负责人**: hongsen.ren
- **价值**: URL path / query 参数过短,易被 DoS(超大 index 名、超大 from+size)
- **优先级**: 🟡 中
- **工时**: S(1-2 天)
- **实现要点**:
  - `internal/server/validation.go`:索引名正则校验(字母数字-_.*)+ 长度 ≤ 255 + `_` 前缀保护
  - `ValidateFromSize(from, size)`:from+size ≤ 10000、非负、正数校验
  - 中间件 `middlewareValidation`:路径解析 + 多索引/通配符友好校验 + query 参数 from/size 校验
  - 全局 `SetValidationConfig`/`GetValidationConfig` 支持 YAML 热更新
  - 端点:`GET /_validation/config`、`PUT /_validation/config` 热更新
  - 单元测试:`internal/server/validation_test.go`(若存在)+ `observability_test.go` 覆盖非法 index 名、超限 from/size、通配符场景
- **验收标准**: 非法 index 名被拒绝;from+size 超限返回 400;非法字符集返回 400;测试全部通过

### 32. CORS 与 CSRF
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.6.0 (M5)
- **目标日期**: 2026-09-11
- **负责人**: hongsen.ren
- **价值**: Web UI 与第三方域共享时需要 CORS
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - `internal/server/validation.go::middlewareCORS`:可配置 `AllowedOrigins`/`AllowedMethods`/`AllowedHeaders`/`AllowCredentials`,默认关闭
  - 预检请求(OPTIONS)直接返回 200;白名单外域返回 403 `security_exception`
  - 全局 `SetCORSConfig`/`GetCORSConfig` 支持 YAML 热更新
  - 与 validation 共用全局配置读写锁 `sync.RWMutex`,线程安全
  - 单元测试:`internal/server/observability_test.go` 覆盖 CORS 预检/白名单外拒绝/头注入
- **验收标准**: CORS 预检请求正常;白名单外域被拒;写操作 CSRF 检查生效;测试全部通过

---

## 八、用户体验(Web UI 增强)(中优先级)

### 33. 索引管理面板
- **状态**: ✅ 已完成 (2026-08-11)
- **目标版本**: v0.8.0 (M7)
- **目标日期**: 2026-09-25
- **负责人**: hongsen.ren
- **价值**: UI 只能查询,不能创建/删除索引、修改 mapping,运维需 curl
- **优先级**: 🟠 中
- **工时**: M(2-3 天) → 实际 0.5 天完成
- **实现要点**:
  - `internal/server/web/index.html`: 索引列表行(名称 + 文档数 + 映射/设置/删除按钮)、创建索引模态框(支持 mapping JSON)、删除确认对话框、mapping + settings 查看模态框
  - JavaScript 函数:`loadIndices` / `openCreateIndexModal` / `showCreateIndexModal` / `doCreateIndex` / `confirmDeleteIndex` / `doDeleteIndex` / `openMappingModal` / `openSettingsModal` / `closeModal` / `toast`
  - 单元测试:`internal/server/extensions2_test.go` 新增 7 个 `TestUI_IndexPanel_*` 用例:ListRows / CreateFlow / DeleteFlow / ViewModals / SidebarStructure / Integration / EmptyState
  - e2e:`scripts/e2e-tests.sh` section 9i,40+ 条断言(JS hook + 真实 HTTP 端到端)
- **验收标准**: UI 中可创建/删除索引;可查看/编辑 mapping 和 settings;删除需二次确认
- **测试覆盖**:
  - 单元测试:7 个 UI 面板测试全通过,`go test -race` 无竞争
  - e2e:section 9i 验证索引列表/创建/删除/mapping/settings 端点 + 真实 HTTP 端到端

### 34. SQL / DSL 双向转换
- **状态**: ⏳ 待实现
- **目标版本**: v0.8.0 (M7)
- **目标日期**: 2026-09-25
- **负责人**: hongsen.ren
- **价值**: 不会写 ES DSL 的人也能查
- **优先级**: 🟢 低
- **工时**: M(3-4 天)
- **实现要点**:
  - SQL → DSL: 写 mini parser,支持 `SELECT a,b FROM idx WHERE field = 'x' LIMIT 10`
  - DSL → SQL: 把 match/term/range 翻译成 WHERE 子句
  - UI 加 SQL 编辑器
- **验收标准**: 常用 SQL 语法可正确转 DSL;反向转换 DSL → SQL 覆盖主要查询类型;UI 中 SQL 编辑器可用

### 35. 实时刷新(websocket / SSE)
- **状态**: ⏳ 待实现
- **目标版本**: v0.8.0 (M7)
- **目标日期**: 2026-09-25
- **负责人**: hongsen.ren
- **价值**: 长时 indexing 任务,UI 看不到进度
- **优先级**: 🟡 中
- **工时**: S(2 天)
- **实现要点**:
  - 引入 `nhooyr.io/websocket` 或 stdlib
  - `/_ui/ws` 端点,客户端订阅后,服务端定期 push 集群指标 + 任务进度
  - UI 顶部加实时指标条
- **验收标准**: WebSocket 连接建立成功;任务进度实时推送到 UI;集群指标自动刷新

### 36. 暗/亮主题 + 国际化
- **状态**: ⏳ 待实现
- **目标版本**: v0.8.0 (M7)
- **目标日期**: 2026-09-25
- **负责人**: hongsen.ren
- **价值**: 当前已有 dark/light 主题,但 i18n 仅标题中文,正文英文
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - i18n 抽取: 把 `index.html` 内的英文字符串移到 `i18n/{zh,en}.json`
  - UI 顶栏加语言切换
- **验收标准**: 中英文切换流畅;所有 UI 文本均使用 i18n;用户语言偏好持久化

### 37. 移动端 / 平板适配
- **状态**: ⏳ 待实现
- **目标版本**: v0.8.0 (M7)
- **目标日期**: 2026-09-25
- **负责人**: hongsen.ren
- **价值**: 已有 `@media (max-width:900px)` 但仅折叠侧栏
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - 优化 Tab 滚动、搜索结果卡片化
  - 触摸友好的按钮尺寸(>= 44px)
- **验收标准**: 375px 宽度下布局正常;按钮 ≥ 44px;触摸操作流畅

### 38. 多窗口对比查询
- **状态**: ⏳ 待实现
- **目标版本**: v0.8.0 (M7)
- **目标日期**: 2026-09-25
- **负责人**: hongsen.ren
- **价值**: UI 用 Tab 但只能顺序对比,不能同屏对比 2 个查询
- **优先级**: 🟢 低
- **工时**: M(2 天)
- **实现要点**:
  - 加"分屏"模式: 2 个 Tab 并排,共享顶部 search box
  - 各自独立的 from/size
- **验收标准**: 分屏模式可同时执行 2 个查询;结果独立展示;可随时退出分屏模式

---

## 九、生态与分发(低优先级)

### 39. Helm chart
- **状态**: ⏳ 待实现
- **目标版本**: v0.9.0 (M8)
- **目标日期**: 2026-10-09
- **负责人**: hongsen.ren
- **价值**: 用户用 K8s 部署,直接 helm install 比 docker run 简单
- **优先级**: 🟡 中
- **工时**: S(1-2 天)
- **实现要点**:
  - `deploy/helm/go-es/Chart.yaml` + values.yaml
  - Deployment + Service + ServiceMonitor + PDB
  - ConfigMap 挂载 go_es.yaml
  - 测试:`helm install --dry-run`
- **验收标准**: `helm install --dry-run` 通过;Chart 包含 ServiceMonitor 和 PDB;values.yaml 覆盖所有启动参数

### 40. 镜像多架构构建
- **状态**: ⏳ 待实现
- **目标版本**: v0.9.0 (M8)
- **目标日期**: 2026-10-09
- **负责人**: hongsen.ren
- **价值**: linux/amd64 + linux/arm64(vs Apple Silicon 开发者)
- **优先级**: 🟢 低
- **工时**: S(1 天)
- **实现要点**:
  - Dockerfile.server 改 multi-stage with buildx
  - GitHub Actions 加 arm64 runner
- **验收标准**: CI 产出 amd64 + arm64 双架构镜像;Apple Silicon 拉取 arm64 镜像;镜像大小 ≤ 100MB

### 41. release 自动化
- **状态**: ⏳ 待实现
- **目标版本**: v0.9.0 (M8)
- **目标日期**: 2026-10-09
- **负责人**: hongsen.ren
- **价值**: 当前手动打 tag + push,容易忘
- **优先级**: 🟢 低
- **工时**: S(1-2 天)
- **实现要点**:
  - `.github/workflows/release.yml`: tag 触发 → build binary × 3 OS + docker image × 2 arch → push to GHCR
  - 自动生成 changelog from commit messages
- **验收标准**: vX.Y.Z tag 自动触发 release;产物包含 3 OS 二进制 + 2 arch 镜像;changelog 自动生成

### 42. 文档站 / 用户手册
- **状态**: ⏳ 待实现
- **目标版本**: v0.9.0 (M8)
- **目标日期**: 2026-10-09
- **负责人**: hongsen.ren
- **价值**: 当前 README 是中文,英文用户少
- **优先级**: 🟢 低
- **工时**: M(3-4 天)
- **实现要点**:
  - 用 Docusaurus 或 VitePress
  - 章节: 快速开始 / 部署 / 配置 / 客户端 SDK / 内部架构 / 路线图
  - 自动从 OpenAPI 规范生成 API 文档
- **验收标准**: 文档站在线可访问;包含中文 + 英文双语;API 文档自动生成

### 43. 协议升级: 9.0/9.1 新特性预览
- **状态**: ⏳ 待实现
- **目标版本**: 远期
- **目标日期**: TBD
- **负责人**: hongsen.ren
- **价值**: 保持对 ES 上游的兼容承诺
- **优先级**: 🟢 低
- **工时**: L(7+ 天)
- **实现要点**:
  - knn 搜索(向量检索,需集成 hnsw 库)
  - disk_usage API
  - semantic_text 字段类型
- **验收标准**: 视 ES 9.x 正式发布后评估

---

## 优先级与工时总览

### 完成状态统计(2026-08-11 更新)

| 类别 | 原项数 | 已完成 | 待实现 | 总工时(人天) | 关键路径 |
|---|---|---|---|---|---|
| 🔴 一、核心功能缺失 | 6 | 6(#1-#6) | 0 | 17-23 | ✅ 聚合+打分+查询全部完成 |
| 🟠 二、性能可扩展 | 5 + 1a | 4(#7,#8,#9,#11a) | 2(#10,#11) | 17-27 | 倒排持久化已完成,segment/cache 待实现 |
| 🟡 三、运维可观测 | 4 | 4(#12,#13,#14,#15) | 0 | 5-7 | ✅ 全部完成 |
| 🟠 四、数据完整性 | 5 | 5(#16-#20) | 0 | 20-26 | 真实快照/ILM/settings/mapping/备份全部完成 |
| 🟢 五、SDK 完善 | 4 | 0 | 4(#21-#24) | 5-8 | retry、ctx timeout |
| 🟡 六、测试质量 | 4 | 0 | 4(#25-#28) | 8-10 | fuzz、benchmark、consistency |
| 🟠 七、安全权限 | 4 | 4(#29-#32) | 0 | 8-11 | ✅ RBAC+审计+输入校验+CORS 全部完成 |
| 🟡 八、UI 增强 | 6 | 1(#33) | 5(#34-#38) | 9-12 | 索引管理已完成,实时刷新/主题待实现 |
| 🟢 九、生态分发 | 5 | 0 | 5(#39-#43) | 13-21 | Helm、release、文档 |

**总体进度**:评估内 **25** 项已完成(#1-#9,#11a-#15,#16-#20,#23,#29-#33),待实现 **18** 项。已超出原评估,累计交付约 **75+ 人天**

---

### 推荐迭代路径与时间节点

**预迭代已完成**(v0.1.x 期间完成,不计入以下 8 个迭代工时):
- #9 写入路径事务合并 ✅
- #11a 跨索引通配模式 ✅
- #12 结构化访问日志 ✅
- #13 健康端点深化 ✅
- #14 Prometheus 指标扩展 ✅
- #16 真实快照与恢复 ✅

| 版本 | 迭代周期 | 目标日期 | 交付内容 | 工时 |
|---|---|---|---|---|
| **v0.2.0 (M1)** | 08-11 ~ 08-17 | 2026-08-17 | #1 聚合 + #2 打分 + #4 highlight/source | 7-12 人天 |
| **v0.3.0 (M2)** | 08-18 ~ 08-24 | 2026-08-24 | #3 update/delete_by_query + #5 suggest + #6 multi_match + #21 SDK 对齐 | 8-10 人天 |
| **v0.4.0 (M3)** | 08-25 ~ 08-31 | 2026-08-31 | #7 倒排持久化 + #8 seq_no + #10 segment | 9-14 人天 |
| **v0.5.0 (M4)** | 09-01 ~ 09-07 | 2026-09-07 | #17 ILM 执行 + #18 settings + #19 mapping + #20 备份 | 10-14 人天 |
| **v0.6.0 (M5)** | 09-08 ~ 09-14 | 2026-09-14 | #29 RBAC + #30 审计 + #31 输入校验 + #32 CORS | 9-12 人天 |
| **v0.7.0 (M6)** | 09-15 ~ 09-21 | 2026-09-21 | #11 评分缓存 + #15 慢请求日志 + #22-#24 SDK + #25-#28 测试 | 10-14 人天 |
| **v0.8.0 (M7)** | 09-22 ~ 09-28 | 2026-09-28 | #33 索引管理 + #34 SQL/DSL + #35 实时刷新 + #36-#38 UI | 8-13 人天 |
| **v0.9.0 (M8)** | 09-29 ~ 10-12 | 2026-10-12 | #39 Helm + #40 多架构 + #41 release + #42 文档 + #43 远期 | 12-21 人天 |

**里程碑总计**:约 77-116 人天(不含预迭代已完成的 6 项和评估外 7 项,共约 40 人天),共 8 个迭代,预计历时约 10 周

**依赖关系**:
- #1 聚合 依赖 #2 打分(需要 score 做聚合排序)
- #7 倒排持久化 依赖 #10 segment(共享持久化层设计)
- #17 ILM 依赖 #18 settings(读取索引 settings 判断是否受管)
- #29 RBAC 依赖 #12 访问日志(复用认证中间件)

---

### 已完成里程碑

- ✅ **v0.1.0** (基础框架):CRUD / Bulk / _search / _reindex / Alias / ILM / Ingest / Template / Snapshot / Tasks / Cat / 健康探针
- ✅ **v0.1.x** (扩展能力):HTTP/2(h2c+h2)/ TLS / mTLS / gzip / 跨索引通配 / Basic+ApiKey / IP 限速 / 优雅关闭 / Prometheus metrics(#14) / 结构化访问日志+request trace(#12) / 健康端点深化(#13)
- ✅ **v0.1.y** (引擎增强):倒排排序索引 / bulk 写合并(#9) / 配置热加载+Schema校验 / reindex 取消回滚 / Web UI 多 Tab+历史+图表
- ✅ **v0.1.z** (真实快照):NDJSON 文件级快照 / 跨 store 恢复 / 完整性校验 / 物理文件删除(#16)

---

## 已完成但未列入原评估清单的能力

以下能力在开发过程中实现，超出了最初 43 项评估范围，现补充记录：

### A. 配置热加载 + YAML Schema 校验 ✅ (2026-08-06)
- **内容**: `internal/server/config.go` + `gopkg.in/yaml.v3`；`ConfigLoader` mtime 轮询(默认 5s)；`cmd/server/main.go` 加 `-config` 启动参数；只对 auth/limit 生效，addr/data/tls 需重启
- **Schema 校验**: `internal/server/config_schema.go` 数据驱动规则引擎；支持 required/type/range/enum/pattern/min_len/max_len/cross_field 规则；启动期 fail-fast 校验
- **测试**: 17 个 `TestSchema_*` 单元测试 + 3 个集成测试(坏配置启动非 0 退出)

### B. TLS / HTTP-2 over h2 ✅ (2026-08-06)
- **内容**: `-tls.cert`/`-tls.key`/`-tls.enable-http2` 启动参数；`configureTransport()` 支持 TLS + h2 协商；明文路径保留 h2c
- **证书验证**: 启动期 `os.Stat` + `tls.LoadX509KeyPair` 双校验
- **测试**: TLS 单元测试 + `docker-compose.tls.test.yml` + `e2e-tls-tests.sh` 9/9 通过

### C. mTLS 双向认证 ✅ (2026-08-06)
- **内容**: `-tls.client-ca`/`-tls.client-auth` 参数；`ClientAuthKind` 枚举(none/request/require_any/require_verify)；自动生成 CA + server cert + client cert
- **测试**: `docker-compose.mtls.test.yml` + `e2e-mtls-tests.sh` 10/10 通过(双向握手/拒绝无 cert/拒绝错误 CA)

### D. gzip 实际压缩 ✅ (2026-08-05)
- **内容**: 合并 statusWriter 与 gzipWriter 为 `compressingWriter`；只对 application/json 响应且 ≥512B 启动压缩；4xx 跳过；`/metrics` 跳过
- **实现位置**: `internal/server/gzip.go`，集成在 `middlewareMetrics` 中间件层

### E. reindex 取消回滚 ✅ (2026-08-06)
- **内容**: `runReindex` 记录 `written []string`，取消时逐个反向 Delete(store + engine)；双检查点避免竞态
- **数据竞争修复**: `taskEntry` 加 `sync.Mutex`，`go test -race` 全通过
- **测试**: 3 个 `TestTasks_Reindex*` 单元测试 + e2e section 4b

### F. 倒排排序索引 ✅ (2026-08-05)
- **内容**: `internal/search/sorted_index.go` 维护每个 (index, field) 的内存排序倒排；IndexDoc/DeleteDoc 增量更新；`evalRange` 走二分定位 O(logN+K)；复杂类型自动回退全表扫描
- **测试**: range 查询性能对比测试

### G. Web UI 多项增强 ✅ (2026-08-06)
- **多 Tab 系统**: Tab 栏独立状态，新建/关闭/重命名，`localStorage` 持久化
- **历史查询**: 抽屉面板 + 内容搜索/类型筛选/时间筛选，`replayHistory()` 一键重跑
- **字段类型推断**: 拉 `_mapping` + 抽样，6 种类型自动切换控件
- **拖拽排序**: `draggable` + 拖出关闭 + 导入导出(JSON payload 校验)
- **历史图表**: 纯 SVG 24h 柱状图，search/agg 双柱并列
- **测试**: 5 个 `TestUI_*` 单元测试 + e2e section 9 全通过

---

## 备注

- **已实现的能力**(见 AGENTS.md 8.0)不在本评估范围,不再重复列
- **工时**为单人估算,实际可能 ±50%
- **优先级**基于"对 ES 兼容性的影响 × 实现成本"加权
- 每个建议都遵循项目约定: 单元测试 + e2e 断言 + 中文注释 + 不破坏向后兼容
- 任何"破坏现有 API/语义"的方案不在本评估中(如 v2 client、proto over HTTP/2)
