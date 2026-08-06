#!/usr/bin/env bash
# mTLS 容器化集成测试: 拉起 go_es_server(启用 mTLS), 跑 e2e-mtls-tests.sh
#
# 用法:
#   scripts/test-mtls-in-docker.sh
#   scripts/test-mtls-in-docker.sh -k
#   scripts/test-mtls-in-docker.sh -s
#
# 与 scripts/test-tls-in-docker.sh 互不干扰: 不同 compose project, 不同 host 端口.
# 退出码: 测试全部通过 -> 0; 任意一步失败 -> 1.

set -euo pipefail

KEEP=0
SKIP_BUILD=0
for a in "$@"; do
  case "$a" in
    -k|--keep)  KEEP=1 ;;
    -s|--skip-build) SKIP_BUILD=1 ;;
    -h|--help)
      sed -n '2,16p' "$0"; exit 0 ;;
    *) echo "unknown flag: $a" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROJ="go_es_mtls_test_$(date +%s)"
COMPOSE="docker compose -p $PROJ -f $ROOT/docker-compose.mtls.test.yml"

HOST_GO_ES_MTLS_PORT=19203
export HOST_GO_ES_MTLS_PORT

# 生成 mTLS 自签名证书(server + CA + client)
CERT_DIR="$(mktemp -d -t go_es_mtls_cert.XXXXXX)"
trap 'rm -rf "$CERT_DIR"' EXIT
export MTLS_CERT_DIR="$CERT_DIR"
"$ROOT/scripts/gen-test-cert.sh" "$CERT_DIR" -m >/dev/null

cleanup() {
  if [[ $KEEP -eq 1 ]]; then
    echo
    echo "[test-mtls-in-docker] -k set, 保留容器. 手动清理: docker compose -p $PROJ -f $ROOT/docker-compose.mtls.test.yml down -v"
    return
  fi
  echo
  echo "[test-mtls-in-docker] 清理容器..."
  $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
}
trap 'cleanup; rm -rf "$CERT_DIR"' EXIT

echo "[test-mtls-in-docker] project=$PROJ  host_go_es_mtls=:$HOST_GO_ES_MTLS_PORT"

if [[ $SKIP_BUILD -eq 0 ]]; then
  echo "[test-mtls-in-docker] 构建镜像..."
  $COMPOSE build --no-cache
fi

echo "[test-mtls-in-docker] 启动 go_es_server(mTLS) + tester..."
$COMPOSE up -d go_es_server

# 等待服务就绪(用 host 端 curl -k + client cert 试一次)
echo -n "[test-mtls-in-docker] 等待 go_es_server(mTLS)"
for i in $(seq 1 30); do
  code=$(curl -sk \
    --cert "$CERT_DIR/client.crt" --key "$CERT_DIR/client.key" \
    --cacert "$CERT_DIR/ca.crt" \
    -o /dev/null -w "%{http_code}" --max-time 2 \
    "https://localhost:$HOST_GO_ES_MTLS_PORT/_health/liveness" || echo 000)
  if [[ "$code" == "200" ]]; then echo " OK"; break; fi
  echo -n "."
  sleep 1
  if [[ $i -eq 30 ]]; then echo " TIMEOUT"; exit 1; fi
done

echo "[test-mtls-in-docker] 运行 e2e mTLS 分支..."
set +e
$COMPOSE run --rm tester
rc=$?
set -e

if [[ $rc -ne 0 ]]; then
  echo "[test-mtls-in-docker] 测试失败 rc=$rc"
  echo "[test-mtls-in-docker] 容器日志(尾部):"
  $COMPOSE logs --tail=80 go_es_server || true
  exit 1
fi

echo "[test-mtls-in-docker] 全部通过"
exit 0
