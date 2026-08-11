// 命令 restore:把 NDJSON 文件写回 go_es 服务端
//
// 用法:
//
//	go run ./cmd/restore -url http://localhost:9200 -in data.ndjson
//	go run ./cmd/restore -url http://localhost:9200 -in data.ndjson -target-idx my_idx
//
// 实现:
//   - 逐行解析 NDJSON,忽略 __dump_meta__ 元数据行
//   - 每 batch-size 条走一次 _bulk 写入
//   - 支持 -target-idx 强制覆盖写入索引名(数据迁移场景)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/zixiliuyue/go_es/pkg/dumprestore"
)

func main() {
	var (
		url        string
		in         string
		targetIdx  string
		user       string
		pass       string
		batchSz    int
	)
	flag.StringVar(&url, "url", "http://localhost:9200", "go_es 服务端 URL")
	flag.StringVar(&in, "in", "", "输入 NDJSON 文件路径,- 表示 stdin")
	flag.StringVar(&targetIdx, "target-idx", "", "强制覆盖写入的索引名")
	flag.StringVar(&user, "user", "", "Basic 用户名")
	flag.StringVar(&pass, "pass", "", "Basic 密码")
	flag.IntVar(&batchSz, "batch-size", 500, "每批写入条数")
	flag.Parse()

	if in == "" {
		fmt.Fprintln(os.Stderr, "-in 参数必填")
		os.Exit(2)
	}

	// 支持 Ctrl+C 优雅退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	importer := dumprestore.NewImporter(dumprestore.ImporterConfig{
		BaseURL:     url,
		TargetIndex: targetIdx,
		BatchSize:   batchSz,
		Username:    user,
		Password:    pass,
		Progress: func(restored, errs int) {
			fmt.Fprintf(os.Stderr, "\r[restore] restored=%d errors=%d", restored, errs)
		},
	})

	restored, errs, meta, err := importer.Run(ctx, in)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore failed: %v\n", err)
		os.Exit(1)
	}
	if meta != nil {
		fmt.Fprintf(os.Stderr, "source meta: version=%d source_index=%s doc_count=%d\n",
			meta.Version, meta.SourceIndex, meta.DocCount)
	}
	fmt.Fprintf(os.Stderr, "restore done: restored=%d errors=%d\n", restored, errs)
	if errs > 0 {
		os.Exit(3)
	}
}
