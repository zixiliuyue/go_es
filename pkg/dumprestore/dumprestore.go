// Package dumprestore 提供 go_es 数据的 NDJSON 导出与导入能力。
//
// 设计目标:
//   - 与服务端解耦:纯 HTTP 客户端实现,仅依赖 go 标准库
//   - dump:从 go_es 的 _search 端点滚动读取全部文档,写 NDJSON 文件
//   - restore:读取 NDJSON 文件,通过 _bulk 端点批量写回 go_es
//   - 支持自定义索引过滤、批量大小、Basic 认证等
//
// 文件格式约定(NDJSON):每行一个 JSON 对象,包含 _index / _id / _source 字段,
// 且文件末尾附带一行 __dump_meta__ 元数据(版本、文档数、创建时间),以便 restore 端做完整性校验。
//
// 典型用法:
//
//	err := dumprestore.NewExporter(cfg).Run(ctx, outPath)
//	err := dumprestore.NewImporter(cfg).Run(ctx, inPath)
package dumprestore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// 常量
const (
	// defaultPageSize 默认每页读取的文档数
	defaultPageSize = 1000
	// defaultBatchSize 默认 restore 时每批写入的文档数
	defaultBatchSize = 500
	// dumpVersion dump 文件格式版本号
	dumpVersion = 1
)

// metaMarker 文件末尾元数据行的 marker
const metaMarker = "__dump_meta__"

// ExporterConfig 导出器配置
type ExporterConfig struct {
	// BaseURL go_es 服务端基础 URL,例如 "http://localhost:9200"
	BaseURL string
	// Indices 需要导出的索引列表,为空表示全部索引
	Indices []string
	// PageSize 每 _search 读取的条数,默认 1000
	PageSize int
	// Username Basic 用户名
	Username string
	// Password Basic 密码
	Password string
	// HTTPClient 自定义 http 客户端(可为 nil)
	HTTPClient *http.Client
	// Progress 进度回调,可选
	Progress func(exported int)
}

// ImporterConfig 导入器配置
type ImporterConfig struct {
	// BaseURL go_es 服务端基础 URL
	BaseURL string
	// TargetIndex 强制覆盖写入的索引名(可选,空则以文档内 _index 为准)
	TargetIndex string
	// BatchSize 每 _bulk 写入条数,默认 500
	BatchSize int
	// Username Basic 用户名
	Username string
	// Password Basic 密码
	Password string
	// HTTPClient 自定义 http 客户端(可为 nil)
	HTTPClient *http.Client
	// Progress 进度回调,可选
	Progress func(restored, errors int)
}

// ExportMeta dump 文件末尾元数据行
type ExportMeta struct {
	Marker      string    `json:"_marker"`
	Version     int       `json:"version"`
	DocCount    int       `json:"doc_count"`
	IndexCount  int       `json:"index_count,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	SourceIndex string    `json:"source_index,omitempty"`
}

// ExportedDoc 写入 NDJSON 的单条文档记录
type ExportedDoc struct {
	Index  string                 `json:"_index"`
	ID     string                 `json:"_id"`
	Source map[string]interface{} `json:"_source,omitempty"`
}

// ---------- Exporter ----------

// Exporter 负责把 go_es 文档导出到 NDJSON 文件
type Exporter struct {
	cfg ExporterConfig
	cli *http.Client
}

// NewExporter 创建导出器
func NewExporter(cfg ExporterConfig) *Exporter {
	cli := cfg.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = defaultPageSize
	}
	return &Exporter{cfg: cfg, cli: cli}
}

// Run 执行导出。outPath 为空时写到 stdout。
func (e *Exporter) Run(ctx context.Context, outPath string) (int, error) {
	// 1. 先解析要导出的索引
	indices, err := e.resolveIndices(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve indices: %w", err)
	}
	if len(indices) == 0 {
		return 0, errors.New("no indices to export")
	}

	// 2. 打开输出文件
	var w *bufio.Writer
	var outFile *os.File
	if outPath == "" || outPath == "-" {
		w = bufio.NewWriter(os.Stdout)
	} else {
		f, err := os.Create(outPath)
		if err != nil {
			return 0, fmt.Errorf("create output file: %w", err)
		}
		outFile = f
		w = bufio.NewWriterSize(f, 64*1024)
	}

	// 3. 逐索引滚动导出
	var exported int
	for _, idx := range indices {
		n, err := e.exportIndex(ctx, idx, w)
		if err != nil {
			if outFile != nil {
				_ = outFile.Close()
			}
			return exported, fmt.Errorf("export index %s: %w", idx, err)
		}
		exported += n
		if e.cfg.Progress != nil {
			e.cfg.Progress(exported)
		}
	}

	// 4. 写入元数据行
	exportMeta := ExportMeta{
		Marker:     metaMarker,
		Version:    dumpVersion,
		DocCount:   exported,
		IndexCount: len(indices),
		CreatedAt:  time.Now().UTC(),
	}
	metaLine, _ := json.Marshal(exportMeta)
	if _, err := w.Write(metaLine); err != nil {
		if outFile != nil {
			_ = outFile.Close()
		}
		return exported, fmt.Errorf("write meta line: %w", err)
	}
	if err := w.Flush(); err != nil {
		if outFile != nil {
			_ = outFile.Close()
		}
		return exported, fmt.Errorf("flush output: %w", err)
	}
	if outFile != nil {
		if err := outFile.Close(); err != nil {
			return exported, fmt.Errorf("close output file: %w", err)
		}
	}
	return exported, nil
}

// resolveIndices 得到最终要导出的索引列表
//   - 若 cfg.Indices 非空,直接使用
//   - 否则调 /_cat/indices 解析全部索引
func (e *Exporter) resolveIndices(ctx context.Context) ([]string, error) {
	if len(e.cfg.Indices) > 0 {
		seen := make(map[string]struct{}, len(e.cfg.Indices))
		out := make([]string, 0, len(e.cfg.Indices))
		for _, i := range e.cfg.Indices {
			i = strings.TrimSpace(i)
			if i == "" {
				continue
			}
			if _, ok := seen[i]; ok {
				continue
			}
			seen[i] = struct{}{}
			out = append(out, i)
		}
		return out, nil
	}
	// 调 _cat/indices, 过滤掉系统索引
	u, err := url.Parse(e.cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	u.Path = "/_cat/indices"
	u.RawQuery = "format=json"
	body, err := e.doRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Index string `json:"index"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("parse _cat/indices: %w", err)
		}
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if strings.HasPrefix(r.Index, ".") {
			continue // 跳过系统索引
		}
		out = append(out, r.Index)
	}
	return out, nil
}

// exportIndex 导出单个索引的全部文档
//   - 通过 match_all + from/size 翻页,依赖 total.value
//   - 索引不存在时跳过
func (e *Exporter) exportIndex(ctx context.Context, idx string, w *bufio.Writer) (int, error) {
	var total int64
	var exported int
	from := 0
	for {
		if err := ctx.Err(); err != nil {
			return exported, err
		}
		payload := map[string]interface{}{
			"query": map[string]interface{}{
				"match_all": map[string]interface{}{},
			},
			"from":             from,
			"size":             e.cfg.PageSize,
			"track_total_hits": true,
		}
		body, err := e.doSearch(ctx, idx, payload)
		if err != nil {
			// 索引不存在 / 404 直接跳过
			if isNotFound(err) {
				return exported, nil
			}
			return exported, err
		}
		var resp searchResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return exported, fmt.Errorf("parse search response: %w", err)
		}
		// 首次拿到 total
		if from == 0 {
			total = resp.Hits.Total.Value
			if total == 0 {
				return 0, nil
			}
		}
		if len(resp.Hits.Hits) == 0 {
			break
		}
		for _, h := range resp.Hits.Hits {
			doc := ExportedDoc{
				Index:  idx,
				ID:     h.ID,
				Source: h.Source,
			}
			line, err := json.Marshal(doc)
			if err != nil {
				return exported, fmt.Errorf("marshal doc: %w", err)
			}
			if _, err := w.Write(line); err != nil {
				return exported, fmt.Errorf("write doc: %w", err)
			}
			if _, err := w.Write([]byte("\n")); err != nil {
				return exported, fmt.Errorf("write newline: %w", err)
			}
			exported++
		}
		from += len(resp.Hits.Hits)
		if int64(from) >= total || len(resp.Hits.Hits) < e.cfg.PageSize {
			break
		}
	}
	return exported, nil
}

// searchResponse 对应 _search 响应的最小结构
type searchResponse struct {
	Hits struct {
		Total struct {
			Value int64 `json:"value"`
		} `json:"total"`
		Hits []struct {
			ID     string                 `json:"_id"`
			Source map[string]interface{} `json:"_source,omitempty"`
		} `json:"hits"`
	} `json:"hits"`
}

// doSearch 调用 /{index}/_search
func (e *Exporter) doSearch(ctx context.Context, index string, payload map[string]interface{}) ([]byte, error) {
	u, err := url.Parse(e.cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	u.Path = "/" + index + "/_search"
	body, _ := json.Marshal(payload)
	return e.doRequest(ctx, http.MethodPost, u.String(), body)
}

// doRequest 统一发请求(含 Basic 认证、错误解析)
func (e *Exporter) doRequest(ctx context.Context, method, rawURL string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.Username != "" {
		req.SetBasicAuth(e.cfg.Username, e.cfg.Password)
	}
	resp, err := e.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, &httpError{Status: resp.StatusCode, Body: string(respBody)}
	}
	return respBody, nil
}

// httpError HTTP 错误响应
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// isNotFound 判断是否为 404
func isNotFound(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.Status == http.StatusNotFound
	}
	return false
}

// ---------- Importer ----------

// Importer 负责把 NDJSON 文件导入 go_es
type Importer struct {
	cfg ImporterConfig
	cli *http.Client
}

// NewImporter 创建导入器
func NewImporter(cfg ImporterConfig) *Importer {
	cli := cfg.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	return &Importer{cfg: cfg, cli: cli}
}

// Run 执行导入。inPath 为空时从 stdin 读取。
func (im *Importer) Run(ctx context.Context, inPath string) (restored, errs int, meta *ExportMeta, err error) {
	var r io.Reader
	var inFile *os.File
	if inPath == "" || inPath == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(inPath)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("open input file: %w", err)
		}
		inFile = f
		r = f
	}
	if inFile != nil {
		defer inFile.Close()
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024) // 最大 32MB 单行

	var batch []ExportedDoc
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, batchErrs, errBatch := im.flushBatch(ctx, batch)
		if errBatch != nil {
			errs += len(batch)
		} else {
			restored += n
			errs += batchErrs
		}
		if im.cfg.Progress != nil {
			im.cfg.Progress(restored, errs)
		}
		batch = batch[:0]
		return errBatch
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return restored, errs, meta, err
		}
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		// 元数据行只解析,不写入
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return restored, errs, meta, fmt.Errorf("parse line: %w", err)
		}
		if m, ok := raw["_marker"]; ok && m == metaMarker {
			// 解析元数据(忽略错误,仅用于返回)
			var em ExportMeta
			_ = json.Unmarshal([]byte(line), &em)
			meta = &em
			continue
		}
		var doc ExportedDoc
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			return restored, errs, meta, fmt.Errorf("parse doc line: %w", err)
		}
		if im.cfg.TargetIndex != "" {
			doc.Index = im.cfg.TargetIndex
		}
		if doc.Index == "" || doc.ID == "" {
			errs++
			continue
		}
		batch = append(batch, doc)
		if len(batch) >= im.cfg.BatchSize {
			if err := flush(); err != nil {
				return restored, errs, meta, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return restored, errs, meta, fmt.Errorf("scan input: %w", err)
	}
	if err := flush(); err != nil {
		return restored, errs, meta, err
	}
	return restored, errs, meta, nil
}

// flushBatch 将一批文档通过 _bulk 写入
//   - 生成 NDJSON 动作行(索引 + 文档)
//   - 解析 bulk 响应统计成功/失败数
func (im *Importer) flushBatch(ctx context.Context, batch []ExportedDoc) (restored, errs int, err error) {
	var buf strings.Builder
	for _, d := range batch {
		metaLine := map[string]interface{}{
			"index": map[string]interface{}{
				"_index": d.Index,
				"_id":    d.ID,
			},
		}
		metaBytes, _ := json.Marshal(metaLine)
		buf.Write(metaBytes)
		buf.WriteByte('\n')
		srcBytes, _ := json.Marshal(d.Source)
		buf.Write(srcBytes)
		buf.WriteByte('\n')
	}

	u, err := url.Parse(im.cfg.BaseURL)
	if err != nil {
		return 0, len(batch), err
	}
	u.Path = "/_bulk"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(buf.String()))
	if err != nil {
		return 0, len(batch), err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if im.cfg.Username != "" {
		req.SetBasicAuth(im.cfg.Username, im.cfg.Password)
	}
	resp, err := im.cli.Do(req)
	if err != nil {
		return 0, len(batch), err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return 0, len(batch), &httpError{Status: resp.StatusCode, Body: string(body)}
	}

	// 解析 bulk 响应, 统计每条的 status
	var bulkResp struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			Status int `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &bulkResp); err != nil {
		return 0, len(batch), fmt.Errorf("parse bulk response: %w", err)
	}
	for _, it := range bulkResp.Items {
		for _, action := range it {
			if action.Status >= 200 && action.Status < 300 {
				restored++
			} else {
				errs++
			}
		}
	}
	return restored, errs, nil
}

// ---------- 便捷函数 ----------

// DumpToFile 将指定索引导出到 NDJSON 文件,返回导出文档数
func DumpToFile(ctx context.Context, baseURL, outPath string, indices []string) (int, error) {
	return NewExporter(ExporterConfig{
		BaseURL: baseURL,
		Indices: indices,
	}).Run(ctx, outPath)
}

// RestoreFromFile 从 NDJSON 文件导入, 返回 (restored, errs, meta, err)
func RestoreFromFile(ctx context.Context, baseURL, inPath, targetIndex string) (int, int, *ExportMeta, error) {
	return NewImporter(ImporterConfig{
		BaseURL:     baseURL,
		TargetIndex: targetIndex,
	}).Run(ctx, inPath)
}
