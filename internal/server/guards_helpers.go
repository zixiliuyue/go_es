// guards.go 配套工具函数
package server

import "encoding/base64"

// base64Decode 标准 base64 解码, 错误时返回空串
func base64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
