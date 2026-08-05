// Package search 内部 JSON 工具,集中 import encoding/json
// 避免在 engine.go 中出现多个 import 块
package search

import "encoding/json"

// stdJSONUnmarshal 代理给 encoding/json
func stdJSONUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// stdJSONMarshal 代理给 encoding/json
func stdJSONMarshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}
