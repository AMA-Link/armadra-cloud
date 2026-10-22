// Package tlsx 是个人中转的 TLS 材料：自签 CA 与叶证书、文件证书热换、ACME http-01，
// 以及可热换证书的 tls.Config（对应 apps/relay/src/personal 的 tls.ts、acme.ts、der.ts）。
package tlsx

import (
	"crypto/tls"
	"time"
)

// 数据目录下的位置与有效期（与 TS 版逐项一致）。
const (
	TLSDir     = "tls"
	CACert     = "ca.crt"
	CAKey      = "ca.key"
	LeafCert   = "leaf.crt"
	LeafKey    = "leaf.key"
	ACMEDir    = "acme"
	CADays     = 3650
	LeafDays   = 90
	LeafRenew  = 7 * 24 * time.Hour
	RenewCheck = 12 * time.Hour
)

// Material 是一套在用的证书。
type Material struct {
	Certificate tls.Certificate
	// Anchor 是 /ca.crt 发的 CA（PEM）；ACME 没有要钉的锚，为 nil。
	Anchor []byte
	// Fingerprint 是信任锚（自签的 CA、文件模式的链顶）DER 的 SHA-256，小写十六进制、无分隔符（与 TS 版一致）；ACME 为空。
	Fingerprint string
	NotAfter    time.Time
	// Source 是 "self-signed"、"file" 或 "acme"。
	Source string
}

// Manager 管一套会变的证书。
type Manager interface {
	Current() Material
	// Start 开始后台续期 / 轮询；证书换了回调 onChange，失败回调 onError（继续用旧证书）。答停止函数。
	Start(onChange func(Material), onError func(error)) (stop func())
}

// Error 是 TLS 材料的问题（坏的 CA、读不出的证书……）；CLI 打印 Msg 后退出码 1。
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }
