#!/usr/bin/env bash
# 生成自签名证书, 用于容器化 TLS 测试
# 用法: scripts/gen-test-cert.sh <out_dir> [-m]
#   out_dir: 生成的 server.crt / server.key 所在目录(会自动创建)
#   -m / --mtls: 额外生成 mTLS 需要的 ca.crt / client.crt / client.key
#                 此时 server.crt 也由该 CA 签发(可被同 CA 池里的 client.crt 互验)
# 输出: 退出码 0 + cert/key 路径; 失败则非 0
#
# 仅供测试, 私钥是 EC P-256(SEC1 格式), SAN=localhost+127.0.0.1, 有效期 1 天.
#
# 实现说明: 用 openssl ecparam 先生成 SEC1 EC PRIVATE KEY, 再用 req -x509 出证书.
# 不用 -pkeyopt ec_paramgen_curve 的 inline PKCS#8, 因为部分 Go 版本(1.25)
# 的 tls.LoadX509KeyPair 对 PKCS#8 内嵌的 ECPrivateKey 参数校验更严格,
# 会报 "x509: invalid ECDSA parameters".

set -euo pipefail

OUT_DIR="${1:-}"
shift || true

MTLS=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--mtls) MTLS=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$OUT_DIR" ]]; then
  echo "usage: $0 <out_dir> [-m]" >&2
  exit 2
fi

mkdir -p "$OUT_DIR"

if ! command -v openssl >/dev/null 2>&1; then
  echo "openssl not found in PATH" >&2
  exit 3
fi

# --- 共用: SAN 配置 ---
# SAN 包含 localhost + 127.0.0.1(本机访问) + go_es_server/docker host(compose 容器名)
cat > "$OUT_DIR/san.cnf" <<'EOF'
[req]
distinguished_name = dn
req_extensions     = v3_req
prompt             = no
[dn]
CN = localhost
[v3_req]
keyUsage         = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName   = @alt
[alt]
DNS.1 = localhost
DNS.2 = go_es_server
IP.1  = 127.0.0.1
EOF

CERT="$OUT_DIR/server.crt"
KEY="$OUT_DIR/server.key"

if [[ "$MTLS" -eq 0 ]]; then
  # 1) 单独生成 SEC1 格式的 EC 私钥
  openssl ecparam -name prime256v1 -genkey -noout -out "$KEY" 2>/dev/null
  # 2) 用私钥 + 临时配置出证书
  openssl req -new -x509 -key "$KEY" -out "$CERT" -days 1 -config "$OUT_DIR/san.cnf" 2>/dev/null
  rm -f "$OUT_DIR/san.cnf"
  echo "CERT=$CERT"
  echo "KEY=$KEY"
  exit 0
fi

# --- mTLS 模式: 生成 CA + 用 CA 签 server 和 client ---
CA_KEY="$OUT_DIR/ca.key"
CA_CRT="$OUT_DIR/ca.crt"
CA_CNF="$OUT_DIR/ca.cnf"

# 1) CA 私钥 + 自签证书 (CA:TRUE, EKU=serverAuth+clientAuth)
openssl ecparam -name prime256v1 -genkey -noout -out "$CA_KEY" 2>/dev/null
cat > "$CA_CNF" <<'EOF'
[req]
distinguished_name = dn
x509_extensions    = v3_ca
prompt             = no
[dn]
CN = go_es test CA
[v3_ca]
keyUsage               = critical, digitalSignature, cRLSign, keyCertSign
basicConstraints       = critical, CA:TRUE
subjectKeyIdentifier   = hash
EOF
openssl req -new -x509 -key "$CA_KEY" -out "$CA_CRT" -days 1 -config "$CA_CNF" 2>/dev/null

# 2) server 私钥 + 由 CA 签 (EKU=serverAuth, SAN=localhost+127.0.0.1)
openssl ecparam -name prime256v1 -genkey -noout -out "$KEY" 2>/dev/null
openssl req -new -key "$KEY" -out "$OUT_DIR/server.csr" -config "$OUT_DIR/san.cnf" 2>/dev/null
cat > "$OUT_DIR/server-ext.cnf" <<'EOF'
[v3_req]
keyUsage         = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName   = @alt
[alt]
DNS.1 = localhost
DNS.2 = go_es_server
IP.1  = 127.0.0.1
EOF
openssl x509 -req -in "$OUT_DIR/server.csr" -CA "$CA_CRT" -CAkey "$CA_KEY" -CAcreateserial \
  -out "$CERT" -days 1 -sha256 -extfile "$OUT_DIR/server-ext.cnf" -extensions v3_req 2>/dev/null

# 3) client 私钥 + 由 CA 签 (EKU=clientAuth, CN=test-client)
CLIENT_KEY="$OUT_DIR/client.key"
CLIENT_CRT="$OUT_DIR/client.crt"
cat > "$OUT_DIR/client.cnf" <<'EOF'
[req]
distinguished_name = dn
prompt             = no
[dn]
CN = test-client
EOF
openssl ecparam -name prime256v1 -genkey -noout -out "$CLIENT_KEY" 2>/dev/null
openssl req -new -key "$CLIENT_KEY" -out "$OUT_DIR/client.csr" -config "$OUT_DIR/client.cnf" 2>/dev/null
cat > "$OUT_DIR/client-ext.cnf" <<'EOF'
[v3_req]
keyUsage         = digitalSignature
extendedKeyUsage = clientAuth
EOF
openssl x509 -req -in "$OUT_DIR/client.csr" -CA "$CA_CRT" -CAkey "$CA_KEY" -CAcreateserial \
  -out "$CLIENT_CRT" -days 1 -sha256 -extfile "$OUT_DIR/client-ext.cnf" -extensions v3_req 2>/dev/null

# 清理临时文件
rm -f "$OUT_DIR/san.cnf" "$OUT_DIR/ca.cnf" "$OUT_DIR/server-ext.cnf" "$OUT_DIR/client.cnf" \
      "$OUT_DIR/client-ext.cnf" "$OUT_DIR/server.csr" "$OUT_DIR/client.csr" \
      "$OUT_DIR/ca.srl" "$OUT_DIR/ca.key"  # 私钥 ca.key 留着, 方便用户再加 client

echo "CERT=$CERT"
echo "KEY=$KEY"
echo "CA=$CA_CRT"
echo "CLIENT_CERT=$CLIENT_CRT"
echo "CLIENT_KEY=$CLIENT_KEY"
