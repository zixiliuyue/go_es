// 服务端配置文件 schema 校验
//
// 设计目标:
//   - YAML 解析成功(=语法 OK)后, 立即用本模块校验"语义"是否符合预期 schema
//   - 校验失败返回与 parse 错误同级: ConfigLoader.Load() wrap 成同一 fmt.Errorf,
//     cmd/server/main.go 的 log.Fatalf 一视同仁, 启动立即退出(非 0)
//   - 错误信息自带层级路径 + 期望规则 + 实际值, 便于用户定位
//   - 规则用纯结构体声明, 增删规则不需要改 Validate 核心代码
//
// 与 yaml 库的关系:
//   - yaml.v3 已完成"语法 -> Go struct"映射(类型推断 + unmarshal),
//     本模块不再做 raw YAML 检查, 只检查 unmarshal 之后的 ConfigFile 值
//   - 若字段类型与 struct 不符(unmarshal 时会尽力转换或保留原始值),
//     本模块再二次校验, 抓到"形似但类型错"的脏数据
//
// 规则集:
//   - Kind: required / type / range / enum / pattern / minLen / crossField
//   - 每条 FieldRule 可声明多个约束, 全部满足才算通过
//   - 错误聚合: 所有规则全部跑完, 一次性返回所有违例(便于一次性修多个)
package server

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SchemaKind 校验规则类型
type SchemaKind string

const (
	KindRequired   SchemaKind = "required"   // 字段必须非空(字符串非 "" / 切片/map 非 nil)
	KindType       SchemaKind = "type"       // 期望类型, 详见 SchemaType
	KindRange      SchemaKind = "range"      // 数值范围: Min <= v <= Max(可省略一端)
	KindEnum       SchemaKind = "enum"       // 字符串枚举, 必须在 Values 内
	KindPattern    SchemaKind = "pattern"    // 正则匹配
	KindFormat     SchemaKind = "format"     // 命名格式, 如 "listen-addr", "duration", "log-level"
	KindMinLen     SchemaKind = "min_len"    // 字符串/数组最小长度
	KindMaxLen     SchemaKind = "max_len"    // 字符串/数组最大长度
)

// SchemaType 期望的 Go 层类型(对原始 ConfigFile 字段做类型约束)
type SchemaType string

const (
	TypeString SchemaType = "string"
	TypeInt    SchemaType = "int"    // int / int64 通用
	TypeFloat  SchemaType = "float"  // float64
	TypeBool   SchemaType = "bool"
	TypeArray  SchemaType = "array"  // []string / []X
	TypeMap    SchemaType = "map"    // map[K]V
	TypeDur    SchemaType = "dur"    // time.Duration
)

// FieldRule 一条 schema 规则.
// 多个字段并存时 AND 关系(必须全部通过).
type FieldRule struct {
	// Path 点分路径, 如 "auth.basic", "limit.max_body_bytes", "tls.cert"
	Path string
	// Kind 规则类型
	Kind SchemaKind
	// 必填时, 用于 required / type / range / enum / pattern
	Type   SchemaType // Kind=type
	// Min / Max 用指针, 区分 "未设置" 与 "=0", 允许规则限定包含 0
	Min *float64
	Max *float64
	Values []string   // Kind=enum
	// Pattern 必须编译通过的 *regexp.Regexp, Kind=pattern
	Pattern *regexp.Regexp
	// Format 命名格式, Kind=format
	Format string
	// LenKind=min_len / max_len 时, 对字符串/数组的最小/最大长度
	MinLen int
	MaxLen int
	// Message 自定义错误信息(可选). 为空时根据 Kind 自动生成.
	Message string
	// When 条件表达式, 用于 crossField 规则: 当 When 为 true 时才应用本规则
	// 当前简化: 用 dot-path 形式 "field.path=literal" 或 "field.path!=literal"
	// 例如: `auth.enabled=true` 表示 auth.enabled==true 时本规则生效
	When string
}

// ConfigSchema 校验规则集合
type ConfigSchema struct {
	// Rules 所有字段规则. 校验时按 Path 排序后逐条应用, 错误聚合返回
	Rules []FieldRule
}

// ValidationError 一条校验失败
type ValidationError struct {
	Path   string // 配置项路径, 如 "limit.max_body_bytes"
	Reason string // 人类可读的原因
	Value  any    // 实际值(用于错误信息)
	Rule   string // 违反的规则摘要
}

// Error 实现 error 接口
func (e *ValidationError) Error() string {
	v := formatValue(e.Value)
	if v == "" {
		return fmt.Sprintf("config: %s %s", e.Path, e.Reason)
	}
	return fmt.Sprintf("config: %s %s (实际值=%s)", e.Path, e.Reason, v)
}

// ValidationErrors 错误聚合, 内部按 Path 排序保证输出稳定
type ValidationErrors []ValidationError

func (es ValidationErrors) Error() string {
	if len(es) == 0 {
		return ""
	}
	sort.SliceStable(es, func(i, j int) bool { return es[i].Path < es[j].Path })
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return fmt.Sprintf("%d schema violation(s):\n  - %s", len(es), strings.Join(parts, "\n  - "))
}

func (es ValidationErrors) Is(target error) bool {
	_, ok := target.(ValidationErrors)
	return ok
}

// formatValue 调试/错误用, 简洁呈现实际值
func formatValue(v any) string {
	if v == nil {
		return "<nil>"
	}
	switch x := v.(type) {
	case string:
		return strconv.Quote(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case time.Duration:
		return x.String()
	case []string:
		if len(x) == 0 {
			return "[]"
		}
		return fmt.Sprintf("%v", x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// DefaultConfigSchema 默认规则集, 覆盖 ConfigFile 全部字段.
// 任何新加字段都必须在此补规则, 单元测试会强制覆盖.
func DefaultConfigSchema() *ConfigSchema {
	addrPattern := regexp.MustCompile(`^:\d+$`)
	return &ConfigSchema{
		Rules: []FieldRule{
			// ---------- addr ----------
			{
				Path: "addr", Kind: KindType, Type: TypeString,
				Message: "addr 必须是非空字符串, 形如 :9200",
			},
			{
				Path: "addr", Kind: KindFormat, Format: "listen-addr",
				Pattern: addrPattern,
				Message: "addr 必须是 :port 形式(冒号+端口号)",
			},

			// ---------- data ----------
			{
				Path: "data", Kind: KindType, Type: TypeString,
				Message: "data 必须是字符串(空字符串=内存模式, 启动期会校验)",
			},

			// ---------- auth.enabled ----------
			{
				Path: "auth.enabled", Kind: KindType, Type: TypeBool,
				Message: "auth.enabled 必须是布尔值(true/false)",
			},

			// ---------- auth.basic ----------
			{
				Path: "auth.basic", Kind: KindType, Type: TypeMap,
				Message: "auth.basic 必须是 map (key=用户名, value=密码字符串)",
			},
			{
				Path: "auth.basic", Kind: KindMaxLen, MaxLen: 1024,
				Message: "auth.basic 用户名数量不能超过 1024",
			},

			// ---------- auth.api_keys ----------
			{
				Path: "auth.api_keys", Kind: KindType, Type: TypeArray,
				Message: "auth.api_keys 必须是字符串数组",
			},
			{
				Path: "auth.api_keys", Kind: KindMaxLen, MaxLen: 4096,
				Message: "auth.api_keys 长度不能超过 4096",
			},

			// ---------- auth 业务规则: enabled=true 时必须有凭据 ----------
			{
				Path: "auth.enabled", Kind: KindCrossField,
				Message: "auth.enabled=true 时, auth.basic 或 auth.api_keys 至少需要 1 个凭据(否则开认证没人能进)",
				When:   "auth.enabled=true",
			},

			// ---------- limit.max_body_bytes ----------
			{
				Path: "limit.max_body_bytes", Kind: KindType, Type: TypeInt,
				Message: "limit.max_body_bytes 必须是整数",
			},
			{
				Path: "limit.max_body_bytes", Kind: KindRange, Min: ptrF(0), Max: ptrF(1 << 30), // 0 ~ 1 GiB
				Message: "limit.max_body_bytes 必须在 0 ~ 1073741824 (1 GiB) 之间",
			},

			// ---------- limit.rate_per_second ----------
			{
				Path: "limit.rate_per_second", Kind: KindType, Type: TypeFloat,
				Message: "limit.rate_per_second 必须是数字",
			},
			{
				Path: "limit.rate_per_second", Kind: KindRange, Min: ptrF(0), Max: ptrF(1e7),
				Message: "limit.rate_per_second 必须在 0 ~ 10000000 之间",
			},

			// ---------- limit.burst ----------
			{
				Path: "limit.burst", Kind: KindType, Type: TypeInt,
				Message: "limit.burst 必须是整数",
			},
			{
				Path: "limit.burst", Kind: KindRange, Min: ptrF(0), Max: ptrF(1e6),
				Message: "limit.burst 必须在 0 ~ 1000000 之间",
			},

			// ---------- tls.cert / tls.key (单边不允许) ----------
		{
			Path: "tls.cert", Kind: KindType, Type: TypeString,
			Message: "tls.cert 必须是字符串",
		},
		{
			Path: "tls.key", Kind: KindType, Type: TypeString,
			Message: "tls.key 必须是字符串",
		},
		// TLS 业务规则: cert/key 必须同时配置
		{
			Path: "tls.cert", Kind: KindCrossField,
			Message: "tls.cert 与 tls.key 必须同时配置(只配一个无法启动)",
			When:   "tls.one-sided=true",
		},
		{
			Path: "tls.enable_http2", Kind: KindType, Type: TypeBool,
			Message: "tls.enable_http2 必须是布尔值",
		},

		// ---------- mTLS: tls.client_ca / tls.client_auth ----------
		{
			Path: "tls.client_ca", Kind: KindType, Type: TypeString,
			Message: "tls.client_ca 必须是字符串(PEM CA 池路径, 留空 = 不启用 mTLS)",
		},
		{
			Path: "tls.client_auth", Kind: KindEnum, Values: []string{"none", "request", "require_any", "require_verify"},
			Message: "tls.client_auth 必须是 none/request/require_any/require_verify 之一",
		},
		// mTLS 业务规则: client_ca 与 client_auth 必须配对(单边不允许)
		//   - 设了 client_ca 但 client_auth = none: 应报错(client_ca 无意义)
		//   - 设了 client_auth != none 但 client_ca 为空: 应报错(无法验证)
		{
			Path: "tls.client_ca", Kind: KindCrossField,
			Message: "tls.client_ca 与 tls.client_auth 必须配对: 配 client_ca 时 client_auth 不能是 none, 设 client_auth 非 none 时也必须配 client_ca",
			When:   "tls.mtls-one-sided=true",
		},

			// ---------- log_level ----------
			{
				Path: "log_level", Kind: KindType, Type: TypeString,
				Message: "log_level 必须是字符串",
			},
			{
				Path: "log_level", Kind: KindEnum, Values: []string{"debug", "info", "warn", "error"},
				Message: "log_level 必须是 debug/info/warn/error 之一",
			},

			// ---------- watch_interval ----------
			{
				Path: "watch_interval", Kind: KindType, Type: TypeDur,
				Message: "watch_interval 必须是 time.Duration 字符串(如 5s / 100ms)",
			},
			// duration 范围 0 ~ 1h(防止用户写 1ns 频繁轮询或 1h 看不到变更)
			{
				Path: "watch_interval", Kind: KindRange, Min: ptrF(0), Max: ptrF(float64(time.Hour)),
				Message: "watch_interval 应在 0 ~ 1h 之间",
			},

			// ---------- tracing.enabled ----------
			{
				Path: "tracing.enabled", Kind: KindType, Type: TypeBool,
				Message: "tracing.enabled 必须是布尔值(true/false)",
			},
			// ---------- tracing.service_name ----------
			{
				Path: "tracing.service_name", Kind: KindType, Type: TypeString,
				Message: "tracing.service_name 必须是字符串(服务名)",
			},
			// ---------- tracing.service_version ----------
			{
				Path: "tracing.service_version", Kind: KindType, Type: TypeString,
				Message: "tracing.service_version 必须是字符串(版本号)",
			},
			// ---------- tracing.propagation ----------
			{
				Path: "tracing.propagation", Kind: KindEnum, Values: []string{"tracecontext", "b3", "both"},
				Message: "tracing.propagation 必须是 tracecontext/b3/both 之一",
			},
			// ---------- tracing.sampling_rate ----------
			{
				Path: "tracing.sampling_rate", Kind: KindType, Type: TypeFloat,
				Message: "tracing.sampling_rate 必须是浮点数(0.0 ~ 1.0)",
			},
			{
				Path: "tracing.sampling_rate", Kind: KindRange, Min: ptrF(0), Max: ptrF(1.0),
				Message: "tracing.sampling_rate 应在 0.0 ~ 1.0 之间",
			},
		},
	}
}

// KindCrossField 跨字段业务规则
const KindCrossField SchemaKind = "cross_field"

// Validate 对 ConfigFile 跑一遍规则, 返回聚合错误(可能为空切片表示 OK)
func (s *ConfigSchema) Validate(cfg *ConfigFile) ValidationErrors {
	if cfg == nil {
		return ValidationErrors{{Path: "<root>", Reason: "config is nil"}}
	}
	var errs ValidationErrors
	for _, r := range s.Rules {
		if !shouldApply(r, cfg) {
			continue
		}
		if e := applyRule(r, cfg); e != nil {
			errs = append(errs, *e)
		}
	}
	return errs
}

// shouldApply 检查 When 条件(简化版: 解析 "field.path=literal" / "field.path!=literal")
func shouldApply(r FieldRule, cfg *ConfigFile) bool {
	if r.When == "" {
		return true
	}
	// 形如 "auth.enabled=true"
	parts := strings.SplitN(r.When, "=", 2)
	if len(parts) != 2 {
		return true
	}
	path, expected := parts[0], parts[1]
	actual := lookupField(path, cfg)
	expectedBool, _ := strconv.ParseBool(expected)
	switch v := actual.(type) {
	case bool:
		// When 用 'true'/'false' 字面量
		if expectedBool {
			return v
		}
		return !v
	default:
		_ = v
	}
	// 字符串匹配(去除引号)
	expected = strings.Trim(expected, `"`)
	if s, ok := actual.(string); ok {
		return s == expected
	}
	return true
}

// applyRule 对一条规则做一次校验, 失败返回 *ValidationError, 通过返回 nil
// 统一约定: 零值(nil / "" / 0 / false)跳过非 required 规则, 由 required 单独抓
// 业务规则(cross_field)无论零值与否都跑
func applyRule(r FieldRule, cfg *ConfigFile) *ValidationError {
	val := lookupField(r.Path, cfg)
	if r.Kind != KindRequired && r.Kind != KindCrossField && isZero(val) {
		return nil
	}
	switch r.Kind {
	case KindRequired:
		if isZero(val) {
			return &ValidationError{Path: r.Path, Reason: r.Message, Value: val, Rule: "required"}
		}
	case KindType:
		if !matchType(val, r.Type) {
			return &ValidationError{Path: r.Path, Reason: r.Message, Value: val, Rule: "type=" + string(r.Type)}
		}
	case KindRange:
		f, ok := toFloat(val)
		if !ok {
			return &ValidationError{Path: r.Path, Reason: r.Message + "(值不是数字)", Value: val, Rule: "range"}
		}
		// Min/Max 用 *float64 区分 "未设置" 与 "0"
		if r.Min != nil && f < *r.Min {
			return &ValidationError{Path: r.Path, Reason: r.Message, Value: val, Rule: fmt.Sprintf("range[min=%v]", *r.Min)}
		}
		if r.Max != nil && f > *r.Max {
			return &ValidationError{Path: r.Path, Reason: r.Message, Value: val, Rule: fmt.Sprintf("range[max=%v]", *r.Max)}
		}
	case KindEnum:
		// 直接 string 或 *string(指针形式, 允许 yaml 显式给 "none" 作为合法值)
		var s string
		var ok bool
		switch v := val.(type) {
		case string:
			s, ok = v, true
		case *string:
			if v != nil {
				s, ok = *v, true
			}
		}
		if !ok {
			return &ValidationError{Path: r.Path, Reason: r.Message + "(值不是字符串)", Value: val, Rule: "enum"}
		}
		if !sliceContains(r.Values, s) {
			return &ValidationError{Path: r.Path, Reason: r.Message, Value: val, Rule: "enum"}
		}
	case KindPattern, KindFormat:
		s, ok := val.(string)
		if !ok {
			return &ValidationError{Path: r.Path, Reason: r.Message + "(值不是字符串)", Value: val, Rule: string(r.Kind)}
		}
		if s == "" {
			// 空字符串不参与 pattern 校验(required 会单独抓)
			return nil
		}
		if r.Pattern != nil && !r.Pattern.MatchString(s) {
			return &ValidationError{Path: r.Path, Reason: r.Message, Value: val, Rule: "pattern"}
		}
	case KindMinLen:
		if lengthOf(val) < r.MinLen {
			return &ValidationError{Path: r.Path, Reason: r.Message, Value: val, Rule: fmt.Sprintf("min_len=%d", r.MinLen)}
		}
	case KindMaxLen:
		if lengthOf(val) > r.MaxLen {
			return &ValidationError{Path: r.Path, Reason: r.Message, Value: val, Rule: fmt.Sprintf("max_len=%d", r.MaxLen)}
		}
	case KindCrossField:
		// 业务规则: 在 checkCrossField 里实现
		if msg := checkCrossField(r, cfg); msg != "" {
			return &ValidationError{Path: r.Path, Reason: msg, Value: nil, Rule: "cross_field"}
		}
	}
	return nil
}

// checkCrossField 业务规则聚合.
// 返回非空字符串 = 违反, 空字符串 = 满足.
func checkCrossField(r FieldRule, cfg *ConfigFile) string {
	switch r.When {
	case "auth.enabled=true":
		if !cfg.Auth.Enabled {
			return ""
		}
		if len(cfg.Auth.Basic) == 0 && len(cfg.Auth.APIKeys) == 0 {
			return r.Message
		}
	case "tls.one-sided=true":
		// 单边: cert 或 key 其中一个非空, 另一个为空 -> 违反
		cert := cfg.TLS.CertFile
		key := cfg.TLS.KeyFile
		if (cert != "" && key == "") || (cert == "" && key != "") {
			return r.Message
		}
	case "tls.mtls-one-sided=true":
		// mTLS 单边: client_ca 与 client_auth 非 none 必须成对出现
		ca := cfg.TLS.ClientCAFile
		auth := cfg.TLS.AuthKind()
		hasCA := ca != ""
		hasAuth := auth != ClientAuthNone
		if hasCA != hasAuth {
			return r.Message
		}
	}
	return ""
}

// lookupField 路径查值. 支持点分路径深入嵌套 struct 字段, 不支持索引(list[i]).
// 时间类型特殊处理: 内部 watch_interval 是 time.Duration, 但用户填的是字符串(未反序列化),
// 所以此处只在值已就绪时返回; 否则返回 nil 由 type 规则报"空字符串或 nil"
func lookupField(path string, cfg *ConfigFile) any {
	switch path {
	case "addr":
		return cfg.Addr
	case "data":
		return cfg.Data
	case "auth.enabled":
		return cfg.Auth.Enabled
	case "auth.basic":
		return cfg.Auth.Basic
	case "auth.api_keys":
		return cfg.Auth.APIKeys
	case "limit.max_body_bytes":
		return cfg.Limit.MaxBodyBytes
	case "limit.rate_per_second":
		return cfg.Limit.RatePerSecond
	case "limit.burst":
		return cfg.Limit.Burst
	case "tls.cert":
		return cfg.TLS.CertFile
	case "tls.key":
		return cfg.TLS.KeyFile
	case "tls.enable_http2":
		// 故意返回 *bool 指针, 区分 "未设置(nil)" 与 "显式 false"
		// isZero 把 *bool nil 视为零, 非 nil 视为非零
		return cfg.TLS.EnableHTTP2
	case "tls.client_ca":
		return cfg.TLS.ClientCAFile
	case "tls.client_auth":
		// ClientAuth 是 *string, 显式 none 是合法值
		return cfg.TLS.ClientAuth
	case "tracing.enabled":
		return cfg.Tracing.Enabled
	case "tracing.service_name":
		return cfg.Tracing.ServiceName
	case "tracing.service_version":
		return cfg.Tracing.ServiceVer
	case "tracing.propagation":
		return cfg.Tracing.Propagation
	case "tracing.sampling_rate":
		return cfg.Tracing.SamplingRate
	case "log_level":
		return cfg.Log
	case "watch_interval":
		// watch_interval 是 time.Duration, 不暴露原始字符串, 走专属 type 规则
		return cfg.Watch
	}
	return nil
}

func isZero(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case string:
		return x == ""
	case int:
		return x == 0
	case int64:
		return x == 0
	case float64:
		return x == 0
	case bool:
		// bool false 是一个合法值, 不算 zero(zero 留给 type 规则的"未设置"语义)
		// 但 *bool 的 zero 是 nil, 见下
		return false
	case *bool:
		return x == nil
	case *string:
		return x == nil
	case []string:
		return len(x) == 0
	case map[string]string:
		return len(x) == 0
	case time.Duration:
		return x == 0
	default:
		return false
	}
}

func matchType(v any, t SchemaType) bool {
	switch t {
	case TypeString:
		_, ok := v.(string)
		return ok
	case TypeInt:
		switch v.(type) {
		case int, int64:
			return true
		}
		return false
	case TypeFloat:
		_, ok := v.(float64)
		return ok
	case TypeBool:
		switch v.(type) {
		case bool, *bool:
			return true
		}
		return false
	case TypeArray:
		switch v.(type) {
		case []string, []any:
			return true
		}
		return false
	case TypeMap:
		switch v.(type) {
		case map[string]string, map[string]any:
			return true
		}
		return false
	case TypeDur:
		_, ok := v.(time.Duration)
		return ok
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case time.Duration:
		// time.Duration 底层是 int64, Go 类型系统视作不同类型, 显式转换
		return float64(x), true
	}
	return 0, false
}

// sliceContains 在字符串切片中查找给定元素(仅 schema 内部使用)
func sliceContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func lengthOf(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []string:
		return len(x)
	case map[string]string:
		return len(x)
	}
	return 0
}

// SanityCheck 提供给 main 启动期的"地址可解析"附加校验(非 schema 范畴,
// 但与 addr 强相关). 启动期监听失败由 net.Listen 报, 这里只校验"端口号可解析".
func SanityCheck(cfg *ConfigFile) error {
	if cfg.Addr == "" {
		return nil // 留空 = 用默认 :9200, 在 cmd/server/main.go 里 fallback
	}
	_, portStr, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return fmt.Errorf("config: addr %q 无法解析为 host:port: %w", cfg.Addr, err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 0 || p > 65535 {
		return fmt.Errorf("config: addr %q 端口号 %q 非法(0-65535)", cfg.Addr, portStr)
	}
	return nil
}

// ptrF 便捷构造 *float64(用于 FieldRule.Min/Max)
func ptrF(v float64) *float64 { return &v }
