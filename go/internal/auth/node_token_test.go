package auth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/auth"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

func (e env) nodeToken(t *testing.T, over claims, nowMs int64) string {
	iat := nowMs / 1000
	return e.sign(t, gen.TokenTypeNode, merge(claims{
		"iss": e.issuer.Issuer, "sub": "a", "aud": "relay-node",
		"iat": iat, "exp": iat + 30*86400, "jti": "n.a." + newJTI(), "ver": 1,
	}, over), e.issuer.Private, e.issuer.Kid)
}

func TestNodeToken(t *testing.T) {
	t.Run("黄金夹具 node-token.jwt 通过", func(t *testing.T) {
		e := newEnv(t, testutil.FixedNow)
		res := auth.VerifyNodeToken(ctx, e.auth, fixture(t, "assertion/node-token.jwt"))
		if !res.OK || res.Claims.Aud != gen.TokenAudienceRelayNode || res.Claims.Sub == "" {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("伪造、过期、错类型、错 aud、错 iss、撤销都拒绝", func(t *testing.T) {
		e := newEnv(t, testutil.FixedNow)
		_, stranger, _ := ed25519.GenerateKey(rand.Reader)
		good := e.nodeToken(t, nil, testutil.FixedNow)
		revokedJTI := "n.a." + newJTI()
		if err := e.control.Tokens().Revoke(ctx, revokedJTI, time.UnixMilli(testutil.FixedNow).Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		cases := []struct{ name, failure, token string }{
			{"缺令牌", "missing", ""},
			{"过期", "expired", e.nodeToken(t, claims{"exp": testutil.FixedNow/1000 - 7200}, testutil.FixedNow-3*3600_000)},
			{"中继令牌冒充节点令牌", "wrong_type", e.relayToken(t, nil, testutil.FixedNow)},
			{"错 aud", "bad_claims", e.nodeToken(t, claims{"aud": "relay"}, testutil.FixedNow)},
			{"节点名不合格式", "bad_claims", e.nodeToken(t, claims{"sub": "Bad Name"}, testutil.FixedNow)},
			{"错 iss", "issuer_mismatch", e.nodeToken(t, claims{"iss": "https://evil"}, testutil.FixedNow)},
			{"坏签名", "bad_signature", good[:len(good)-4] + "AAAA"},
			{"别人的钥签的", "unknown_key", e.sign(t, gen.TokenTypeNode, claims{"iss": e.issuer.Issuer}, stranger, "nope")},
			{"已撤销", "revoked", e.nodeToken(t, claims{"jti": revokedJTI}, testutil.FixedNow)},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				res := auth.VerifyNodeToken(ctx, e.auth, c.token)
				if res.OK || res.Failure != c.failure {
					t.Fatalf("%+v", res)
				}
			})
		}
	})
}
