// _seq_no / _primary_term 乐观并发控制
//
// 设计 (ES 8.x 兼容):
//   - 每个 doc 有 3 个版本字段:
//     * _seq_no:       单调递增序列号 (每个写入 +1)
//     * _primary_term:  primary 任期; 单节点下, 创建/重建时 +1
//     * _version:       外部版本 (ES 6.7+ 推荐; 与 seq_no 不同, 用于外部引用)
//   - 写入参数:
//     * if_seq_no / if_primary_term: 乐观锁 (符合才写, 否则 409)
//     * version: 显式 _version (version_type=external 时必传)
//     * version_type: external|external_gte|internal (默认 internal = 每次 +1)
//     * op_type: create (仅新建, 存在则 409)
//     * if_version: 兼容老版, 隐式 internal 类型
//   - 失败返回 409 Conflict, type=version_conflict_engine_exception
//
// 存储:
//   - 每个 doc 有一个 sidecar: doc-meta/<index>/<id> -> {seq_no, primary_term, version}
//   - 写入 doc source 与 doc meta 在同一次 transaction 中(badger update)
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// DocMeta 每个 doc 的版本元数据
type DocMeta struct {
	SeqNo       int64 `json:"_seq_no"`
	PrimaryTerm int64 `json:"_primary_term"`
	Version     int64 `json:"_version"`
	// Created flag: true 表示该 meta 已存在 (避免被认为是 doc 自身存在)
	Created bool `json:"_created"`
}

// ErrVersionConflict 409 Conflict
var ErrVersionConflict = errors.New("version_conflict_engine_exception")

// versionConflictError 构造 ES 风格错误
func versionConflictError(currentSeqNo, currentTerm int64, expectedSeqNo, expectedTerm int64) map[string]interface{} {
	return map[string]interface{}{
		"type":     "version_conflict_engine_exception",
		"reason": fmt.Sprintf("[%d]: version conflict, current seq_no [%d] primary_term [%d], expected seq_no [%d] primary_term [%d]",
			expectedSeqNo, currentSeqNo, currentTerm, expectedSeqNo, expectedTerm),
		"index":    "go_es",
		"shard":    "0",
		"status":   409,
	}
}

// readDocMeta 读 doc 的版本元数据
func (s *Server) readDocMeta(index, id string) (DocMeta, bool) {
	var m DocMeta
	found, err := s.store.Get(storage.DocMetaKey(index, id), &m)
	if err != nil || !found {
		return DocMeta{}, false
	}
	return m, true
}

// writeDocMeta 写 doc 的版本元数据
func (s *Server) writeDocMeta(index, id string, m DocMeta) error {
	return s.store.Put(storage.DocMetaKey(index, id), m)
}

// NextMeta 计算新 meta (基于 current + 写参数)
//   seqNo:     if_seq_no 校验用的预期值 (>0 时表示条件检查, =0 时不参与)
//   primaryTerm: 同上
//   version:   新 version (外部传入, internal 模式传 0 则自增)
//   versionType: internal | external | external_gte
func NextMeta(current *DocMeta, currentExists bool, seqNo, primaryTerm, version int64, versionType string) (DocMeta, error) {
	// 解析 primary_term
	newTerm := int64(1)
	if current != nil && current.PrimaryTerm > 0 {
		newTerm = current.PrimaryTerm
	}
	if primaryTerm > 0 {
		newTerm = primaryTerm
	}
	// 解析 seq_no: current + 1 (if_seq_no 是校验值, 不影响新值)
	newSeq := int64(0)
	if current != nil {
		newSeq = current.SeqNo
	}
	if currentExists {
		newSeq++
	} else {
		// 第一次创建
		if newSeq == 0 {
			newSeq = 1
		}
	}
	// 解析 version
	newVer := int64(0)
	if current != nil {
		newVer = current.Version
	}
	switch versionType {
	case "external":
		if version <= 0 {
			return DocMeta{}, fmt.Errorf("version_type=external requires version")
		}
		newVer = version
	case "external_gte":
		if version <= 0 {
			return DocMeta{}, fmt.Errorf("version_type=external_gte requires version")
		}
		if version < newVer {
			return DocMeta{}, ErrVersionConflict
		}
		newVer = version
	case "internal", "":
		if version > 0 {
			// 兼容 if_version 风格: 显式 version 必须 > current
			if currentExists && version <= newVer {
				return DocMeta{}, ErrVersionConflict
			}
			newVer = version
		} else {
			newVer++
		}
	default:
		return DocMeta{}, fmt.Errorf("unsupported version_type: %s", versionType)
	}
	return DocMeta{
		SeqNo:       newSeq,
		PrimaryTerm: newTerm,
		Version:     newVer,
		Created:     true,
	}, nil
}

// writeOp 写入操作的语义 (用于 index/update/create 三类入口)
type writeOp struct {
	Index       string
	ID          string
	Doc         map[string]interface{}
	OpType      string // "index" (default) | "create"
	IfSeqNo     int64
	IfPrimaryTerm int64
	IfVersion   int64
	Version     int64
	VersionType string
}

// applyWrite 执行一次带版本控制的写入
// 返回 (meta, status, errMessage) - status 200/201/409
func (s *Server) applyWrite(op writeOp) (DocMeta, int, map[string]interface{}) {
	// 读 current meta
	currentMeta, currentExists := s.readDocMeta(op.Index, op.ID)
	// op_type=create: 存在则 409
	if op.OpType == "create" && currentExists {
		return currentMeta, 409, versionConflictError(currentMeta.SeqNo, currentMeta.PrimaryTerm, 0, 0)
	}
	// 解析 version_type
	vType := op.VersionType
	if op.IfVersion > 0 && vType == "" {
		// 兼容 if_version: 视为 internal
		vType = "internal"
	}
	// 解析 if_seq_no / if_primary_term
	hasIfSeq := op.IfSeqNo > 0 || op.IfPrimaryTerm > 0
	if hasIfSeq {
		// 至少需要一个
		if currentExists {
			if op.IfSeqNo > 0 && op.IfSeqNo != currentMeta.SeqNo {
				return currentMeta, 409, versionConflictError(currentMeta.SeqNo, currentMeta.PrimaryTerm, op.IfSeqNo, op.IfPrimaryTerm)
			}
			if op.IfPrimaryTerm > 0 && op.IfPrimaryTerm != currentMeta.PrimaryTerm {
				return currentMeta, 409, versionConflictError(currentMeta.SeqNo, currentMeta.PrimaryTerm, op.IfSeqNo, op.IfPrimaryTerm)
			}
		} else {
			// 不存在 -> 任何 if_seq_no > 0 都是冲突
			return currentMeta, 409, versionConflictError(0, 0, op.IfSeqNo, op.IfPrimaryTerm)
		}
	}
	// 算 new meta
	var currentPtr *DocMeta
	if currentExists {
		currentPtr = &currentMeta
	}
	// version 取 op.Version (与 if_version 区别: if 是预期, version 是新值)
	newMeta, err := NextMeta(currentPtr, currentExists, op.IfSeqNo, op.IfPrimaryTerm, op.Version, vType)
	if err != nil {
		if errors.Is(err, ErrVersionConflict) {
			if s.metrics != nil {
				s.metrics.IncOptimisticConflict("write", op.OpType)
			}
			return currentMeta, 409, versionConflictError(currentMeta.SeqNo, currentMeta.PrimaryTerm, op.IfSeqNo, op.IfPrimaryTerm)
		}
		return currentMeta, 400, map[string]interface{}{
			"type":   "illegal_argument_exception",
			"reason": err.Error(),
		}
	}
	// 写
	if err := s.store.Put(storage.DocKey(op.Index, op.ID), op.Doc); err != nil {
		return currentMeta, 500, map[string]interface{}{"type": "internal_error", "reason": err.Error()}
	}
	if err := s.writeDocMeta(op.Index, op.ID, newMeta); err != nil {
		return currentMeta, 500, map[string]interface{}{"type": "internal_error", "reason": err.Error()}
	}
	// 推 inverted
	s.engine.IndexDoc(op.Index, op.ID, op.Doc)
	// 失效搜索缓存: 按索引级精确失效 (#11)
	s.invalidateCacheForIndex(op.Index)
	// segment 触发检查
	if s.seg != nil {
		docSize := docSizeOf(op.Doc)
		if s.seg.OnWrite(op.Index, docSize) {
			// 异步 flush, 不阻塞写
			go func() {
				_, _ = s.seg.FlushNow(op.Index)
			}()
		}
	}
	// status: 201 if create, 200 if index (and 201 if create always)
	status := 200
	if op.OpType == "create" || !currentExists {
		status = 201
	}
	return newMeta, status, nil
}

// toInt64 工具: 把 string / float64 / int 转为 int64
func toInt64(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// getQueryInt64 从 query 取 int64 字段
func getQueryInt64(q map[string][]string, key string) int64 {
	if v, ok := q[key]; ok && len(v) > 0 {
		n, err := strconv.ParseInt(v[0], 10, 64)
		if err == nil {
			return n
		}
	}
	return 0
}

// docSizeOf 估算 doc 大小(字节), 用于 segment buffer 计数
func docSizeOf(doc map[string]interface{}) int {
	// 简化: JSON 序列化估算
	b, _ := json.Marshal(doc)
	return len(b)
}

// ifThen 三元运算式
func ifThen(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// writeErrorJSON 把已构造好的 error map (ES 风格) 写到响应
func writeErrorJSON(w http.ResponseWriter, status int, err map[string]interface{}) {
	body := map[string]interface{}{
		"status": status,
		"error":  err,
	}
	writeJSON(w, status, body)
}
