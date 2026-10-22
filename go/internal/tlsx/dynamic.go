package tlsx

import (
	"crypto/tls"
	"sync/atomic"
)

// Dynamic 是可热换证书的 tls.Config 来源。
type Dynamic struct {
	cert atomic.Pointer[tls.Certificate]
}

// NewDynamic 用一套证书建。
func NewDynamic(m Material) *Dynamic {
	d := &Dynamic{}
	d.Set(m)
	return d
}

// Set 换证书；已建立的连接不受影响，新握手用新的。
func (d *Dynamic) Set(m Material) {
	c := m.Certificate
	d.cert.Store(&c)
}

// Config 答 TLS ≥ 1.2、只走 HTTP/1.1 的服务端配置，证书经 GetCertificate 现取。
func (d *Dynamic) Config() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return d.cert.Load(), nil
		},
	}
}
