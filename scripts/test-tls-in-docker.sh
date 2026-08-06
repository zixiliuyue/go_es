#!/usr/bin/env bash
# 容器化 TLS 集成测试: 拉起 go_es_server(启用 TLS+h2), 跑 e2e-tests.sh 的 TLS 分支
#
# 用法:
#   scripts/test-tls-in-docker.sh
#   scripts/test-tls-in-docker.sh -k
#   scripts/test-tls-in-docker.sh -s
#
# 与 scripts/test-in-docker.sh(明文) 互不干扰: 不同 compose project, 不同 host 端口.
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
PROJ="go_es_tls_test_$(date +%s)"
COMPOSE="docker compose -p $PROJ -f $ROOT/docker-compose.tls.test.yml"

HOST_GO_ES_TLS_PORT=19202
export HOST_GO_ES_TLS_PORT

# 生成自签名证书
CERT_DIR="$(mktemp -d -t go_es_tls_cert.XXXXXX)"
trap 'rm -rf "$CERT_DIR"' EXIT
# 注意: TLS_CERT_DIR 必须在父 shell 赋值(而不是 inline env 前缀),
# 因为 docker compose 后续要从父 shell env 读取. 顺序: 1)赋值; 2)传参给子命令; 3)export.
export TLS_CERT_DIR="$CERT_DIR"
"$ROOT/scripts/gen-test-cert.sh" "$CERT_DIR" >/dev/null

cleanup() {
  if [[ $KEEP -eq 1 ]]; then
    echo
    echo "[test-tls-in-docker] -k set, 保留容器. 手动清理: docker compose -p $PROJ -f $ROOT/docker-compose.tls.test.yml down -v"
    return
  fi
  echo
  echo "[test-tls-in-docker] 清理容器..."
  $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
}
trap 'cleanup; rm -rf "$CERT_DIR"' EXIT

echo "[test-tls-in-docker] project=$PROJ  host_go_es_tls=:$HOST_GO_ES_TLS_PORT"

if [[ $SKIP_BUILD -eq 0 ]]; then
  echo "[test-tls-in-docker] 构建镜像..."
  $COMPOSE build --no-cache
fi

echo "[test-tls-in-docker] 启动 go_es_server(TLS) + tester..."
$COMPOSE up -d go_es_server

# 等待 TLS 服务就绪: 本机直接用 curl -k 探活
echo -n "[test-tls-in-docker] 等待 go_es_server(TLS)"
for i in $(seq 1 30); do
  code=$(curl -sk -o /dev/null -w "%{http_code}" --max-time 2 "https://localhost:$HOST_GO_ES_TLS_PORT/_health/liveness" || echo 000)
  if [[ "$code" == "200" ]]; then echo " OK"; break; fi
  echo -n "."
  sleep 1
  if [[ $i -eq 30 ]]; then echo " TIMEOUT"; exit 1; fi
done

echo "[test-tls-in-docker] 运行 e2e TLS 分支..."
set +e
$COMPOSE run --rm tester
rc=$?
set -e

if [[ $rc -ne 0 ]]; then
  echo "[test-tls-in-docker] 测试失败 rc=$rc"
  echo "[test-tls-in-docker] 容器日志(尾部):"
  $COMPOSE logs --tail=80 go_es_server || true
  exit 1
fi

echo "[test-tls-in-docker] 全部通过"
exit 0
