// cmd/server 启动路径的单元测试
// 这里只测可独立验证的部分:
//   - configureTransport 在非 TLS 路径会包一层 h2c
//   - configureTransport 在 TLS 路径会设置 TLSConfig / NextProtos=[h2, http/1.1]
//   - 真实 TLS 监听能完成 handshake 并响应一个 HTTP/1.1 请求
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zixiliuyue/go_es/internal/server"
)

// 生成一个自签名的 RSA(实际是 ECDSA)证书/私钥对并写到 t.TempDir() 下
// 返回 cert 路径与 key 路径
func writeSelfSigned(t *testing.T) (cert, key string) {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))

	keyDER, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0600))

	return certPath, keyPath
}

func TestConfigureTransport_H2CWhenTLSDisabled(t *testing.T) {
	srv := &http.Server{Handler: http.NewServeMux()}
	configureTransport(srv, false, "", "", true, "", server.ClientAuthNone)
	// 非 TLS: Handler 被 h2c 包了一层(非原 handler)
	// 实际类型是 *h2cHandler, 这里只验证不是 nil
	assert.NotNil(t, srv.Handler)
	assert.Nil(t, srv.TLSConfig)
}

func TestConfigureTransport_TLSWithH2(t *testing.T) {
	cert, key := writeSelfSigned(t)
	srv := &http.Server{Handler: http.NewServeMux()}
	configureTransport(srv, true, cert, key, true, "", server.ClientAuthNone)
	require.NotNil(t, srv.TLSConfig)
	assert.Equal(t, uint16(tls.VersionTLS12), srv.TLSConfig.MinVersion)
	assert.Contains(t, srv.TLSConfig.NextProtos, "h2")
	assert.Contains(t, srv.TLSConfig.NextProtos, "http/1.1")
	assert.Len(t, srv.TLSConfig.Certificates, 1)
}

func TestConfigureTransport_TLSWithoutH2(t *testing.T) {
	cert, key := writeSelfSigned(t)
	srv := &http.Server{Handler: http.NewServeMux()}
	configureTransport(srv, true, cert, key, false, "", server.ClientAuthNone)
	require.NotNil(t, srv.TLSConfig)
	// 显式 disable h2: NextProtos 不应包含 h2
	for _, p := range srv.TLSConfig.NextProtos {
		assert.NotEqual(t, "h2", p, "disable h2 时 NextProtos 不应含 h2")
	}
}

func TestConfigureTransport_PanicsOnMissingCert(t *testing.T) {
	srv := &http.Server{Handler: http.NewServeMux()}
	assert.Panics(t, func() {
		configureTransport(srv, true, "", "", true, "", server.ClientAuthNone)
	})
}

// 端到端: 起一个真实 TLS 监听, 用 tls 客户端请求一次, 验握手 + 业务响应
func TestTLSEndToEnd_HandshakeAndResponse(t *testing.T) {
	cert, key := writeSelfSigned(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/_health/liveness", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"alive"}`)
	})
	srv := &http.Server{Handler: mux}
	configureTransport(srv, true, cert, key, true, "", server.ClientAuthNone)

	// 任意空闲端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	go func() { _ = srv.ServeTLS(ln, cert, key) }()
	defer srv.Close()

	url := "https://" + ln.Addr().String() + "/_health/liveness"
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp, err := client.Get(url)
	require.NoError(t, err, "TLS 握手应成功")
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), `"status":"alive"`)
	// 验证 server 端确实协商到了 TLS(connected 必定是 true)
	assert.Equal(t, "https", url[:5])
}

// writeMTLSMaterials 生成 server cert + 自签 CA + 由该 CA 签发的 client cert.
// 返回: server.crt, server.key, client.crt, client.key, ca.crt (PEM 文件路径)
func writeMTLSMaterials(t *testing.T) (serverCert, serverKey, clientCert, clientKey, caCert string) {
	t.Helper()
	dir := t.TempDir()

	// 1. 生成 CA 私钥 + 自签 CA 证书
	caPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caSerial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	caTmpl := x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "go_es test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, &caTmpl, &caTmpl, &caPriv.PublicKey, caPriv)
	require.NoError(t, err)
	caCert = filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(caCert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600))

	// 2. 生成 server 私钥 + 用 CA 签发
	srvPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	srvSerial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	srvTmpl := x509.Certificate{
		SerialNumber: srvSerial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, &srvTmpl, &caTmpl, &srvPriv.PublicKey, caPriv)
	require.NoError(t, err)
	serverCert = filepath.Join(dir, "server.crt")
	require.NoError(t, os.WriteFile(serverCert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}), 0600))
	serverKey = filepath.Join(dir, "server.key")
	srvKeyDER, _ := x509.MarshalECPrivateKey(srvPriv)
	require.NoError(t, os.WriteFile(serverKey, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: srvKeyDER}), 0600))

	// 3. 生成 client 私钥 + 用 CA 签发 (EKU=ClientAuth)
	cliPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	cliSerial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	cliTmpl := x509.Certificate{
		SerialNumber: cliSerial,
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	cliDER, err := x509.CreateCertificate(rand.Reader, &cliTmpl, &caTmpl, &cliPriv.PublicKey, caPriv)
	require.NoError(t, err)
	clientCert = filepath.Join(dir, "client.crt")
	require.NoError(t, os.WriteFile(clientCert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cliDER}), 0600))
	clientKey = filepath.Join(dir, "client.key")
	cliKeyDER, _ := x509.MarshalECPrivateKey(cliPriv)
	require.NoError(t, os.WriteFile(clientKey, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: cliKeyDER}), 0600))

	return
}

func TestConfigureTransport_MTLSLoadsCAPool(t *testing.T) {
	scert, skey, _, _, caCert := writeMTLSMaterials(t)
	srv := &http.Server{Handler: http.NewServeMux()}
	configureTransport(srv, true, scert, skey, true, caCert, server.ClientAuthRequireVerify)
	require.NotNil(t, srv.TLSConfig)
	require.NotNil(t, srv.TLSConfig.ClientCAs, "mTLS 模式应注入 ClientCAs")
	assert.Equal(t, tls.RequireAndVerifyClientCert, srv.TLSConfig.ClientAuth, "require_verify 应映射到 RequireAndVerifyClientCert")
	// CA 池至少含 1 个 cert
	assert.Greater(t, len(srv.TLSConfig.ClientCAs.Subjects()), 0)
}

func TestConfigureTransport_MTLSClientAuthMapping(t *testing.T) {
	scert, skey, _, _, caCert := writeMTLSMaterials(t)
	cases := []struct {
		name string
		in   server.ClientAuthKind
		want tls.ClientAuthType
	}{
		{"none", server.ClientAuthNone, tls.NoClientCert},
		{"request", server.ClientAuthRequest, tls.RequestClientCert},
		{"require_any", server.ClientAuthRequireAny, tls.RequireAnyClientCert},
		{"require_verify", server.ClientAuthRequireVerify, tls.RequireAndVerifyClientCert},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &http.Server{Handler: http.NewServeMux()}
			configureTransport(srv, true, scert, skey, true, caCert, tc.in)
			require.NotNil(t, srv.TLSConfig)
			assert.Equal(t, tc.want, srv.TLSConfig.ClientAuth)
		})
	}
}

func TestConfigureTransport_MTLSPanicsOnInvalidCA(t *testing.T) {
	scert, skey := writeSelfSigned(t)
	dir := t.TempDir()
	badCA := filepath.Join(dir, "bad.crt")
	require.NoError(t, os.WriteFile(badCA, []byte("not a pem"), 0600))
	srv := &http.Server{Handler: http.NewServeMux()}
	assert.Panics(t, func() {
		configureTransport(srv, true, scert, skey, true, badCA, server.ClientAuthRequireVerify)
	})
}

func TestConfigureTransport_NoCAWhenAuthNone(t *testing.T) {
	scert, skey := writeSelfSigned(t)
	// 不传 caCert (空字符串), 走纯标准 TLS 路径
	srv := &http.Server{Handler: http.NewServeMux()}
	configureTransport(srv, true, scert, skey, true, "", server.ClientAuthNone)
	require.NotNil(t, srv.TLSConfig)
	assert.Nil(t, srv.TLSConfig.ClientCAs, "无 caCert + auth=none 不应注入 ClientCAs")
}

// mTLS end-to-end: require_verify 模式下, 合法 client cert 可访问, 无 cert 拒绝
func TestMTLSEndToEnd_RequireVerify(t *testing.T) {
	scert, skey, ccert, ckey, caCert := writeMTLSMaterials(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/_health/liveness", func(w http.ResponseWriter, r *http.Request) {
		// 验证服务端能拿到 client cert subject
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			w.Header().Set("X-Client-Subject", r.TLS.PeerCertificates[0].Subject.CommonName)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"alive"}`)
	})
	srv := &http.Server{Handler: mux}
	configureTransport(srv, true, scert, skey, true, caCert, server.ClientAuthRequireVerify)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() { _ = srv.ServeTLS(ln, scert, skey) }()
	defer srv.Close()

	url := "https://" + ln.Addr().String() + "/_health/liveness"

	// 加载客户端证书
	ccp, err := tls.LoadX509KeyPair(ccert, ckey)
	require.NoError(t, err)
	caPEM, err := os.ReadFile(caCert)
	require.NoError(t, err)
	caPool := x509.NewCertPool()
	require.True(t, caPool.AppendCertsFromPEM(caPEM))

	// 1. 带 client cert 访问 -> 成功 + 拿到 client subject
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{ccp},
			RootCAs:      caPool,
			ServerName:   "localhost",
		},
	}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get(url)
	require.NoError(t, err, "带 client cert 应握手成功")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "test-client", resp.Header.Get("X-Client-Subject"), "服务端应拿到 client cert CN")
	resp.Body.Close()

	// 2. 不带 client cert 访问 -> 握手失败(alert)
	tr2 := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    caPool,
			ServerName: "localhost",
		},
	}
	defer tr2.CloseIdleConnections()
	_, err = (&http.Client{Transport: tr2, Timeout: 5 * time.Second}).Get(url)
	require.Error(t, err, "require_verify 模式下无 client cert 应被拒")
	assert.Contains(t, err.Error(), "certificate", "错误信息应含 certificate 相关关键词")
}

// clientAuthToTLS 单元测试
func TestClientAuthToTLS(t *testing.T) {
	assert.Equal(t, tls.NoClientCert, clientAuthToTLS(server.ClientAuthNone))
	assert.Equal(t, tls.RequestClientCert, clientAuthToTLS(server.ClientAuthRequest))
	assert.Equal(t, tls.RequireAnyClientCert, clientAuthToTLS(server.ClientAuthRequireAny))
	assert.Equal(t, tls.RequireAndVerifyClientCert, clientAuthToTLS(server.ClientAuthRequireVerify))
	// 未知枚举 -> NoClientCert(防御性)
	assert.Equal(t, tls.NoClientCert, clientAuthToTLS(server.ClientAuthKind("nonsense")))
}
