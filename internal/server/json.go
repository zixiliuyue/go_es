// 共享 JSON 工具
package server

import "encoding/json"

// jsonUnmarshal 与 search 包的 jsonUnmarshal 含义一致,集中放置以避免文件顶部重复 import
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
