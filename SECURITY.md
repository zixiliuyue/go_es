# Security Policy

## Supported Versions

go_es 跟隨 Go 工具鏈的安全補丁策略,只為最新 Go 小版本提供支持.
生產部署前請升級 Go 至最新 patch 版本.

| 版本 | 支持狀態 |
| --- | --- |
| `cmd/server` latest (追蹤 `main` 分支) | ✅ Active |
| 任何已歸檔 release tag | ❌ EOL |

## 報告漏洞

**請勿在 GitHub Issue 公開披露安全漏洞.**

對於尚未修補的漏洞,請郵件聯係維護者,描述:
- 受影響的版本/提交
- 重現步驟 / PoC
- 預期行為 vs 實際行為
- 潛在影響 (data exposure / RCE / DoS / auth bypass 等)

我們會在 7 個工作日內回覆確認並評估嚴重性 (CVSS v3.1).

## 安全設計要點

go_es 自研服務端已內建以下安全防護,部署時**務必開啟**:

1. **認證** — `-auth.user` + `-auth.password` (Basic) 或 `apikey=...` (ApiKey)
   - 密鑰比對使用 `crypto/subtle.ConstantTimeCompare`,防時序攻擊
2. **TLS** — `-tls.cert` + `-tls.key` (PEM),預設協商 h2
3. **mTLS** — `-tls.client-ca` + `-tls.client-auth=require_verify`,強制雙向認證
4. **限速** — `-rate 1000` (per-IP rps,預設不限速)
5. **請求體限制** — `-max-body 100` (MiB,預設 100)
6. **Health endpoints 白名單** — `/_health/*`, `/metrics`, `/_ui` 不需認證
7. **gzip 4xx 跳過** — 錯誤響應不壓縮,節省 CPU
8. **Web UI** — 所有用戶輸入用 `escapeHtml` 處理,防 XSS

## 已知風險

- **無 SQL** — 純 BadgerDB KV 存儲,無 SQL 注入風險
- **無 `os/exec`** — 不執行外部命令
- **No embedded passwords** — 憑據僅來自啟動 flag / env / yaml,不入倉庫
- **依賴漏洞掃描** — CI 跑 `govulncheck`,見 `.github/workflows/ci.yml`

## 漏洞修復 SLA

| 嚴重性 | 修復目標 |
| --- | --- |
| Critical (RCE / auth bypass) | 24h |
| High (data exposure / DoS) | 7d |
| Medium (info disclosure) | 30d |
| Low (best practice) | 下一個 release |

## 配置示例 (安全基線)

```bash
go_es_server \
  -addr ":9200" \
  -data "/var/lib/go_es" \
  -auth.user admin \
  -auth.password "${GO_ES_PASSWORD}" \
  apikey="${GO_ES_API_KEY}" \
  -rate 1000 \
  -max-body 100 \
  -tls.cert /etc/go_es/server.crt \
  -tls.key /etc/go_es/server.key \
  -tls.client-ca /etc/go_es/client_ca.crt \
  -tls.client-auth require_verify
```

不要把密碼寫入命令行歷史(用 env 或 `-config` + yaml 從 secret store 注入).
