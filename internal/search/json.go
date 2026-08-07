// Package search 内部 JSON + time 工具,集中 import 第三方包
// 避免在 engine.go 中出现多个 import 块
package search

import (
	"encoding/json"
	"time"
)

// stdJSONUnmarshal 代理给 encoding/json
func stdJSONUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// stdJSONMarshal 代理给 encoding/json
func stdJSONMarshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// stdTimeNow 返回当前毫秒时间戳
func stdTimeNow() int64 {
	return time.Now().UnixMilli()
}
