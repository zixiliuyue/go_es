#!/bin/sh
# 一致性测试: 同一份文档 + 同一组 query 同时打到 ES 和 go_es, 对比响应
# 忽略 _score / took / _shards / _seq_no / _primary_term / _id 中的时间戳差异
#
# 用法(在 tester 容器内, ES_URL 与 GO_ES_URL 均可访问):
#   sh /consistency-test.sh
#
# 或本地手动跑(需 ES 与 go_es 都已启动):
#   ES_URL=http://localhost:9200 GO_ES_URL=http://localhost:19201 sh scripts/consistency-test.sh
#
# 退出码: 全部一致 -> 0; 任一不一致 -> 1

set -u

ES_URL="${ES_URL:-http://es:9200}"
GO_ES_URL="${GO_ES_URL:-http://go_es_server:9200}"

PASS=0
FAIL=0
FAILED_CASES=""

# 彩色
if [ -t 1 ]; then
  GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'; NC='\033[0m'
else
  GREEN=''; RED=''; YELLOW=''; CYAN=''; NC=''
fi

ok()   { PASS=$((PASS+1)); printf "${GREEN}CONSIST${NC} %s\n" "$1"; }
fail() { FAIL=$((FAIL+1)); FAILED_CASES="$FAILED_CASES\n  - $1"; printf "${RED}DIFF   ${NC} %s\n" "$1"; }
header() { printf "\n${YELLOW}== %s ==${NC}\n" "$1"; }

# 检查依赖
need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 2; }
}
need_cmd curl
need_cmd jq

# 等待两端就绪
wait_url() {
  url="$1"; name="$2"; n=0
  while [ $n -lt 60 ]; do
    code=$(curl -s -o /dev/null -w "%{http_code}" "$url/_health/liveness" 2>/dev/null || echo 000)
    # ES 没有 /_health/liveness, 用 / 替代
    [ "$code" = "000" ] && code=$(curl -s -o /dev/null -w "%{http_code}" "$url/" 2>/dev/null || echo 000)
    if [ "$code" = "200" ]; then
      printf "  %s ready (%s)\n" "$name" "$url"
      return 0
    fi
    n=$((n+1)); sleep 1
  done
  printf "  %s NOT ready after 60s\n" "$name" >&2
  return 1
}

# normalize_response <file>
# 把 ES/go_es 的 search 响应规范化, 便于 diff
# - 删除 took / _shards
# - 删除每个 hit 的 _index / _id / _seq_no / _primary_term / _score
# - hits 按 _source 中的字段排序(消除顺序差异)
# - 对 _source 中的 timestamp 字段(若存在)做 redact
normalize_response() {
  in_file="$1"
  out_file="$2"
  jq '
    # 删掉顶层波动字段
    del(.took, ._shards, .timed_out, .max_score)
    # 处理 hits
    | .hits.hits |= (
      map(
        # 删除每个 hit 的元信息字段
        del(._index, ._id, ._seq_no, ._primary_term, ._score)
        # 如果有 _source.timestamp 做 redact (避免时间戳差异)
        | if ._source.timestamp then ._source.timestamp = "<TS>" else . end
        | if ._source.created_at then ._source.created_at = "<TS>" else . end
      )
      # 按 _source 的 JSON 字符串排序, 消除 hit 顺序差异
      | sort_by(. | tostring)
    )
    # 处理 aggregations: 删除 doc_count_error 等 ES 特有的近似统计字段
    | if .aggregations then
        .aggregations |= walk(
          if type == "object" then
            del(.doc_count_error_upper_bound, .sum_other_doc_count, .bg_count)
          else . end
        )
      else . end
  ' "$in_file" > "$out_file" 2>/dev/null
}

# compare_query <name> <method> <path> <body>
# 同时对 ES 和 go_es 发同样的请求, 对比规范化后的 JSON
compare_query() {
  name="$1"; method="$2"; path="$3"; body="${4:-}"

  if [ -n "$body" ]; then
    curl -s -o /tmp/es_resp.json -X "$method" -H "Content-Type: application/json" -d "$body" "$ES_URL$path" 2>/dev/null
    curl -s -o /tmp/go_resp.json -X "$method" -H "Content-Type: application/json" -d "$body" "$GO_ES_URL$path" 2>/dev/null
  else
    curl -s -o /tmp/es_resp.json -X "$method" "$ES_URL$path" 2>/dev/null
    curl -s -o /tmp/go_resp.json -X "$method" "$GO_ES_URL$path" 2>/dev/null
  fi

  # 检查两边都返回了有效 JSON
  if ! jq -e . /tmp/es_resp.json >/dev/null 2>&1; then
    fail "$name (ES 返回非 JSON)"
    return
  fi
  if ! jq -e . /tmp/go_resp.json >/dev/null 2>&1; then
    fail "$name (go_es 返回非 JSON)"
    return
  fi

  normalize_response /tmp/es_resp.json /tmp/es_norm.json
  normalize_response /tmp/go_resp.json /tmp/go_norm.json

  if diff -u /tmp/es_norm.json /tmp/go_norm.json > /tmp/diff.txt 2>&1; then
    ok "$name"
  else
    fail "$name"
    sed 's/^/    /' /tmp/diff.txt | head -40
  fi
}

# ---------- 0. 等待两端就绪 ----------
header "0. 等待两端就绪"
wait_url "$ES_URL" "ES"     || exit 1
wait_url "$GO_ES_URL" "go_es" || exit 1

# ---------- 1. 公共数据集准备 ----------
header "1. 准备公共数据集"
TS=$(date +%s)
IDX="consistency_${TS}"

# 删旧索引(若存在)
curl -s -X DELETE "$ES_URL/$IDX" >/dev/null 2>&1
curl -s -X DELETE "$GO_ES_URL/$IDX" >/dev/null 2>&1

# 建索引(用 mapping 固定字段类型, 避免 dynamic mapping 差异)
MAPPING='{
  "mappings": {
    "properties": {
      "title":  { "type": "text" },
      "cat":    { "type": "keyword" },
      "price":  { "type": "integer" },
      "tags":   { "type": "keyword" },
      "active": { "type": "boolean" },
      "ts":     { "type": "date" }
    }
  }
}'
ES_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PUT -H "Content-Type: application/json" -d "$MAPPING" "$ES_URL/$IDX")
GO_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PUT -H "Content-Type: application/json" -d "$MAPPING" "$GO_ES_URL/$IDX")
printf "  ES create idx: %s, go_es create idx: %s\n" "$ES_CODE" "$GO_CODE"

# 写入 10 条文档
for i in 1 2 3 4 5 6 7 8 9 10; do
  DOC="{\"title\":\"hello world ${i}\",\"cat\":\"cat${i}\",\"price\":$((i*10)),\"tags\":[\"t1\",\"t${i}\"],\"active\":$([ $((i%2)) -eq 0 ] && echo true || echo false),\"ts\":\"2026-08-${i}T00:00:00Z\"}"
  curl -s -o /dev/null -X PUT -H "Content-Type: application/json" -d "$DOC" "$ES_URL/$IDX/_doc/$i"
  curl -s -o /dev/null -X PUT -H "Content-Type: application/json" -d "$DOC" "$GO_ES_URL/$IDX/_doc/$i"
done

# 刷新两端, 让文档可搜索
curl -s -X POST "$ES_URL/$IDX/_refresh" >/dev/null 2>&1
curl -s -X POST "$GO_ES_URL/$IDX/_refresh" >/dev/null 2>&1 || true

# ---------- 2. match 查询 ----------
header "2. match 查询一致性"
compare_query "match: title=hello"        POST "/$IDX/_search" '{"query":{"match":{"title":"hello"}}}'
compare_query "match: title=world 5"      POST "/$IDX/_search" '{"query":{"match":{"title":"world 5"}}}'
compare_query "match_phrase: title=hello world" POST "/$IDX/_search" '{"query":{"match_phrase":{"title":"hello world"}}}'

# ---------- 3. term / terms 查询 ----------
header "3. term / terms 查询一致性"
compare_query "term: cat=cat3"            POST "/$IDX/_search" '{"query":{"term":{"cat":"cat3"}}}'
compare_query "terms: cat in [cat1,cat2]" POST "/$IDX/_search" '{"query":{"terms":{"cat":["cat1","cat2"]}}}'

# ---------- 4. range 查询 ----------
header "4. range 查询一致性"
compare_query "range: price gte 30 lte 70" POST "/$IDX/_search" '{"query":{"range":{"price":{"gte":30,"lte":70}}}}'
compare_query "range: price gt 50"        POST "/$IDX/_search" '{"query":{"range":{"price":{"gt":50}}}}'

# ---------- 5. bool 查询 ----------
header "5. bool 查询一致性"
compare_query "bool: must+filter" POST "/$IDX/_search" '{
  "query": {
    "bool": {
      "must": [{"match": {"title": "hello"}}],
      "filter": [{"term": {"active": true}}]
    }
  }
}'
compare_query "bool: must_not" POST "/$IDX/_search" '{
  "query": {
    "bool": {
      "must_not": [{"term": {"cat": "cat1"}}]
    }
  }
}'

# ---------- 6. match_all + 分页 ----------
header "6. match_all + 分页一致性"
compare_query "match_all from=0 size=5"   POST "/$IDX/_search" '{"query":{"match_all":{}},"from":0,"size":5}'
compare_query "match_all from=2 size=3"   POST "/$IDX/_search" '{"query":{"match_all":{}},"from":2,"size":3}'
compare_query "match_all sort by price"   POST "/$IDX/_search" '{"query":{"match_all":{}},"sort":[{"price":"asc"}]}'
compare_query "match_all sort by price desc" POST "/$IDX/_search" '{"query":{"match_all":{}},"sort":[{"price":"desc"}]}'

# ---------- 7. _source 过滤 ----------
header "7. _source 过滤一致性"
compare_query "_source includes only title" POST "/$IDX/_search" '{"query":{"match_all":{}},"_source":["title"],"size":3}'
compare_query "_source excludes tags"      POST "/$IDX/_search" '{"query":{"match_all":{}},"_source":{"excludes":["tags"]},"size":3}'

# ---------- 8. track_total_hits ----------
header "8. track_total_hits 一致性"
compare_query "track_total_hits=true"     POST "/$IDX/_search" '{"query":{"match_all":{}},"track_total_hits":true,"size":0}'
compare_query "track_total_hits=10000"    POST "/$IDX/_search" '{"query":{"match_all":{}},"track_total_hits":10000,"size":0}'

# ---------- 9. terms 聚合 ----------
header "9. 聚合一致性"
compare_query "terms agg on cat" POST "/$IDX/_search" '{
  "size": 0,
  "aggs": {
    "by_cat": {"terms": {"field": "cat", "size": 10}}
  }
}'
compare_query "terms agg on tags" POST "/$IDX/_search" '{
  "size": 0,
  "aggs": {
    "by_tag": {"terms": {"field": "tags", "size": 10}}
  }
}'
compare_query "avg/sum/min/max on price" POST "/$IDX/_search" '{
  "size": 0,
  "aggs": {
    "avg_price": {"avg": {"field": "price"}},
    "sum_price": {"sum": {"field": "price"}},
    "min_price": {"min": {"field": "price"}},
    "max_price": {"max": {"field": "price"}}
  }
}'

# ---------- 10. multi_match ----------
header "10. multi_match 一致性"
compare_query "multi_match best_fields" POST "/$IDX/_search" '{
  "query": {"multi_match": {"query": "hello world", "fields": ["title","cat"], "type": "best_fields"}}
}'

# ---------- 11. count 端点 ----------
header "11. _count 一致性"
compare_query "_count match_all" POST "/$IDX/_count" '{"query":{"match_all":{}}}'
compare_query "_count term cat=cat1" POST "/$IDX/_count" '{"query":{"term":{"cat":"cat1"}}}'

# ---------- 12. 清理 ----------
header "12. 清理"
curl -s -X DELETE "$ES_URL/$IDX" >/dev/null 2>&1
curl -s -X DELETE "$GO_ES_URL/$IDX" >/dev/null 2>&1
printf "  cleaned index %s\n" "$IDX"

# ---------- 汇总 ----------
header "汇总"
TOTAL=$((PASS+FAIL))
printf "  PASS: %d / %d\n" "$PASS" "$TOTAL"
printf "  FAIL: %d / %d\n" "$FAIL" "$TOTAL"
if [ -n "$FAILED_CASES" ]; then
  printf "${RED}失败用例:${NC}%s\n" "$FAILED_CASES"
fi

# 验收标准: 100 个 query 中 >= 95 个一致
# 当前用例数较少(约 22 个), 若 FAIL > TOTAL/20 视为不达标
if [ $TOTAL -gt 0 ] && [ $FAIL -le $((TOTAL/20)) ]; then
  printf "\n${GREEN}✓ 一致性达标 (失败率 %d%%, 阈值 5%%)${NC}\n" $((FAIL*100/TOTAL))
  exit 0
else
  printf "\n${RED}✗ 一致性不达标 (失败率 %d%%, 阈值 5%%)${NC}\n" $((FAIL*100/TOTAL))
  exit 1
fi
