#!/usr/bin/env bash
# 容器化集成测试: 同时拉起 ES + 自研 server, 跑端到端测试, 自动清理
#
# 用法:
#   scripts/test-in-docker.sh            # 跑测试后清理容器
#   scripts/test-in-docker.sh -k         # 保留容器(供手动调试)
#   scripts/test-in-docker.sh -s         # 跳过镜像重建
#
# 退出码: 测试全部通过 -> 0; 任意一步失败 -> 1
#
# 设计:
#   - 独立 compose project 名 go_es_test_<ts>, 与日常 dev compose 隔离
#   - 复用仓库根 docker-compose.yml 的服务定义, 不污染它
#   - 端口映射到 19200/19201, 不与本地 :9200 冲突

set -euo pipefail

KEEP=0
SKIP_BUILD=0
for a in "$@"; do
  case "$a" in
    -k|--keep)  KEEP=1 ;;
    -s|--skip-build) SKIP_BUILD=1 ;;
    -h|--help)
      sed -n '2,18p' "$0"; exit 0 ;;
    *) echo "unknown flag: $a" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROJ="go_es_test_$(date +%s)"
COMPOSE="docker compose -p $PROJ -f $ROOT/docker-compose.test.yml"

# 选择未占用的本机端口映射
HOST_ES_PORT=19200
HOST_GO_ES_PORT=19201

export HOST_ES_PORT HOST_GO_ES_PORT

cleanup() {
  if [[ -n "${TMPDIR_SCHEMA:-}" ]]; then
    rm -rf "$TMPDIR_SCHEMA"
  fi
  if [[ $KEEP -eq 1 ]]; then
    echo
    echo "[test-in-docker] -k set, 保留容器. 手动清理: docker compose -p $PROJ -f $ROOT/docker-compose.test.yml down -v"
    return
  fi
  echo
  echo "[test-in-docker] 清理容器..."
  $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "[test-in-docker] project=$PROJ  host_es=:$HOST_ES_PORT  host_go_es=:$HOST_GO_ES_PORT"

if [[ $SKIP_BUILD -eq 0 ]]; then
  echo "[test-in-docker] 构建镜像..."
  $COMPOSE build --no-cache
fi

echo "[test-in-docker] 启动 ES + go_es_server + tester..."
$COMPOSE up -d es go_es_server
# 等待 ES 就绪
echo -n "[test-in-docker] 等待 ES"
for i in $(seq 1 60); do
  code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 2 "http://localhost:$HOST_ES_PORT/" || echo 000)
  if [[ "$code" == "200" ]]; then echo " OK"; break; fi
  echo -n "."
  sleep 1
  if [[ $i -eq 60 ]]; then echo " TIMEOUT"; exit 1; fi
done

# 等待 go_es_server 就绪(暴露 /metrics 不需要 auth)
echo -n "[test-in-docker] 等待 go_es_server"
for i in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 2 "http://localhost:$HOST_GO_ES_PORT/_health/liveness" || echo 000)
  if [[ "$code" == "200" ]]; then echo " OK"; break; fi
  echo -n "."
  sleep 1
  if [[ $i -eq 30 ]]; then echo " TIMEOUT"; exit 1; fi
done

echo "[test-in-docker] 运行端到端测试..."
set +e
$COMPOSE run --rm tester
rc=$?
set -e

if [[ $rc -ne 0 ]]; then
  echo "[test-in-docker] 测试失败 rc=$rc"
  echo "[test-in-docker] 容器日志(尾部):"
  $COMPOSE logs --tail=80 es go_es_server || true
  exit 1
fi

# ---------- schema 校验 e2e (host-side, 在容器外验证) ----------
echo "[test-in-docker] schema 校验 e2e (host-side)..."
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMPDIR_SCHEMA="$(mktemp -d -t go_es_schema.XXXXXX)"
trap 'rm -rf "$TMPDIR_SCHEMA"' EXIT
SCHEMA_BIN="$TMPDIR_SCHEMA/go_es_server"
if ! (cd "$ROOT" && CGO_ENABLED=0 go build -o "$SCHEMA_BIN" ./cmd/server) >"$TMPDIR_SCHEMA/build.log" 2>&1; then
  echo "[test-in-docker] FAIL: 构建 go_es_server 失败, 看 build log"
  cat "$TMPDIR_SCHEMA/build.log"
  exit 1
fi

# case 1: 坏配置(log_level 非法) -> 启动应非 0
cat > "$TMPDIR_SCHEMA/bad.yaml" <<'EOF'
addr: ":9200"
log_level: "trace"
limit:
  max_body_bytes: -1
EOF
if "$SCHEMA_BIN" -config "$TMPDIR_SCHEMA/bad.yaml" >"$TMPDIR_SCHEMA/bad.out" 2>&1; then
  echo "[test-in-docker] FAIL: 坏配置未触发启动失败 (log_level=trace + max_body_bytes=-1)"
  cat "$TMPDIR_SCHEMA/bad.out"
  exit 1
fi
if ! grep -q "validate config" "$TMPDIR_SCHEMA/bad.out"; then
  echo "[test-in-docker] FAIL: 错误信息缺 'validate config' 标记"
  cat "$TMPDIR_SCHEMA/bad.out"
  exit 1
fi
echo "[test-in-docker] PASS: 坏配置启动被拒绝 + 错误含 'validate config'"

# case 2: TLS 单边配置 -> 应非 0
cat > "$TMPDIR_SCHEMA/half_tls.yaml" <<'EOF'
addr: ":9200"
tls:
  cert: "/nonexistent.crt"
EOF
if "$SCHEMA_BIN" -config "$TMPDIR_SCHEMA/half_tls.yaml" >"$TMPDIR_SCHEMA/half_tls.out" 2>&1; then
  echo "[test-in-docker] FAIL: TLS 单边配置未触发启动失败"
  cat "$TMPDIR_SCHEMA/half_tls.out"
  exit 1
fi
if ! grep -q "必须同时配置" "$TMPDIR_SCHEMA/half_tls.out"; then
  echo "[test-in-docker] FAIL: TLS 单边错误信息缺'必须同时配置'"
  cat "$TMPDIR_SCHEMA/half_tls.out"
  exit 1
fi
echo "[test-in-docker] PASS: TLS 单边配置启动被拒绝"

# case 3: auth.enabled=true 但无凭据 -> 应非 0
cat > "$TMPDIR_SCHEMA/auth_nocred.yaml" <<'EOF'
addr: ":9200"
auth:
  enabled: true
EOF
if "$SCHEMA_BIN" -config "$TMPDIR_SCHEMA/auth_nocred.yaml" >"$TMPDIR_SCHEMA/auth_nocred.out" 2>&1; then
  echo "[test-in-docker] FAIL: auth.enabled=true 无凭据未触发启动失败"
  cat "$TMPDIR_SCHEMA/auth_nocred.out"
  exit 1
fi
echo "[test-in-docker] PASS: auth.enabled=true 无凭据启动被拒绝"

echo "[test-in-docker] 全部通过"
exit 0
