#!/bin/sh
# 等待 Elasticsearch 就绪后启动 demo
set -e
ES="${ES_ADDR:-http://localhost:9200}"
TIMEOUT="${ES_WAIT_TIMEOUT:-120}"

echo "Waiting for Elasticsearch at $ES (timeout ${TIMEOUT}s) ..."

end=$(( $(date +%s) + TIMEOUT ))
while :; do
  code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 3 "$ES" || echo "000")
  if [ "$code" = "200" ]; then
    echo "Elasticsearch is up."
    break
  fi
  if [ "$(date +%s)" -ge "$end" ]; then
    echo "Timeout waiting for Elasticsearch." >&2
    exit 1
  fi
  sleep 2
done

exec "$@"
