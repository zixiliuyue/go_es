// settings 端点单元测试
// 覆盖: 创建索引时 settings 持久化、GET /{index}/_settings 单索引查询、
// 多索引 (idx1,idx2) 与通配 (idx*) 查询、GET /_settings 全量查询、
// 不存在索引返回 404、空 settings 返回空对象
package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSettings_PersistAndReturn 创建索引时写入 settings,GET 能正确取回
func TestSettings_PersistAndReturn(t *testing.T) {
	ts := newTestServer(t)

	// 1. 创建带 settings 的索引
	body := map[string]interface{}{
		"settings": map[string]interface{}{
			"number_of_shards":   "3",
			"number_of_replicas": "1",
			"refresh_interval":   "5s",
		},
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"title": map[string]interface{}{"type": "text"},
			},
		},
	}
	resp, _ := do(t, ts, "PUT", "/myindex", body)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "创建索引应返回 200")

	// 2. 取回 settings
	resp, raw := do(t, ts, "GET", "/myindex/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))

	myindex, ok := result["myindex"].(map[string]interface{})
	require.True(t, ok, "响应应包含 myindex 键")

	settings, ok := myindex["settings"].(map[string]interface{})
	require.True(t, ok, "myindex 应包含 settings 键")

	assert.Equal(t, "3", settings["number_of_shards"], "number_of_shards 应为 3")
	assert.Equal(t, "1", settings["number_of_replicas"], "number_of_replicas 应为 1")
	assert.Equal(t, "5s", settings["refresh_interval"], "refresh_interval 应为 5s")
}

// TestSettings_EmptySettings 创建时无 settings,返回空对象
func TestSettings_EmptySettings(t *testing.T) {
	ts := newTestServer(t)

	// 1. 创建无 settings 的索引
	resp, _ := do(t, ts, "PUT", "/emptyindex", map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "keyword"},
			},
		},
	})
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// 2. 取回 settings,应为空对象
	resp, raw := do(t, ts, "GET", "/emptyindex/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))

	emptyindex, ok := result["emptyindex"].(map[string]interface{})
	require.True(t, ok)

	settings, ok := emptyindex["settings"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, 0, len(settings), "无 settings 时应返回空对象")
}

// TestSettings_MultiIndex 多索引 (idx1,idx2) 查询
func TestSettings_MultiIndex(t *testing.T) {
	ts := newTestServer(t)

	// 创建两个带不同 settings 的索引
	do(t, ts, "PUT", "/idx1", map[string]interface{}{
		"settings": map[string]interface{}{"number_of_shards": "1"},
	})
	do(t, ts, "PUT", "/idx2", map[string]interface{}{
		"settings": map[string]interface{}{"number_of_shards": "2"},
	})

	// 批量查询
	resp, raw := do(t, ts, "GET", "/idx1,idx2/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))

	// 两个索引都应存在
	require.Contains(t, result, "idx1")
	require.Contains(t, result, "idx2")

	idx1 := result["idx1"].(map[string]interface{})
	idx2 := result["idx2"].(map[string]interface{})

	s1 := idx1["settings"].(map[string]interface{})
	s2 := idx2["settings"].(map[string]interface{})

	assert.Equal(t, "1", s1["number_of_shards"])
	assert.Equal(t, "2", s2["number_of_shards"])
}

// TestSettings_Wildcard 通配模式 (idx*) 查询
func TestSettings_Wildcard(t *testing.T) {
	ts := newTestServer(t)

	// 创建通配可匹配的索引
	do(t, ts, "PUT", "/logs-2025-01", map[string]interface{}{
		"settings": map[string]interface{}{"number_of_shards": "1"},
	})
	do(t, ts, "PUT", "/logs-2025-02", map[string]interface{}{
		"settings": map[string]interface{}{"number_of_shards": "2"},
	})
	// 创建不应被匹配的索引
	do(t, ts, "PUT", "/events", map[string]interface{}{
		"settings": map[string]interface{}{"number_of_shards": "3"},
	})

	// 通配查询
	resp, raw := do(t, ts, "GET", "/logs-*/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))

	// 应匹配 logs-2025-01 和 logs-2025-02,不应包含 events
	assert.Contains(t, result, "logs-2025-01")
	assert.Contains(t, result, "logs-2025-02")
	assert.NotContains(t, result, "events")

	// 验证匹配到的 settings 正确
	s1 := result["logs-2025-01"].(map[string]interface{})["settings"].(map[string]interface{})
	assert.Equal(t, "1", s1["number_of_shards"])
}

// TestSettings_AllIndices GET /_settings 返回全部索引
func TestSettings_AllIndices(t *testing.T) {
	ts := newTestServer(t)

	do(t, ts, "PUT", "/idx_a", map[string]interface{}{
		"settings": map[string]interface{}{"number_of_shards": "1"},
	})
	do(t, ts, "PUT", "/idx_b", map[string]interface{}{
		"settings": map[string]interface{}{"number_of_replicas": "2"},
	})

	resp, raw := do(t, ts, "GET", "/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))

	// 两个索引都应存在
	assert.Contains(t, result, "idx_a")
	assert.Contains(t, result, "idx_b")

	sa := result["idx_a"].(map[string]interface{})["settings"].(map[string]interface{})
	sb := result["idx_b"].(map[string]interface{})["settings"].(map[string]interface{})

	assert.Equal(t, "1", sa["number_of_shards"])
	assert.Equal(t, "2", sb["number_of_replicas"])
}

// TestSettings_NonExistentIndex 不存在的索引返回 404
func TestSettings_NonExistentIndex(t *testing.T) {
	ts := newTestServer(t)

	resp, _ := do(t, ts, "GET", "/nonexistent/_settings", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestSettings_PartialSettings 部分 settings 字段(只有 number_of_shards)
func TestSettings_PartialSettings(t *testing.T) {
	ts := newTestServer(t)

	do(t, ts, "PUT", "/partial", map[string]interface{}{
		"settings": map[string]interface{}{
			"number_of_shards": "5",
		},
	})

	resp, raw := do(t, ts, "GET", "/partial/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))

	partial := result["partial"].(map[string]interface{})
	settings := partial["settings"].(map[string]interface{})
	assert.Equal(t, "5", settings["number_of_shards"])
	// 不存在的字段不应出现
	_, hasReplicas := settings["number_of_replicas"]
	assert.False(t, hasReplicas, "未设置的字段不应出现在响应中")
}

// TestSettings_AfterDeletion 删除索引后 settings 不再返回
func TestSettings_AfterDeletion(t *testing.T) {
	ts := newTestServer(t)

	do(t, ts, "PUT", "/todelete", map[string]interface{}{
		"settings": map[string]interface{}{"number_of_shards": "1"},
	})

	// 确认存在
	resp, raw := do(t, ts, "GET", "/todelete/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// 删除索引
	do(t, ts, "DELETE", "/todelete", nil)

	// 应返回 404
	resp, _ = do(t, ts, "GET", "/todelete/_settings", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// GET /_settings 不应包含已删除的索引
	resp, raw = do(t, ts, "GET", "/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.NotContains(t, result, "todelete", "删除后的索引不应出现在 /_settings 中")
}

// TestSettings_Integration 端到端: 创建索引 → 写入文档 → 查询 settings 仍正确
func TestSettings_Integration(t *testing.T) {
	ts := newTestServer(t)

	// 创建索引
	do(t, ts, "PUT", "/integration", map[string]interface{}{
		"settings": map[string]interface{}{
			"number_of_shards":   "2",
			"number_of_replicas": "0",
		},
	})

	// 写入文档
	do(t, ts, "PUT", "/integration/_doc/1", map[string]interface{}{
		"title": "hello",
	})

	// 查询 settings
	resp, raw := do(t, ts, "GET", "/integration/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))

	settings := result["integration"].(map[string]interface{})["settings"].(map[string]interface{})
	assert.Equal(t, "2", settings["number_of_shards"])
	assert.Equal(t, "0", settings["number_of_replicas"])
}

// TestSettings_WildcardNoMatch 通配无匹配时返回 200 + 空对象
func TestSettings_WildcardNoMatch(t *testing.T) {
	ts := newTestServer(t)

	// 无任何匹配的通配
	resp, raw := do(t, ts, "GET", "/nonexistent-*/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Equal(t, 0, len(result), "通配无匹配应返回空对象")
}

// TestSettings_MultiIndexPartialMissing 多索引查询中部分索引不存在
func TestSettings_MultiIndexPartialMissing(t *testing.T) {
	ts := newTestServer(t)

	do(t, ts, "PUT", "/existing", map[string]interface{}{
		"settings": map[string]interface{}{"number_of_shards": "1"},
	})

	// existing 存在, nonexistent 不存在
	resp, raw := do(t, ts, "GET", "/existing,nonexistent/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))

	// 应只包含 existing
	assert.Contains(t, result, "existing")
	assert.NotContains(t, result, "nonexistent")

	settings := result["existing"].(map[string]interface{})["settings"].(map[string]interface{})
	assert.Equal(t, "1", settings["number_of_shards"])
}

// TestSettings_AllIndicesEmpty 无索引时 GET /_settings 返回空对象
func TestSettings_AllIndicesEmpty(t *testing.T) {
	ts := newTestServer(t)

	resp, raw := do(t, ts, "GET", "/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Equal(t, 0, len(result), "无索引时应返回空对象")
}

// TestSettings_ExactNonExistentWithStar 含 * 的模式即使只含 * 也不应走 404 分支
func TestSettings_ExactNonExistentWithStar(t *testing.T) {
	ts := newTestServer(t)

	// 含 * 但无匹配 → 200 空对象(因为 isExact=false)
	resp, _ := do(t, ts, "GET", "/*not-exact/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestSettings_AllIndicesWithEmptySettings 全量查询时部分索引无 settings
func TestSettings_AllIndicesWithEmptySettings(t *testing.T) {
	ts := newTestServer(t)

	// 创建一个带 settings 和一个不带 settings 的索引
	do(t, ts, "PUT", "/has-settings", map[string]interface{}{
		"settings": map[string]interface{}{"number_of_shards": "1"},
	})
	do(t, ts, "PUT", "/no-settings", map[string]interface{}{})

	resp, raw := do(t, ts, "GET", "/_settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))

	// 两个索引都应存在
	assert.Contains(t, result, "has-settings")
	assert.Contains(t, result, "no-settings")

	// 无 settings 的索引应返回空对象
	noSettings := result["no-settings"].(map[string]interface{})
	settings := noSettings["settings"].(map[string]interface{})
	assert.Equal(t, 0, len(settings), "无 settings 的索引应返回空对象")
}
