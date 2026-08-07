// Package storage 提供基于 BadgerDB 的持久化能力,作为本仓库
// 自研 Elasticsearch 服务端(见 internal/server) 的存储后端
//
// 存储模型
//
//	doc/<index>/<id>          -> JSON _source
//	meta/<index>              -> 索引元信息(mapping/settings/aliases-元数据)
//	alias/<alias>             -> 别名指向的索引列表(JSON 数组)
//	ilm/<policy>              -> ILM 策略 JSON
//	tpl/index/<name>          -> Index Template JSON
//	tpl/component/<name>      -> Component Template JSON
//	ingest/<name>             -> Ingest Pipeline JSON
//	cluster                   -> 集群元信息(cluster_name, uuid)
//	snapshot/repo/<name>      -> 仓库元信息
//	snapshot/<repo>/<name>    -> 快照元信息
//	doc-tf/<index>/<id>       -> 文档分词倒排(Bool -> posting list),用于服务端内置搜索
//
// 所有写操作走 RW 事务;读操作走 View 事务以保证快照一致性
package storage

import (
	"encoding/json"
	"fmt"
	"sync"

	badger "github.com/dgraph-io/badger/v4"
)

// Store 顶层存储句柄
type Store struct {
	db *badger.DB
	// mu 保护 SDK 内部对 BadgerDB 视图外的可变状态
	mu sync.Mutex
}

// Open 打开(创建)一个 BadgerDB 实例
// 参数:
//
//	path: 数据目录;为空时使用内存模式
//
// 返回:
//
//	*Store: 存储实例
//	error: 错误
func Open(path string) (*Store, error) {
	opts := badger.DefaultOptions(path)
	if path == "" {
		// 内存模式:适合单元测试
		opts = opts.WithInMemory(true)
	}
	// 默认 logger 静音,避免测试日志噪声
	opts.Logger = nil

	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open badger: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库
func (s *Store) Close() error {
	return s.db.Close()
}

// DB 返回底层 badger 句柄(给 server 层做高级遍历用)
func (s *Store) DB() *badger.DB { return s.db }

// Put 写一个键值对(JSON 自动序列化 value)
// 参数:
//
//	key: 字节切片
//	value: 任意可序列化结构
//
// 返回:
//
//	error: 错误
func (s *Store) Put(key []byte, value interface{}) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, payload)
	})
}

// PutRaw 写原始字节
func (s *Store) PutRaw(key, value []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, value)
	})
}

// Get 读取一个键,value 通过 JSON 反序列化到 out
// 键不存在时返回 false 与 nil error
func (s *Store) Get(key []byte, out interface{}) (bool, error) {
	var raw []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		raw, err = item.ValueCopy(nil)
		return err
	})
	if err != nil {
		return false, err
	}
	if raw == nil {
		return false, nil
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return false, fmt.Errorf("unmarshal value: %w", err)
		}
	}
	return true, nil
}

// GetRaw 读取原始字节
func (s *Store) GetRaw(key []byte) ([]byte, bool, error) {
	var raw []byte
	found := false
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		var v []byte
		v, err = item.ValueCopy(nil)
		if err == nil {
			raw = v
		}
		return err
	})
	return raw, found, err
}

// Delete 删除一个键
func (s *Store) Delete(key []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
}

// Exists 判断键是否存在
func (s *Store) Exists(key []byte) (bool, error) {
	err := s.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return nil
		}
		return err
	})
	if err != nil {
		return false, err
	}
	// 二次判断需要重新查询
	found := false
	_ = s.db.View(func(txn *badger.Txn) error {
		_, e := txn.Get(key)
		if e == nil {
			found = true
		}
		return nil
	})
	return found, nil
}

// Ping 轻量健康检查 (View 一遍数据库)
func (s *Store) Ping() error {
	return s.db.View(func(txn *badger.Txn) error {
		// 读 meta 探测 (Key 不存在也 ok, View 失败才算 down)
		_, _ = txn.Get([]byte("__ping__"))
		return nil
	})
}

// WithTransaction 在一个读写事务内执行操作
// 适用于需要"读-改-写"原子性的场景(如更新别名+索引元信息)
func (s *Store) WithTransaction(fn func(txn *badger.Txn) error) error {
	return s.db.Update(fn)
}

// ViewTransaction 只读事务
func (s *Store) ViewTransaction(fn func(txn *badger.Txn) error) error {
	return s.db.View(fn)
}

// Scan 遍历指定前缀的所有键值对(浅拷贝 value,避免大对象持锁)
func (s *Store) Scan(prefix []byte, fn func(key, value []byte) error) error {
	return s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			k := item.KeyCopy(nil)
			v, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			if err := fn(k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeletePrefix 删除指定前缀的所有键
func (s *Store) DeletePrefix(prefix []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		it := txn.NewIterator(opts)
		defer it.Close()
		var keys [][]byte
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			k := it.Item().KeyCopy(nil)
			keys = append(keys, k)
		}
		for _, k := range keys {
			if err := txn.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// 常用 key 构造器

// DocKey 构造文档主键
func DocKey(index, id string) []byte {
	return []byte("doc/" + index + "/" + id)
}

// DocPrefix 文档前缀
func DocPrefix(index string) []byte {
	return []byte("doc/" + index + "/")
}

// MetaKey 索引元信息键
func MetaKey(index string) []byte {
	return []byte("meta/" + index)
}

// AliasKey 别名键
func AliasKey(alias string) []byte {
	return []byte("alias/" + alias)
}

// ILMKey ILM 策略键
func ILMKey(policy string) []byte {
	return []byte("ilm/" + policy)
}

// IndexTplKey 索引模板键
func IndexTplKey(name string) []byte {
	return []byte("tpl/index/" + name)
}

// ComponentTplKey 组件模板键
func ComponentTplKey(name string) []byte {
	return []byte("tpl/component/" + name)
}

// DocTFPrefix per-doc 分词结果前缀(doc-tf/<index>/<id> -> {field: [tokens]})
func DocTFPrefix(index string) []byte {
	return []byte("doc-tf/" + index + "/")
}

// DocTFKey per-doc 分词结果键
func DocTFKey(index, id string) []byte {
	return []byte("doc-tf/" + index + "/" + id)
}

// DocMetaKey 文档版本元数据键(doc-meta/<index>/<id> -> {seq_no, primary_term, version})
func DocMetaKey(index, id string) []byte {
	return []byte("doc-meta/" + index + "/" + id)
}

// PostingsVersionKey 倒排版本号(每次写递增, 用于缓存失效判定)
func PostingsVersionKey(index string) []byte {
	return []byte("postings-version/" + index)
}

// IngestKey Ingest 管道键
func IngestKey(name string) []byte {
	return []byte("ingest/" + name)
}

// SnapshotRepoKey 快照仓库键
func SnapshotRepoKey(name string) []byte {
	return []byte("snapshot/repo/" + name)
}

// SnapshotKey 快照元信息键
func SnapshotKey(repo, name string) []byte {
	return []byte("snapshot/" + repo + "/" + name)
}
