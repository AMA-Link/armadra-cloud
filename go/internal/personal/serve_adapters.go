package personal

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

// limiterAdapter 把控制面的限流器接成控制核要的 RateLimiter。
type limiterAdapter struct{ l control.Limiter }

func (a limiterAdapter) Hit(ctx context.Context, kind core.LimitKind, key string) (core.LimitResult, error) {
	r, err := a.l.Hit(ctx, control.LimitKind(kind), key)
	return core.LimitResult{Allowed: r.Allowed, RetryAfter: r.RetryAfter}, err
}

// presenceAdapter 把控制面的在线事件接成控制核的 PresenceBus。
type presenceAdapter struct{ p control.Presence }

func (a presenceAdapter) Subscribe(fn func(core.PresenceEvent)) func() {
	return a.p.Subscribe(func(ev control.PresenceEvent) {
		fn(core.PresenceEvent{
			Online: ev.Type == control.PresenceSourceOnline, SourceID: ev.SourceID,
			CoreVersion: ev.CoreVersion, AtMs: ev.AtMs,
		})
	})
}

// relayAdapter 是控制核对中继的依赖：查在线、关隧道、断开客户端。
type relayAdapter struct {
	routes     control.RouteTable
	command    func(control.NodeCommand)
	disconnect func(edge.ConnectionFilter)
}

func (a relayAdapter) Route(ctx context.Context, sourceID string) (string, bool, error) {
	route, err := a.routes.Get(ctx, sourceID)
	if err != nil || route == nil {
		return "", false, err
	}
	return route.CoreVersion, true, nil
}

func (a relayAdapter) CloseTunnel(sourceID string) {
	a.command(control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: sourceID})
}

func (a relayAdapter) Disconnect(f core.ConnectionFilter) {
	a.disconnect(edge.ConnectionFilter{SourceID: f.SourceID, SubjectPrefix: f.SubjectPrefix})
}

// trafficWriter 把计量器的一批行加进存储（同键相加）。
func trafficWriter(ts store.TrafficStore) traffic.Writer {
	return func(ctx context.Context, rows []traffic.Row) error {
		out := make([]store.TrafficRow, len(rows))
		for i, row := range rows {
			out[i] = store.TrafficRow{
				BucketStartMs: row.BucketStartMs, Node: row.Node, SourceID: row.SourceID,
				Subject: row.Subject, DeviceID: row.DeviceID, Kind: string(row.Kind),
				Connections: row.Connections, BytesToSource: row.BytesToSource, BytesToClient: row.BytesToClient,
			}
		}
		return ts.Upsert(ctx, store.DefaultTenant, out)
	}
}

// storedRateLimits 是租户设置里的限流额度（settings.rateLimits）。
func storedRateLimits(raw json.RawMessage) *RateLimits {
	if len(raw) == 0 {
		return nil
	}
	var in struct {
		Anonymous int `json:"anonymous"`
		Preflight int `json:"preflight"`
		Token     int `json:"token"`
		Account   int `json:"account"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	return &RateLimits{Anonymous: in.Anonymous, Preflight: in.Preflight, Token: in.Token, Account: in.Account}
}

// EdgeLimitRules 是边缘限流：命令行 / 环境变量 > 状态文件 > 缺省；登录等防暴力破解的规则不在其中。
func EdgeLimitRules(sources ...*RateLimits) map[control.LimitKind]control.LimitRule {
	rules := map[control.LimitKind]control.LimitRule{}
	for _, pair := range []struct {
		kind control.LimitKind
		get  func(*RateLimits) int
	}{
		{control.LimitEdgeConnectIP, func(l *RateLimits) int { return l.Anonymous }},
		{control.LimitEdgePreflightIP, func(l *RateLimits) int { return l.Preflight }},
		{control.LimitEdgeConnectToken, func(l *RateLimits) int { return l.Token }},
		{control.LimitEdgeConnectAcct, func(l *RateLimits) int { return l.Account }},
	} {
		for _, source := range sources {
			if source == nil || pair.get(source) == 0 {
				continue
			}
			rules[pair.kind] = control.LimitRule{Max: pair.get(source), Window: time.Minute}
			break
		}
	}
	return rules
}

// storedTrustedProxy 是租户设置里的受信代理（settings.trustedProxy）。
func storedTrustedProxy(raw json.RawMessage) *edge.TrustedProxyConfig {
	if len(raw) == 0 {
		return nil
	}
	var in struct {
		Proxies []string `json:"proxies"`
		Prefer  string   `json:"prefer"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	return &edge.TrustedProxyConfig{Proxies: in.Proxies, Prefer: in.Prefer}
}

// requestIP 把边缘的 ClientIP 取法接成按请求取。
func requestIP(ip edge.ClientIP) func(*http.Request) string {
	return func(r *http.Request) string { return ip(r.RemoteAddr, r.Header) }
}
