// 写入路径事务合并 + 写回压
//
// 设计:
//   - 写合并 (Bulk): 多个 doc 写入合并为一次 badger 事务
//   - 写回压 (Backpressure): 信号量限制并发写, 满了返回 429
//
// WriteCoordinator:
//   - 暴露 SubmitBulk: 把一组写操作合并提交, 返回每个 op 的结果
//   - 暴露 Acquire/Release: 用于单写 (POST/PUT doc) 串行化限流
//
// 不做:
//   - 不做磁盘级 IO 限制 (由 badger 自己处理)
//   - 不做客户端连接级 backpressure (由 IP 限速中间件处理)
package server

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// WriteOp 一次写操作
type WriteOp struct {
	Index string
	ID    string
	// Kind: index / create / delete
	Kind  string
	Doc   map[string]interface{}
	// VersionMeta (可选) — 写时同步写 doc-meta sidecar
	VersionMeta *DocMeta
	// 之前存在的 doc (用于 delete) — 倒排扣减标记
}

// WriteOpResult 单个 op 的结果
type WriteOpResult struct {
	Status int                    // 200/201/409
	Meta   *DocMeta               // 写入后的 meta (index/create 才有)
	Error  error                  // 非 nil 表示该 op 失败
	ErrBody map[string]interface{} // 写时构造的 ES 风格错误 (用于 409 等)
}

// WriteConfig 写配置
type WriteConfig struct {
	// MaxConcurrent 最多同时进行中的写事务, 默认 64
	MaxConcurrent int
	// MaxBatchSize 单次合并最大 op 数, 默认 1000
	MaxBatchSize int
}

// WriteCoordinator 写协调器
type WriteCoordinator struct {
	cfg WriteConfig

	// sem 信号量, 控制并发写事务
	sem chan struct{}

	// 监控指标 (atomic)
	stats WriteStats
}

// WriteStats 写统计 (只读)
type WriteStats struct {
	InFlight     int64
	TotalOK      int64
	TotalFailed  int64
	TotalBatches int64
}

// NewWriteCoordinator 构造
func NewWriteCoordinator(cfg WriteConfig) *WriteCoordinator {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 64
	}
	if cfg.MaxBatchSize <= 0 {
		cfg.MaxBatchSize = 1000
	}
	return &WriteCoordinator{
		cfg: cfg,
		sem: make(chan struct{}, cfg.MaxConcurrent),
	}
}

// ErrWriteBusy 回压触发, 客户端应稍后重试
var ErrWriteBusy = errors.New("write_busy: too many concurrent writes, retry later")

// Acquire 拿一个写信号量 (非阻塞)
// 拿到 -> 释放函数; 拿不到 -> ErrWriteBusy
func (c *WriteCoordinator) Acquire() (func(), error) {
	select {
	case c.sem <- struct{}{}:
		atomic.AddInt64(&c.stats.InFlight, 1)
		return func() {
			<-c.sem
			atomic.AddInt64(&c.stats.InFlight, -1)
		}, nil
	default:
		return nil, ErrWriteBusy
	}
}

// Stats 取得当前统计
func (c *WriteCoordinator) Stats() WriteStats {
	return WriteStats{
		InFlight:     atomic.LoadInt64(&c.stats.InFlight),
		TotalOK:      atomic.LoadInt64(&c.stats.TotalOK),
		TotalFailed:  atomic.LoadInt64(&c.stats.TotalFailed),
		TotalBatches: atomic.LoadInt64(&c.stats.TotalBatches),
	}
}

// SubmitBulk 把一组写操作合并到一次 badger 事务
// 返回每个 op 的结果 (按入参顺序)
//   - index/create: 写 doc + doc-meta + inverted in-memory
//   - delete: 删 doc + doc-meta + inverted in-memory
//
// 失败模式: 整批事务原子, 任一 op 失败整个 batch 回滚
// 409 等单 op 逻辑错误在事务内抛 ErrVersionConflict, 单 op 失败但事务可继续
func (c *WriteCoordinator) SubmitBulk(store *storage.Store, engine bulkEngine, ops []WriteOp) []WriteOpResult {
	results := make([]WriteOpResult, len(ops))
	if len(ops) == 0 {
		return results
	}
	if len(ops) > c.cfg.MaxBatchSize {
		// 切分多批
		// 简化: 直接错误返回
		for i := range ops {
			results[i] = WriteOpResult{
				Status: 413,
				Error:  errors.New("batch too large"),
			}
		}
		return results
	}
	release, err := c.Acquire()
	if err != nil {
		for i := range ops {
			results[i] = WriteOpResult{Status: 429, Error: err}
		}
		return results
	}
	defer release()

	atomic.AddInt64(&c.stats.TotalBatches, 1)

	// 一次性事务
	err = store.WithTransaction(func(txn *badger.Txn) error {
		for i, op := range ops {
			switch op.Kind {
			case "index", "create":
				// 检查 op_type=create 冲突
				if op.Kind == "create" {
					var m DocMeta
					_, gerr := engineGet(txn, storage.DocMetaKey(op.Index, op.ID), &m)
					if gerr == nil {
						results[i] = WriteOpResult{
							Status: 409,
							ErrBody: versionConflictError(m.SeqNo, m.PrimaryTerm, 0, 0),
						}
						continue
					}
				}
				// 写 doc
				raw, _ := json.Marshal(op.Doc)
				if perr := txn.Set(storage.DocKey(op.Index, op.ID), raw); perr != nil {
					return perr
				}
				// 写 meta
				if op.VersionMeta != nil {
					mraw, _ := json.Marshal(*op.VersionMeta)
					if perr := txn.Set(storage.DocMetaKey(op.Index, op.ID), mraw); perr != nil {
						return perr
					}
				}
				results[i] = WriteOpResult{Status: 201, Meta: op.VersionMeta}
				atomic.AddInt64(&c.stats.TotalOK, 1)
			case "delete":
				if derr := txn.Delete(storage.DocKey(op.Index, op.ID)); derr != nil {
					return derr
				}
				_ = txn.Delete(storage.DocMetaKey(op.Index, op.ID))
				results[i] = WriteOpResult{Status: 200}
				atomic.AddInt64(&c.stats.TotalOK, 1)
			default:
				results[i] = WriteOpResult{
					Status: 400,
					Error:  errors.New("unknown op kind: " + op.Kind),
				}
			}
		}
		return nil
	})
	if err != nil {
		// 整批失败
		atomic.AddInt64(&c.stats.TotalFailed, int64(len(ops)))
		for i := range results {
			if results[i].Status == 0 {
				results[i] = WriteOpResult{Status: 500, Error: err}
			}
		}
		return results
	}
	// 事务成功后: 更新内存 inverted
	for i, op := range ops {
		if results[i].Status == 201 && (op.Kind == "index" || op.Kind == "create") {
			engine.IndexDoc(op.Index, op.ID, op.Doc)
		} else if results[i].Status == 200 && op.Kind == "delete" {
			engine.DeleteDoc(op.Index, op.ID)
		}
	}
	return results
}

// bulkEngine 抽象 engine 所需方法, 便于 mock 测试
type bulkEngine interface {
	IndexDoc(index, id string, doc map[string]interface{})
	DeleteDoc(index, id string)
}

// engineGet 读 store value 到 out (事务内)
// 解决 import badger: 事务内 Get
func engineGet(txn *badger.Txn, key []byte, out interface{}) (interface{}, error) {
	item, err := txn.Get(key)
	if err != nil {
		return nil, err
	}
	var raw []byte
	raw, err = item.ValueCopy(nil)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, err
	}
	return raw, nil
}

// 防止 import 漂移
var (
	_ = sync.Mutex{}
)
