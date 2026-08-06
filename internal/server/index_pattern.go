// 索引模式匹配
//
// 背景: ES 的 URL 段 {index} 支持以下形式:
//   *                 -> 所有索引
//   idx1,idx2         -> 多个精确名
//   prefix*           -> 前缀通配
//   *suffix           -> 后缀通配
//   prefix*suffix     -> 前后通配
//   _all              -> 同 *
//   -idx1,idx2*       -> 排除(出现在其它模式中时)
//
// 本文件实现 getIndicesByPattern: 把模式字符串展开为具体索引名列表.
// 索引名来源 = storage 中 meta/ 前缀对应的所有索引.
package server

import (
	"sort"
	"strings"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// MaxMatchedIndices 通配展开上限(防 OOM)
const MaxMatchedIndices = 1000

// getIndicesByPattern 展开一个或多个逗号分隔的模式
// 返回的具体索引名按字典序排序
func (s *Server) getIndicesByPattern(pattern string) []string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || pattern == "_all" || pattern == "*" {
		return s.allIndexNames()
	}
	parts := strings.Split(pattern, ",")
	include := map[string]struct{}{}
	exclude := map[string]struct{}{}

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "-") {
			// 排除规则
			name := strings.TrimPrefix(p, "-")
			if name == "" {
				continue
			}
			if strings.ContainsAny(name, "*") {
				// 通配排除: 展开后逐个加入
				matches := s.expandGlob(name)
				for _, idx := range matches {
					exclude[idx] = struct{}{}
				}
			} else {
				exclude[name] = struct{}{}
			}
			continue
		}
		if strings.ContainsAny(p, "*") {
			matches := s.expandGlob(p)
			for _, idx := range matches {
				include[idx] = struct{}{}
			}
		} else {
			include[p] = struct{}{}
		}
	}

	// 排除优先
	for ex := range exclude {
		delete(include, ex)
	}

	out := make([]string, 0, len(include))
	for k := range include {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > MaxMatchedIndices {
		out = out[:MaxMatchedIndices]
	}
	return out
}

// expandGlob 展开一个含 * 的模式
// 只支持 * 出现在开头/结尾/中间一次的简单情形
func (s *Server) expandGlob(pattern string) []string {
	all := s.allIndexNames()
	// 只有一个 * 时, 走前缀/后缀/子串三种快速路径
	if strings.Count(pattern, "*") == 1 {
		idx := strings.IndexByte(pattern, '*')
		prefix := pattern[:idx]
		suffix := pattern[idx+1:]
		out := make([]string, 0)
		for _, name := range all {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			if !strings.HasSuffix(name, suffix) {
				continue
			}
			// 中间至少要有一个字符(* 必须"吞"到至少 0 字符)
			if len(name) < len(prefix)+len(suffix) {
				continue
			}
			out = append(out, name)
		}
		return out
	}
	// 多个 * 的复杂情形: 用正则式 glob 化
	// 这里简化为"全部转 .*"
	re := "^" + strings.ReplaceAll(regexpQuoteMeta(pattern), "\\*", ".*") + "$"
	out := make([]string, 0)
	for _, name := range all {
		if regexpMatchString(re, name) {
			out = append(out, name)
		}
	}
	return out
}

// allIndexNames 从 storage 扫描所有索引名
func (s *Server) allIndexNames() []string {
	all := make([]string, 0, 64)
	_ = s.store.Scan([]byte("meta/"), func(_, v []byte) error {
		var meta IndexMeta
		if err := jsonUnmarshal(v, &meta); err == nil {
			all = append(all, meta.Name)
		}
		return nil
	})
	sort.Strings(all)
	return all
}

// 后续小工具: 正则匹配(自己实现极简版本, 不引 regexp 防止包膨胀)
// 实际使用方是 expandGlob 的多 * 分支, 调用频率低.

// regexpMatchString 检查 name 是否匹配 pattern(.* / 任意字符 .)
func regexpMatchString(pattern, name string) bool {
	// pattern 形如 "^prefix.*mid.*$"
	// 拆分为字面段子串, 必须全部出现
	if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") {
		return false
	}
	body := pattern[1 : len(pattern)-1]
	segs := strings.Split(body, ".*")
	for _, s := range segs {
		if s == "" {
			continue
		}
		if !strings.Contains(name, s) {
			return false
		}
	}
	return true
}

// regexpQuoteMeta 极简版本: 只转义本工具关心的元字符
func regexpQuoteMeta(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '*' || c == '.' || c == '+' || c == '?' || c == '(' || c == ')' || c == '|' || c == '^' || c == '$' || c == '\\' {
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	return string(out)
}

// 抑制 storage 未用警告(本文件实际通过 s.store.Scan 用了, 但加一行保险)
var _ = storage.DocKey
