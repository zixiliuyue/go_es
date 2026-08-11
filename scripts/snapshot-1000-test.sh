#!/bin/sh
# 1000 条文档快照与恢复 E2E 测试
# 用法: bash scripts/snapshot-1000-test.sh [GO_ES_URL]
# 默认: GO_ES_URL=http://localhost:9200

set -u

GO_ES_URL="${1:-http://localhost:9200}"
TIMEOUT="${TIMEOUT:-30}"

PASS=0
FAIL=0
FAILED_TESTS=""

# 彩色
if [ -t 1 ]; then
  GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'; NC='\033[0m'
else
  GREEN=''; RED=''; YELLOW=''; CYAN=''; NC=''
fi

ok() { PASS=$((PASS+1)); printf "${GREEN}PASS${NC} %s\n" "$1"; }
fail() { FAIL=$((FAIL+1)); FAILED_TESTS="$FAILED_TESTS\n  - $1"; printf "${RED}FAIL${NC} %s\n" "$1"; }
info() { printf "${CYAN}INFO${NC} %s\n" "$1"; }
warn() { printf "${YELLOW}WARN${NC} %s\n" "$1"; }

assert_status() {
  name="$1"; expected="$2"; method="$3"; url="$4"; body="${5:-}"
  if [ -n "$body" ]; then
    code=$(curl -s -o /tmp/last.json -w "%{http_code}" --max-time "$TIMEOUT" -X "$method" -H "Content-Type: application/json" -d "$body" "$url" 2>/dev/null || echo 000)
  else
    code=$(curl -s -o /tmp/last.json -w "%{http_code}" --max-time "$TIMEOUT" -X "$method" "$url" 2>/dev/null || echo 000)
  fi
  if [ "$code" = "$expected" ]; then
    ok "$name (HTTP $code)"
  else
    body_short=$(head -c 200 /tmp/last.json 2>/dev/null)
    fail "$name" "want $expected got $code body=$body_short"
  fi
}

# ========== 开始 ==========
printf "${CYAN}========================================${NC}\n"
printf "${CYAN}  1000 文档快照与恢复 E2E 测试${NC}\n"
printf "${CYAN}  目标: %s${NC}\n" "$GO_ES_URL"
printf "${CYAN}========================================${NC}\n\n"

# 检查服务可用性
info "检查服务可用性..."
RES=$(curl -s --max-time 5 "$GO_ES_URL" 2>/dev/null)
if [ -z "$RES" ]; then
  fail "服务检查" "无法连接到 $GO_ES_URL"
  echo "请先启动 go_es_server: go run cmd/server/main.go"
  exit 1
fi
ok "服务可用"

# ========== 1. 创建测试索引 ==========
printf "\n${YELLOW}--- 1. 创建测试索引 ---${NC}\n"
TS=$(date +%s)
IDX="products_${TS}"
REPO="repo_${TS}"
SNAP="snap_${TS}"

assert_status "创建索引" 200 PUT "$GO_ES_URL/$IDX"

# ========== 2. 创建快照仓库 ==========
printf "\n${YELLOW}--- 2. 创建快照仓库 ---${NC}\n"
assert_status "创建仓库" 200 PUT "$GO_ES_URL/_snapshot/$REPO" \
  '{"type":"fs","settings":{"location":"/tmp/snapshots"}}'

# ========== 3. 写入 1000 条文档 ==========
printf "\n${YELLOW}--- 3. 写入 1000 条文档 ---${NC}\n"
info "正在写入 1000 条 mock 文档..."

WRITE_START=$(date +%s%N)
for n in $(seq 1 1000); do
  PRD_IDX=$(( (n-1) % 10 ))
  case $PRD_IDX in
    0) PRD="laptop" ;;
    1) PRD="phone" ;;
    2) PRD="tablet" ;;
    3) PRD="monitor" ;;
    4) PRD="keyboard" ;;
    5) PRD="mouse" ;;
    6) PRD="headphone" ;;
    7) PRD="camera" ;;
    8) PRD="printer" ;;
    9) PRD="speaker" ;;
  esac
  CAT_IDX=$(( (n-1) % 5 ))
  case $CAT_IDX in
    0) CAT="electronics" ;;
    1) CAT="office" ;;
    2) CAT="gaming" ;;
    3) CAT="mobile" ;;
    4) CAT="accessories" ;;
  esac

  curl -s --max-time 2 -X PUT "$GO_ES_URL/$IDX/_doc/doc_$(printf '%04d' $n)" \
    -H 'Content-Type: application/json' \
    -d "{
      \"name\": \"${PRD}_${n}\",
      \"description\": \"High quality ${PRD} for ${CAT} users, model-${n}, latest edition with premium features and extended warranty coverage up to 3 years\",
      \"price\": $(( n * 10 + 99 )),
      \"stock\": $(( n * 7 )),
      \"rating\": $(( 3 + n % 3 )).5,
      \"category\": \"${CAT}\",
      \"tags\": [\"${PRD}\", \"${CAT}\", \"tag_$(( n % 20 ))\"],
      \"active\": $( [ $(( n % 3 )) -ne 0 ] && echo true || echo false ),
      \"created_at\": \"2026-$(printf '%02d' $(( 1 + n % 12 )))-$(printf '%02d' $(( 1 + n % 28 )))T10:30:00Z\",
      \"metadata\": {
        \"weight\": $(( n % 50 )).1,
        \"color\": \"color_$(( n % 10 ))\",
        \"seller_id\": \"seller_$(printf '%03d' $(( n % 50 )))\"
      }
    }" >/dev/null
done
WRITE_END=$(date +%s%N)
WRITE_MS=$(( (WRITE_END - WRITE_START) / 1000000 ))
ok "写入 1000 条文档 (耗时 ${WRITE_MS}ms)"

# 验证写入数量
RES=$(curl -s --max-time "$TIMEOUT" -X POST "$GO_ES_URL/$IDX/_search" \
  -H 'Content-Type: application/json' \
  -d '{"query":{"match_all":{}},"size":0}')
COUNT=$(echo "$RES" | jq -r '.hits.total.value // 0' 2>/dev/null)
if [ "$COUNT" = "1000" ]; then
  ok "索引文档数: 1000"
else
  fail "索引文档数" "got=$COUNT (expect 1000)"
fi

# ========== 4. 创建快照 ==========
printf "\n${YELLOW}--- 4. 创建快照 ---${NC}\n"
SNAP_START=$(date +%s%N)
RES=$(curl -s --max-time "$TIMEOUT" -w '\n%{http_code}' -X PUT "$GO_ES_URL/_snapshot/$REPO/$SNAP")
SNAP_CODE=$(echo "$RES" | tail -n1)
SNAP_BODY=$(echo "$RES" | sed '$d')
SNAP_END=$(date +%s%N)
SNAP_MS=$(( (SNAP_END - SNAP_START) / 1000000 ))

if [ "$SNAP_CODE" = "200" ]; then
  ok "创建快照 (HTTP 200, 耗时 ${SNAP_MS}ms)"
else
  fail "创建快照" "HTTP $SNAP_CODE"
fi

# 验证快照内容
ACCEPTED=$(echo "$SNAP_BODY" | jq -r '.accepted // false' 2>/dev/null)
DOC_COUNT=$(echo "$SNAP_BODY" | jq -r '.doc_count // 0' 2>/dev/null)
if [ "$ACCEPTED" = "true" ] && [ "$DOC_COUNT" = "1000" ]; then
  ok "快照响应: accepted=true, doc_count=1000"
else
  fail "快照响应" "accepted=$ACCEPTED doc_count=$DOC_COUNT body=$(echo "$SNAP_BODY" | head -c 200)"
fi

# ========== 5. 验证快照元信息 ==========
printf "\n${YELLOW}--- 5. 验证快照元信息 ---${NC}\n"
RES=$(curl -s --max-time "$TIMEOUT" "$GO_ES_URL/_snapshot/$REPO/$SNAP")
STATE=$(echo "$RES" | jq -r '.snapshots[0].state // ""' 2>/dev/null)
REPO_RET=$(echo "$RES" | jq -r '.snapshots[0].repository // ""' 2>/dev/null)
SNAP_RET=$(echo "$RES" | jq -r '.snapshots[0].snapshot // ""' 2>/dev/null)
if [ "$STATE" = "SUCCESS" ] && [ "$REPO_RET" = "$REPO" ] && [ "$SNAP_RET" = "$SNAP" ]; then
  ok "快照元信息: state=SUCCESS repo=$REPO snap=$SNAP"
else
  fail "快照元信息" "state=$STATE repo=$REPO snap=$SNAP"
fi

# ========== 6. 删除原数据 ==========
printf "\n${YELLOW}--- 6. 删除原数据 ---${NC}\n"
DEL_START=$(date +%s%N)
for n in $(seq 1 1000); do
  curl -s --max-time 2 -X DELETE "$GO_ES_URL/$IDX/_doc/doc_$(printf '%04d' $n)" >/dev/null
done
DEL_END=$(date +%s%N)
DEL_MS=$(( (DEL_END - DEL_START) / 1000000 ))
ok "删除 1000 条文档 (耗时 ${DEL_MS}ms)"

# 验证已删除
RES=$(curl -s --max-time "$TIMEOUT" -X POST "$GO_ES_URL/$IDX/_search" \
  -H 'Content-Type: application/json' \
  -d '{"query":{"match_all":{}},"size":0}')
COUNT=$(echo "$RES" | jq -r '.hits.total.value // 0' 2>/dev/null)
if [ "$COUNT" = "0" ]; then
  ok "原数据已清空 (0 docs)"
else
  fail "删除验证" "got=$COUNT docs still exist"
fi

# ========== 7. 从快照恢复 ==========
printf "\n${YELLOW}--- 7. 从快照恢复 ---${NC}\n"
REST_START=$(date +%s%N)
RES=$(curl -s --max-time "$TIMEOUT" -w '\n%{http_code}' -X POST "$GO_ES_URL/_snapshot/$REPO/$SNAP/_restore")
REST_CODE=$(echo "$RES" | tail -n1)
REST_BODY=$(echo "$RES" | sed '$d')
REST_END=$(date +%s%N)
REST_MS=$(( (REST_END - REST_START) / 1000000 ))

if [ "$REST_CODE" = "200" ]; then
  ok "恢复快照 (HTTP 200, 耗时 ${REST_MS}ms)"
else
  fail "恢复快照" "HTTP $REST_CODE body=$(echo "$REST_BODY" | head -c 200)"
fi

# 验证恢复响应字段
HAS_RESTORED=$(echo "$REST_BODY" | jq 'has("restored_docs")' 2>/dev/null)
HAS_EXPECTED=$(echo "$REST_BODY" | jq 'has("expected_docs")' 2>/dev/null)
if [ "$HAS_RESTORED" = "true" ] && [ "$HAS_EXPECTED" = "true" ]; then
  ok "恢复响应包含 restored_docs 和 expected_docs 字段"
else
  fail "恢复响应字段" "restored_docs=$HAS_RESTORED expected_docs=$HAS_EXPECTED"
fi

# ========== 8. 验证恢复后数据 ==========
printf "\n${YELLOW}--- 8. 验证恢复后数据 ---${NC}\n"

# 8a. 数量验证
RES=$(curl -s --max-time "$TIMEOUT" -X POST "$GO_ES_URL/$IDX/_search" \
  -H 'Content-Type: application/json' \
  -d '{"query":{"match_all":{}},"size":0}')
COUNT=$(echo "$RES" | jq -r '.hits.total.value // 0' 2>/dev/null)
if [ "$COUNT" = "1000" ]; then
  ok "恢复后文档数: 1000"
else
  fail "恢复后文档数" "got=$COUNT (expect 1000)"
fi

# 8b. 抽样获取验证
SAMPLES="doc_0001 doc_0100 doc_0500 doc_0999 doc_1000"
ALL_SAMPLES_OK=true
for SAMPLE in $SAMPLES; do
  RES=$(curl -s --max-time "$TIMEOUT" "$GO_ES_URL/$IDX/_doc/$SAMPLE")
  FOUND=$(echo "$RES" | jq -r 'found // false' 2>/dev/null)
  NAME=$(echo "$RES" | jq -r '._source.name // ""' 2>/dev/null)
  if [ "$FOUND" != "true" ]; then
    warn "文档 $SAMPLE 未找到"
    ALL_SAMPLES_OK=false
  elif [ -z "$NAME" ]; then
    warn "文档 $SAMPLE 缺少 name 字段"
    ALL_SAMPLES_OK=false
  fi
done
if [ "$ALL_SAMPLES_OK" = "true" ]; then
  ok "抽样验证通过: doc_0001, doc_0100, doc_0500, doc_0999, doc_1000"
else
  fail "抽样验证" "部分文档内容异常"
fi

# 8c. 搜索验证
RES=$(curl -s --max-time "$TIMEOUT" -X POST "$GO_ES_URL/$IDX/_search" \
  -H 'Content-Type: application/json' \
  -d '{"query":{"match":{"name":"laptop_1"}},"size":5}')
SEARCH_COUNT=$(echo "$RES" | jq -r '.hits.total.value // 0' 2>/dev/null)
if [ "$SEARCH_COUNT" -gt 0 ]; then
  ok "搜索 'laptop_1' 命中 ${SEARCH_COUNT} 条"
else
  fail "搜索验证" "got 0 results"
fi

# 8d. 聚合验证
RES=$(curl -s --max-time "$TIMEOUT" -X POST "$GO_ES_URL/$IDX/_search" \
  -H 'Content-Type: application/json' \
  -d '{"query":{"match_all":{}},"aggs":{"by_category":{"terms":{"field":"category"}}},"size":0}')
BUCKETS=$(echo "$RES" | jq '.aggregations.by_category.buckets | length' 2>/dev/null)
if [ "$BUCKETS" -gt 0 ]; then
  ok "分类聚合: ${BUCKETS} 个桶"
else
  fail "聚合验证" "no buckets"
fi

# ========== 9. 快照删除测试 ==========
printf "\n${YELLOW}--- 9. 快照删除测试 ---${NC}\n"
assert_status "删除快照" 200 DELETE "$GO_ES_URL/_snapshot/$REPO/$SNAP"
assert_status "获取已删除快照 -> 404" 404 GET "$GO_ES_URL/_snapshot/$REPO/$SNAP"
assert_status "恢复已删除快照 -> 404" 404 POST "$GO_ES_URL/_snapshot/$REPO/$SNAP/_restore"
assert_status "删除仓库" 200 DELETE "$GO_ES_URL/_snapshot/$REPO"

# ========== 10. 清理测试数据 ==========
printf "\n${YELLOW}--- 10. 清理测试数据 ---${NC}\n"
assert_status "删除测试索引" 200 DELETE "$GO_ES_URL/$IDX"

# ========== 总结 ==========
echo
printf "${CYAN}========================================${NC}\n"
printf "${YELLOW}== 总结 ==${NC}\n"
printf "PASS=%d  FAIL=%d\n" "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then
  printf "${RED}失败项:${NC}%b\n" "$FAILED_TESTS"
  exit 1
fi

# 输出性能摘要
echo
printf "${GREEN}性能摘要:${NC}\n"
printf "  写入 1000 条:  %d ms\n" "$WRITE_MS"
printf "  创建快照:      %d ms\n" "$SNAP_MS"
printf "  删除 1000 条:  %d ms\n" "$DEL_MS"
printf "  恢复快照:      %d ms\n" "$REST_MS"
echo
printf "${GREEN}全部测试通过!${NC}\n"
exit 0