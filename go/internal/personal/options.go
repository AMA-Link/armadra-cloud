package personal

import (
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
)

// ListenAddress 是 host:port（IPv6 写成 [::1]:8100）。
type ListenAddress struct {
	Host string
	Port int
}

// Common 是每个子命令都收的：数据目录与（用 PostgreSQL 时的）连接。
type Common struct {
	DataDir  string
	Database *DatabaseOptions
}

// InitOptions 是 personal init 的参数。
type InitOptions struct {
	Common
	Account string
	// PasswordFile 是口令文件；"-" 是标准输入；空 = 终端交互。
	PasswordFile string
	Host         string
	Port         int
	TLS          string
	PublicOrigin string
	// Store 缺省（空）= sqlite。
	Store StoreKind
}

// ACMEArgs 是 --tls acme 的参数。
type ACMEArgs struct {
	Email      string
	Directory  string
	CABundle   string
	HTTPListen ListenAddress
}

// RateLimits 是边缘限流额度（次 / 分钟）；0 = 没给。
type RateLimits struct {
	Anonymous, Preflight, Token, Account int
}

// ChallengeOptions 是人机挑战（--challenge turnstile）。
type ChallengeOptions struct {
	SiteKey    string
	SecretFile string
	Scope      []core.ChallengeScope
	Hostnames  []string
}

// ServeOptions 是 personal serve 的参数。
type ServeOptions struct {
	Common
	// Store 空 = 按数据目录里已有的（新装 sqlite）。
	Store StoreKind
	// Control 是 memory | redis | postgres，已填缺省。
	Control  string
	RedisURL string
	// NodeID 已填缺省（--host 截 64）。
	NodeID        string
	Host          string
	Port          int
	Listen        ListenAddress
	TLS           string
	LogLevel      string
	PublicOrigin  string
	Account       string
	PasswordFile  string
	WebRoot       string
	TLSCert       string
	TLSKey        string
	ACME          *ACMEArgs
	MetricsListen *ListenAddress
	MetricsToken  string
	RateLimits    *RateLimits
	TrustedProxy  *edge.TrustedProxyConfig
	Name          string
	Challenge     *ChallengeOptions
	// TrafficRetentionDays 缺省 90，0 = 不记录。
	TrafficRetentionDays int
}

// DefaultTrafficRetentionDays 是流量统计的缺省保留天数。
const DefaultTrafficRetentionDays = 90

// TrafficOptions 是 personal traffic 的参数。
type TrafficOptions struct {
	Common
	// SinceMs 是从多久以前起（毫秒）。
	SinceMs  int64
	SourceID string
}

// DoctorOptions 是 personal doctor 的参数（收 serve 的同名参数，只看不改）。
type DoctorOptions struct {
	Common
	Listen ListenAddress
	TLS    string
	// TrustedProxy 是受信代理的原文；nil = 没给（查存储里的）。
	TrustedProxy *string
	// PasswordFile 是自动初始化用过的口令文件；"-" 不收。
	PasswordFile string
}
