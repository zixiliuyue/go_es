## 概要

<!-- 一句話總結這個 PR 做了什麼 -->

## 變更類型

- [ ] Bug fix (non-breaking)
- [ ] New feature (non-breaking)
- [ ] Breaking change (會改變現有 API / 配置 / 命令行 flag)
- [ ] Documentation only

## 變更清單

<!-- 列出每個改動的文件 + 為什麼 -->

- ...
- ...

## 測試

- [ ] `go test -count=1 ./internal/...` 全綠
- [ ] `bash scripts/test-in-docker.sh` 通過 (如改 cmd/server 或路由)
- [ ] 新增 Go 單測 case
- [ ] 新增 e2e 斷言
- [ ] `govulncheck ./...` 0 報 (如改依賴 / Go 版本)
- [ ] `go vet ./...` 0 報

## 安全檢查

- [ ] 未引入明文密碼 / API key / PII
- [ ] 用戶輸入已 `escapeHtml` / 校驗
- [ ] 密鑰比對用 `subtle.ConstantTimeCompare`
- [ ] 新增依賴符合 [MIT compatible](CONTRIBUTING.md#許可證) 許可證

## 文檔

- [ ] README / AGENTS.md 更新 (如改 API / 啟動 flag)
- [ ] 新能力在 e2e / 單測有斷言
- [ ] CHANGELOG / commit message 描述"為什麼"

## 關聯

Refs: #
