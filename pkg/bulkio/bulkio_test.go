// Package bulkio 包的单元测试
// 测试批量数据导入导出功能
package bulkio

import (
	"bytes"
	"context"
	"encoding/csv"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"github.com/zixiliuyue/go_es/pkg/document"
	"github.com/zixiliuyue/go_es/pkg/index"
	"github.com/zixiliuyue/go_es/pkg/search"
	"go.uber.org/zap"
)

func setupBulkIOTest(t *testing.T) (*BulkIO, context.Context, string) {
	logger, _ := zap.NewDevelopment()

	cfg := client.Config{
		Addresses: []string{"http://localhost:9200"},
		Logger:    logger,
	}

	c, err := client.NewClient(cfg)
	if err != nil {
		t.Logf("Cannot connect to Elasticsearch: %v", err)
		t.Skip("Skipping test because Elasticsearch is not available")
	}

	// 创建测试索引
	indexManager := index.NewManager(c.GetES(), logger)
	indexName := "test_bulkio"
	_ = indexManager.DeleteIndex(context.Background(), indexName)

	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"name":  map[string]string{"type": "text"},
				"age":   map[string]string{"type": "integer"},
				"email": map[string]string{"type": "keyword"},
			},
		},
	}
	err = indexManager.CreateIndex(context.Background(), indexName, mapping)
	assert.NoError(t, err)

	docManager := document.NewManager(c.GetES(), logger)
	searcher := search.NewSearcher(c.GetES(), logger)
	bulk := NewBulkIO(docManager, searcher, logger)

	return bulk, context.Background(), indexName
}

func TestNewBulkIO(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	// 实际上我们需要client，这里简化测试
	// 只要能创建就说明没问题
	bulk := NewBulkIO(nil, nil, logger)
	assert.NotNil(t, bulk)
}

func TestImportFromCSVReader(t *testing.T) {
	bulk, ctx, indexName := setupBulkIOTest(t)

	// CSV数据，第一行是表头
	csvData := `name,age,email
	zhangsan,25,zhangsan@example.com
	lisi,30,lisi@example.com
	wangwu,35,wangwu@example.com
	`

	reader := bytes.NewReader([]byte(csvData))
	config := ImportConfig{
		IndexName:  indexName,
		BatchSize:   2,
		SkipHeader:  true,
		IDField:     "",
	}

	total, success, failed, err := bulk.ImportFromCSVReader(ctx, reader, config, nil)
	assert.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Equal(t, int64(3), success)
	assert.Equal(t, int64(0), failed)
}

func TestImportFromCSVReader_NoSkipHeader(t *testing.T) {
	bulk, ctx, indexName := setupBulkIOTest(t)

	// CSV数据，没有表头，所有行都导入，按位置命名
	csvData := `zhangsan,25,zhangsan@example.com
	lisi,30,lisi@example.com
	`

	reader := bytes.NewReader([]byte(csvData))
	config := ImportConfig{
		IndexName:  indexName,
		BatchSize:   2,
		SkipHeader:  false,
		IDField:     "",
	}

	total, success, failed, err := bulk.ImportFromCSVReader(ctx, reader, config, nil)
	assert.NoError(t, err)
	assert.Equal(t, int64(2), total)
	// 每条都会成功，因为导入只是创建文档
	assert.GreaterOrEqual(t, success, int64(1))
}

func TestImportFromJSONLinesReader(t *testing.T) {
	bulk, ctx, indexName := setupBulkIOTest(t)

	// JSON Lines数据
	jsonData := `{"name": "zhangsan", "age": 25, "email": "zhangsan@example.com"}
{"name": "lisi", "age": 30, "email": "lisi@example.com"}
{"name": "wangwu", "age": 35, "email": "wangwu@example.com"}
`

	reader := bytes.NewReader([]byte(jsonData))
	config := ImportConfig{
		IndexName: indexName,
		BatchSize:  2,
		IDField:    "age",
	}

	total, success, failed, err := bulk.ImportFromJSONLinesReader(ctx, reader, config, nil)
	assert.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Equal(t, int64(3), success)
	assert.Equal(t, int64(0), failed)
}

func TestImportFromJSONLinesReader_InvalidJSON(t *testing.T) {
	bulk, ctx, indexName := setupBulkIOTest(t)

	// 包含无效JSON
	jsonData := `{"name": "zhangsan", "age": 25}
	this is invalid json
	{"name": "wangwu", "age": 35}
`

	reader := bytes.NewReader([]byte(jsonData))
	config := ImportConfig{
		IndexName: indexName,
		BatchSize:  2,
	}

	total, success, failed, err := bulk.ImportFromJSONLinesReader(ctx, reader, config, nil)
	assert.NoError(t, err)
	assert.Equal(t, int64(3), total)
	// 第二行解析失败
	assert.Equal(t, int64(2), success)
	assert.Equal(t, int64(1), failed)
}

func TestEstimateImportProgress(t *testing.T) {
	// 创建临时文件
	tmpFile := t.TempDir() + "/test_import.txt"
	content := `line1
line2
line3
`
	// 写入临时文件
	err := writeTempFile(tmpFile, content)
	assert.NoError(t, err)

	count, err := EstimateImportProgress(tmpFile, true)
	assert.NoError(t, err)
	assert.Equal(t, int64(3), count)
}

func TestValidateImportConfig(t *testing.T) {
	// 空索引名称
	config := ImportConfig{
		IndexName: "",
		BatchSize:  100,
	}
	err := ValidateImportConfig(&config)
	assert.Error(t, err)

	// 负批量大小
	config = ImportConfig{
		IndexName: "test",
		BatchSize:  -1,
	}
	err = ValidateImportConfig(&config)
	assert.Error(t, err)

	// 正常配置
	config = ImportConfig{
		IndexName: "test",
		BatchSize:  100,
	}
	err = ValidateImportConfig(&config)
	assert.NoError(t, err)
}

func TestProgressCallback(t *testing.T) {
	bulk, ctx, indexName := setupBulkIOTest(t)

	var called bool
	var processed int64
	var success int64
	var failed int64

	callback := func(p, t int64, s, f int64) {
		called = true
		processed = p
		success = s
		failed = f
	}

	csvData := `a,b,c
	1,2,3
	4,5,6
	`

	reader := bytes.NewReader([]byte(csvData))
	config := ImportConfig{
		IndexName:  indexName,
		BatchSize:   1,
		SkipHeader:  true,
	}

	total, _, _, err := bulk.ImportFromCSVReader(ctx, reader, config, callback)
	assert.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, int64(2), processed)
	assert.Equal(t, total, processed)
}

// Helper to write temp file
func writeTempFile(path, content string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}
