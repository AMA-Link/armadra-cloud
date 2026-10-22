package edge_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// fakeGate 记下每次准入的入参与名额的归还。
type fakeGate struct {
	mu       sync.Mutex
	refuse   gen.ErrorCode
	deny     gen.ErrorCode
	fail     error
	inputs   []edge.GateInput
	released int
}

func (g *fakeGate) Refuse() gen.ErrorCode { g.mu.Lock(); defer g.mu.Unlock(); return g.refuse }

func (g *fakeGate) Admit(_ context.Context, in edge.GateInput) (func(), gen.ErrorCode, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inputs = append(g.inputs, in)
	if g.fail != nil {
		return nil, "", g.fail
	}
	if g.deny != "" {
		return nil, g.deny, nil
	}
	return func() { g.mu.Lock(); g.released++; g.mu.Unlock() }, "", nil
}

func gateRig(t *testing.T, gate *fakeGate) (*edge.Policy, *relayenv.Env, func(token string) (edge.Decision, error)) {
	t.Helper()
	env := relayenv.New(t, relayenv.Options{Control: control.MemoryOptions{
		Limits: map[control.LimitKind]control.LimitRule{control.LimitEdgeConcurrentAcct: {Concurrent: true, Max: 1}},
	}})
	tun := tunnel.New("tun1", env.SourceID, "0.2.0", nullTransport{}, tunnel.DefaultConfig(), tunnel.Hooks{})
	tun.SetCapabilities([]string{"e2e"})
	t.Cleanup(func() { tun.Close(protocol.CloseNormal, "done") })
	p := edge.NewPolicy(edge.PolicyOptions{
		NodeID: "n", Addressing: edge.HostAddressing("example.test"), Auth: env.Auth, Control: env.Control,
		Lookup:  func(context.Context, string) (*tunnel.Tunnel, error) { return tun, nil },
		Metrics: obs.NewMetrics(nil), Gate: gate,
	})
	decide := func(token string) (edge.Decision, error) {
		return p.DecideUpgrade(context.Background(), edge.Request{
			Method: "GET", URL: "/_relay/e2e", Host: env.SourceID + ".src.example.test", ClientIP: "203.0.113.9",
			Header: http.Header{"Upgrade": {"websocket"}, "Sec-Websocket-Protocol": {"armadra-e2e.1, armadra-relay." + token}},
		})
	}
	return p, env, decide
}

func TestGate(t *testing.T) {
	t.Run("事故开关：验令牌之前就拒（4429）", func(t *testing.T) {
		gate := &fakeGate{refuse: gen.ErrLimitReached}
		_, _, decide := gateRig(t, gate)
		d, err := decide("not-a-token")
		if err != nil || d.Accept || d.Code != gen.ErrLimitReached {
			t.Fatalf("%+v %v", d, err)
		}
		if c, _ := edge.WSCloseCode(d.Code); c != protocol.CloseLimitReached {
			t.Fatal("limit_reached 对应 4429")
		}
		if len(gate.inputs) != 0 {
			t.Fatal("被拒时不该再占名额")
		}
	})

	t.Run("放行时带上令牌里的 team、dev，名额连同账号并发一起还", func(t *testing.T) {
		gate := &fakeGate{}
		p, env, decide := gateRig(t, gate)
		d, err := decide(env.RelayToken(t, relayenv.Claims{"team": "team_1", "dev": "dev_1"}))
		if err != nil || !d.Accept || d.Team != "team_1" || d.Release == nil {
			t.Fatalf("%+v %v", d, err)
		}
		if gate.inputs[0] != (edge.GateInput{SourceID: env.SourceID, Subject: "acct_test", DeviceID: "dev_1", Team: "team_1"}) {
			t.Fatalf("%+v", gate.inputs[0])
		}
		// 账号并发上限 1：没还之前第二条被拒。
		if d2, _ := decide(env.RelayToken(t, nil)); d2.Accept || d2.Code != gen.ErrLimitReached {
			t.Fatalf("%+v", d2)
		}
		p.Release(d)
		if gate.released != 1 {
			t.Fatalf("Gate 的名额要还：%d", gate.released)
		}
		d3, _ := decide(env.RelayToken(t, nil))
		if !d3.Accept {
			t.Fatalf("还了之后能再开：%+v", d3)
		}
		p.ReleaseUpgradeAsync(d3.Subject, d3.Release)
		p.Flush(5 * time.Second)
		if gate.released != 2 {
			t.Fatalf("异步也要还：%d", gate.released)
		}
	})

	t.Run("Gate 拒绝（团队并发满）：还回账号并发，答 4429", func(t *testing.T) {
		gate := &fakeGate{deny: gen.ErrLimitReached}
		_, env, decide := gateRig(t, gate)
		for i := 0; i < 2; i++ {
			d, err := decide(env.RelayToken(t, relayenv.Claims{"team": "team_1"}))
			if err != nil || d.Accept || d.Code != gen.ErrLimitReached {
				t.Fatalf("%+v %v", d, err)
			}
		}
		gate.mu.Lock()
		gate.deny = ""
		gate.mu.Unlock()
		if d, _ := decide(env.RelayToken(t, nil)); !d.Accept {
			t.Fatalf("账号并发应已还回：%+v", d)
		}
	})

	t.Run("Gate 出错：答错误（503），账号并发还回", func(t *testing.T) {
		gate := &fakeGate{fail: errors.New("redis down")}
		_, env, decide := gateRig(t, gate)
		if _, err := decide(env.RelayToken(t, nil)); err == nil {
			t.Fatal("应报错")
		}
		gate.mu.Lock()
		gate.fail = nil
		gate.mu.Unlock()
		if d, _ := decide(env.RelayToken(t, nil)); !d.Accept {
			t.Fatalf("%+v", d)
		}
	})
}

func TestTakeBySubjectAndDevice(t *testing.T) {
	r := newPolicy(t, false)
	a, b, c, d := &handle{"a"}, &handle{"b"}, &handle{"c"}, &handle{"d"}
	r.policy.TrackDevice(a, "s1", "acct_1", "dev_1")
	r.policy.TrackDevice(b, "s1", "acct_1", "dev_2")
	r.policy.TrackDevice(c, "s1", "acct_2", "dev_1")
	r.policy.TrackDevice(d, "s2", "acct_1", "dev_1")
	got := r.policy.Take(edge.ConnectionFilter{SourceID: "s1", Subject: "acct_1", DeviceID: "dev_1"})
	if len(got) != 1 || got[0] != edge.Handle(a) {
		t.Fatalf("%v", got)
	}
	got = r.policy.Take(edge.ConnectionFilter{SourceID: "s1", Subject: "acct_1"})
	if len(got) != 1 || got[0] != edge.Handle(b) {
		t.Fatalf("%v", got)
	}
	// subject 是精确匹配：acct_ 不是 acct_2 的前缀匹配。
	if got := r.policy.Take(edge.ConnectionFilter{Subject: "acct_"}); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := r.policy.Take(edge.ConnectionFilter{SourceID: "s1"}); len(got) != 1 || got[0] != edge.Handle(c) {
		t.Fatalf("%v", got)
	}
}
