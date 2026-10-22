package personal

import "crypto/tls"

// tlsInsecure 是探活用的客户端配置：自签证书不验（只打本机回环端口）。
func tlsInsecure() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 只探本机监听端口的 /health
}
