// writeBatch 封装 badger.WriteBatch, 提供给 bulk handler 使用
//
// 背景: 当前 bulk handler 每条 op 走一次 RW 事务, fsync 频繁, 吞吐受限.
// 优化: 在 bulk handler 中先解析所有 op, 然后一次性用 WriteBatch 写入.
// engine 内存索引仍逐条更新(轻量, 无 IO).
//
// 限制: WriteBatch 是异步的, 调用 wb.Flush() 才真正落盘.
// 我们的语义: handler 返回前必须 Flush, 保证客户端拿到响应时数据已持久化.
package server

import (
	badger "github.com/dgraph-io/badger/v4"
)

// pendingBulkOp 累积的待写入 op
type pendingBulkOp struct {
	index  string
	id     string
	action string // index / create / delete
	doc    []byte // raw json bytes
}

// bulkWriter 累积 op, 一次性 WriteBatch
type bulkWriter struct {
	ops []*pendingBulkOp
}

// newBulkWriter 构造
func newBulkWriter(capHint int) *bulkWriter {
	return &bulkWriter{ops: make([]*pendingBulkOp, 0, capHint)}
}

// addIndex 添加一个 index/create op
func (b *bulkWriter) addIndex(index, id string, raw []byte) {
	b.ops = append(b.ops, &pendingBulkOp{index: index, id: id, action: "index", doc: raw})
}

// addDelete 添加一个 delete op
func (b *bulkWriter) addDelete(index, id string) {
	b.ops = append(b.ops, &pendingBulkOp{index: index, id: id, action: "delete"})
}

// flush 把累积的 op 一次性写入 badger WB
// 写完后更新 engine 内存索引(本进程内可见)
func (b *bulkWriter) flush(s *Server) error {
	if len(b.ops) == 0 {
		return nil
	}
	wb := s.store.DB().NewWriteBatch()
	defer wb.Cancel()
	for _, op := range b.ops {
		switch op.action {
		case "index":
			if err := wb.Set(DocKeyBytes(op.index, op.id), op.doc); err != nil {
				return err
			}
		case "delete":
			if err := wb.Delete(DocKeyBytes(op.index, op.id)); err != nil {
				return err
			}
		}
	}
	if err := wb.Flush(); err != nil {
		return err
	}
	// 同步更新 engine
	for _, op := range b.ops {
		switch op.action {
		case "index":
			var doc map[string]interface{}
			if err := jsonUnmarshal(op.doc, &doc); err == nil {
				s.engine.IndexDoc(op.index, op.id, doc)
			}
		case "delete":
			s.engine.DeleteDoc(op.index, op.id)
		}
	}
	return nil
}

// DocKeyBytes 等价 storage.DocKey 但放这里避免 import 循环
func DocKeyBytes(index, id string) []byte {
	return []byte("doc/" + index + "/" + id)
}

// 抑制 badger 未用警告
var _ = badger.DefaultOptions
