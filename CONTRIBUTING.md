# Contributing to go_es

感謝你願意為 go_es 貢獻!本文件説明開發流程、編碼規範與提交流程.

## 開發環境

- Go **1.25.12** 或更新 (見 `go.mod`)
- Docker (可選,跑容器化測試)
- make / bash

```bash
go version   # 確認 ≥ 1.25.12
```

## 本地構建

```bash
go build ./...
go test -count=1 ./internal/...
```

## 容器化測試 (推薦)

PR 必須通過 `bash scripts/test-in-docker.sh` 才算合規,該脚本會:
1. 起 ES 8.13.4 + 自研 `go_es_server`
2. 跑 `scripts/e2e-tests.sh` 全套斷言
3. 自動清理容器

```bash
bash scripts/test-in-docker.sh
```

Trick: 帶 `-k` 保留容器便於手動 debug.

## 編碼規範

遵循 [Effective Go](https://go.dev/doc/effective_go) + 下列工程約定 (摘自 [AGENTS.md](AGENTS.md)):

1. **先看,後改** — 改任何文件前先 `Read`
2. **小步走** — 單 commit 圍繞一個能力
3. **測試驅動** — 新能力必須:
   - Go 單測在 `internal/server/extensions_test.go` 加 case
   - e2e 斷言在 `scripts/e2e-tests.sh` 加 case
4. **向後兼容** — `New(...)` 保持原簽名,新能力走 `NewWithOptions(...)`
5. **不造輪子** — BadgerDB / zap / prometheus / `golang.org/x/time/rate` 已有依賴直接用
6. **代碼自包含** — 減少跨文件耦合
7. **不加覆蓋鎖** — 新加鎖請在註釋説明臨界區 / 是否可重入 / 順序
8. **不提交大文件** — 容器數據目錄已 `.gitignore`

### 註釋 / 文檔

- 公開函數 / 類必須有中文 doc comment (本倉庫統一中文)
- 複雜邏輯 (例如倒排構建、路由分發) 必須有 inline 註釋
- 算法 / 併發約定 (mutex 順序、race 防護) 必須寫在結構體上方

### 安全

詳見 [SECURITY.md](SECURITY.md). 提交前自檢:

- [ ] 無明文密碼 / API key 進倉庫 (CI 的 `secrets-scan` 會卡)
- [ ] 用戶輸入過 `escapeHtml` / `subtle.ConstantTimeCompare` / 路徑校驗
- [ ] `govulncheck ./...` 0 報

## 提交流程

1. Fork → 創建 feature 分支 (`git checkout -b feat/xxx`)
2. 提交 (commit message 推薦中文,描述"為什麼"而非"做了什麼")
3. 跑 `bash scripts/test-in-docker.sh` 確認 e2e 全綠
4. 推送到 fork → 在 GitHub 開 PR
5. 等待 CI 通過 + 維護者 review
6. Squash merge 後自動關聯 issue

## Commit Message 格式

```
<type>(<scope>): <簡短中文描述>

<詳細描述, 含動機 / 影響面 / 測試覆蓋>

Refs: #<issue>
```

類型:
- `feat` — 新功能
- `fix` — 修復 bug
- `docs` — 文檔 / 註釋
- `test` — 測試
- `refactor` — 重構(無功能變化)
- `chore` — 構建 / CI / 工具鏈
- `security` — 安全修補

## Issue / PR 模板

- 開 issue 請用 `.github/ISSUE_TEMPLATE/`
- 開 PR 請用 `.github/PULL_REQUEST_TEMPLATE.md`

## 行為準則

詳見 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## 許可證

貢獻代碼默認採用 [MIT License](LICENSE).
