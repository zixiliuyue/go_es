#!/bin/sh
# TLS 专用 e2e 断言(只在启用 TLS 的 compose 中跑)
# 与 e2e-tests.sh 互补: e2e-tests.sh 覆盖明文 h2c 路径, 这里只覆盖 TLS/h2 路径
# 测试用自签名证书, 容器内 curl 必须加 -k

set -u

GO_ES_TLS_URL="${GO_ES_TLS_URL:-https://go_es_server:9200}"

PASS=0
FAIL=0
FAILED_TESTS=""

# 彩色(若 tty)
if [ -t 1 ]; then
  GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; NC='\033[0m'
else
  GREEN=''; RED=''; YELLOW=''; NC=''
fi

# 需要 curl + jq + openssl
apk add --no-cache curl jq openssl >/dev/null 2>&1 || true

ok() { PASS=$((PASS+1)); printf "${GREEN}PASS${NC} %s\n" "$1"; }
fail() { FAIL=$((FAIL+1)); FAILED_TESTS="$FAILED_TESTS\n  - $1"; printf "${RED}FAIL${NC} %s -- %s\n" "$1" "$2"; }
header() { printf "\n${YELLOW}== %s ==${NC}\n" "$1"; }

# ---------- 1. TLS 握手 ----------
header "1. TLS 握手 + 业务端点"
code=$(curl -sk -o /tmp/last.json -w "%{http_code}" --max-time 5 "$GO_ES_TLS_URL/_health/liveness")
if [ "$code" = "200" ]; then
  ok "TLS handshake + liveness=200"
else
  fail "TLS handshake" "code=$code"
fi
if grep -q '"status"' /tmp/last.json 2>/dev/null; then
  ok "liveness body 含 status 字段"
else
  fail "liveness body" "no status field, body=$(cat /tmp/last.json 2>/dev/null)"
fi

# readiness
code=$(curl -sk -o /tmp/last.json -w "%{http_code}" "$GO_ES_TLS_URL/_health/readiness")
if [ "$code" = "200" ] && grep -q '"status":"ready"' /tmp/last.json; then
  ok "TLS readiness=200 ready"
else
  fail "TLS readiness" "code=$code body=$(cat /tmp/last.json 2>/dev/null)"
fi

# ---------- 2. ALPN 协商到 h2 ----------
header "2. ALPN 协商到 h2"
HOST_PORT="${GO_ES_TLS_URL#https://}"
ALPN=$(echo | openssl s_client -connect "$HOST_PORT" -alpn h2,http/1.1 -servername localhost 2>/dev/null | awk -F': ' '/^ALPN protocol/{print $2}' | tr -d '\r\n')
if [ "$ALPN" = "h2" ]; then
  ok "ALPN 协商到 h2"
else
  fail "ALPN h2 协商" "got='$ALPN'"
fi

# ---------- 3. 业务写读 ----------
header "3. TLS 路径下完整 CRUD"
TS=$(date +%s)
IDX="tls_${TS}"
# PUT index
code=$(curl -sk -o /tmp/last.json -w "%{http_code}" -X PUT "$GO_ES_TLS_URL/$IDX")
if [ "$code" = "200" ]; then ok "PUT /$IDX=200"; else fail "PUT index" "code=$code"; fi
# PUT doc
code=$(curl -sk -o /tmp/last.json -w "%{http_code}" -X PUT "$GO_ES_TLS_URL/$IDX/_doc/1" -H 'Content-Type: application/json' -d '{"v":1,"name":"tls"}')
if [ "$code" = "200" ] || [ "$code" = "201" ]; then ok "PUT doc=200/201"; else fail "PUT doc" "code=$code"; fi
# GET doc
code=$(curl -sk -o /tmp/last.json -w "%{http_code}" "$GO_ES_TLS_URL/$IDX/_doc/1")
if [ "$code" = "200" ] && grep -q '"v":1' /tmp/last.json; then
  ok "GET doc 拿到 v=1"
else
  fail "GET doc" "code=$code body=$(cat /tmp/last.json 2>/dev/null)"
fi
# _search
RES=$(curl -sk -X POST "$GO_ES_TLS_URL/$IDX/_search" -H 'Content-Type: application/json' -d '{"query":{"match_all":{}}}')
N=$(echo "$RES" | jq -r '.hits.total.value // 0' 2>/dev/null)
if [ "$N" -ge 1 ] 2>/dev/null; then
  ok "_search match_all 命中 ($N)"
else
  fail "TLS _search" "body=$RES"
fi

# ---------- 4. /metrics 在 TLS 下仍可达 ----------
header "4. /metrics over TLS"
code=$(curl -sk -o /tmp/last.json -w "%{http_code}" "$GO_ES_TLS_URL/metrics")
if [ "$code" = "200" ] && grep -q 'go_es_http_requests_total' /tmp/last.json; then
  ok "/metrics over TLS 含 go_es_http_requests_total"
else
  fail "/metrics over TLS" "code=$code"
fi

# ---------- 总结 ----------
echo
printf "${YELLOW}== 总结 ==${NC}\n"
printf "PASS=%d  FAIL=%d\n" "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then
  printf "${RED}FAILED:${NC}%b\n" "$FAILED_TESTS"
  exit 1
fi
printf "${GREEN}全部通过${NC}\n"
exit 0
