package edge_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 对应 packages/relay-kernel/src/edge/policy.test.ts。

type nullTransport struct{}

func (nullTransport) Send([]byte) error                { return nil }
func (nullTransport) Close(protocol.CloseCode, string) {}
func (nullTransport) Terminate()                       {}

type nopSink struct{}

func (nopSink) Data(*tunnel.Stream, []byte)            {}
func (nopSink) End(*tunnel.Stream)                     {}
func (nopSink) Reset(*tunnel.Stream, protocol.RstCode) {}
func (nopSink) Resume(*tunnel.Stream)                  {}

type policyRig struct {
	env     *relayenv.Env
	policy  *edge.Policy
	metrics *obs.Metrics
	tun     *tunnel.Tunnel
}

func newPolicy(t *testing.T, withTunnel bool) *policyRig {
	t.Helper()
	env := relayenv.New(t, relayenv.Options{})
	r := &policyRig{env: env, metrics: obs.NewMetrics(func() (int, int) { return 0, 0 })}
	if withTunnel {
		r.tun = tunnel.New("tun1", env.SourceID, "0.2.0", nullTransport{}, tunnel.DefaultConfig(), tunnel.Hooks{})
		t.Cleanup(func() { r.tun.Close(protocol.CloseNormal, "done") })
	}
	r.policy = edge.NewPolicy(edge.PolicyOptions{
		NodeID: "relay-1", Addressing: edge.PathAddressing(), Auth: env.Auth, Control: env.Control,
		Lookup:  func(context.Context, string) (*tunnel.Tunnel, error) { return r.tun, nil },
		Metrics: r.metrics, CORSOrigins: func(string) []string { return []string{"https://app.example"} },
	})
	return r
}

func (r *policyRig) request(over func(*edge.Request)) edge.Request {
	req := edge.Request{
		Method: "GET", URL: "/s/" + r.env.SourceID + "/api/x", Host: "relay",
		Header: http.Header{}, ClientIP: "203.0.113.7",
	}
	if over != nil {
		over(&req)
	}
	return req
}

func TestPolicy(t *testing.T) {
	t.Run("源地址上的 HTTP（含预检、带令牌的）一律 404，不验令牌、不进隧道；名单内来源带 CORS 头", func(t *testing.T) {
		r := newPolicy(t, true)
		overrides := []func(*edge.Request){
			func(q *edge.Request) { q.Header.Set("Origin", "https://app.example") },
			func(q *edge.Request) {
				q.Header.Set("Origin", "https://app.example")
				q.Header.Set("Armadra-Relay-Token", r.env.RelayToken(t, nil))
			},
			func(q *edge.Request) {
				q.Method = "OPTIONS"
				q.Header.Set("Origin", "https://app.example")
				q.Header.Set("Access-Control-Request-Method", "POST")
			},
			func(q *edge.Request) { q.URL = "/s/" + r.env.SourceID + "/_relay/m/ticket" },
			func(q *edge.Request) { q.Method = "POST"; q.URL = "/s/" + r.env.SourceID + "/_relay/media-tickets" },
		}
		for i, over := range overrides {
			d := r.policy.DecideRequest(r.request(over))
			if d.Code != gen.ErrNotFound || !d.Drain {
				t.Fatalf("第 %d 种：%+v", i, d)
			}
		}
		d := r.policy.DecideRequest(r.request(func(q *edge.Request) { q.Header.Set("Origin", "https://app.example") }))
		if d.CORS["access-control-allow-origin"] != "https://app.example" {
			t.Fatalf("%+v", d.CORS)
		}
		if r.tun.StreamCount() != 0 {
			t.Fatal("不该开流")
		}
		if !strings.Contains(r.metrics.Render(), `result="not_found"`) {
			t.Fatal("指标里缺 not_found")
		}
	})

	t.Run("WS 拒绝的关闭码", func(t *testing.T) {
		for code, want := range map[gen.ErrorCode]protocol.CloseCode{
			gen.ErrRelayTokenInvalid: protocol.CloseUnauthenticated,
			gen.ErrSourceOffline:     protocol.CloseSourceOffline,
		} {
			if got, ok := edge.WSCloseCode(code); !ok || got != want {
				t.Fatalf("%s：%d %v", code, got, ok)
			}
		}
		if _, ok := edge.WSCloseCode(gen.ErrNotFound); ok {
			t.Fatal("not_found 没有对应关闭码")
		}
	})

	t.Run("_relay/e2e：只认 WS 升级，普通 HTTP 答 404（不进隧道）", func(t *testing.T) {
		r := newPolicy(t, false)
		for path, want := range map[string]bool{"/_relay/e2e": true, "/_relay/e2e?v=1": true, "/_relay/e2ex": false} {
			if edge.IsE2EPath(path) != want {
				t.Fatalf("%s 应为 %v", path, want)
			}
		}
		d := r.policy.DecideRequest(r.request(func(q *edge.Request) { q.URL = "/s/" + r.env.SourceID + "/_relay/e2e" }))
		if d.Code != gen.ErrNotFound || !d.Drain {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("_relay/e2e 升级：令牌先验；缺版本子协议 4409；源没报 e2e 4404（原因 e2e_unsupported）；报了才接受，带 dev", func(t *testing.T) {
		r := newPolicy(t, true)
		upgrade := func(protocols string) edge.Decision {
			d, err := r.policy.DecideUpgrade(context.Background(), r.request(func(q *edge.Request) {
				q.URL = "/s/" + r.env.SourceID + "/_relay/e2e"
				q.Header.Set("Upgrade", "websocket")
				q.Header.Set("Sec-WebSocket-Protocol", protocols)
			}))
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
		token := r.env.RelayToken(t, relayenv.Claims{"dev": "dev_1"})
		if d := upgrade("armadra-e2e.1"); d.Accept || d.Code != gen.ErrRelayTokenInvalid {
			t.Fatalf("无令牌：%+v", d)
		}
		if d := upgrade("armadra-relay." + token); d.Accept || d.Code != gen.ErrProtocolUnsupported {
			t.Fatalf("缺版本子协议：%+v", d)
		}
		if c, _ := edge.WSCloseCode(gen.ErrProtocolUnsupported); c != protocol.CloseProtocolUnsupported {
			t.Fatal("protocol_unsupported 应对应 4409")
		}
		d := upgrade("armadra-e2e.1, armadra-relay." + token)
		if d.Accept || d.Code != gen.ErrSourceOffline || d.SourceID != r.env.SourceID || d.Reason != "e2e_unsupported" {
			t.Fatalf("源没报 e2e：%+v", d)
		}
		r.tun.SetCapabilities([]string{"e2e"})
		d = upgrade("armadra-e2e.1, armadra-relay." + token)
		if !d.Accept || d.Subject != "acct_test" || len(d.Subprotocols) != 1 || d.Subprotocols[0] != "armadra-e2e.1" || d.Device != "dev_1" {
			t.Fatalf("应接受：%+v", d)
		}
		req := r.request(func(q *edge.Request) { q.Header.Set("Origin", "https://app.example") })
		s, code := r.policy.OpenE2E(r.tun, req, "203.0.113.7", nil, nopSink{})
		if code != "" || s == nil {
			t.Fatalf("开流失败：%s", code)
		}
		if !strings.Contains(r.metrics.Render(), `relay_edge_streams_total{kind="e2e"} 1`) {
			t.Fatal("指标里缺 kind=e2e")
		}
		r.policy.ReleaseUpgrade(d.Subject)
	})

	t.Run("别的路径上的 WS 升级（明文 WS）：404，不升级、不验令牌", func(t *testing.T) {
		r := newPolicy(t, true)
		r.tun.SetCapabilities([]string{"e2e"})
		d, err := r.policy.DecideUpgrade(context.Background(), r.request(func(q *edge.Request) {
			q.URL = "/s/" + r.env.SourceID + "/api/ws"
			q.Header.Set("Upgrade", "websocket")
			q.Header.Set("Sec-WebSocket-Protocol", "armadra-ticket.t, armadra-relay."+r.env.RelayToken(t, nil))
		}))
		if err != nil || d.Accept || d.Code != gen.ErrNotFound || d.SourceID != r.env.SourceID {
			t.Fatalf("%+v %v", d, err)
		}
	})

	t.Run("已升级连接：按源 / 账号前缀摘出", func(t *testing.T) {
		r := newPolicy(t, false)
		a, b, c := &handle{"a"}, &handle{"b"}, &handle{"c"}
		r.policy.Track(a, "s1", "acct_1")
		r.policy.Track(b, "s1", "guest:l1:x")
		r.policy.Track(c, "s2", "guest:l1:y")
		got := r.policy.Take(edge.ConnectionFilter{SourceID: "s1", SubjectPrefix: "guest:l1:"})
		if len(got) != 1 || got[0] != edge.Handle(b) {
			t.Fatalf("%v", got)
		}
		got = r.policy.Take(edge.ConnectionFilter{SubjectPrefix: "guest:"})
		if len(got) != 1 || got[0] != edge.Handle(c) {
			t.Fatalf("%v", got)
		}
		r.policy.Untrack(a)
		if all := r.policy.TakeAll(); len(all) != 0 {
			t.Fatalf("%v", all)
		}
	})

	t.Run("saas 寻址：Host 取自请求的 Host 字段", func(t *testing.T) {
		env := relayenv.New(t, relayenv.Options{})
		p := edge.NewPolicy(edge.PolicyOptions{
			NodeID: "n", Addressing: edge.HostAddressing("relay.example"), Auth: env.Auth, Control: env.Control,
			Metrics: obs.NewMetrics(func() (int, int) { return 0, 0 }),
		})
		got, ok := p.Resolve(edge.Request{URL: "/x", Host: env.SourceID + ".src.relay.example:8443", Header: http.Header{}})
		if !ok || got.SourceID != env.SourceID || got.Path != "/x" {
			t.Fatalf("%+v %v", got, ok)
		}
	})
}

type handle struct{ name string }

func (*handle) Disconnect() {}
