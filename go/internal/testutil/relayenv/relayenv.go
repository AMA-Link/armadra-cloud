// Package relayenv 给数据面测试搭环境：测试钥、令牌签发、认测试签发者的内存控制面，
// 以及一个走真连接的假 core（源）。只在 _test.go 里用。
package relayenv

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/auth"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

// Claims 是令牌声明的覆盖项。
type Claims = map[string]any

// Env 是一套测试环境。
type Env struct {
	Issuer, Source testutil.Key
	SourceID       string
	Control        *control.Memory
	Auth           *auth.Context
	// Now 是环境的时钟，可用 SetNow 调整；缺省是真实时间。
	nowMs atomic.Int64
	fixed atomic.Bool
}

// Options 配置环境。
type Options struct {
	// FixedNowMs 非零时用固定时钟（夹具令牌用 testutil.FixedNow）。
	FixedNowMs int64
	Control    control.MemoryOptions
}

// New 建环境。
func New(t testing.TB, o Options) *Env {
	t.Helper()
	issuer, source := testutil.Keys(t)
	e := &Env{Issuer: issuer, Source: source, SourceID: source.Kid}
	if o.FixedNowMs != 0 {
		e.nowMs.Store(o.FixedNowMs)
		e.fixed.Store(true)
	}
	jwks, err := json.Marshal(Claims{"keys": []Claims{{
		"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(issuer.Public), "kid": issuer.Kid,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	mo := o.Control
	mo.Issuer = issuer.Issuer
	mo.JWKS = func(context.Context) ([]byte, error) { return jwks, nil }
	if mo.Now == nil {
		mo.Now = e.Now
	}
	e.Control = control.NewMemory(mo)
	e.Auth = auth.NewContext(e.Control, e.Now)
	return e
}

// Now 是环境的当前时刻。
func (e *Env) Now() time.Time {
	if e.fixed.Load() {
		return time.UnixMilli(e.nowMs.Load())
	}
	return time.Now()
}

// SetNow 固定时钟到某个时刻。
func (e *Env) SetNow(t time.Time) {
	e.nowMs.Store(t.UnixMilli())
	e.fixed.Store(true)
}

// Sign 用签发者的钥签一个令牌。
func (e *Env) Sign(t testing.TB, typ string, payload Claims) string {
	t.Helper()
	return SignWith(t, e.Issuer.Private, e.Issuer.Kid, typ, payload)
}

// SignWith 用指定钥签。
func SignWith(t testing.TB, key ed25519.PrivateKey, kid, typ string, payload Claims) string {
	t.Helper()
	tok, err := jose.Sign(key, jose.Header{Kid: kid, Typ: typ}, payload)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func merge(base, over Claims) Claims {
	for k, v := range over {
		if v == nil {
			delete(base, k)
		} else {
			base[k] = v
		}
	}
	return base
}

// NewJTI 返回随机的令牌 ID。
func NewJTI() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "jti-" + hex.EncodeToString(b)
}

// TunnelToken 签一枚隧道令牌（cnf 是测试源的钥）；over 里值为 nil 的键会被删掉。
func (e *Env) TunnelToken(t testing.TB, over Claims) string {
	t.Helper()
	return e.TunnelTokenAt(t, over, e.Now())
}

// TunnelTokenAt 同 TunnelToken，签发时刻自选。
func (e *Env) TunnelTokenAt(t testing.TB, over Claims, at time.Time) string {
	t.Helper()
	iat := at.Unix()
	return e.Sign(t, gen.TokenTypeTunnel, merge(Claims{
		"iss": e.Issuer.Issuer, "sub": e.SourceID, "aud": "relay-tunnel",
		"cnf": Claims{"jwk": Claims{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(e.Source.Public)}},
		"iat": iat, "exp": iat + 3600, "jti": NewJTI(), "ver": 1,
	}, over))
}

// RelayToken 签一枚中继令牌（src 是测试源）。
func (e *Env) RelayToken(t testing.TB, over Claims) string {
	t.Helper()
	iat := e.Now().Unix()
	return e.Sign(t, gen.TokenTypeRelay, merge(Claims{
		"iss": e.Issuer.Issuer, "sub": "acct_test", "aud": "relay", "src": e.SourceID,
		"iat": iat, "exp": iat + 3600, "jti": NewJTI(), "ver": 1,
	}, over))
}
