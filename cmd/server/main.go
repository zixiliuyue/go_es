// go_es 自研 Elasticsearch 服务端入口
// 启动一个最小可运行的 ES 8 兼容服务端,数据持久化在本地 BadgerDB
//
// 用法:
//
//	go run ./cmd/server -addr :9200 -data /tmp/go_es_data
//	go run ./cmd/server -config ./go_es.yaml
//	go run ./cmd/server -tls.cert ./certs/server.crt -tls.key ./certs/server.key
//
// 默认监听 :9200,数据目录为当前目录下的 ./data
// 通过环境变量 ES_ADDR / ES_DATA 可覆盖
//
// 配置文件格式见 internal/server/config.go.
// 配置文件可热更新(每 5s 轮询 mtime), 修改后无需重启.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/server"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	addr := flag.String("addr", envOr("ES_ADDR", ":9200"), "HTTP listen address (override by -config)")
	data := flag.String("data", envOr("ES_DATA", "./data"), "BadgerDB data directory (empty for in-memory)")
	authUser := flag.String("auth.user", "", "Basic auth username (empty disables auth)")
	authPass := flag.String("auth.password", "", "Basic auth password")
	maxBody := flag.Int("max-body", 100, "max request body size in MiB")
	rate := flag.Float64("rate", 0, "per-IP rate limit (rps, 0 = unlimited)")
	configPath := flag.String("config", "", "path to YAML config file (overrides flags; supports hot reload)")
	tlsCert := flag.String("tls.cert", "", "TLS cert file (PEM); empty disables TLS")
	tlsKey := flag.String("tls.key", "", "TLS key file (PEM); required when -tls.cert is set")
	tlsHTTP2 := flag.Bool("tls.enable-http2", true, "negotiate h2 on TLS (requires -tls.cert/-tls.key)")
	tlsClientCA := flag.String("tls.client-ca", "", "client CA pool file (PEM); enables mTLS when combined with -tls.cert/-tls.key/-tls.client-auth")
	tlsClientAuth := flag.String("tls.client-auth", "none", "client cert enforcement: none|request|require_any|require_verify (mTLS mode)")
	sessionEnabled := flag.Bool("session.enabled", false, "enable session management (token-based auth with login/logout)")
	sessionTimeout := flag.Duration("session.timeout", 24*time.Hour, "session timeout duration (e.g. 1h, 24h)")
	sessionMaxSessions := flag.Int("session.max-sessions", 5, "max concurrent sessions per user (0 = unlimited)")
	sessionSecret := flag.String("session.secret", "", "HMAC signing key for session tokens (empty = auto-generated)")
	flag.Parse()

	logger, _ := zap.NewDevelopment()
	defer func() { _ = logger.Sync() }()

	// 配置加载: -config 优先, 其次命令行 flag
	loader := server.NewConfigLoader(*configPath)
	if *configPath != "" {
		if err := loader.Load(); err != nil {
			log.Fatalf("load config: %v", err)
		}
		cfg := loader.Get()
		// -config 优先生效
		if cfg.Addr != "" {
			*addr = cfg.Addr
		}
		if cfg.Data != "" {
			*data = cfg.Data
		}
		if cfg.Auth.Enabled {
			*authUser = "" // 让下面 cfg.Auth 直接覆盖
		}
		if cfg.Limit.MaxBodyBytes > 0 {
			*maxBody = int(cfg.Limit.MaxBodyBytes >> 20)
		}
		if cfg.Limit.RatePerSecond > 0 {
			*rate = cfg.Limit.RatePerSecond
		}
		// TLS: -config 优先生效(都只在启动时生效)
		if cfg.TLS.CertFile != "" {
			*tlsCert = cfg.TLS.CertFile
		}
		if cfg.TLS.KeyFile != "" {
			*tlsKey = cfg.TLS.KeyFile
		}
		// enable_http2: yaml 显式设置时覆盖 flag 默认值
		if cfg.TLS.EnableHTTP2 != nil {
			*tlsHTTP2 = *cfg.TLS.EnableHTTP2
		}
		// mTLS 字段: yaml 显式设置时覆盖 flag 默认
		if cfg.TLS.ClientCAFile != "" {
			*tlsClientCA = cfg.TLS.ClientCAFile
		}
		if cfg.TLS.ClientAuth != nil {
			*tlsClientAuth = *cfg.TLS.ClientAuth
		}
		// 热更新: 每 5s 轮询, 改动后仅刷新 auth/limit(其它启动时决定)
		loader.SetOnChange(func(old, new *server.ConfigFile) {
			logger.Info("config reloaded",
				zap.String("path", *configPath),
				zap.Bool("auth_enabled", new.Auth.Enabled))
		})
		go loader.Watch()
		defer loader.Stop()
	}

	// 校验 TLS 参数一致性
	tlsOn := *tlsCert != "" || *tlsKey != ""
	if tlsOn && (*tlsCert == "" || *tlsKey == "") {
		log.Fatalf("TLS: 必须同时提供 -tls.cert 和 -tls.key")
	}
	if tlsOn {
		// 立即检查文件可读, 避免启动后 ListenAndServeTLS 才报错
		if _, err := os.Stat(*tlsCert); err != nil {
			log.Fatalf("TLS cert not readable: %v", err)
		}
		if _, err := os.Stat(*tlsKey); err != nil {
			log.Fatalf("TLS key not readable: %v", err)
		}
	}
	// mTLS 校验: client_ca 与 client_auth 必须配对
	clientAuthVal := server.ClientAuthKind(*tlsClientAuth)
	if (*tlsClientCA != "" || clientAuthVal != server.ClientAuthNone) && *tlsClientCA == "" {
		log.Fatalf("mTLS: 设置了 -tls.client-auth=%q 但 -tls.client-ca 为空(无法验证客户端)", *tlsClientAuth)
	}
	if *tlsClientCA != "" && clientAuthVal == server.ClientAuthNone {
		log.Fatalf("mTLS: 设置了 -tls.client-ca 但 -tls.client-auth=none(client_ca 无意义)")
	}
	if !tlsOn && *tlsClientCA != "" {
		log.Fatalf("mTLS: -tls.client-ca 需要先启用 TLS(同时提供 -tls.cert/-tls.key)")
	}
	if clientAuthVal != server.ClientAuthNone {
		switch clientAuthVal {
		case server.ClientAuthRequest, server.ClientAuthRequireAny, server.ClientAuthRequireVerify:
			// 合法值
		default:
			log.Fatalf("mTLS: 未知 -tls.client-auth=%q (期望 none|request|require_any|require_verify)", *tlsClientAuth)
		}
		if _, err := os.Stat(*tlsClientCA); err != nil {
			log.Fatalf("mTLS client CA not readable: %v", err)
		}
	}

	store, err := storage.Open(*data)
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	engine := search.New(store)
	if err := engine.LoadAll(); err != nil {
		logger.Warn("load all failed", zap.Error(err))
	}

	// 认证配置: -config 优先
	auth := server.AuthConfig{Enabled: false}
	if loader.Get().Auth.Enabled {
		auth = loader.Get().Auth
	} else if *authUser != "" {
		auth.Enabled = true
		auth.Basic = map[string]string{*authUser: *authPass}
	}
	// 收集 -apikey 参数(从 flag.Args() 解析 key=value)
	for _, a := range flag.Args() {
		if k, v, ok := splitKV(a); ok && k == "apikey" && v != "" {
			auth.Enabled = true
			auth.APIKeys = append(auth.APIKeys, v)
		}
	}
	// 限速与体限制: -config 优先
	limit := server.LimitConfig{
		MaxBodyBytes:  int64(*maxBody) << 20,
		RatePerSecond: *rate,
	}
	if cfg := loader.Get(); cfg.Limit.MaxBodyBytes > 0 {
		limit.MaxBodyBytes = cfg.Limit.MaxBodyBytes
	}
	if cfg := loader.Get(); cfg.Limit.RatePerSecond > 0 {
		limit.RatePerSecond = cfg.Limit.RatePerSecond
	}

	// 会话管理配置: -config 优先, 其次命令行 flag
	sessionCfg := server.SessionConfig{
		Enabled:         *sessionEnabled,
		Timeout:         *sessionTimeout,
		MaxSessions:     *sessionMaxSessions,
		Secret:          *sessionSecret,
		CleanupInterval: 5 * time.Minute,
	}
	if cfg := loader.Get(); cfg.Session.Enabled {
		sessionCfg = cfg.Session
		if *sessionEnabled {
			sessionCfg.Enabled = true
		}
	}

	srv := server.NewWithOptions(store, engine, logger, server.ServerOptions{
		Auth:         auth,
		Limit:        limit,
		ConfigLoader: loader,
		Session:      sessionCfg,
	})
	srv.MarkStartupDone()
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// 协议协商:
	//   - 启用 TLS: 由 stdlib 在 TLS handshake 中自动协商 h2(NextProtos 默认含 "h2")
	//   - 明文: 用 h2c.NewHandler 包一层, 客户端可选 h2c
	configureTransport(httpSrv, tlsOn, *tlsCert, *tlsKey, *tlsHTTP2, *tlsClientCA, clientAuthVal)

	// 优雅关闭: 标记 shutting down + 等待 inflight 任务
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("go_es server listening",
			zap.String("addr", *addr),
			zap.String("data", *data),
			zap.Bool("auth_enabled", auth.Enabled),
			zap.Int64("max_body_bytes", limit.MaxBodyBytes),
			zap.Float64("rate_per_second", limit.RatePerSecond),
			zap.Bool("tls", tlsOn),
			zap.Bool("tls_h2", tlsOn && *tlsHTTP2),
			zap.Bool("mtls", tlsOn && *tlsClientCA != ""),
			zap.String("client_auth", string(clientAuthVal)),
			zap.Bool("session_enabled", sessionCfg.Enabled),
			zap.Duration("session_timeout", sessionCfg.Timeout),
			zap.Int("session_max_sessions", sessionCfg.MaxSessions))
		var serveErr error
		if tlsOn {
			serveErr = httpSrv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			serveErr = httpSrv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Fatalf("listen: %v", serveErr)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down... waiting for inflight tasks to drain")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 先标记服务进入 shutting down(readiness 会立即返回 503)
	srv.Shutdown(shutdownCtx)
	_ = httpSrv.Shutdown(shutdownCtx)
}

// envOr 优先返回 env,否则 fallback
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// splitKV 解析 "key=value" 形式
func splitKV(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// 抑制 strconv 未用警告
var _ = strconv.Itoa

// configureTransport 根据是否启用 TLS 选择协议协商方式.
//   - tlsOn=true: 让 Go stdlib 在 TLS handshake 中自动支持 h2(http2.ConfigureServer 是冗余但显式).
//   - tlsOn=false: 用 h2c.NewHandler 把明文 HTTP/2 暴露在同端口.
//
// mTLS 模式: clientCAFile 非空 且 clientAuth != none, 加载 CA 池后挂到 ClientCAs + ClientAuth.
// 这里只动 httpSrv 的 Handler / TLSConfig, 不动监听逻辑.
func configureTransport(httpSrv *http.Server, tlsOn bool, certFile, keyFile string, enableH2 bool, clientCAFile string, clientAuth server.ClientAuthKind) {
	if !tlsOn {
		httpSrv.Handler = h2c.NewHandler(httpSrv.Handler, &http2.Server{})
		return
	}
	// 证书在 main() 已校验过; 这里用 stdlib 加载, 让它在启动期就 fail-fast.
	// 真正的监听仍由 ListenAndServeTLS 在外部完成, 此处只配 TLSConfig 以启用 h2.
	if certFile == "" || keyFile == "" {
		// 防御性: 调用方应已保证; 但保留兜底, 启动期直接 panic 让进程退出.
		panic("configureTransport: tlsOn but cert/key empty (caller bug)")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		// 同上, 启动期 fail-fast
		panic("configureTransport: load x509 key pair: " + err.Error())
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if enableH2 {
		// 显式声明 h2 + http/1.1 协商, stdlib 本身默认就含, 这里更清楚.
		tlsCfg.NextProtos = []string{"h2", "http/1.1"}
	}
	// mTLS: 加载客户端 CA 池 + 设置 ClientAuth
	if clientCAFile != "" {
		caBytes, err := os.ReadFile(clientCAFile)
		if err != nil {
			panic("configureTransport: read client CA: " + err.Error())
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			panic("configureTransport: client CA pool invalid (no certs appended)")
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = clientAuthToTLS(clientAuth)
	}
	httpSrv.TLSConfig = tlsCfg
	if enableH2 {
		// 显式 ConfigureServer: 即使 NextProtos 已含 h2, 也能保证 server 知道如何处理.
		// ListenAndServeTLS 会自动调 ConfigureServer, 这里跳过重复.
		_ = http2.ConfigureServer(httpSrv, nil)
	}
}

// clientAuthToTLS 把内部枚举映射到 crypto/tls 的 ClientAuthType.
// "none" -> NoClientCert(不需要证书, 标准 TLS 行为).
func clientAuthToTLS(k server.ClientAuthKind) tls.ClientAuthType {
	switch k {
	case server.ClientAuthNone:
		return tls.NoClientCert
	case server.ClientAuthRequest:
		return tls.RequestClientCert
	case server.ClientAuthRequireAny:
		return tls.RequireAnyClientCert
	case server.ClientAuthRequireVerify:
		return tls.RequireAndVerifyClientCert
	}
	// 防御性: 未知枚举走 NoClientCert
	return tls.NoClientCert
}
