package tunnel_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 对应 packages/relay-kernel/src/tunnel/handshake.test.ts（黄金夹具 handshake.json）。

type handshakeFixture struct {
	Hello     json.RawMessage `json:"hello"`
	Challenge gen.Challenge   `json:"challenge"`
	Auth      json.RawMessage `json:"auth"`
	Reject    json.RawMessage `json:"reject"`
}

type machine struct {
	hs  *tunnel.Handshake
	env *relayenv.Env
	fx  handshakeFixture
}

func newMachine(t *testing.T, limits control.Limiter, nonce string) machine {
	t.Helper()
	var fx handshakeFixture
	testutil.FixtureJSON(t, "tunnel/handshake.json", &fx)
	env := relayenv.New(t, relayenv.Options{FixedNowMs: testutil.FixedNow})
	if limits == nil {
		limits = env.Control.Limits()
	}
	if nonce == "" {
		nonce = fx.Challenge.Nonce2
	}
	hs := tunnel.NewHandshake(tunnel.HandshakeDeps{
		Auth: env.Auth, Limits: limits, RelayNode: fx.Challenge.RelayNode,
		Nonce: func() string { return nonce },
	})
	return machine{hs: hs, env: env, fx: fx}
}

func (m machine) step(t *testing.T, msg []byte) tunnel.Step {
	t.Helper()
	s, err := m.hs.OnText(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func patchHello(t *testing.T, raw json.RawMessage, patch map[string]any) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range patch {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

type denyLimits struct{ control.Limiter }

func (denyLimits) Hit(context.Context, control.LimitKind, string) (control.LimitResult, error) {
	return control.LimitResult{Allowed: false}, nil
}

func TestHandshake(t *testing.T) {
	t.Run("hello → challenge → auth → ready", func(t *testing.T) {
		m := newMachine(t, nil, "")
		ch := m.step(t, m.fx.Hello)
		if ch.Kind != tunnel.StepSend || ch.Challenge != m.fx.Challenge {
			t.Fatalf("%+v", ch)
		}
		ready := m.step(t, m.fx.Auth)
		if ready.Kind != tunnel.StepReady || ready.Hello.SourceID != m.env.SourceID || ready.Hello.CoreVersion != "0.2.0" {
			t.Fatalf("%+v", ready)
		}
		if m.step(t, m.fx.Auth).Kind != tunnel.StepFail {
			t.Fatal("完成之后再来消息应直接失败")
		}
	})

	t.Run("reject 消息与夹具相同", func(t *testing.T) {
		step := tunnel.RejectStep("tunnel_token_expired")
		b, _ := json.Marshal(step.Reject)
		var got, want map[string]any
		_ = json.Unmarshal(b, &got)
		_ = json.Unmarshal(testutilReject(t), &want)
		if len(got) != len(want) || got["code"] != want["code"] || got["message"] != want["message"] || got["type"] != want["type"] {
			t.Fatalf("%s != %s", b, testutilReject(t))
		}
		if step.CloseCode != protocol.CloseTunnelAuthFailed {
			t.Fatalf("%d", step.CloseCode)
		}
	})

	t.Run("坏 major → reject protocol_unsupported，4409", func(t *testing.T) {
		m := newMachine(t, nil, "")
		step := m.step(t, patchHello(t, m.fx.Hello, map[string]any{"protocol": map[string]any{"major": 2, "minor": 0}}))
		if step.Kind != tunnel.StepReject || step.Reject.Code != "protocol_unsupported" || step.CloseCode != protocol.CloseProtocolUnsupported {
			t.Fatalf("%+v", step)
		}
	})

	tokenCases := []struct {
		name   string
		patch  func(t *testing.T, m machine) map[string]any
		reject string
	}{
		{"过期", func(t *testing.T, m machine) map[string]any {
			return map[string]any{"tunnelToken": m.env.TunnelTokenAt(t, nil, m.env.Now().Add(-3*3600*1e9))}
		}, "tunnel_token_expired"},
		{"错 aud", func(t *testing.T, m machine) map[string]any {
			return map[string]any{"tunnelToken": m.env.TunnelToken(t, relayenv.Claims{"aud": "relay"})}
		}, "tunnel_token_invalid"},
		{"错 sub（令牌是别的源的）", func(*testing.T, machine) map[string]any {
			return map[string]any{"sourceId": "fedcba9876543210fedcba9876543210"}
		}, "tunnel_token_invalid"},
	}
	for _, c := range tokenCases {
		t.Run("令牌"+c.name+" → reject，4490", func(t *testing.T) {
			m := newMachine(t, nil, "")
			step := m.step(t, patchHello(t, m.fx.Hello, c.patch(t, m)))
			if step.Kind != tunnel.StepReject || step.Reject.Code != c.reject || step.CloseCode != protocol.CloseTunnelAuthFailed {
				t.Fatalf("%+v", step)
			}
		})
	}

	t.Run("令牌已撤销 → tunnel_token_invalid", func(t *testing.T) {
		m := newMachine(t, nil, "")
		if err := m.env.Control.Tokens().Revoke(context.Background(), "jti-tunnel-0001", m.env.Now().Add(3600e9)); err != nil {
			t.Fatal(err)
		}
		step := m.step(t, m.fx.Hello)
		if step.Kind != tunnel.StepReject || step.Reject.Code != "tunnel_token_invalid" {
			t.Fatalf("%+v", step)
		}
	})

	t.Run("签名错 → reject signature_invalid，4490", func(t *testing.T) {
		m := newMachine(t, nil, "AAAAAAAAAAAAAAAAAAAAAA")
		m.step(t, m.fx.Hello)
		step := m.step(t, m.fx.Auth)
		if step.Kind != tunnel.StepReject || step.Reject.Code != "signature_invalid" || step.CloseCode != protocol.CloseTunnelAuthFailed {
			t.Fatalf("%+v", step)
		}
	})

	t.Run("用源私钥对新 nonce2 签名同样通过", func(t *testing.T) {
		const nonce2 = "AAAAAAAAAAAAAAAAAAAAAA"
		m := newMachine(t, nil, nonce2)
		m.step(t, m.fx.Hello)
		var hello gen.Hello
		_ = json.Unmarshal(m.fx.Hello, &hello)
		sig := ed25519.Sign(m.env.Source.Private, protocol.SigningInput(m.env.SourceID, hello.Nonce, nonce2, m.fx.Challenge.RelayNode))
		auth, _ := json.Marshal(gen.Auth{Type: "auth", Sig: base64.RawURLEncoding.EncodeToString(sig)})
		if m.step(t, auth).Kind != tunnel.StepReady {
			t.Fatal("应通过")
		}
	})

	t.Run("按 sourceId 限流 → reject rate_limited，4429", func(t *testing.T) {
		m := newMachine(t, denyLimits{}, "")
		step := m.step(t, m.fx.Hello)
		if step.Kind != tunnel.StepReject || step.Reject.Code != "rate_limited" || step.CloseCode != protocol.CloseLimitReached {
			t.Fatalf("%+v", step)
		}
	})

	for _, c := range []struct {
		name string
		msg  func(m machine) []byte
	}{
		{"不是 JSON", func(machine) []byte { return []byte("{") }},
		{"不符 schema", func(machine) []byte { return []byte(`{"type":"hello"}`) }},
		{"先发 auth", func(m machine) []byte { return m.fx.Auth }},
	} {
		t.Run(c.name+" → 直接关 4400", func(t *testing.T) {
			m := newMachine(t, nil, "")
			step := m.step(t, c.msg(m))
			if step.Kind != tunnel.StepFail || step.CloseCode != protocol.CloseBadFrame {
				t.Fatalf("%+v", step)
			}
		})
	}

	t.Run("challenge 之后发的不是 auth → 4400", func(t *testing.T) {
		m := newMachine(t, nil, "")
		m.step(t, m.fx.Hello)
		step := m.step(t, m.fx.Hello)
		if step.Kind != tunnel.StepFail || step.CloseCode != protocol.CloseBadFrame {
			t.Fatalf("%+v", step)
		}
	})
}

func testutilReject(t *testing.T) []byte {
	t.Helper()
	var fx handshakeFixture
	testutil.FixtureJSON(t, "tunnel/handshake.json", &fx)
	return fx.Reject
}
