package edge

import "github.com/AMA-Link/armadra-cloud/go/internal/clientip"

// 客户端 IP 的取法在 internal/clientip（控制面也用它）；这里是别名，保持边缘与装配方的写法不变。
type (
	// ClientIP 见 clientip.ClientIP。
	ClientIP = clientip.ClientIP
	// TrustedProxyConfig 见 clientip.TrustedProxyConfig。
	TrustedProxyConfig = clientip.TrustedProxyConfig
)

// NormalizeIP 见 clientip.NormalizeIP。
func NormalizeIP(text string) (string, bool) { return clientip.NormalizeIP(text) }

// ParseProxyList 见 clientip.ParseProxyList。
func ParseProxyList(text string) []string { return clientip.ParseProxyList(text) }

// NewClientIP 见 clientip.NewClientIP。
func NewClientIP(cfg *TrustedProxyConfig) (ClientIP, error) { return clientip.NewClientIP(cfg) }
