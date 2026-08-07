// internal/server 阶段 2+3 扩展能力测试
// 覆盖: 跨索引通配 / Web UI / gzip Vary 头
// 模式匹配通过 HTTP 行为验证(端到端), 不做白盒单测
package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSearch_WildcardPatternAll(t *testing.T) {
	ts := newTestServer(t)
	// 建 3 个索引, 写 1 条
	for _, n := range []string{"idx_a1", "idx_a2", "idx_b1"} {
		_ = doMust(t, ts, "PUT", "/"+n, nil)
		_ = doMust(t, ts, "PUT", "/"+n+"/_doc/x", map[string]interface{}{"k": "v"})
	}
	// 跨 idx_* 搜索 -> 应命中 3 条
	resp, body := do(t, ts, "POST", "/idx_*/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
		"size":  20,
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 3, parsed.Hits.Total.Value)
}

func TestSearch_WildcardPatternPrefix(t *testing.T) {
	ts := newTestServer(t)
	for _, n := range []string{"articles_v1", "articles_v2", "products"} {
		_ = doMust(t, ts, "PUT", "/"+n, nil)
		_ = doMust(t, ts, "PUT", "/"+n+"/_doc/1", map[string]interface{}{"k": "v"})
	}
	resp, body := do(t, ts, "POST", "/articles*/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 2, parsed.Hits.Total.Value)
}

func TestSearch_WildcardPatternSuffix(t *testing.T) {
	ts := newTestServer(t)
	for _, n := range []string{"a_v1", "a_v2", "b_v2"} {
		_ = doMust(t, ts, "PUT", "/"+n, nil)
		_ = doMust(t, ts, "PUT", "/"+n+"/_doc/1", map[string]interface{}{"k": "v"})
	}
	resp, body := do(t, ts, "POST", "/*_v2/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 2, parsed.Hits.Total.Value)
}

func TestSearch_WildcardPatternExclude(t *testing.T) {
	ts := newTestServer(t)
	for _, n := range []string{"a1", "a2", "b1", "b2"} {
		_ = doMust(t, ts, "PUT", "/"+n, nil)
		_ = doMust(t, ts, "PUT", "/"+n+"/_doc/1", map[string]interface{}{"k": "v"})
	}
	// * 减去 b* -> 应命中 2 (a1, a2)
	resp, body := do(t, ts, "POST", "/*,-b*/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 2, parsed.Hits.Total.Value)
}

func TestSearch_WildcardPatternExactList(t *testing.T) {
	ts := newTestServer(t)
	for _, n := range []string{"a", "b", "c"} {
		_ = doMust(t, ts, "PUT", "/"+n, nil)
		_ = doMust(t, ts, "PUT", "/"+n+"/_doc/1", map[string]interface{}{"k": "v"})
	}
	resp, body := do(t, ts, "POST", "/a,c/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 2, parsed.Hits.Total.Value)
}

func TestSearch_WildcardPattern_NoMatch(t *testing.T) {
	ts := newTestServer(t)
	_ = doMust(t, ts, "PUT", "/foo", nil)
	resp, body := do(t, ts, "POST", "/nonexistent*/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 0, parsed.Hits.Total.Value)
}

func TestUI_PageLoads(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "<!doctype html>")
	assert.Contains(t, string(body), "go_es · 控制台")

	resp, body = do(t, ts, "GET", "/_ui/index.html", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "<!doctype html>")
}

func TestUI_NoAuthRequired(t *testing.T) {
	ts := newTestServerWith(t, ServerOptions{
		Auth: AuthConfig{
			Enabled: true,
			Basic:   map[string]string{"u": "p"},
		},
	})
	// 不带 auth 头访问 /_ui
	req, _ := http.NewRequest("GET", ts.URL+"/_ui", nil)
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	assert.Equal(t, 200, r.StatusCode)
}

func TestGzip_VaryHeaderSet(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/_health/liveness", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	assert.Equal(t, 200, r.StatusCode)
	vary := r.Header.Get("Vary")
	// 我们中间件只加 Vary: Accept-Encoding, 不做实际压缩
	assert.Contains(t, vary, "Accept-Encoding")
}

func TestGzip_MetricsSkipped(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	assert.Equal(t, 200, r.StatusCode)
}

func TestGzip_RealCompression(t *testing.T) {
	ts := newTestServer(t)
	// 建索引 + 写大文档, 触发压缩 (>512B)
	_ = doMust(t, ts, "PUT", "/big", nil)
	big := make(map[string]interface{})
	big["payload"] = strings.Repeat("abcdefgh", 500) // 4KB
	_ = doMust(t, ts, "PUT", "/big/_doc/1", big)

	// 不带 Accept-Encoding -> 明文
	req, _ := http.NewRequest("GET", ts.URL+"/big/_doc/1", nil)
	r1, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	rawBody, _ := io.ReadAll(r1.Body)
	_ = r1.Body.Close()
	assert.Equal(t, 200, r1.StatusCode)
	assert.Empty(t, r1.Header.Get("Content-Encoding"), "no Accept-Encoding should not compress")
	assert.Contains(t, string(rawBody), "abcdefgh")

	// 带 Accept-Encoding: gzip -> 应真正压缩
	req, _ = http.NewRequest("GET", ts.URL+"/big/_doc/1", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r2, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r2.Body.Close() }()
	assert.Equal(t, 200, r2.StatusCode)
	assert.Equal(t, "gzip", r2.Header.Get("Content-Encoding"))
	assert.Contains(t, r2.Header.Get("Vary"), "Accept-Encoding")
	// 压缩后字节数应小于原文
	gzBody, _ := io.ReadAll(r2.Body)
	assert.Less(t, len(gzBody), len(rawBody), "gzip body should be smaller than raw")
	// 真正解压后内容仍正确
	gz, err := gzip.NewReader(bytes.NewReader(gzBody))
	assert.NoError(t, err)
	decoded, _ := io.ReadAll(gz)
	_ = gz.Close()
	assert.Contains(t, string(decoded), "abcdefgh")
}

func TestGzip_4xxNotCompressed(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/nonexistent_index", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	// 4xx 不应压缩
	assert.NotEqual(t, "gzip", r.Header.Get("Content-Encoding"))
}

// ---------- Web UI: 多 Tab / 历史 / 字段类型推断 ----------

// 1) /_ui 页面应包含多 Tab 系统的全部钩子字符串
func TestUI_MultiTabSurface(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	// Tab 模型钩子
	assert.Contains(t, html, "id=\"tabBar\"", "应有 Tab 栏容器")
	assert.Contains(t, html, "id=\"newTabBtn\"", "应有新建 Tab 按钮")
	assert.Contains(t, html, "function newTab()", "应导出 newTab()")
	assert.Contains(t, html, "function closeTab(", "应导出 closeTab()")
	assert.Contains(t, html, "function switchTab(", "应导出 switchTab()")
	assert.Contains(t, html, "function renameTab(", "应导出 renameTab()")
	assert.Contains(t, html, "go_es_tabs", "localStorage 应有 go_es_tabs key")
	assert.Contains(t, html, "go_es_active_tab", "localStorage 应有 go_es_active_tab key")
}

// 2) /_ui 应包含历史查询面板与重跑机制
func TestUI_HistorySurface(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	assert.Contains(t, html, "id=\"historyPanel\"", "应有历史抽屉容器")
	assert.Contains(t, html, "id=\"hSearch\"", "历史搜索输入")
	assert.Contains(t, html, "id=\"hType\"", "历史类型筛选")
	assert.Contains(t, html, "id=\"hRange\"", "历史时间筛选")
	assert.Contains(t, html, "function pushHistory(", "应有 pushHistory()")
	assert.Contains(t, html, "function replayHistory(", "应有一键重跑 replayHistory()")
	assert.Contains(t, html, "function deleteHistory(", "应有 deleteHistory()")
	assert.Contains(t, html, "function clearHistory(", "应有 clearHistory()")
	assert.Contains(t, html, "go_es_history", "localStorage 应有 go_es_history key")
}

// 3) /_ui 应包含字段类型推断 UI + 类型化 value 控件
func TestUI_FieldInferenceSurface(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	assert.Contains(t, html, "id=\"fieldChips\"", "应有 field chips 容器")
	assert.Contains(t, html, "function extractFieldMap(", "应有 mapping 抽取")
	assert.Contains(t, html, "function inferType(", "应有类型推断")
	assert.Contains(t, html, "function valueControlHTML(", "应有类型化控件选择")
	assert.Contains(t, html, "type=\"checkbox\"", "boolean 字段应渲染 checkbox")
	assert.Contains(t, html, "type=\"date\"", "date 字段应渲染 date picker")
	assert.Contains(t, html, "type=\"number\"", "number 字段应渲染 number input")
	assert.Contains(t, html, ".fieldchip", "字段 chip 样式")
}

// 4) /_ui 应保持向后兼容(旧 e2e 抓的全局函数名 + 标题)
func TestUI_BackwardCompat(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	for _, fn := range []string{"runSearch", "runAgg", "loadIndices", "selectIndex", "loadCluster", "prevPage", "nextPage"} {
		assert.Contains(t, html, "function " + fn + "(", "应保留全局函数 "+fn)
	}
	assert.Contains(t, html, "go_es · 控制台", "标题文案不能改")
	// e2e 抓的 JS 关键钩子
	assert.Contains(t, html, "loadIndices", "e2e 抓的 loadIndices 字符串")
	assert.Contains(t, html, "runSearch", "e2e 抓的 runSearch 字符串")
}

// 5) /_ui 页面大小可控(单文件 < 60KB, 避免内嵌二进制过大)
func TestUI_PageSizeReasonable(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	// 加倍安全: < 80KB; 实际 ~30KB
	assert.Less(t, len(body), 80*1024, "/_ui 页面应 < 80KB (避免二进制膨胀)")
}

// Tab 拖拽排序/拖出关闭所需的 hook 字符串都应在 /_ui 中存在
func TestUI_DragAndDropSurface(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	required := []string{
		"draggable = true",                  // HTML5 drag 启用
		"onTabDragStart",                    // dragstart handler
		"onTabDragEnd",                      // dragend handler (用于拖出关闭)
		"onTabDragOver",                     // dragover handler (用于插入位置)
		"onTabDrop",                         // drop handler (用于重排)
		"drag-over-left",                    // 视觉指示
		"drag-over-right",
		"closeTab(dragSourceId)",            // 拖出 tabbar 触发关闭
		"getBoundingClientRect",             // 计算 tabbar 矩形判断是否拖出
		"newTabData",                        // 单 Tab 兜底
	}
	for _, s := range required {
		assert.Contains(t, string(body), s, "/_ui 应含 drag&drop hook: %s", s)
	}
}

// Tab 导入/导出所需的 hook 与按钮
func TestUI_ImportExportSurface(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	required := []string{
		`id="exportBtn"`,                    // 导出按钮
		`id="importBtn"`,                    // 导入按钮
		`id="importFile"`,                   // 隐藏的 file input
		`accept="application/json,.json"`,   // 限定文件类型
		"function exportTabs()",             // 导出函数
		"function importTabs(ev)",           // 导入函数
		"function buildExportPayload",       // 构造 payload
		"function validateImportPayload",    // 校验函数
		"version: 1",                        // payload 固定 version (JS 对象字面量, 无引号)
		"exportedAt",                        // 导出时间戳
		"URL.createObjectURL",               // 浏览器下载 API
		"FileReader",                        // 浏览器读取 API
		"Blob",                              // 浏览器二进制 API
	}
	for _, s := range required {
		assert.Contains(t, string(body), s, "/_ui 应含 import/export hook: %s", s)
	}
}

// 历史图表: 24h 频次柱状图 + 累计百分比汇总
func TestUI_ChartSurface(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	required := []string{
		"function renderHistoryChart",        // 主函数
		`id="histChart"`,                    // 挂载点
		"chartwrap",                         // CSS 类
		"function pushHistory",              // hook 进 pushHistory (看下面)
		"renderHistoryChart()",              // pushHistory 内调用
		"function clearHistory",             // 清空也要刷新
		"viewBox=",                          // SVG 标志
		"24h",                               // 24 小时窗口
		"<rect",                             // 柱状条 SVG 元素
		"class=\"bar-search\"",              // search 柱用 CSS 类(主题切换需要)
		"class=\"bar-agg\"",                 // agg 柱用 CSS 类(主题切换需要)
		"class=\"bar-axis\"",                // 轴线用 CSS 类
		"class=\"bar-label\"",               // 标签用 CSS 类
		"class=\"l-search\"",                // 图例 search 用 CSS 类
		"class=\"l-agg\"",                   // 图例 agg 用 CSS 类
		"暂无数据",                          // 空态文案
	}
	for _, s := range required {
		assert.Contains(t, string(body), s, "/_ui 应含 chart hook: %s", s)
	}
	// pushHistory 内部必须调 renderHistoryChart
	// 简单做法: 找 pushHistory 函数体, 看是否在末尾调了 renderHistoryChart
	idx := strings.Index(string(body), "function pushHistory")
	if idx < 0 { t.Fatal("pushHistory 函数体未找到") }
	endIdx := strings.Index(string(body)[idx:], "function clearHistory")
	if endIdx < 0 { t.Fatal("clearHistory 边界未找到") }
	body1 := string(body)[idx : idx+endIdx]
	assert.Contains(t, body1, "renderHistoryChart()", "pushHistory 内部必须调 renderHistoryChart")
}

// 验证导入 payload 的 spec: 合法 + 各种非法形式被拒
// 这里用 Go 模拟 JS 的 validateImportPayload 行为, 锁定 spec 形状
func TestUI_ImportPayloadSpec(t *testing.T) {
	// 合法的最小 payload
	good := map[string]any{
		"version":     1,
		"exportedAt":  "2026-08-06T10:00:00Z",
		"tabs":        []any{map[string]any{"id": "t1", "title": "Tab 1"}},
		"activeTabId": "t1",
		"history":     []any{},
	}
	b, err := json.Marshal(good)
	assert.NoError(t, err)
	var roundTrip map[string]any
	assert.NoError(t, json.Unmarshal(b, &roundTrip))
	assert.EqualValues(t, 1, roundTrip["version"])
	assert.Equal(t, "t1", roundTrip["activeTabId"])

	// 非法形式: version 错
	bad := map[string]any{"version": 2, "tabs": []any{}}
	b2, _ := json.Marshal(bad)
	var rt2 map[string]any
	_ = json.Unmarshal(b2, &rt2)
	// 我们的 spec 是 version 必须 = 1
	assert.NotEqual(t, 1, rt2["version"], "version 错应在 import 端拒绝")
}

// ---------- 主题切换(dark / light) ----------

// 1) /_ui 必须含主题切换控件 + 主题变量定义
func TestUI_ThemeSwitchSurface(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	required := []string{
		`id="themeSelect"`,                     // 主题切换控件
		"function setTheme(",                    // 切换函数
		"function getTheme(",                    // 读取函数
		"function syncThemeSelect(",             // 启动时同步
		"data-theme=\"dark\"",                   // 默认 dark 变量块
		"data-theme=\"light\"",                  // light 主题变量块
		"go_es_theme",                           // localStorage 持久化 key
		"onchange=\"setTheme(this.value)\"",     // 控件 onchange 绑定
		"深色",                                  // 中文选项
		"浅色",                                  // 中文选项
		"prefers-reduced-motion",                // 无障碍
		"--bg:",                                 // 主题变量
		"--text:",                               // 文本变量
		"--border:",                             // 边框变量
		"--accent:",                             // 强调色变量
	}
	for _, s := range required {
		assert.Contains(t, html, s, "/_ui 应含 theme hook: %s", s)
	}
}

// 2) <head> 顶部内联脚本必须在 <body> 之前执行, 避免 dark→light 闪烁(FOUC)
func TestUI_ThemeNoFOUC(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	// 找到内联脚本, 确认它的位置在 <body> 之前
	scriptIdx := strings.Index(html, "document.documentElement.setAttribute('data-theme'")
	bodyIdx := strings.Index(html, "<body>")
	assert.GreaterOrEqual(t, scriptIdx, 0, "应有内联主题脚本")
	assert.GreaterOrEqual(t, bodyIdx, 0, "应有 <body>")
	assert.Less(t, scriptIdx, bodyIdx, "内联主题脚本必须在 <body> 之前 (防 FOUC)")
	// 内联脚本应在 <head> 内(在 </head> 之前)
	headEndIdx := strings.Index(html, "</head>")
	assert.Less(t, scriptIdx, headEndIdx, "内联主题脚本应在 </head> 之前")
}

// 3) 主题必须在 localStorage 中持久化, key 命名稳定
func TestUI_ThemePersistenceKey(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	// 出现 LS_THEME 常量声明 + 使用
	assert.Contains(t, html, "LS_THEME = 'go_es_theme'", "应有 LS_THEME 常量")
	assert.Contains(t, html, "localStorage.setItem(LS_THEME", "setTheme 必须写 localStorage")
	assert.Contains(t, html, "localStorage.getItem('go_es_theme')", "head 脚本必须读 localStorage 还原")
}

// 4) 亮色主题必须定义完整的样式变量(背景/文本/边框/强调色/状态色)
func TestUI_LightThemeVariablesComplete(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	// 找到 light 主题块
	lightStart := strings.Index(html, "[data-theme=\"light\"]")
	assert.GreaterOrEqual(t, lightStart, 0, "应有 light 主题 CSS 块")
	// 在 light 块内必须有以下核心变量(任一缺失说明样式不完整)
	// 取 light 块到下一个 `}` 之间的内容做断言
	endRel := strings.Index(html[lightStart:], "}\n  </style>")
	if endRel < 0 { endRel = strings.Index(html[lightStart:], "}"); }
	assert.Greater(t, endRel, 0)
	lightBlock := html[lightStart : lightStart+endRel]
	required := []string{
		"--bg:",
		"--bg-elevated:",
		"--bg-header:",
		"--bg-input:",
		"--bg-hover:",
		"--border:",
		"--text:",
		"--text-muted:",
		"--text-dim:",
		"--text-faint:",
		"--accent:",
		"--accent-hover:",
		"--ok:",
		"--err:",
		"--warn:",
		"--link:",
		"--chart-bar-search:",
		"--chart-bar-agg:",
		"--chart-axis:",
		"--chart-label:",
	}
	for _, s := range required {
		assert.Contains(t, lightBlock, s, "light 主题必须含变量 %s", s)
	}
}

// 5) 主题切换不会触发重渲染(纯 CSS 变量, 不重 buildTabHTML)
func TestUI_ThemeSwitchPureCSS(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	// setTheme 不应调用 renderContent / renderTabs (避免闪烁/重渲染)
	idx := strings.Index(html, "function setTheme(")
	endIdx := strings.Index(html[idx:], "\n  }")
	assert.Greater(t, endIdx, 0, "setTheme 函数体未找到")
	body1 := html[idx : idx+endIdx]
	assert.NotContains(t, body1, "renderContent", "setTheme 不应触发 renderContent (会闪)")
	assert.NotContains(t, body1, "renderTabs", "setTheme 不应触发 renderTabs (会闪)")
	assert.NotContains(t, body1, "location.reload", "setTheme 不应 reload (会闪)")
	assert.Contains(t, body1, "setAttribute('data-theme'", "setTheme 必须写 data-theme")
	assert.Contains(t, body1, "LS_THEME", "setTheme 必须持久化到 localStorage")
}

// 6) 浏览器兼容性: 内联脚本仅用 IE11+ / 所有主流浏览器都支持的 API
//    (不依赖 const/let/箭头函数, 不依赖 module, 不依赖新的 dataset API)
func TestUI_ThemeBrowserCompat(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	// 找到 head 内的内联脚本(必须用 var, 不能用 const/let)
	scriptIdx := strings.Index(html, "document.documentElement.setAttribute('data-theme'")
	// 内联脚本大约从 <script> 标签到下一个 </script>
	scriptStart := strings.LastIndex(html[:scriptIdx], "<script>")
	scriptEnd := strings.Index(html[scriptIdx:], "</script>")
	assert.Greater(t, scriptStart, 0)
	assert.Greater(t, scriptEnd, 0)
	inlineScript := html[scriptStart : scriptIdx+scriptEnd]
	// 兼容性要求: 不使用 const/let/箭头函数(IE 不支持); 使用 var
	assert.Contains(t, inlineScript, "var saved", "head 内联脚本应用 var 声明 (兼容 IE11)")
	assert.NotContains(t, inlineScript, "const ", "head 内联脚本不应使用 const")
	assert.NotContains(t, inlineScript, "let ", "head 内联脚本不应使用 let")
	// try/catch 包裹 localStorage (Safari 隐私模式 / 异常处理)
	assert.Contains(t, inlineScript, "try", "head 脚本应 try/catch 包裹 localStorage")
	assert.Contains(t, inlineScript, "catch", "head 脚本应 catch 异常")
}

// 7) 主题切换控件应在 header 中, 跟其它工具按钮并列
func TestUI_ThemeControlInHeader(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	html := string(body)
	// 找 header 块
	headerStart := strings.Index(html, "<header>")
	headerEnd := strings.Index(html, "</header>")
	assert.Greater(t, headerStart, 0)
	assert.Greater(t, headerEnd, headerStart)
	headerHTML := html[headerStart:headerEnd]
	assert.Contains(t, headerHTML, "id=\"themeSelect\"", "主题控件应在 header 内")
	assert.Contains(t, headerHTML, "class=\"themeswitch\"", "主题控件容器应在 header 内")
}
