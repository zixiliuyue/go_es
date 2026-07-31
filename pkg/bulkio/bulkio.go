// Package bulkio 提供批量数据导入导出功能
// 支持从CSV/JSON文件批量导入数据到Elasticsearch，支持将搜索结果批量导出到文件
package bulkio

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/zixiliuyue/go_es/pkg/document"
	"github.com/zixiliuyue/go_es/pkg/search"
	"go.uber.org/zap"
)

// ImportConfig 导入配置
type ImportConfig struct {
	// IndexName 目标索引名称
	IndexName string
	// BatchSize 批量大小
	BatchSize int
	// IDField 文档ID字段（为空则自动生成）
	IDField string
	// SkipHeader 是否跳过CSV表头
	SkipHeader bool
	// MaxRetries 失败批量最大重试次数（0表示不重试）
	MaxRetries int
	// RetryDelayMs 重试间隔毫秒数
	RetryDelayMs int
}

// ExportConfig 导出配置
type ExportConfig struct {
	// IndexName 源索引名称
	IndexName string
	// Query 导出查询条件
	Query map[string]interface{}
	// BatchSize 每批读取大小
	ScrollSize int
	// ScrollKeepAlive scroll保持时间
	ScrollKeepAlive string
}

// ProgressCallback 进度回调
type ProgressCallback func(processed, total int64, success, failed int)

// BulkIO 批量导入导出管理器
type BulkIO struct {
	docManager *document.Manager
	searcher   *search.Searcher
	log        *zap.Logger
}

// NewBulkIO 创建批量导入导出管理器
func NewBulkIO(docManager *document.Manager, searcher *search.Searcher, log *zap.Logger) *BulkIO {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &BulkIO{
		docManager: docManager,
		searcher:   searcher,
		log:        log,
	}
}

// ImportFromCSV 从CSV文件批量导入
// 参数:
//
//	ctx: 上下文
//	filePath: CSV文件路径
//	config: 导入配置
//	callback: 进度回调，可以为nil
//
// 返回:
//
//	total: 总记录数
//	success: 成功数
//	failed: 失败数
//	error: 错误
func (b *BulkIO) ImportFromCSV(ctx context.Context, filePath string, config ImportConfig, callback ProgressCallback) (int64, int64, int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		b.log.Error("Failed to open CSV file", zap.String("path", filePath), zap.Error(err))
		return 0, 0, 0, err
	}
	defer file.Close()

	return b.ImportFromCSVReader(ctx, file, config, callback)
}

// ImportFromCSVReader 从io.Reader批量导入CSV
func (b *BulkIO) ImportFromCSVReader(ctx context.Context, reader io.Reader, config ImportConfig, callback ProgressCallback) (int64, int64, int64, error) {
	if config.BatchSize <= 0 {
		config.BatchSize = 100 // 默认批量100条
	}

	csvReader := csv.NewReader(reader)
	var total int64
	var success int64
	var failed int64

	operations := make([]document.BulkOperation, 0, config.BatchSize)

	rowCount := 0
	for {
		record, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			b.log.Error("Failed to read CSV record", zap.Int64("total", total), zap.Error(err))
			failed++
			total++
			continue
		}

		rowCount++
		if config.SkipHeader && rowCount == 1 {
			continue
		}

		// 将CSV行转换为map
		doc := make(map[string]interface{})
		// 如果CSV有表头，第一行是表头，我们已经跳过了，这里需要假设第一行是表头，但是用户没有跳过，这里无法处理
		// 简化处理：按位置存储，如果用户有表头，需要开启SkipHeader
		for i, value := range record {
			key := fmt.Sprintf("field_%d", i)
			doc[key] = value
		}

		var docID string
		if config.IDField != "" {
			if idVal, ok := doc[config.IDField]; ok {
				docID = fmt.Sprintf("%v", idVal)
			}
		}

		operations = append(operations, document.BulkOperation{
			Operation: "index",
			Index:     config.IndexName,
			ID:        docID,
			Doc:       doc,
		})

		total++

		// 达到批量大小，执行批量操作
		if len(operations) >= config.BatchSize {
			succ, fail := b.BulkWithRetry(ctx, operations, config.MaxRetries, config.RetryDelayMs)
			success += int64(succ)
			failed += int64(fail)
			operations = operations[:0]

			if callback != nil {
				callback(total, -1, int(success), int(failed))
			}
		}
	}

	// 处理剩余的
	if len(operations) > 0 {
		succ, fail := b.BulkWithRetry(ctx, operations, config.MaxRetries, config.RetryDelayMs)
		success += int64(succ)
		failed += int64(fail)

		if callback != nil {
			callback(total, -1, int(success), int(failed))
		}
	}

	b.log.Info("CSV import completed",
		zap.Int64("total", total),
		zap.Int64("success", success),
		zap.Int64("failed", failed))

	return total, success, failed, nil
}

// ImportFromJSONLines 从JSON Lines文件批量导入
// 每行一个JSON对象
func (b *BulkIO) ImportFromJSONLines(ctx context.Context, filePath string, config ImportConfig, callback ProgressCallback) (int64, int64, int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		b.log.Error("Failed to open JSON file", zap.String("path", filePath), zap.Error(err))
		return 0, 0, 0, err
	}
	defer file.Close()

	return b.ImportFromJSONLinesReader(ctx, file, config, callback)
}

// ImportFromJSONLinesReader 从io.Reader批量导入JSON Lines
func (b *BulkIO) ImportFromJSONLinesReader(ctx context.Context, reader io.Reader, config ImportConfig, callback ProgressCallback) (int64, int64, int64, error) {
	if config.BatchSize <= 0 {
		config.BatchSize = 100
	}

	scanner := bufio.NewScanner(reader)
	var total int64
	var success int64
	var failed int64

	operations := make([]document.BulkOperation, 0, config.BatchSize)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var doc map[string]interface{}
		err := json.Unmarshal([]byte(line), &doc)
		if err != nil {
		b.log.Warn("Failed to parse JSON line", zap.String("line", line), zap.Error(err))
		failed++
		total++
		continue
	}

		var docID string
		if config.IDField != "" {
			if idVal, ok := doc[config.IDField]; ok {
				docID = fmt.Sprintf("%v", idVal)
			}
		}

		operations = append(operations, document.BulkOperation{
			Operation: "index",
			Index:     config.IndexName,
			ID:        docID,
			Doc:       doc,
		})

		total++

		if len(operations) >= config.BatchSize {
			succ, fail := b.BulkWithRetry(ctx, operations, config.MaxRetries, config.RetryDelayMs)
			success += int64(succ)
			failed += int64(fail)
			operations = operations[:0]

			if callback != nil {
				callback(total, -1, int(success), int(failed))
			}
		}
	}

	if err := scanner.Err(); err != nil {
		b.log.Error("Error scanning JSON file", zap.Error(err))
		return total, success, failed, err
	}

	if len(operations) > 0 {
		succ, fail, _ := b.docManager.Bulk(ctx, operations)
		success += int64(succ)
		failed += int64(fail)

		if callback != nil {
			callback(total, -1, int(success), int(failed))
		}
	}

	b.log.Info("JSON Lines import completed",
		zap.Int64("total", total),
		zap.Int64("success", success),
		zap.Int64("failed", failed))

	return total, success, failed, nil
}

// ExportToCSV 将搜索结果导出到CSV文件
func (b *BulkIO) ExportToCSV(ctx context.Context, filePath string, config ExportConfig, fields []string) error {
	file, err := os.Create(filePath)
	if err != nil {
		b.log.Error("Failed to create output CSV file", zap.String("path", filePath), zap.Error(err))
		return err
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// 写入表头
	if len(fields) > 0 {
		if err := writer.Write(fields); err != nil {
			b.log.Error("Failed to write CSV header", zap.Error(err))
			return err
		}
	}

	err = b.ExportToCSVWriter(ctx, writer, config, fields)
	if err != nil {
		return err
	}

	b.log.Info("CSV export completed", zap.String("path", filePath))
	return nil
}

// ExportToCSVWriter 将搜索结果导出到csv.Writer
func (b *BulkIO) ExportToCSVWriter(ctx context.Context, writer *csv.Writer, config ExportConfig, fields []string) error {
	if config.ScrollSize <= 0 {
		config.ScrollSize = 100
	}
	if config.ScrollKeepAlive == "" {
		config.ScrollKeepAlive = "5m"
	}

	// 构建查询
	builder := search.NewSearch()
	if config.Query != nil {
		builder.SetQuery(config.Query)
	}
	builder.Pagination(0, config.ScrollSize)

	// 创建scroll迭代器
	iterator, err := b.searcher.NewScrollIterator(ctx, config.IndexName, config.ScrollKeepAlive, builder)
	if err != nil {
		return err
	}
	defer iterator.Close()

	var total int
	for iterator.Next() {
		resp := iterator.Result()
		for _, hit := range resp.Hits.Hits {
			var doc map[string]interface{}
			if err := json.Unmarshal(hit.Source, &doc); err != nil {
				b.log.Warn("Failed to unmarshal hit source", zap.String("id", hit.ID), zap.Error(err))
				continue
			}

			// 提取字段值
			record := make([]string, 0, len(fields))
			for _, field := range fields {
				val, ok := doc[field]
				if !ok {
					record = append(record, "")
				} else {
					record = append(record, fmt.Sprintf("%v", val))
				}
			}

			if err := writer.Write(record); err != nil {
				b.log.Error("Failed to write CSV record", zap.Error(err))
				continue
			}
			total++
		}

		writer.Flush()
	}

	if err := iterator.Err(); err != nil {
		return err
	}

	b.log.Info("CSV export completed", zap.Int("total_records", total))
	return nil
}

// ExportToJSONLines 将搜索结果导出为JSON Lines文件
func (b *BulkIO) ExportToJSONLines(ctx context.Context, filePath string, config ExportConfig) error {
	file, err := os.Create(filePath)
	if err != nil {
		b.log.Error("Failed to create output JSON file", zap.String("path", filePath), zap.Error(err))
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	defer writer.Flush()

	return b.ExportToJSONLinesWriter(ctx, writer, config)
}

// ExportToJSONLinesWriter 将搜索结果导出为JSON Lines到io.Writer
func (b *BulkIO) ExportToJSONLinesWriter(ctx context.Context, writer *bufio.Writer, config ExportConfig) error {
	if config.ScrollSize <= 0 {
		config.ScrollSize = 100
	}
	if config.ScrollKeepAlive == "" {
		config.ScrollKeepAlive = "5m"
	}

	// 构建查询
	builder := search.NewSearch()
	if config.Query != nil {
		builder.SetQuery(config.Query)
	}
	builder.Pagination(0, config.ScrollSize)

	// 创建scroll迭代器
	iterator, err := b.searcher.NewScrollIterator(ctx, config.IndexName, config.ScrollKeepAlive, builder)
	if err != nil {
		return err
	}
	defer iterator.Close()

	var total int
	for iterator.Next() {
		resp := iterator.Result()
		for _, hit := range resp.Hits.Hits {
			// 直接输出原始source
			line := string(hit.Source) + "\n"
			if _, err := writer.WriteString(line); err != nil {
				b.log.Error("Failed to write JSON line", zap.Error(err))
				continue
			}
			total++
		}
	}

	if err := iterator.Err(); err != nil {
		return err
	}

	writer.Flush()

	b.log.Info("JSON export completed", zap.Int("total_records", total))
	return nil
}

// EstimateImportProgress 估算导入进度
// 读取文件行数但不处理，用于提前知道总记录数
func EstimateImportProgress(filePath string, isCSV bool) (int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	var count int64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			count++
		}
	}

	if err := scanner.Err(); err != nil {
		return count, err
	}

	return count, nil
}

// ValidateImportConfig 验证导入配置
func ValidateImportConfig(config *ImportConfig) error {
	if config.IndexName == "" {
		return errors.New("index name is required")
	}
	if config.BatchSize < 0 {
		return errors.New("batch size must be non-negative")
	}
	if config.MaxRetries < 0 {
		return errors.New("max retries must be non-negative")
	}
	if config.RetryDelayMs < 0 {
		return errors.New("retry delay must be non-negative")
	}
	return nil
}

// BulkWithRetry 执行批量操作并在失败时重试
// 内部使用，对一个batch进行重试
func (b *BulkIO) BulkWithRetry(
	ctx context.Context,
	operations []document.BulkOperation,
	maxRetries int,
	retryDelayMs int,
) (int, int) {
	if maxRetries <= 0 {
		succ, fail, _ := b.docManager.Bulk(ctx, operations)
		return succ, fail
	}

	var (
		success int
		failed  int
		currentOps = operations
	)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if len(currentOps) == 0 {
			break
		}

		succ, fail, _ := b.docManager.Bulk(ctx, currentOps)
		success += succ
		failed += fail

		if fail == 0 {
			// All succeeded, no need to retry
			break
		}

		// If this is not the last attempt, wait before retrying
		if attempt < maxRetries && retryDelayMs > 0 {
			select {
			case <-ctx.Done():
				// Context cancelled, mark remaining as failed
				failed += len(currentOps) - succ
				return success, failed
			case <-time.After(time.Duration(retryDelayMs) * time.Millisecond):
			}
		}

		b.log.Warn("Bulk operation had failures, will retry",
			zap.Int("attempt", attempt+1),
			zap.Int("failures", fail),
			zap.Int("remaining", len(currentOps)-succ))
	}

	return success, failed
}

// ImportFromCSVWithRetry 从CSV文件批量导入，支持失败重试
// Deprecated: Use ImportFromCSV with ImportConfig.MaxRetries instead
func (b *BulkIO) ImportFromCSVWithRetry(
	ctx context.Context,
	filePath string,
	config ImportConfig,
	callback ProgressCallback,
) (int64, int64, int64, error) {
	return b.ImportFromCSV(ctx, filePath, config, callback)
}
