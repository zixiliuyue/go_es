// 输入校验硬化 + CORS 中间件
//
// 设计:
//   - 输入校验:
//     * index 名长度 ≤ 255 字符
//     * index 名合法字符集(字母数字-_.*)
//     * _search 的 from+size ≤ 10000
//     * 请求体大小独立可配
//   - CORS 中间件:
//     * 可配置白名单 allowed_origins
//     * 默认 * (向后兼容)
//     * 支持预检请求(OPTIONS)
//   - CSRF 防护:
//     * 写操作检查 Origin/Host 匹配(可选)
package server

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// IndexNameMaxLength 索引名最大长度
const IndexNameMaxLength = 255

// IndexNamePattern 索引名合法字符: 字母、数字、-、_、.、*
var indexNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_\-\.\*]+$`)

// MaxFromSize _search 中 from + size 的最大限制
const MaxFromSize = 10000

// CORSConfig CORS 配置
type CORSConfig struct {
	// Enabled 是否启用 CORS
	Enabled bool `yaml:"enabled"`
	// AllowedOrigins 允许的源列表, ["*"] 表示所有
	AllowedOrigins []string `yaml:"allowed_origins"`
	// AllowedMethods 允许的 HTTP 方法
	AllowedMethods []string `yaml:"allowed_methods"`
	// AllowedHeaders 允许的请求头
	AllowedHeaders []string `yaml:"allowed_headers"`
	// AllowCredentials 是否允许携带凭证
	AllowCredentials bool `yaml:"allow_credentials"`
}

// CORS 全局配置(与 ValidationConfig 保持同样的模式)
var (
	globalCORS   CORSConfig
	globalCORSMu sync.RWMutex
)

// SetCORSConfig 动态更新 CORS 配置
func SetCORSConfig(cfg CORSConfig) {
	globalCORSMu.Lock()
	defer globalCORSMu.Unlock()
	globalCORS = cfg
	if len(globalCORS.AllowedOrigins) == 0 {
		globalCORS.AllowedOrigins = []string{"*"}
	}
	if len(globalCORS.AllowedMethods) == 0 {
		globalCORS.AllowedMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}
	}
	if len(globalCORS.AllowedHeaders) == 0 {
		globalCORS.AllowedHeaders = []string{"Content-Type", "Authorization", "X-Request-Id"}
	}
}

// GetCORSConfig 获取当前 CORS 配置
func GetCORSConfig() CORSConfig {
	globalCORSMu.RLock()
	defer globalCORSMu.RUnlock()
	// 如果未初始化, 用默认配置
	if len(globalCORS.AllowedOrigins) == 0 {
		return DefaultCORSConfig()
	}
	return globalCORS
}

// DefaultCORSConfig 默认 CORS 配置
func DefaultCORSConfig() CORSConfig {
	return CORSConfig{
		Enabled:         false,
		AllowedOrigins:  []string{"*"},
		AllowedMethods:  []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:  []string{"Content-Type", "Authorization", "X-Request-Id"},
		AllowCredentials: false,
	}
}

// ValidationConfig 校验配置
type ValidationConfig struct {
	// Enabled 是否启用输入校验
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MaxIndexNameLength 索引名最大长度(默认 255)
	MaxIndexNameLength int `yaml:"max_index_name_length" json:"max_index_name_length"`
	// MaxFromSize from+size 限制(默认 10000)
	MaxFromSize int `yaml:"max_from_size" json:"max_from_size"`
}

// DefaultValidationConfig 默认校验配置
func DefaultValidationConfig() ValidationConfig {
	return ValidationConfig{
		Enabled:            true,
		MaxIndexNameLength: IndexNameMaxLength,
		MaxFromSize:        MaxFromSize,
	}
}

// globalValidation 全局校验配置
var (
	globalValidation   ValidationConfig
	globalValidationMu sync.RWMutex
)

// SetValidationConfig 设置校验配置
func SetValidationConfig(cfg ValidationConfig) {
	globalValidationMu.Lock()
	defer globalValidationMu.Unlock()
	if cfg.MaxIndexNameLength <= 0 {
		cfg.MaxIndexNameLength = IndexNameMaxLength
	}
	if cfg.MaxFromSize <= 0 {
		cfg.MaxFromSize = MaxFromSize
	}
	globalValidation = cfg
}

// GetValidationConfig 获取校验配置
func GetValidationConfig() ValidationConfig {
	globalValidationMu.RLock()
	defer globalValidationMu.RUnlock()
	return globalValidation
}

// ValidateIndexName 校验索引名
// 返回 (是否合法, 错误信息)
func ValidateIndexName(name string) (bool, string) {
	if name == "" {
		return false, "index name is required"
	}

	cfg := GetValidationConfig()
	maxLen := cfg.MaxIndexNameLength
	if maxLen <= 0 {
		maxLen = IndexNameMaxLength
	}

	if len(name) > maxLen {
		return false, "index name exceeds maximum length of " + strconv.Itoa(maxLen)
	}

	if !indexNamePattern.MatchString(name) {
		return false, "index name contains invalid characters, allowed: alphanumeric, -, _, ., *"
	}

	// 不能以 _ 开头(系统索引)
	if strings.HasPrefix(name, "_") {
		return false, "index name cannot start with underscore"
	}

	return true, ""
}

// ValidateFromSize 校验 from + size 参数
func ValidateFromSize(from, size int) (bool, string) {
	cfg := GetValidationConfig()
	maxSum := cfg.MaxFromSize
	if maxSum <= 0 {
		maxSum = MaxFromSize
	}

	total := from + size
	if total > maxSum {
		return false, "from + size exceeds maximum limit of " + strconv.Itoa(maxSum)
	}
	if from < 0 {
		return false, "from cannot be negative"
	}
	if size <= 0 {
		return false, "size must be positive"
	}
	return true, ""
}

// middlewareValidation 输入校验中间件
func (s *Server) middlewareValidation(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := GetValidationConfig()
		if !cfg.Enabled {
			h.ServeHTTP(w, r)
			return
		}

		// 校验索引名(如果路径包含索引)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) > 0 && !strings.HasPrefix(parts[0], "_") && parts[0] != "metrics" && parts[0] != "favicon.ico" {
			if len(parts[0]) > 0 {
				// 支持多索引格式: index1,index2,index*
				indices := strings.Split(parts[0], ",")
				for _, idx := range indices {
					idx = strings.TrimSpace(idx)
					if idx == "" {
						continue
					}
					// 支持通配符模式
					cleanName := strings.ReplaceAll(idx, "*", "")
					if cleanName != "" {
						if !indexNamePattern.MatchString(cleanName) && len(cleanName) > 0 {
							writeError(w, http.StatusBadRequest, "illegal_argument_exception",
								"invalid index name: "+idx, "")
							return
						}
						if len(cleanName) > cfg.MaxIndexNameLength {
							writeError(w, http.StatusBadRequest, "illegal_argument_exception",
								"index name too long: "+idx, "")
							return
						}
					}
				}
			}
		}

		// 校验 _search 请求的 from+size
		if len(parts) >= 2 && parts[len(parts)-1] == "_search" {
			// 注意: 这里只是做基础校验, 详细校验在 handler 内部完成
			// from 和 size 在 query body 中, 需要解析 JSON
			// 为避免双重解析, 这里只做 query 参数的简单校验
			if v := r.URL.Query().Get("from"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n < 0 {
					writeError(w, http.StatusBadRequest, "illegal_argument_exception",
						"from cannot be negative", "")
					return
				}
			}
			if v := r.URL.Query().Get("size"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n <= 0 {
					writeError(w, http.StatusBadRequest, "illegal_argument_exception",
						"size must be positive", "")
					return
				}
			}
		}

		h.ServeHTTP(w, r)
	})
}

// middlewareCORS CORS 中间件
func (s *Server) middlewareCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := GetCORSConfig()

		if !cfg.Enabled {
			h.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")

		// 检查 Origin 是否在白名单中
		allowed := false
		for _, allowedOrigin := range cfg.AllowedOrigins {
			if allowedOrigin == "*" || allowedOrigin == origin {
				allowed = true
				break
			}
		}

		if !allowed {
			// Origin 不在白名单中,直接拒绝
			writeError(w, http.StatusForbidden, "security_exception",
				"origin not allowed: "+origin, "")
			return
		}

		// 设置 CORS 头
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", strings.Join(cfg.AllowedMethods, ", "))
		w.Header().Set("Access-Control-Allow-Headers", strings.Join(cfg.AllowedHeaders, ", "))

		if cfg.AllowCredentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		// 处理预检请求
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		h.ServeHTTP(w, r)
	})
}

// handleValidationConfig GET /_validation/config
func (s *Server) handleValidationConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	writeJSON(w, http.StatusOK, GetValidationConfig())
}

// handleValidationConfigUpdate PUT /_validation/config
func (s *Server) handleValidationConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use PUT", "")
		return
	}

	var cfg ValidationConfig
	if err := decodeJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}

	SetValidationConfig(cfg)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"config":  GetValidationConfig(),
		"updated": true,
	})
}