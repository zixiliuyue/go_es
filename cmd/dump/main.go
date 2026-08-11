// 命令 dump:把 go_es 服务端的文档导出为 NDJSON 文件
//
// 用法:
//
//	go run ./cmd/dump -url http://localhost:9200 -out data.ndjson
//	go run ./cmd/dump -url http://localhost:9200 -idx idx1,idx2 -out data.ndjson
//
// 实现:
//   - 用 pkg/dumprestore 客户端包与服务端解耦
//   - 自动滚动翻页读取 _search 结果
//   - 文件末尾附带 __dump_meta__ 元数据行,便于 restore 侧完整性校验
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/zixiliuyue/go_es/pkg/dumprestore"
)

func main() {
	var (
		url     string
		out     string
		idxs    string
		user    string
		pass    string
		pageSz  int
	)
	flag.StringVar(&url, "url", "http://localhost:9200", "go_es 服务端 URL")
	flag.StringVar(&out, "out", "-", "输出文件路径,- 表示 stdout")
	flag.StringVar(&idxs, "idx", "", "要导出的索引,逗号分隔,空为全部")
	flag.StringVar(&user, "user", "", "Basic 用户名")
	flag.StringVar(&pass, "pass", "", "Basic 密码")
	flag.IntVar(&pageSz, "page-size", 1000, "每页读取条数")
	flag.Parse()

	// 支持 Ctrl+C 优雅退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	var indices []string
	if idxs != "" {
		for _, i := range strings.Split(idxs, ",") {
			if v := strings.TrimSpace(i); v != "" {
				indices = append(indices, v)
			}
		}
	}

	exporter := dumprestore.NewExporter(dumprestore.ExporterConfig{
		BaseURL:  url,
		Indices:  indices,
		PageSize: pageSz,
		Username: user,
		Password: pass,
		Progress: func(n int) {
			fmt.Fprintf(os.Stderr, "\r[dump] exported %d docs...", n)
		},
	})

	n, err := exporter.Run(ctx, out)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dump failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "dump done: %d docs exported to %s\n", n, out)
}
