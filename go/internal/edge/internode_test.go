package edge_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// recordTransport 记下隧道写出的帧。
type recordTransport struct {
	mu     sync.Mutex
	frames []protocol.Frame
}

func (r *recordTransport) Send(b []byte) error {
	f, err := protocol.Decode(b)
	if err == nil {
		r.mu.Lock()
		r.frames = append(r.frames, f)
		r.mu.Unlock()
	}
	return nil
}
func (*recordTransport) Close(protocol.CloseCode, string) {}
func (*recordTransport) Terminate()                       {}

func (r *recordTransport) opens() []*gen.OpenPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*gen.OpenPayload
	for _, f := range r.frames {
		if f.Type == protocol.FrameOpen {
			out = append(out, f.Open)
		}
	}
	return out
}

func TestInternodeOpenRoundTrip(t *testing.T) {
	const src = "0123456789abcdef0123456789abcdef"
	origin := "https://app.example"
	open := edge.InternodeOpen(src, gen.OpenPayload{Kind: "e2e", Method: "GET", Path: gen.E2EPath, Headers: [][2]string{}, ClientOrigin: &origin, RemoteIP: "203.0.113.1", RelayNode: "b"})
	if open.Kind != "ws" || open.Path != "/s/"+src+gen.E2EPath || len(open.Headers) != 0 || open.RelayNode != "b" || *open.ClientOrigin != origin {
		t.Fatalf("%+v", open)
	}
	// 帧编解码要过（e2e 的 OPEN 不许带头、路径固定，所以节点之间用 ws + 路径寻址）。
	if _, err := protocol.Encode(protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: open}); err != nil {
		t.Fatal(err)
	}
	if got, ok := edge.ParseInternodeOpen(open); !ok || got != src {
		t.Fatalf("%s %v", got, ok)
	}
	for _, bad := range []*gen.OpenPayload{
		nil,
		{Kind: "e2e", Method: "GET", Path: gen.E2EPath},
		{Kind: "ws", Method: "GET", Path: "/s/" + src + "/other"},
		{Kind: "ws", Method: "GET", Path: "/s/nothex/_relay/e2e"},
		{Kind: "ws", Method: "POST", Path: "/s/" + src + gen.E2EPath},
		{Kind: "ws", Method: "GET", Path: "/s/" + src + gen.E2EPath, Headers: [][2]string{{"x", "y"}}},
	} {
		if _, ok := edge.ParseInternodeOpen(bad); ok {
			t.Errorf("应拒绝：%+v", bad)
		}
	}
}

// 在节点间连接上开流：发 InternodeOpen，流量记在目标源名下（入口节点计）。
func TestOpenE2EOnLink(t *testing.T) {
	env := relayenv.New(t, relayenv.Options{})
	tr := &recordTransport{}
	link := tunnel.New("x.1", "", "", tr, tunnel.DefaultConfig(), tunnel.Hooks{})
	link.SetLink("a")
	link.Start()
	t.Cleanup(func() { link.Close(protocol.CloseNormal, "done") })
	var rows []traffic.Row
	var mu sync.Mutex
	meter := traffic.NewMeter(traffic.MeterOptions{Node: "b", Write: func(_ context.Context, r []traffic.Row) error {
		mu.Lock()
		rows = append(rows, r...)
		mu.Unlock()
		return nil
	}})
	p := edge.NewPolicy(edge.PolicyOptions{
		NodeID: "b", Addressing: edge.HostAddressing("example.test"), Auth: env.Auth, Control: env.Control,
		Metrics: obs.NewMetrics(nil), Traffic: meter,
	})
	req := edge.Request{Method: "GET", URL: gen.E2EPath, Host: env.SourceID + ".src.example.test", Header: http.Header{}}
	s, code := p.OpenE2E(link, req, "203.0.113.9", &edge.TrafficIdentity{Subject: "acct_1", Team: "team_1"}, nopSink{})
	if code != "" || s == nil {
		t.Fatalf("开流失败：%s", code)
	}
	eventually(t, "OPEN 写出", func() bool { return len(tr.opens()) == 1 })
	open := tr.opens()[0]
	if got, ok := edge.ParseInternodeOpen(open); !ok || got != env.SourceID || open.RelayNode != "b" || open.RemoteIP != "203.0.113.9" {
		t.Fatalf("%+v", open)
	}
	if err := meter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(rows) != 1 || rows[0].SourceID != env.SourceID || rows[0].TeamID != "team_1" || rows[0].Connections != 1 {
		t.Fatalf("%+v", rows)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
