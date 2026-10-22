package auth_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/auth"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

const otherSource = "fedcba9876543210fedcba9876543210"

var ctx = context.Background()

type env struct {
	issuer, source testutil.Key
	sourceID       string
	control        *control.Memory
	auth           *auth.Context
}

type claims = map[string]any

func newEnv(t *testing.T, nowMs int64) env {
	issuer, source := testutil.Keys(t)
	b64 := base64.RawURLEncoding.EncodeToString
	jwks, _ := json.Marshal(claims{"keys": []claims{{"kty": "OKP", "crv": "Ed25519", "x": b64(issuer.Public), "kid": issuer.Kid}}})
	now := func() time.Time { return time.UnixMilli(nowMs) }
	c := control.NewMemory(control.MemoryOptions{
		Issuer: issuer.Issuer,
		JWKS:   func(context.Context) ([]byte, error) { return jwks, nil },
		Now:    now,
	})
	return env{issuer: issuer, source: source, sourceID: source.Kid, control: c, auth: auth.NewContext(c, now)}
}

func newJTI() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "jti-" + hex.EncodeToString(b)
}

func (e env) sign(t *testing.T, typ string, payload claims, key ed25519.PrivateKey, kid string) string {
	t.Helper()
	tok, err := jose.Sign(key, jose.Header{Kid: kid, Typ: typ}, payload)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func merge(base, over claims) claims {
	for k, v := range over {
		base[k] = v
	}
	return base
}

func (e env) relayToken(t *testing.T, over claims, nowMs int64) string {
	iat := nowMs / 1000
	return e.sign(t, gen.TokenTypeRelay, merge(claims{
		"iss": e.issuer.Issuer, "sub": "acct_test", "aud": "relay", "src": e.sourceID,
		"iat": iat, "exp": iat + 3600, "jti": newJTI(), "ver": 1,
	}, over), e.issuer.Private, e.issuer.Kid)
}

func (e env) tunnelToken(t *testing.T, over claims, nowMs int64) string {
	iat := nowMs / 1000
	return e.sign(t, gen.TokenTypeTunnel, merge(claims{
		"iss": e.issuer.Issuer, "sub": e.sourceID, "aud": "relay-tunnel",
		"cnf": claims{"jwk": claims{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(e.source.Public)}},
		"iat": iat, "exp": iat + 3600, "jti": newJTI(), "ver": 1,
	}, over), e.issuer.Private, e.issuer.Kid)
}

func fixture(t *testing.T, name string) string {
	return strings.TrimSpace(string(testutil.Fixture(t, name)))
}

func TestRelayToken(t *testing.T) {
	t.Run("黄金夹具 relay-token.jwt 通过", func(t *testing.T) {
		e := newEnv(t, testutil.FixedNow)
		res := auth.VerifyRelayToken(ctx, e.auth, fixture(t, "assertion/relay-token.jwt"), e.sourceID)
		if !res.OK || res.Claims.Src != e.sourceID {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("src 不符 → relay_source_mismatch", func(t *testing.T) {
		e := newEnv(t, testutil.FixedNow)
		res := auth.VerifyRelayToken(ctx, e.auth, fixture(t, "assertion/relay-token.jwt"), otherSource)
		if res.OK || res.Error != gen.ErrRelaySourceMismatch || res.Failure != "source_mismatch" {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("令牌坏 → relay_token_invalid", func(t *testing.T) {
		cases := []struct{ name, token, failure string }{
			{"缺令牌", "", "missing"},
			{"乱码", "abc", "malformed"},
			{"断言冒充中继令牌（typ 不对）", fixture(t, "assertion/valid.jwt"), "wrong_type"},
			{"隧道令牌冒充中继令牌", fixture(t, "assertion/tunnel-token.jwt"), "wrong_type"},
			{"头声称 HS256", fixture(t, "assertion/wrong-alg.jwt"), "malformed"},
		}
		e := newEnv(t, testutil.FixedNow)
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				res := auth.VerifyRelayToken(ctx, e.auth, c.token, e.sourceID)
				if res.OK || res.Error != gen.ErrRelayTokenInvalid || res.Failure != c.failure {
					t.Fatalf("%+v", res)
				}
			})
		}
	})

	t.Run("过期、错 aud、错 iss、坏签名、未知 kid、撤销都拒绝", func(t *testing.T) {
		e := newEnv(t, testutil.FixedNow)
		_, stranger, _ := ed25519.GenerateKey(rand.Reader)
		good := e.relayToken(t, nil, testutil.FixedNow)
		cases := []struct{ failure, token string }{
			{"expired", e.relayToken(t, nil, testutil.FixedNow-3*3600_000)},
			// aud 在 schema 里是字面量，错 aud 在声明校验那一步就被拒。
			{"bad_claims", e.relayToken(t, claims{"aud": "relay-tunnel"}, testutil.FixedNow)},
			{"issuer_mismatch", e.relayToken(t, claims{"iss": "https://evil"}, testutil.FixedNow)},
			{"bad_claims", e.relayToken(t, claims{"src": "nothex"}, testutil.FixedNow)},
			{"bad_signature", good[:len(good)-4] + "AAAA"},
			{"unknown_key", e.sign(t, gen.TokenTypeRelay, claims{"iss": e.issuer.Issuer}, stranger, "nope")},
		}
		for _, c := range cases {
			res := auth.VerifyRelayToken(ctx, e.auth, c.token, e.sourceID)
			if res.OK || res.Error != gen.ErrRelayTokenInvalid || res.Failure != c.failure {
				t.Errorf("%s：%+v", c.failure, res)
			}
		}
		_ = e.control.Tokens().Revoke(ctx, "jti-relay-0001", time.UnixMilli(testutil.FixedNow+3600_000))
		res := auth.VerifyRelayToken(ctx, e.auth, fixture(t, "assertion/relay-token.jwt"), e.sourceID)
		if res.OK || res.Failure != "revoked" {
			t.Fatalf("撤销应拒绝：%+v", res)
		}
	})

	t.Run("从子协议里取令牌，其余子协议留下", func(t *testing.T) {
		cases := []struct {
			name, header, token string
			rest                []string
		}{
			{"混合", "armadra-ticket.abc, armadra-relay.x.y.z ,armadra-relay.second", "x.y.z", []string{"armadra-ticket.abc"}},
			{"空头", "", "", []string{}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				token, rest := auth.SplitRelaySubprotocol(c.header)
				if token != c.token || !slices.Equal(rest, c.rest) || rest == nil {
					t.Fatalf("token=%q rest=%#v", token, rest)
				}
			})
		}
	})
}

func TestTunnelToken(t *testing.T) {
	t.Run("黄金夹具 tunnel-token.jwt 通过并取出 cnf 钥", func(t *testing.T) {
		e := newEnv(t, testutil.FixedNow)
		var hs struct {
			Hello struct {
				TunnelToken string `json:"tunnelToken"`
			} `json:"hello"`
		}
		testutil.FixtureJSON(t, "tunnel/handshake.json", &hs)
		res := auth.VerifyTunnelToken(ctx, e.auth, hs.Hello.TunnelToken, e.sourceID)
		if !res.OK {
			t.Fatalf("%+v", res)
		}
		if !res.SourceKey.Equal(e.source.Public) {
			t.Fatal("cnf 钥应是源的公钥")
		}
	})

	t.Run("过期 → tunnel_token_expired；错 sub / 错 aud / 错 typ / 撤销 → tunnel_token_invalid", func(t *testing.T) {
		e := newEnv(t, testutil.FixedNow)
		cases := []struct{ reject, failure, token string }{
			{auth.RejectTunnelTokenExpired, "expired", e.tunnelToken(t, nil, testutil.FixedNow-3*3600_000)},
			{auth.RejectTunnelTokenInvalid, "subject_mismatch", e.tunnelToken(t, claims{"sub": otherSource}, testutil.FixedNow)},
			{auth.RejectTunnelTokenInvalid, "bad_claims", e.tunnelToken(t, claims{"aud": "relay"}, testutil.FixedNow)},
			{auth.RejectTunnelTokenInvalid, "wrong_type", fixture(t, "assertion/relay-token.jwt")},
		}
		for _, c := range cases {
			res := auth.VerifyTunnelToken(ctx, e.auth, c.token, e.sourceID)
			if res.OK || res.Reject != c.reject || res.Failure != c.failure {
				t.Errorf("%s：%+v", c.failure, res)
			}
		}
		_ = e.control.Tokens().Revoke(ctx, "jti-tunnel-0001", time.UnixMilli(testutil.FixedNow+3600_000))
		res := auth.VerifyTunnelToken(ctx, e.auth, fixture(t, "assertion/tunnel-token.jwt"), e.sourceID)
		if res.OK || res.Reject != auth.RejectTunnelTokenInvalid || res.Failure != "revoked" {
			t.Fatalf("撤销应拒绝：%+v", res)
		}
	})

	t.Run("reject 码都登记在协议里", func(t *testing.T) {
		for _, code := range []string{auth.RejectTunnelTokenInvalid, auth.RejectTunnelTokenExpired} {
			if !slices.Contains(gen.RejectCodeValues, code) {
				t.Errorf("%s 未登记", code)
			}
		}
	})

	t.Run("cnf 不是合法的 Ed25519 钥 → bad_cnf 或 bad_claims", func(t *testing.T) {
		e := newEnv(t, testutil.FixedNow)
		short := base64.RawURLEncoding.EncodeToString([]byte("short"))
		for _, jwk := range []claims{
			{"kty": "OKP", "crv": "Ed25519", "x": short},
			{"kty": "OKP", "crv": "Ed25519", "x": "!!"},
		} {
			tok := e.tunnelToken(t, claims{"cnf": claims{"jwk": jwk}}, testutil.FixedNow)
			res := auth.VerifyTunnelToken(ctx, e.auth, tok, e.sourceID)
			if res.OK || res.Reject != auth.RejectTunnelTokenInvalid || (res.Failure != "bad_cnf" && res.Failure != "bad_claims") {
				t.Errorf("%v：%+v", jwk, res)
			}
		}
	})
}
