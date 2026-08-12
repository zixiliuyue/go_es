#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# loadtest.sh - go_es 端到端压测脚本
#
# 用 vegeta (Go 写的 HTTP 负载工具) 对 go_es_server 打 4 个阶段:
#   1) warmup  : 批量写入 N 条文档(默认 10k), 建立倒排基线
#   2) read    : 只读压测, 混合 match_all / match / range / bool / multi_match 五种 query
#   3) write   : 只写压测, 用 POST /_bulk 发 100 条一批的 NDJSON
#   4) mixed   : 读写混合(默认 70% 读 + 30% 写), 模拟线上真实流量
#
# 输出:
#   每个阶段独立的 vegeta 文本报告(P50 / P95 / P99 / mean / QPS / success%)
#   + 汇总 CSV(供后续导入分析) + 全量 JSON result
#   + server 进程内存峰值(通过 ps RSS 每 1s 采样)
#
# 使用:
#   # 默认模式(smoke): 1 分钟, rate=500/s, warmup 1k 文档, 压已运行的 :9200
#   bash scripts/loadtest.sh
#
#   # CI smoke: 1 分钟, rate=500/s, 只跑 read+mixed(用于 PR gate)
#   bash scripts/loadtest.sh ci-smoke
#
#   # full: 10 分钟, rate=2000/s, warmup 10k 文档
#   bash scripts/loadtest.sh full -rate 2000 -dur 10m -warmup 10000 -url http://localhost:9201
#
#   # 只跑某个阶段:
#   bash scripts/loadtest.sh stage read -dur 2m -rate 1000 -url http://localhost:9200
#
#   # 自定义端口+启动内嵌服务(若本地没起 go_es_server):
#   bash scripts/loadtest.sh -server ./cmd/server -data /tmp/loadtest_data
#
# 参数(flag 形式, 可任意顺序):
#   -url <URL>           目标服务端地址, 默认 http://127.0.0.1:9200
#   -rate <n>            目标 RPS, 默认 smoke=500 / full=2000
#   -dur <duration>      每个阶段时长(1m/30s/2h), 默认 smoke=1m / full=10m
#   -warmup <n>          warmup 写入 doc 数, 默认 smoke=1000 / full=10000
#   -read-write-ratio R:W  mixed 阶段读:写比例, 默认 70:30
#   -index <name>        写入/查询索引名前缀, 默认 loadtest
#   -out <dir>           输出目录, 默认 ./loadtest-<ts>
#   -server <bin>        内嵌服务端二进制路径;若给出则脚本启动/关闭服务
#   -data <dir>          配合 -server 使用, 数据目录, 默认内存
#   -auth <user:pass>    Basic 认证用户名密码(如 admin:secret)
#   -apikey <token>      ApiKey 认证 token (与 -auth 二选一)
#   -keep                保留中间 json 结果文件(默认保留)
#
# 退出码:
#   0: 所有阶段完成, 且 read/mixed 阶段 QPS >= QPS_THRESHOLD(默认 500, 可 -qps 覆盖)
#   1: 参数错误 / vegeta 未找到且安装失败
#   2: 目标不可达 / warmup 写入失败
#   3: 压测完成但 QPS 不达标
# ---------------------------------------------------------------------------

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

# ------------------------------------------------------------------
# 默认参数(按模式)
# ------------------------------------------------------------------
MODE="${1:-smoke}"
[[ "$MODE" == -* ]] && MODE="smoke" || shift || true

case "$MODE" in
  smoke|"")       STAGE_RATE=500;   STAGE_DUR="1m";   WARMUP_N=1000;   STAGES="read write mixed" ;;
  ci-smoke)       STAGE_RATE=500;   STAGE_DUR="1m";   WARMUP_N=1000;   STAGES="read mixed" ;;
  full)           STAGE_RATE=2000;  STAGE_DUR="10m";  WARMUP_N=10000;  STAGES="read write mixed" ;;
  stage)          STAGE_RATE=1000;  STAGE_DUR="1m";   WARMUP_N=1000;   STAGES=""   # 用户必须指定
                  ;;
  -h|--help|help) : ;;
  *)              echo "未知模式: $MODE" >&2; exit 1 ;;
esac

TARGET_URL="http://127.0.0.1:9200"
READ_WRITE_RATIO="70:30"
INDEX_PREFIX="loadtest"
QPS_THRESHOLD=500
SERVER_BIN=""
SERVER_DATA=""
AUTH_BASIC=""
APIKEY=""
OUT_DIR=""

# 解析 flag 参数(与模式参数可交错)
while [[ $# -gt 0 ]]; do
  case "$1" in
    -url)              TARGET_URL="${2:?缺少值 for -url}"; shift 2 ;;
    -rate)             STAGE_RATE="${2:?缺少值 for -rate}"; shift 2 ;;
    -dur)              STAGE_DUR="${2:?缺少值 for -dur}"; shift 2 ;;
    -warmup)           WARMUP_N="${2:?缺少值 for -warmup}"; shift 2 ;;
    -read-write-ratio) READ_WRITE_RATIO="${2:?缺少值 for -read-write-ratio}"; shift 2 ;;
    -index)            INDEX_PREFIX="${2:?缺少值 for -index}"; shift 2 ;;
    -qps)              QPS_THRESHOLD="${2:?缺少值 for -qps}"; shift 2 ;;
    -out)              OUT_DIR="${2:?缺少值 for -out}"; shift 2 ;;
    -server)           SERVER_BIN="${2:?缺少值 for -server}"; shift 2 ;;
    -data)             SERVER_DATA="${2:?缺少值 for -data}"; shift 2 ;;
    -auth)             AUTH_BASIC="${2:?缺少值 for -auth}"; shift 2 ;;
    -apikey)           APIKEY="${2:?缺少值 for -apikey}"; shift 2 ;;
    -keep)             shift ;;
    *)
      # stage 模式下接收阶段名(可多次)
      if [[ "$MODE" == "stage" && "$1" =~ ^(read|write|mixed)$ ]]; then
        STAGES="${STAGES:+$STAGES }$1"; shift
      else
        echo "未知参数: $1" >&2; exit 1
      fi
      ;;
  esac
done

# 若 stage 模式但未给阶段, 报错
if [[ "$MODE" == "stage" && -z "$STAGES" ]]; then
  echo "stage 模式需要至少一个阶段: read / write / mixed" >&2; exit 1
fi

# 读:写比例拆成两个整数(用于 mixed 阶段)
RATIO_READ="${READ_WRITE_RATIO%%:*}"
RATIO_WRITE="${READ_WRITE_RATIO##*:}"
[[ "$RATIO_READ" =~ ^[0-9]+$ && "$RATIO_WRITE" =~ ^[0-9]+$ && $((RATIO_READ+RATIO_WRITE)) -gt 0 ]] || \
  { echo "无效的 read-write-ratio: $READ_WRITE_RATIO (格式 70:30)" >&2; exit 1; }

# ------------------------------------------------------------------
# 输出目录
# ------------------------------------------------------------------
TS="$(date '+%Y%m%d_%H%M%S')"
[[ -z "$OUT_DIR" ]] && OUT_DIR="$REPO_ROOT/loadtest-${TS}"
mkdir -p "$OUT_DIR"

# 统一日志
LOG_FILE="$OUT_DIR/loadtest.log"
log()  { echo "[$(date '+%Y-%m-%dT%H:%M:%S')] $*" | tee -a "$LOG_FILE"; }
die()  { log "❌ $*"; exit "${2:-1}"; }

# ------------------------------------------------------------------
# 工具: vegeta
# ------------------------------------------------------------------
ensure_vegeta() {
  local bin="${LOADTEST_VEGETA:-vegeta}"
  if ! command -v "$bin" >/dev/null 2>&1; then
    log "ℹ️  未找到 vegeta, 尝试: go install github.com/tsenart/vegeta@latest"
    GOFLAGS="-mod=mod" go install github.com/tsenart/vegeta@latest || \
      die "安装 vegeta 失败, 请手动安装后重试 (brew install vegeta / go install ...)" 1
    local gopath_bin
    gopath_bin="$(go env GOPATH)/bin/vegeta"
    if [[ -x "$gopath_bin" ]]; then bin="$gopath_bin"; else bin=vegeta; fi
  fi
  echo "$bin"
}

# 把 10s / 1m / 1h / 2m30s 这种 vegeta duration 转换为秒数 (无单位默认秒)
normalize_duration_seconds() {
  local s="${1:-0}" lower num unit rest total=0
  lower="$(printf '%s' "$s" | tr '[:upper:]' '[:lower:]')"
  while [[ -n "$lower" ]]; do
    if [[ "$lower" =~ ^([0-9]+)([a-z]+)(.*)$ ]]; then
      num="${BASH_REMATCH[1]}"
      unit="${BASH_REMATCH[2]}"
      rest="${BASH_REMATCH[3]}"
      case "$unit" in
        s|sec|secs|second|seconds) total=$(( total + num + 0 )) ;;
        m|min|mins|minute|minutes) total=$(( total + num * 60 )) ;;
        h|hr|hrs|hour|hours)       total=$(( total + num * 3600 )) ;;
        ms|msec|msecs)             total=$(( total + (num + 999)/1000 )) ;; # 向上取整 1ms
        us|µs|µsec)                total=$(( total + 1 )) ;;                # 至少 1s
        *) die "无法解析 duration 单位: $unit (输入=$s)" 1 ;;
      esac
      lower="$rest"
    elif [[ "$lower" =~ ^([0-9]+)$ ]]; then
      total=$(( total + BASH_REMATCH[1] ))
      lower=""
    else
      die "无法解析 duration: $s" 1
    fi
  done
  echo "$total"
}

# ------------------------------------------------------------------
# 构造请求头(认证)
# ------------------------------------------------------------------
AUTH_HEADERS=()
AUTH_CURL_ARGS=()
if [[ -n "$AUTH_BASIC" ]]; then
  AUTH_HEADERS+=("-header" "Authorization: Basic $(printf '%s' "$AUTH_BASIC" | base64)")
  AUTH_CURL_ARGS+=("-u" "$AUTH_BASIC")
fi
if [[ -n "$APIKEY" ]]; then
  AUTH_HEADERS+=("-header" "Authorization: ApiKey $APIKEY")
  AUTH_CURL_ARGS+=("-H" "Authorization: ApiKey $APIKEY")
fi
# 兼容 set -u: 空数组 [@] 展开会报错; 若为空则在前面加占位, 再 ${var[@]:0} 也不稳, 改用显式 wrapper:
# CURL_AUTH() 展开 AUTH_CURL_ARGS 为空时输出 "", 不为空展开内容; 调用: curl $(curl_auth_args)
curl_auth_args() { if (( ${#AUTH_CURL_ARGS[@]} )); then printf '%s\n' "${AUTH_CURL_ARGS[@]}"; fi; }
# vegeta_auth_headers() 同上
vegeta_auth_header_args() { if (( ${#AUTH_HEADERS[@]} )); then printf '%s\n' "${AUTH_HEADERS[@]}"; fi; }

# ------------------------------------------------------------------
# 内嵌服务端(可选)
# ------------------------------------------------------------------
SERVER_PID=""
start_server() {
  [[ -z "$SERVER_BIN" ]] && return 0
  [[ -x "$SERVER_BIN" ]] || die "服务端二进制不可执行: $SERVER_BIN" 2
  local logf="$OUT_DIR/server.log"
  local args=("-addr" "${TARGET_URL##*//}")   # 把 http://host:port 的 host:port 切出来
  if [[ -n "$SERVER_DATA" ]]; then args+=("-data" "$SERVER_DATA"); fi
  log "🚀 启动内嵌服务端: $SERVER_BIN ${args[*]}  (日志: $logf)"
  nohup "$SERVER_BIN" "${args[@]}" >"$logf" 2>&1 &
  SERVER_PID=$!
  disown "$SERVER_PID" 2>/dev/null || true
  # 等待健康检查 30s
  for i in $(seq 1 30); do
    # shellcheck disable=SC2046
    if curl -fsS $(curl_auth_args) "${TARGET_URL}/_health/readiness" >/dev/null 2>&1; then
      log "✅ 内嵌服务端就绪 (pid=$SERVER_PID)"
      return 0
    fi
    sleep 1
  done
  die "内嵌服务端 30s 内未就绪, 详见 $logf" 2
}

stop_server() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    log "🛑 关闭内嵌服务端 pid=$SERVER_PID"
    kill "$SERVER_PID" 2>/dev/null || true
    for i in $(seq 1 10); do
      kill -0 "$SERVER_PID" 2>/dev/null || return 0
      sleep 0.5
    done
    kill -9 "$SERVER_PID" 2>/dev/null || true
  fi
}

# ------------------------------------------------------------------
# 检查目标可达性
# ------------------------------------------------------------------
probe_target() {
  log "🔎 探测目标服务: ${TARGET_URL}"
  local body
  # shellcheck disable=SC2046
  if ! body="$(curl -fsS $(curl_auth_args) "$TARGET_URL/" 2>&1)"; then
    die "目标不可达: curl 失败 (${body})" 2
  fi
  log "✅ 服务端响应: $(echo "$body" | head -c 200)"
}

# ------------------------------------------------------------------
# 内存采样: 后台每 1s 记录目标 server 进程 RSS 峰值
#   若用户给定 -server, 则追踪 SERVER_PID;
#   否则尝试通过 lsof/ss 找到监听端口的进程
# ------------------------------------------------------------------
MEM_PID=""
MEM_FILE="$OUT_DIR/memory.csv"
MEM_MAX_KB=0
MEM_SAMPLER_PID=""

find_listen_pid() {
  local port="${TARGET_URL##*:}"
  port="${port%%/*}"
  # Linux: ss / macOS: lsof
  local pid=""
  if command -v lsof >/dev/null 2>&1; then
    pid="$(lsof -n -iTCP:"${port}" -sTCP:LISTEN -t 2>/dev/null | head -1)"
  elif command -v ss >/dev/null 2>&1; then
    pid="$(ss -ltnp 2>/dev/null | awk -v p=":$port" '$4~p {split($7,a,","); for (k in a) if (a[k] ~ /pid=/) {gsub(/pid=/,"",a[k]); print a[k]; exit}}')"
  fi
  echo "$pid"
}

start_mem_sampler() {
  # 先尝试用 SERVER_PID, 或尝试找到监听端口的进程
  MEM_PID="${SERVER_PID:-}"
  if [[ -z "$MEM_PID" ]]; then
    for _ in 1 2 3 4 5; do
      MEM_PID="$(find_listen_pid)"; [[ -n "$MEM_PID" ]] && break
      sleep 1
    done
  fi
  [[ -z "$MEM_PID" ]] && { log "⚠️  未能找到 server 进程, 跳过内存采样"; return 0; }
  : >"$MEM_FILE"
  log "📊 内存采样: 追踪 pid=$MEM_PID, 写入 $MEM_FILE (每 1s)"
  # 后台 1s 采样一次, 父脚本退出时 cleanup
  (
    while kill -0 "$MEM_PID" 2>/dev/null; do
      local ts rss_kb
      ts="$(date '+%s')"
      # macOS/BSD: ps -o rss= ; Linux 一样
      rss_kb="$(ps -o rss= -p "$MEM_PID" 2>/dev/null | tr -d ' ' || echo 0)"
      echo "$ts,$rss_kb" >> "$MEM_FILE"
      sleep 1
    done
  ) &
  MEM_SAMPLER_PID=$!
  disown "$MEM_SAMPLER_PID" 2>/dev/null || true
}

stop_mem_sampler() {
  if [[ -n "$MEM_SAMPLER_PID" ]] && kill -0 "$MEM_SAMPLER_PID" 2>/dev/null; then
    kill "$MEM_SAMPLER_PID" 2>/dev/null || true
    wait "$MEM_SAMPLER_PID" 2>/dev/null || true
  fi
  # 取最大值
  if [[ -f "$MEM_FILE" ]] && [[ -s "$MEM_FILE" ]]; then
    MEM_MAX_KB="$(awk -F',' 'BEGIN{m=0} {if($2+0>m) m=$2+0} END{printf "%d", m}' "$MEM_FILE")"
  fi
}

# ------------------------------------------------------------------
# 生成 vegeta targets (请求定义)
#   格式参考: https://github.com/tsenart/vegeta#usage
#   GET url / POST url + header + @body
#
#   我们用 "attack format": 每行一条请求, 支持 body 内联或来自文件
#   但 vegeta attack 更推荐用 `-format=http` 直接写原始 HTTP, 或 `-body` 配合固定 body。
#   为简便 + 支持多查询变体, 我们生成独立 body 文件后在 attack 里轮转。
# ------------------------------------------------------------------

# gen_n_bulk_body <file> <N_per_batch> <start_i> <idx_name>
#   生成含 N_per_batch 条 index action 的 NDJSON body
gen_n_bulk_body() {
  local out="$1"; local N="$2"; local start_i="$3"; local idx="$4"
  : >"$out"
  local i doc_id title count active
  for ((i=0; i<N; i++)); do
    doc_id=$((start_i+i))
    title="loadtest_doc_$((doc_id % 1000)) hello world benchmark quick brown fox"
    count=$(( (doc_id+1) * 3 % 10000 ))
    active=$(( doc_id % 2 ))
    printf '{"index":{"_index":"%s","_id":"doc-%d"}}\n' "$idx" "$doc_id" >>"$out"
    printf '{"title":%q,"count":%d,"active":%s}\n' "$title" "$count" "$([ $active -eq 0 ] && echo true || echo false)" >>"$out"
  done
}

# gen_search_bodies <dir>
#   在 <dir> 里生成 5 个常用 query JSON:
#     q0_match_all.json  q1_match_hello.json  q2_range_count.json
#     q3_bool_must_filter.json  q4_multimatch.json
gen_search_bodies() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/q0_match_all.json" <<'EOF'
{"query":{"match_all":{}},"size":10}
EOF
  cat >"$dir/q1_match_hello.json" <<'EOF'
{"query":{"match":{"title":"hello"}},"size":10}
EOF
  cat >"$dir/q2_range_count.json" <<'EOF'
{"query":{"range":{"count":{"gte":100,"lte":5000}}},"size":10}
EOF
  cat >"$dir/q3_bool_must_filter.json" <<'EOF'
{"query":{"bool":{"must":[{"match":{"title":"hello"}}],"filter":[{"term":{"active":true}}]}},"size":10}
EOF
  cat >"$dir/q4_multimatch.json" <<'EOF'
{"query":{"multi_match":{"query":"hello world","fields":["title"],"type":"best_fields"}},"size":10}
EOF
}

# ------------------------------------------------------------------
# 阶段 0: warmup — 批量写入 WARMUP_N 条文档 (vegeta 也可, 但直接 curl + 并行分块更简单)
# ------------------------------------------------------------------
run_warmup() {
  local idx="${INDEX_PREFIX}_warmup"
  local batch=500
  local total="$WARMUP_N"
  [[ $total -eq 0 ]] && { log "⏭️  warmup 跳过(WARMUP_N=0)"; return 0; }
  log "🌡️  warmup: 写入 ${total} 条文档到索引 $idx (batch=${batch})"

  local wdir="$OUT_DIR/warmup"
  mkdir -p "$wdir"

  local sent=0; local fail=0
  while (( sent < total )); do
    local n=$(( total-sent < batch ? total-sent : batch ))
    local body="$wdir/batch_${sent}.ndjson"
    gen_n_bulk_body "$body" "$n" "$sent" "$idx"
    local resp
    # shellcheck disable=SC2046
    if ! resp="$(curl -sS $(curl_auth_args) -H 'Content-Type: application/x-ndjson' \
                --data-binary @"$body" "${TARGET_URL}/_bulk" 2>&1)"; then
      log "warmup batch $sent curl 失败: $resp"; fail=$((fail+n)); sent=$((sent+n)); continue
    fi
    # 统计 errors 字段 true 计数
    local errs
    errs="$(echo "$resp" | grep -oE '"errors":(true|false)' | head -1 | sed 's/"errors"://')"
    if [[ "$errs" == "true" ]]; then
      # 把 items 里带 error 的行数出来
      local err_items
      err_items="$(echo "$resp" | grep -c '"status":4\|"status":5\|"error":{' || echo 0)"
      fail=$((fail+err_items))
    fi
    sent=$((sent+n))
  done

  local ok=$((total-fail))
  log "🌡️  warmup 完成: 写入成功 ${ok}/${total}, 失败 ${fail}"
  [[ $ok -gt 0 ]] || die "warmup 0 文档写入成功, 终止压测" 2
}

# ------------------------------------------------------------------
# 生成 vegeta JSON 格式 target (一个字典对象 / 每行一个 NDJSON):
#   jq -c '{method:url:header:body}' body -> base64(body)
# 没有 jq 就用 python3 代替 (系统一定有 python3)
# ------------------------------------------------------------------
_json_target_py='
import json, sys, base64
# args: out_path method url ct body_path [auth_k auth_v ...]
out_path, method, url, ct, body_path, *auth_pairs = sys.argv[1:]
headers = {"Content-Type": [ct]}
for i in range(0, len(auth_pairs), 2):
    k, v = auth_pairs[i], auth_pairs[i+1]
    headers[k] = [v]
with open(body_path, "rb") as f:
    body = f.read()
obj = {"method": method, "url": url, "header": headers, "body": base64.b64encode(body).decode()}
with open(out_path, "a") as f:
    f.write(json.dumps(obj) + "\n")
'

# emit_json_target <out_file> <method> <url> <ct> <body_path>
emit_json_target() {
  local out="$1"; shift; local method="$1"; shift; local url="$1"; shift; local ct="$1"; shift; local body="$1"; shift
  local auth=()
  if [[ -n "$AUTH_BASIC" ]]; then
    auth+=("Authorization" "Basic $(printf '%s' "$AUTH_BASIC" | base64)")
  fi
  if [[ -n "$APIKEY" ]]; then
    auth+=("Authorization" "ApiKey $APIKEY")
  fi
  if command -v jq >/dev/null 2>&1; then
    local body_b64 auth_json header_json=""
    body_b64="$(base64 <"$body")"
    auth_json="{}"
    if [[ ${#auth[@]} -gt 0 ]]; then
      auth_json="$(printf '%s\n' "${auth[@]}" | jq -Rn '[inputs] | . as $a | reduce range(0; length/2) as $i ({}; . + {($a[$i*2]): [$a[$i*2+1]]})')"
    fi
    header_json="$(jq -cn --arg ct "$ct" --argjson ah "$auth_json" '{"Content-Type":[$ct]} + $ah')"
    jq -cn --arg m "$method" --arg u "$url" --argjson h "$header_json" --arg b "$body_b64" \
      '{method:$m, url:$u, header:$h, body:$b}' >>"$out"
  else
    python3 -c "$_json_target_py" "$out" "$method" "$url" "$ct" "$body" "${auth[@]}"
  fi
}

# ------------------------------------------------------------------
# run_vegeta_read <out_prefix> <idx>
# ------------------------------------------------------------------
run_vegeta_read() {
  local out="$1"; local idx="$2"
  local qdir="$OUT_DIR/bodies"
  gen_search_bodies "$qdir"
  local V
  V="$(ensure_vegeta)"
  local targets_file="${out}_targets.ndjson"
  : >"$targets_file"

  # 构造包含 5 种查询的 target 文件 (每个 query 一行 NDJSON, 重复 10 份 => 50 行总)
  # vegeta attack -lazy -targets=X 会循环读取, 达到 dur 自动停
  local repeat=10 q
  for ((r=0; r<repeat; r++)); do
    for q in q0_match_all.json q1_match_hello.json q2_range_count.json q3_bool_must_filter.json q4_multimatch.json; do
      emit_json_target "$targets_file" "POST" "${TARGET_URL}/${idx}/_search" "application/json" "$qdir/$q"
    done
  done

  log "📖 read 阶段: rate=${STAGE_RATE}/s duration=${STAGE_DUR} index=${idx} targets_lines=$(wc -l <"$targets_file")"
  # `-duration` 控制的是整体持续时间; 用 timeout 外层兜底防止 "-lazy 没读满" 导致提前返回
  local dur_secs
  dur_secs="$(normalize_duration_seconds "$STAGE_DUR")"
  timeout --kill-after=3s $(( dur_secs + 10 )) "$V" attack \
      -format=json \
      -rate="${STAGE_RATE}/1s" \
      -duration="$STAGE_DUR" \
      -targets="$targets_file" \
      >"${out}_results.bin" || true

  "$V" report -type=text "${out}_results.bin" | tee "${out}_report.txt"
  "$V" report -type=json "${out}_results.bin" >"${out}_results.json"
  "$V" report -type=csv  "${out}_results.bin" >"${out}_results.csv" 2>/dev/null || true
}

# ------------------------------------------------------------------
# run_vegeta_write <out_prefix> <idx> <start_docid>
# ------------------------------------------------------------------
run_vegeta_write() {
  local out="$1"; local idx="$2"; local start_docid="$3"
  local V
  V="$(ensure_vegeta)"
  local targets_file="${out}_targets.ndjson"
  local bdir="$OUT_DIR/bodies/write"
  mkdir -p "$bdir"
  : >"$targets_file"

  # 生成 10 个不同 start 的 batch (避免重复覆盖) 组成 targets 文件
  local k
  for ((k=0; k<10; k++)); do
    local body_file="$bdir/batch_${k}.ndjson"
    gen_n_bulk_body "$body_file" 100 "$(( start_docid + k*1000000 ))" "$idx"
    emit_json_target "$targets_file" "POST" "${TARGET_URL}/_bulk" "application/x-ndjson" "$body_file"
  done

  log "✍️  write 阶段: rate=${STAGE_RATE}/s duration=${STAGE_DUR} batch=100 bulk index=${idx} targets_lines=$(wc -l <"$targets_file")"

  local dur_secs
  dur_secs="$(normalize_duration_seconds "$STAGE_DUR")"
  timeout --kill-after=3s $(( dur_secs + 10 )) "$V" attack \
      -format=json \
      -rate="${STAGE_RATE}/1s" \
      -duration="$STAGE_DUR" \
      -targets="$targets_file" \
      >"${out}_results.bin" || true

  "$V" report -type=text "${out}_results.bin" | tee "${out}_report.txt"
  "$V" report -type=json "${out}_results.bin" >"${out}_results.json"
}

# ------------------------------------------------------------------
# run_vegeta_mixed <out_prefix> <read_idx>
# ------------------------------------------------------------------
run_vegeta_mixed() {
  local out="$1"; local read_idx="$2"; local write_idx="${INDEX_PREFIX}_mixed_write"
  local qdir="$OUT_DIR/bodies"
  gen_search_bodies "$qdir"
  local V
  V="$(ensure_vegeta)"
  local targets_file="${out}_targets.ndjson"
  local bdir="$OUT_DIR/bodies/mixed"
  mkdir -p "$bdir"
  : >"$targets_file"

  # 按比例拼接一组: RATIO_READ 个读 + RATIO_WRITE 个写; 整体重复 10 组
  local group repeat=10 i q wcounter bname
  for ((group=0; group<repeat; group++)); do
    # 读: 用 slot + group 做轮转下标
    local qlist=(q0_match_all.json q1_match_hello.json q2_range_count.json q3_bool_must_filter.json q4_multimatch.json)
    for ((i=0; i<RATIO_READ; i++)); do
      qname="${qlist[$(( (group + i) % ${#qlist[@]} ))]}"
      emit_json_target "$targets_file" "POST" "${TARGET_URL}/${read_idx}/_search" "application/json" "$qdir/$qname"
    done
    # 写:
    for ((i=0; i<RATIO_WRITE; i++)); do
      wcounter=$(( group*RATIO_WRITE + i + 1 ))
      bname="$bdir/batch_g${group}_i${i}.ndjson"
      gen_n_bulk_body "$bname" 50 "$(( wcounter * 1000000 ))" "$write_idx"
      emit_json_target "$targets_file" "POST" "${TARGET_URL}/_bulk" "application/x-ndjson" "$bname"
    done
  done

  log "🔀 mixed 阶段: 读:写=$READ_WRITE_RATIO rate=${STAGE_RATE}/s duration=${STAGE_DUR} targets_lines=$(wc -l <"$targets_file")"

  local dur_secs
  dur_secs="$(normalize_duration_seconds "$STAGE_DUR")"
  timeout --kill-after=3s $(( dur_secs + 10 )) "$V" attack \
      -format=json \
      -rate="${STAGE_RATE}/1s" \
      -duration="$STAGE_DUR" \
      -targets="$targets_file" \
      >"${out}_results.bin" || true

  "$V" report -type=text "${out}_results.bin" | tee "${out}_report.txt"
  "$V" report -type=json "${out}_results.bin" >"${out}_results.json"
}

# ------------------------------------------------------------------
# QPS 达标检查: 从 report 文本里提取 "Requests/sec" 数字, 与 QPS_THRESHOLD 比较
#   vegeta 默认 text report 行形如: "Requests/sec:    512.34"
# ------------------------------------------------------------------
# ------------------------------------------------------------------
# QPS 达标检查: 从 vegeta report 文本提取 throughput(实际 QPS)
#   新版 report (单行 requests):
#     Requests      [total, rate, throughput]         5000, 500.14, 500.08
#     Rate          [per second, throughput]          500.00,  500.00
#   旧版文本:
#     Requests/sec:    512.34
# 优先取 throughput (=throughput 最真实 QPS)
# ------------------------------------------------------------------
extract_qps() {
  local report_txt="$1"
  local val=""
  # Requests 行: "Requests      [total, rate, throughput]         5000, 500.14, 500.08"
  # 先取 [ ] 后面的逗号分隔 3 列数字, 第 3 个 = throughput (实际 qps)
  val="$(awk '/^Requests[[:space:]]+\[total,[[:space:]]*rate,[[:space:]]*throughput\]/ {
      # 删除到 最后的 ] 为止
      sub(/^.*\][[:space:]]*/, "", $0)
      # 按逗号拆分 (逗号前后允许空格)
      n = split($0, a, /,[[:space:]]*/)
      if (n >= 3) { gsub(/[^0-9.]/, "", a[3]); print a[3]; exit }
    }' "$report_txt")"
  if [[ -z "$val" ]]; then
    # Rate 行兜底: "Rate  [per second, throughput]  500.00,  500.00"
    val="$(awk '/^Rate[[:space:]]+\[/ {
        sub(/^.*\][[:space:]]*/, "", $0)
        n = split($0, a, /,[[:space:]]*/)
        if (n >= 2) { gsub(/[^0-9.]/, "", a[2]); print a[2]; exit }
      }' "$report_txt")"
  fi
  if [[ -z "$val" ]]; then
    # 回退旧版 Requests/sec:
    val="$(grep -m1 "Requests/sec:" "$report_txt" | awk '{print $2}')"
  fi
  echo "$val"
}

# 汇总输出一行 summary
SUMMARY_CSV="$OUT_DIR/summary.csv"
echo "stage,status,p50_ms,p95_ms,p99_ms,mean_ms,qps,success_pct,mem_max_mb" >"$SUMMARY_CSV"

append_summary_row() {
  local stage="$1"; local status="$2"; local report="$3"
  local p50 p95 p99 mean qps succ lat_line
  # 新版 vegeta text report 单行列所有延迟:
  #   Latencies     [min, mean, 50, 90, 95, 99, max]  197.9µs, 94.2ms, 1.9ms, 338.7ms, 566.3ms, 829.2ms, 1.3s
  lat_line="$(grep -m1 -E '^Latencies\s+\[' "$report" || echo "")"
  if [[ -n "$lat_line" ]]; then
    # 截取最后一块 (逗号分隔的 7 个时间值)
    local tail="${lat_line##*\]}"
    # 按逗号拆分; 使用 awk 切字段, 兼容逗号+空格分隔
    mean="$(to_ms "$(echo "$tail" | awk -F',[[:space:]]*' '{print $2}')")"
    p50="$(to_ms  "$(echo "$tail" | awk -F',[[:space:]]*' '{print $3}')")"
    p95="$(to_ms  "$(echo "$tail" | awk -F',[[:space:]]*' '{print $5}')")"
    p99="$(to_ms  "$(echo "$tail" | awk -F',[[:space:]]*' '{print $6}')")"
  else
    # 兼容旧版分行格式:
    #   50%     2.3ms
    p50="$(to_ms "$(grep -E '^\s*50%\s' "$report"  | awk '{print $2}' || echo 0)")"
    p95="$(to_ms "$(grep -E '^\s*95%\s' "$report"  | awk '{print $2}' || echo 0)")"
    p99="$(to_ms "$(grep -E '^\s*99%\s' "$report"  | awk '{print $2}' || echo 0)")"
    mean="$(to_ms "$(grep -E '^\s*mean\s' "$report" | awk '{print $2}' || echo 0)")"
  fi
  qps="$(extract_qps "$report")"
  # 新版 Success ratio; 兼容旧版 Success:
  succ="$( (grep -m1 -E '^Success\s+\[ratio\]' "$report" | awk '{print $3}' || grep -m1 "Success:" "$report" | awk '{print $2}') | tr -d '%')"
  [[ -z "$succ" ]] && succ="0"
  local mem_mb="0"
  if [[ "$MEM_MAX_KB" -gt 0 ]]; then
    mem_mb="$(awk -v k="$MEM_MAX_KB" 'BEGIN{printf "%.1f", k/1024.0}')"
  fi
  echo "${stage},${status},${p50},${p95},${p99},${mean},${qps},${succ},${mem_mb}" >>"$SUMMARY_CSV"
}

# to_ms "12ms" -> 12; "1.5s" -> 1500; "800us" -> 0.8
to_ms() {
  local v="$1"
  [[ -z "$v" || "$v" == "0" ]] && { echo 0; return; }
  local num="${v//[^0-9.]/}"
  case "$v" in
    *h)  awk -v x="$num" 'BEGIN{printf "%.3f", x*3600*1000}' ;;
    *m)  awk -v x="$num" 'BEGIN{printf "%.3f", x*60*1000}' ;;
    *s)  awk -v x="$num" 'BEGIN{printf "%.3f", x*1000}' ;;
    *ms) awk -v x="$num" 'BEGIN{printf "%.3f", x}' ;;
    *us|*µs) awk -v x="$num" 'BEGIN{printf "%.3f", x/1000}' ;;
    *ns) awk -v x="$num" 'BEGIN{printf "%.3f", x/1000000}' ;;
    *)    echo "$num" ;;
  esac
}

# ------------------------------------------------------------------
# 主流程
# ------------------------------------------------------------------
cleanup() {
  stop_mem_sampler
  stop_server
}
trap cleanup EXIT

if [[ "$MODE" == "-h" ]]; then
  sed -n '1,120p' "$0" | grep -E '^#|^$' | sed 's/^# \{0,1\}//' | head -100
  exit 0
fi

log "🪂 loadtest 开始: mode=$MODE stages='$STAGES' out=$OUT_DIR"
echo "    配置: TARGET=$TARGET_URL rate=$STAGE_RATE dur=$STAGE_DUR warmup=$WARMUP_N ratio=$READ_WRITE_RATIO qps_threshold=$QPS_THRESHOLD"

start_server
probe_target
start_mem_sampler
run_warmup

# 统一用 warmup 索引作为读的查询索引
READ_IDX="${INDEX_PREFIX}_warmup"

# 启动 QPS 达标记录
FAIL_QPS=0

for S in $STAGES; do
  stage_out="$OUT_DIR/stage_${S}"
  case "$S" in
    read)
      run_vegeta_read  "$stage_out" "$READ_IDX" ;;
    write)
      run_vegeta_write "$stage_out" "${INDEX_PREFIX}_write" 10000000 ;;
    mixed)
      run_vegeta_mixed "$stage_out" "$READ_IDX" ;;
    *)
      die "未知阶段: $S" 1
  esac
  report_txt="${stage_out}_report.txt"
  qps="$(extract_qps "$report_txt")"
  status="ok"
  # 只对 read/mixed 做 QPS 阈值校验 (write 视数据大小波动大)
  if [[ "$S" == "read" || "$S" == "mixed" ]]; then
    local_hit=0
    local_hit="$(awk -v a="$qps" -v t="$QPS_THRESHOLD" 'BEGIN{print (a+0 >= t+0) ? 1 : 0}')"
    if [[ "$local_hit" != "1" ]]; then
      status="low_qps"
      FAIL_QPS=$((FAIL_QPS+1))
      log "⚠️  阶段 $S QPS=${qps} 低于阈值 ${QPS_THRESHOLD}"
    fi
  fi
  append_summary_row "$S" "$status" "$report_txt"
done

# 收尾: 输出 summary 表 + 内存峰值
stop_mem_sampler
log "🏁 所有阶段完成"
echo ""
echo "====== Summary (${SUMMARY_CSV}) ======"
column -t -s, "$SUMMARY_CSV" || cat "$SUMMARY_CSV"
echo ""
if [[ -f "$MEM_FILE" ]] && [[ "$MEM_MAX_KB" -gt 0 ]]; then
  mem_mb="$(awk -v k="$MEM_MAX_KB" 'BEGIN{printf "%.1f", k/1024.0}')"
  echo "🫁 内存峰值 (RSS, PID=$MEM_PID): ${mem_mb} MiB (采样序列: $MEM_FILE)"
fi
echo ""
echo "📂 所有输出目录: $OUT_DIR"

if [[ $FAIL_QPS -gt 0 ]]; then
  die "QPS 达标失败: $FAIL_QPS 个阶段低于阈值 $QPS_THRESHOLD (read/mixed)" 3
fi
exit 0
