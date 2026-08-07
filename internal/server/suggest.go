// suggest API:
//   - POST /<index>/_suggest   (单独 suggest 端点)
//   - POST /<index>/_search   (search 请求体中带 "suggest" 字段, 走 doSearch 内部处理)
//
// 请求/响应体格式兼容 ES 8.x:
//   request:  { "mysuggester": { "text": "foo", "term": { "field": "title" } } }
//             { "mysuggester": { "text": "foo", "completion": { "field": "title" } } }
//             { "mysuggester": { "text": "foo", "prefix": { "field": "title" } } }
//   response: [{ "text": "foo", "offset": 0, "length": 3, "options": [...] }, ...]
package server

import (
	"fmt"
	"net/http"

	"github.com/zixiliuyue/go_es/internal/search"
)

// suggestRequest 顶层是 suggest 名 -> SuggestRequest 的 map
type suggestRequest map[string]search.SuggestRequest

// suggestResponse 顶层是 suggest 名 -> []SuggestResult 的 map
// ES 格式是数组, 我们用 map (key=名称), 方便对应请求
type suggestResponse map[string][]search.SuggestResult

// handleSuggest POST /<index>/_suggest 或 POST /_suggest
func (s *Server) handleSuggest(w http.ResponseWriter, r *http.Request, index string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}
	var req suggestRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
			return
		}
	}
	if len(req) == 0 {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "at least one suggestion required", "")
		return
	}
	// 通配展开: index 可能是 "a,b,-c*" 形式
	indices := s.getIndicesByPattern(index)
	if len(indices) == 0 {
		writeError(w, http.StatusNotFound, "index_not_found_exception", fmt.Sprintf("no index matches %q", index), "")
		return
	}
	resp := make(suggestResponse, len(req))
	for name, sr := range req {
		name := name
		sr := sr
		results := make([]search.SuggestResult, 0)
		for _, idx := range indices {
			r := s.runSuggestForIndex(idx, &sr)
			results = append(results, r...)
		}
		resp[name] = results
	}
	// ES 用数组, 但我们保持 map 与请求对应
	// 加一个 wrapper 与 ES 兼容: {"<name>": [...], ...} 直接是顶层
	writeJSON(w, http.StatusOK, resp)
}

// runSuggestForIndex 在单个索引上跑一次 suggest
func (s *Server) runSuggestForIndex(index string, sr *search.SuggestRequest) []search.SuggestResult {
	if sr.Text == "" {
		return nil
	}
	if sr.Term != nil {
		return runTermSuggest(s.engine, index, sr)
	}
	if sr.Completion != nil {
		return runCompletionSuggest(s.engine, index, sr)
	}
	if sr.Prefix != nil {
		return runPrefixSuggest(s.engine, index, sr)
	}
	return nil
}

// runTermSuggest 执行 term suggester
func runTermSuggest(e *search.Engine, index string, sr *search.SuggestRequest) []search.SuggestResult {
	out := []search.SuggestResult{{Text: sr.Text, Offset: 0, Length: len(sr.Text), Options: nil}}
	opts := e.TermSuggest(index, sr.Term.Field, sr.Text, *sr.Term, sr.MaxEdits, sr.PrefixLen, sr.MinWordLen, sr.Size)
	out[0].Options = opts
	return out
}

// runCompletionSuggest 执行 completion suggester
func runCompletionSuggest(e *search.Engine, index string, sr *search.SuggestRequest) []search.SuggestResult {
	out := []search.SuggestResult{{Text: sr.Text, Offset: 0, Length: len(sr.Text), Options: nil}}
	opts := e.CompletionSuggest(index, sr.Completion.Field, sr.Text, *sr.Completion)
	out[0].Options = opts
	return out
}

// runPrefixSuggest 执行 prefix suggester
func runPrefixSuggest(e *search.Engine, index string, sr *search.SuggestRequest) []search.SuggestResult {
	out := []search.SuggestResult{{Text: sr.Text, Offset: 0, Length: len(sr.Text), Options: nil}}
	opts := e.PrefixSuggest(index, sr.Prefix.Field, sr.Text, sr.Size)
	out[0].Options = opts
	return out
}

// attachSuggestToResponse 在 search response 中附加 suggest 字段
// 在 doSearch 计算完 hits / aggregations 之后, 给 resp 加 suggest map
func (s *Server) attachSuggestToResponse(resp map[string]interface{}, index string, req map[string]interface{}) {
	if len(req) == 0 {
		return
	}
	// 解析顶层: name -> suggestSpec
	suggestMap := make(suggestResponse, len(req))
	for name, raw := range req {
		sr, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		// 转 search.SuggestRequest
		req, err := parseSuggestRequest(sr)
		if err != nil {
			suggestMap[name] = []search.SuggestResult{{Text: "", Options: nil}}
			continue
		}
		// 跨索引
		indices := s.getIndicesByPattern(index)
		results := make([]search.SuggestResult, 0)
		for _, idx := range indices {
			results = append(results, s.runSuggestForIndex(idx, req)...)
		}
		suggestMap[name] = results
	}
	resp["suggest"] = suggestMap
}

// attachSuggestToResponseHelper 跨索引合并 version
// 把单个 index 的结果合并到 merged (按 name 累积 options)
func (s *Server) attachSuggestToResponseHelper(index string, req map[string]interface{}, merged map[string][]search.SuggestResult) {
	for name, raw := range req {
		sr, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		parsed, err := parseSuggestRequest(sr)
		if err != nil {
			continue
		}
		results := s.runSuggestForIndex(index, parsed)
		// 合并 options, 去重(同 text)
		existing, ok := merged[name]
		if !ok {
			merged[name] = results
			continue
		}
		seen := make(map[string]bool)
		out := existing
		// 记录已有 options 的 text
		for _, r := range existing {
			for _, o := range r.Options {
				seen[o.Text] = true
			}
		}
		for _, r := range results {
			for _, o := range r.Options {
				if seen[o.Text] {
					continue
				}
				seen[o.Text] = true
				if len(out) > 0 {
					out[0].Options = append(out[0].Options, o)
				} else {
					out = append(out, search.SuggestResult{Text: parsed.Text, Options: []search.SuggestOption{o}})
				}
			}
		}
		merged[name] = out
	}
}

// parseSuggestRequest 把 search 体内的 suggest spec 转为 *search.SuggestRequest
func parseSuggestRequest(raw map[string]interface{}) (*search.SuggestRequest, error) {
	sr := &search.SuggestRequest{}
	if t, ok := raw["text"].(string); ok {
		sr.Text = t
	}
	if term, ok := raw["term"].(map[string]interface{}); ok {
		cfg := search.TermSuggesterConfig{}
		if f, ok := term["field"].(string); ok {
			cfg.Field = f
		}
		if a, ok := term["analyzer"].(string); ok {
			cfg.Analyzer = a
		}
		if s, ok := term["sort"].(string); ok {
			cfg.Sort = s
		}
		if m, ok := term["suggest_mode"].(string); ok {
			cfg.SuggestMode = m
		}
		sr.Term = &cfg
	}
	if comp, ok := raw["completion"].(map[string]interface{}); ok {
		cfg := search.CompletionSuggesterConfig{}
		if f, ok := comp["field"].(string); ok {
			cfg.Field = f
		}
		if sz, ok := comp["size"]; ok {
			if n, ok := toIntAny(sz); ok {
				cfg.Size = n
			}
		}
		if sd, ok := comp["skip_duplicates"].(bool); ok {
			cfg.SkipDuplicates = sd
		}
		sr.Completion = &cfg
	}
	if pf, ok := raw["prefix"].(map[string]interface{}); ok {
		cfg := search.PrefixSuggesterConfig{}
		if f, ok := pf["field"].(string); ok {
			cfg.Field = f
		}
		if sz, ok := pf["size"]; ok {
			if n, ok := toIntAny(sz); ok {
				cfg.Size = n
			}
		}
		sr.Prefix = &cfg
	}
	if sr.Text == "" {
		return sr, nil
	}
	if sr.Term == nil && sr.Completion == nil && sr.Prefix == nil {
		return nil, fmt.Errorf("suggest requires term/completion/prefix")
	}
	return sr, nil
}

// toIntAny 把 json.Number / int / float64 / string 转 int
func toIntAny(v interface{}) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case string:
		// 简单处理
		var n int
		_, err := fmt.Sscanf(x, "%d", &n)
		return n, err == nil
	}
	return 0, false
}
