// go_es 自研 Elasticsearch 服务端入口
// 启动一个最小可运行的 ES 8 兼容服务端,数据持久化在本地 BadgerDB
//
// 用法:
//
//	go run ./cmd/server -addr :9200 -data /tmp/go_es_data
//
// 默认监听 :9200,数据目录为当前目录下的 ./data
// 通过环境变量 ES_ADDR / ES_DATA 可覆盖
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/server"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

func main() {
	addr := flag.String("addr", envOr("ES_ADDR", ":9200"), "HTTP listen address")
	data := flag.String("data", envOr("ES_DATA", "./data"), "BadgerDB data directory (empty for in-memory)")
	flag.Parse()

	logger, _ := zap.NewDevelopment()
	defer func() { _ = logger.Sync() }()

	store, err := storage.Open(*data)
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	engine := search.New(store)
	if err := engine.LoadAll(); err != nil {
		logger.Warn("load all failed", zap.Error(err))
	}

	srv := server.New(store, engine, logger)
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("go_es server listening", zap.String("addr", *addr), zap.String("data", *data))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// envOr 优先返回 env,否则 fallback
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
