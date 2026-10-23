// Package saas 是 `armadra-relay saas serve`：SaaS 中继节点（Go 设计 §3.6，SaaS 实施设计 §2.4、§9.2）。
//
//   - 寻址：`<sourceId>.src.<RELAY_DOMAIN>`（Host 转小写、去端口，路径原样），只转端到端通道；
//   - 控制面：Redis（键与频道前缀 relay:），路由、在线事件、撤销、命令与控制面共用；另订阅本节点的
//     relay:node:<id>，收 quota / incident 广播；
//   - 节点表：每 20 秒写一次 relay:nodes（HPEXPIRE 60 秒），排空时 draining；
//   - 准入：事故开关、团队速率（edge.connect.team）与跨节点并发（edge.concurrent.team）；
//   - kick 只踢客户端连接（按源、人、设备、访客前缀），隧道保留；closeTunnel 关隧道；drain 排空；
//   - 下线（SIGTERM）与 drain：隧道 GOAWAY rebalance，源按 heartbeatHint 重连到别的节点；
//   - 流量只在入口节点计，带 team，经只有流量表权限的角色写 PostgreSQL（可选），节点不清理保留期；
//   - 共享 JWKS：缓存在 relay:jwks:<sha256(issuer)>，只有一个节点回源。
//
// TLS 在前面的反向代理终止：明文监听，客户端 IP 按 RELAY_TRUSTED_PROXY 读代理头。
package saas

import (
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 退出码与 personal 相同：0 成功，64 用法或配置错误，1 运行出错。
const (
	exitOK    = 0
	exitUsage = 64
	exitFail  = 1
)

// Config 是 saas serve 的配置（全部来自 RELAY_* 环境变量，见 deploy/README.md）。
type Config struct {
	Host, NodeID, Domain, CloudIssuer, RedisURL string
	Port                                        int
	AllowInsecureTunnel                         bool
	LogLevel                                    obs.Level

	// TunnelURL 是本节点自己的隧道地址（写进节点表，源按 heartbeatHint 连它）。
	TunnelURL string
	// InternodeURL 是别的节点转发通道时连的 /x/v1；缺省由 TunnelURL 把路径换成 /x/v1。
	InternodeURL string
	// InternodeCheckEvery 是已建立的 /x/v1 连接多久复查一次对端节点令牌有没有被撤销；0 取 5 秒。字段只为测试能缩短，
	// 不走环境变量（原先是包级变量，测试改它与别的节点的 watch 协程读它之间没有同步）。
	InternodeCheckEvery time.Duration
	// Weight 是节点表里的权重（rendezvous 加权），缺省 1。
	Weight int
	// NodeToken 是本节点的节点令牌（armadra-cloud nodes token 签的）；转发通道时出示。空串表示不转发。
	NodeToken string
	// JWKSURL 缺省 <issuer>/.well-known/jwks.json。
	JWKSURL string
	// DatabaseURL 是流量统计的 PostgreSQL（只有 relay_traffic_buckets 的写权限）；空串表示不计流量。
	DatabaseURL string
	// MetricsListen 与 MetricsToken 同 personal 的 --metrics-listen / --metrics-token-file。
	MetricsListen string
	MetricsToken  string
	// TrustedProxy 是受信反向代理（读 X-Forwarded-For / X-Real-IP）。
	TrustedProxy *edge.TrustedProxyConfig
	// DrainGrace 是排空（GOAWAY rebalance）的宽限，缺省协议值 30 秒。
	DrainGrace time.Duration

	// TeamConcurrent 与 TeamConnectPerMinute 是团队额度键缺失（或坏了）时节点用的缺省，取最严格的套餐
	// （Free：并发 10）；控制面写的 relay:quota:team:* 覆盖并发上限。
	TeamConcurrent       int
	TeamConnectPerMinute int

	// RedisPrefix 是键与频道的前缀，缺省 relay:（只给测试改）。
	RedisPrefix string
}

var (
	listenPattern = regexp.MustCompile(`^(?:\[([^\]]+)\]|([^:]+)):(\d{1,5})$`)
	// nodePattern 与契约的 relayNodeIdSchema 同一条（节点令牌的 sub、节点表的字段、命令频道的 id）。
	nodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	nodeUnsafe  = regexp.MustCompile(`[^a-z0-9-]`)
)

// readFile 读 *_FILE 变量指向的文件（秘密），测试替换。
var readFile = os.ReadFile

// LoadConfig 解析环境变量；空串当没设。有问题时答全部问题（「变量名：说明」，不回显取值）。
func LoadConfig(env map[string]string, hostname func() (string, error)) (Config, []string) {
	get := func(key string) (string, bool) {
		v := env[key]
		return v, strings.TrimSpace(v) != ""
	}
	var issues []string
	bad := func(key, why string) { issues = append(issues, key+"："+why) }
	cfg := Config{
		Host: "0.0.0.0", Port: 8443, LogLevel: obs.LevelInfo, Weight: 1,
		TeamConcurrent: DefaultTeamConcurrent, TeamConnectPerMinute: DefaultTeamConnectPerMinute,
		DrainGrace: time.Duration(protocol.GoawayGraceMs) * time.Millisecond,
	}
	if v, ok := get("RELAY_LISTEN"); ok {
		m := listenPattern.FindStringSubmatch(v)
		port := -1
		if m != nil {
			port, _ = strconv.Atoi(m[3])
		}
		if m == nil || port < 0 || port > 65535 {
			bad("RELAY_LISTEN", "不是合法的 host:port")
		} else {
			cfg.Host, cfg.Port = m[1]+m[2], port
		}
	}
	if v, ok := get("RELAY_NODE_ID"); ok {
		if !nodePattern.MatchString(v) {
			bad("RELAY_NODE_ID", "只能是小写字母、数字与 -，以字母或数字开头，1–63 个字符")
		}
		cfg.NodeID = v
	} else {
		cfg.NodeID = NodeIDFromHost(hostname)
		if !nodePattern.MatchString(cfg.NodeID) {
			bad("RELAY_NODE_ID", "没设，主机名也换不出合法的节点名，请显式设置")
		}
	}
	if v, ok := get("RELAY_DOMAIN"); ok {
		cfg.Domain = strings.ToLower(v)
	} else {
		bad("RELAY_DOMAIN", "必填")
	}
	for _, item := range []struct {
		key  string
		into *string
	}{{"RELAY_CLOUD_ISSUER", &cfg.CloudIssuer}, {"RELAY_REDIS_URL", &cfg.RedisURL}} {
		v, ok := get(item.key)
		switch {
		case !ok:
			bad(item.key, "必填")
		case !isURL(v):
			bad(item.key, "不是合法的 URL")
		default:
			*item.into = v
		}
	}
	if v, ok := get("RELAY_ALLOW_INSECURE_TUNNEL"); ok {
		switch v {
		case "1", "true":
			cfg.AllowInsecureTunnel = true
		case "0", "false":
		default:
			bad("RELAY_ALLOW_INSECURE_TUNNEL", "只能是 0、1、true 或 false")
		}
	}
	if v, ok := get("RELAY_LOG_LEVEL"); ok {
		levels := map[string]obs.Level{"debug": obs.LevelDebug, "info": obs.LevelInfo, "warn": obs.LevelWarn, "error": obs.LevelError}
		if level, known := levels[v]; known {
			cfg.LogLevel = level
		} else {
			bad("RELAY_LOG_LEVEL", "只能是 debug、info、warn 或 error")
		}
	}
	// 节点自己的隧道地址：写进节点表，wss（RELAY_ALLOW_INSECURE_TUNNEL 时也许 ws，开发用）。
	const wsWhy = "要是 wss:// 地址（RELAY_ALLOW_INSECURE_TUNNEL=1 时也可以是 ws://）"
	if v, ok := get("RELAY_TUNNEL_URL"); !ok {
		bad("RELAY_TUNNEL_URL", "必填")
	} else if !wsURL(v, cfg.AllowInsecureTunnel) {
		bad("RELAY_TUNNEL_URL", wsWhy)
	} else {
		cfg.TunnelURL = v
	}
	if v, ok := get("RELAY_INTERNODE_URL"); ok {
		if !wsURL(v, cfg.AllowInsecureTunnel) {
			bad("RELAY_INTERNODE_URL", wsWhy)
		} else {
			cfg.InternodeURL = v
		}
	} else if cfg.TunnelURL != "" {
		u, _ := url.Parse(cfg.TunnelURL)
		u.Path, u.RawQuery = gen.PathInternode, ""
		cfg.InternodeURL = u.String()
	}
	if v, ok := get("RELAY_NODE_WEIGHT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			bad("RELAY_NODE_WEIGHT", "要是 1–1000 的整数")
		} else {
			cfg.Weight = n
		}
	}
	secret := func(key string) string {
		value, has := get(key)
		file, hasFile := get(key + "_FILE")
		switch {
		case has && hasFile:
			bad(key, "与 "+key+"_FILE 只能给一个")
			return ""
		case hasFile:
			b, err := readFile(file)
			if err != nil {
				bad(key+"_FILE", "读不出来")
				return ""
			}
			return strings.TrimSpace(string(b))
		}
		return strings.TrimSpace(value)
	}
	cfg.NodeToken = secret("RELAY_NODE_TOKEN")
	if v := secret("RELAY_DATABASE_URL"); v != "" {
		if !isURL(v) {
			bad("RELAY_DATABASE_URL", "不是合法的 URL")
		} else {
			cfg.DatabaseURL = v
		}
	}
	cfg.MetricsToken = secret("RELAY_METRICS_TOKEN")
	if v, ok := get("RELAY_METRICS_LISTEN"); ok {
		if listenPattern.FindStringSubmatch(v) == nil {
			bad("RELAY_METRICS_LISTEN", "不是合法的 host:port")
		} else {
			cfg.MetricsListen = v
		}
	}
	if v, ok := get("RELAY_JWKS_URL"); ok {
		if !isURL(v) {
			bad("RELAY_JWKS_URL", "不是合法的 URL")
		} else {
			cfg.JWKSURL = v
		}
	} else if cfg.CloudIssuer != "" {
		cfg.JWKSURL = strings.TrimRight(cfg.CloudIssuer, "/") + gen.PathJwksWellKnown
	}
	if v, ok := get("RELAY_TRUSTED_PROXY"); ok {
		var list []string
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				list = append(list, part)
			}
		}
		prefer, _ := get("RELAY_TRUSTED_PROXY_PREFER")
		tp := &edge.TrustedProxyConfig{Proxies: list, Prefer: prefer}
		if _, err := edge.NewClientIP(tp); err != nil {
			bad("RELAY_TRUSTED_PROXY", "不是合法的 IP / CIDR 列表")
		} else {
			cfg.TrustedProxy = tp
		}
	}
	for _, item := range []struct {
		key  string
		into *int
	}{{"RELAY_TEAM_CONCURRENT", &cfg.TeamConcurrent}, {"RELAY_TEAM_CONNECT_PER_MINUTE", &cfg.TeamConnectPerMinute}} {
		if v, ok := get(item.key); ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 1_000_000 {
				bad(item.key, "要是 1–1000000 的整数")
			} else {
				*item.into = n
			}
		}
	}
	if v, ok := get("RELAY_DRAIN_GRACE_MS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 600_000 {
			bad("RELAY_DRAIN_GRACE_MS", "要是 0–600000 的整数")
		} else {
			cfg.DrainGrace = time.Duration(n) * time.Millisecond
		}
	}
	return cfg, issues
}

// NodeIDFromHost 把主机名换成节点名：转小写、[^a-z0-9-] 换成 -、去掉开头的 -、截到 63 个字符。
func NodeIDFromHost(hostname func() (string, error)) string {
	name, _ := hostname()
	name = nodeUnsafe.ReplaceAllString(strings.ToLower(name), "-")
	name = strings.TrimLeft(name, "-")
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func isURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && u.Scheme != "" && (u.Host != "" || u.Opaque != "")
}

func wsURL(v string, insecure bool) bool {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "wss" || (insecure && u.Scheme == "ws")
}
