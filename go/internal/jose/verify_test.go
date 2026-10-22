package jose_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

var now = time.UnixMilli(testutil.FixedNow)

type env struct {
	issuer, source testutil.Key
	keys           jose.StaticKeys
}

func newEnv(t *testing.T) env {
	issuer, source := testutil.Keys(t)
	return env{issuer: issuer, source: source, keys: jose.StaticKeys{
		issuer.Kid: issuer.Public,
		source.Kid: source.Public,
	}}
}

func (e env) options(typ string, s *schema.Schema, aud string) jose.Options {
	return jose.Options{
		Typ: typ, Schema: s, ExpectedAud: aud, ExpectedIss: e.issuer.Issuer,
		Now: now, Keys: e.keys,
	}
}

func token(t *testing.T, name string) string {
	return strings.TrimSpace(string(testutil.Fixture(t, "assertion/"+name+".jwt")))
}

// fixtures/assertion/*.jwt 全部走一遍：正例通过，负例报对应的原因。
func TestFixtures(t *testing.T) {
	e := newEnv(t)
	sourceID := e.source.Kid
	assertion := func() jose.Options {
		return e.options(gen.TokenTypeAssertion, gen.AssertionClaimsSchema, sourceID)
	}
	relay := e.options(gen.TokenTypeRelay, gen.RelayTokenClaimsSchema, gen.TokenAudienceRelay)
	tunnel := e.options(gen.TokenTypeTunnel, gen.TunnelTokenClaimsSchema, gen.TokenAudienceTunnel)
	sourceJWS := e.options(gen.TokenTypeSource, gen.SourceJWSClaimsSchema, e.issuer.Issuer)
	sourceJWS.ExpectedIss = sourceID

	cases := []struct {
		name string
		opts jose.Options
		want jose.Failure // 空表示通过
	}{
		{"valid", assertion(), ""},
		{"guest", assertion(), ""},
		{"with-link", assertion(), ""},
		{"scoped-session", assertion(), ""},
		{"team-grant", assertion(), ""},
		{"access-token", e.options(gen.TokenTypeAccess, gen.AccessClaimsSchema, gen.TokenAudienceAccess), ""},
		{"relay-token", relay, ""},
		{"relay-token-ro", relay, ""},
		{"relay-token-dev", relay, ""},
		{"tunnel-token", tunnel, ""},
		{"tunnel-token-org", tunnel, ""},
		{"source-jws", sourceJWS, ""},
		{"expired", assertion(), jose.FailExpired},
		{"wrong-aud", assertion(), jose.FailAudienceMismatch},
		{"wrong-alg", assertion(), jose.FailMalformed},
	}
	files := testutil.Fixtures(t, "assertion", ".jwt")
	if len(cases) != len(files) {
		t.Fatalf("夹具有 %d 个 .jwt，测试只覆盖了 %d 个：%v", len(files), len(cases), files)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := jose.Verify(context.Background(), token(t, c.name), c.opts)
			if c.want == "" {
				if err != nil {
					t.Fatalf("应当通过：%v", err)
				}
				if res.Registered.Jti == "" || res.Header.Kid == "" {
					t.Fatalf("结果缺字段：%+v", res)
				}
				return
			}
			if got := jose.FailureOf(err); got != c.want {
				t.Fatalf("失败原因应为 %s，实际 %q（%v）", c.want, got, err)
			}
		})
	}
}

func TestClaimsDecodeIntoStructs(t *testing.T) {
	e := newEnv(t)
	res, err := jose.Verify(context.Background(), token(t, "with-link"),
		e.options(gen.TokenTypeAssertion, gen.AssertionClaimsSchema, e.source.Kid))
	if err != nil {
		t.Fatal(err)
	}
	var claims gen.AssertionClaims
	if err := res.Into(&claims); err != nil {
		t.Fatal(err)
	}
	if claims.Org == nil || claims.Org.OrgID != "org_01JAAAAAAAAAAAAAAAAAAAAAAA" || claims.Org.Role != "member" {
		t.Errorf("org：%+v", claims.Org)
	}
	if claims.Iat*1000 != testutil.FixedNow || claims.Exp-claims.Iat != gen.IdentityAssertionTTLMs/1000 {
		t.Errorf("时间声明不对：iat=%d exp=%d", claims.Iat, claims.Exp)
	}
	if claims.Device.Platform == "" || claims.Aud != e.source.Kid {
		t.Errorf("%+v", claims)
	}

	guest, err := jose.Verify(context.Background(), token(t, "guest"),
		e.options(gen.TokenTypeAssertion, gen.AssertionClaimsSchema, e.source.Kid))
	if err != nil {
		t.Fatal(err)
	}
	var g gen.AssertionClaims
	if err := guest.Into(&g); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(g.Sub, "guest:") || g.Link == nil || g.Link.InvitationID != "inv_01" || g.Email != nil {
		t.Errorf("guest：%+v", g)
	}

	scoped, err := jose.Verify(context.Background(), token(t, "scoped-session"),
		e.options(gen.TokenTypeAssertion, gen.AssertionClaimsSchema, e.source.Kid))
	if err != nil {
		t.Fatal(err)
	}
	var s gen.AssertionClaims
	if err := scoped.Into(&s); err != nil || len(s.Scp) == 0 {
		t.Errorf("scoped-session 应带 scp：%v %+v", err, s.Scp)
	}

	tun, err := jose.Verify(context.Background(), token(t, "tunnel-token"),
		e.options(gen.TokenTypeTunnel, gen.TunnelTokenClaimsSchema, gen.TokenAudienceTunnel))
	if err != nil {
		t.Fatal(err)
	}
	var tc gen.TunnelTokenClaims
	if err := tun.Into(&tc); err != nil || tc.Cnf.JWK.X != base64.RawURLEncoding.EncodeToString(e.source.Public) || tc.Sub != e.source.Kid {
		t.Errorf("tunnel-token：%v %+v", err, tc)
	}
}

// 握手夹具：hello 里的隧道令牌过验证，令牌里绑定的公钥正是 auth 签名的验签钥。
func TestHandshakeTunnelTokenBindsTheSourceKey(t *testing.T) {
	e := newEnv(t)
	var f struct {
		Hello json.RawMessage `json:"hello"`
		Auth  json.RawMessage `json:"auth"`
	}
	testutil.FixtureJSON(t, "tunnel/handshake.json", &f)
	hello, err := protocol.ParseHello(f.Hello)
	if err != nil {
		t.Fatal(err)
	}
	res, err := jose.Verify(context.Background(), hello.TunnelToken,
		e.options(gen.TokenTypeTunnel, gen.TunnelTokenClaimsSchema, gen.TokenAudienceTunnel))
	if err != nil {
		t.Fatal(err)
	}
	var claims gen.TunnelTokenClaims
	if err := res.Into(&claims); err != nil {
		t.Fatal(err)
	}
	if claims.Sub != hello.SourceID {
		t.Errorf("令牌的 sub %s 应等于 hello.sourceId", claims.Sub)
	}
	bound, err := base64.RawURLEncoding.DecodeString(claims.Cnf.JWK.X)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := protocol.ParseAuth(f.Auth)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(auth.Sig)
	input := protocol.SigningInput(hello.SourceID, hello.Nonce, "srKysrKysrKysrKysrKysg", "relay-1")
	if !ed25519.Verify(bound, input, sig) {
		t.Fatal("auth.sig 应能用令牌 cnf 里的公钥验过")
	}
}

type claimsMap map[string]any

func validClaims() claimsMap {
	return claimsMap{
		"iss": "https://cloud.armadra.test", "sub": "acct_1", "aud": "cloud", "sid": "ses_1",
		"iat": testutil.FixedNow / 1000, "exp": testutil.FixedNow/1000 + 900,
		"jti": "jti-access-9999", "ver": 1,
	}
}

func sign(t *testing.T, e env, typ string, c claimsMap) string {
	t.Helper()
	tok, err := jose.Sign(e.issuer.Private, jose.Header{Kid: e.issuer.Kid, Typ: typ}, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func accessOptions(e env) jose.Options {
	return e.options(gen.TokenTypeAccess, gen.AccessClaimsSchema, gen.TokenAudienceAccess)
}

func expectFailure(t *testing.T, tok string, o jose.Options, want jose.Failure) {
	t.Helper()
	_, err := jose.Verify(context.Background(), tok, o)
	if got := jose.FailureOf(err); got != want {
		t.Fatalf("失败原因应为 %q，实际 %q（%v）", want, got, err)
	}
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	e := newEnv(t)
	tok := sign(t, e, gen.TokenTypeAccess, validClaims())
	res, err := jose.Verify(context.Background(), tok, accessOptions(e))
	if err != nil {
		t.Fatal(err)
	}
	var claims gen.AccessClaims
	if err := res.Into(&claims); err != nil || claims.Sid != "ses_1" || claims.Sub != "acct_1" {
		t.Fatalf("%v %+v", err, claims)
	}
	parts, err := jose.Split(tok)
	if err != nil || parts.Header != (jose.Header{Alg: "EdDSA", Kid: e.issuer.Kid, Typ: gen.TokenTypeAccess}) {
		t.Fatalf("头：%+v %v", parts.Header, err)
	}
	// 头的字段顺序固定。
	head, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[0])
	if !strings.HasPrefix(string(head), `{"alg":"EdDSA","kid":`) {
		t.Errorf("头是 %s", head)
	}
}

// 校验顺序：typ → kid → 签名 → schema → aud → iss → 时间 → 撤销。
func TestVerificationOrder(t *testing.T) {
	e := newEnv(t)
	revoked := func(context.Context, string) (bool, error) { return true, nil }
	everythingWrong := func() claimsMap {
		c := validClaims()
		c["aud"] = "other"
		c["iss"] = "https://evil.test"
		c["exp"] = 1
		return c
	}

	// 时间也错、iss 也错、aud 也错：先报 aud。
	o := accessOptions(e)
	o.IsRevoked = revoked
	expectFailure(t, sign(t, e, gen.TokenTypeAccess, everythingWrong()), o, jose.FailBadClaims) // aud 不是 const "cloud"，schema 先挡住

	// 用 assertion 的 schema（aud 是 hex32）看 aud、iss、时间、撤销的先后。
	assertion := func(mutate func(claimsMap)) string {
		c := claimsMap{
			"iss": e.issuer.Issuer, "sub": "acct_1", "aud": e.source.Kid,
			"iat": testutil.FixedNow / 1000, "exp": testutil.FixedNow/1000 + 300,
			"jti": "jti-order-0001", "ver": 1,
			"device": claimsMap{"deviceId": "d1", "platform": "desktop", "name": "x"},
		}
		mutate(c)
		return sign(t, e, gen.TokenTypeAssertion, c)
	}
	ao := e.options(gen.TokenTypeAssertion, gen.AssertionClaimsSchema, e.source.Kid)
	ao.IsRevoked = revoked
	expectFailure(t, assertion(func(c claimsMap) {
		c["aud"] = "fedcba9876543210fedcba9876543210"
		c["iss"] = "https://evil.test"
		c["exp"] = 1
	}), ao, jose.FailAudienceMismatch)
	expectFailure(t, assertion(func(c claimsMap) {
		c["iss"] = "https://evil.test"
		c["exp"] = 1
	}), ao, jose.FailIssuerMismatch)
	expectFailure(t, assertion(func(c claimsMap) { c["exp"] = 1 }), ao, jose.FailExpired)
	expectFailure(t, assertion(func(claimsMap) {}), ao, jose.FailRevoked)
	ao.IsRevoked = nil
	if _, err := jose.Verify(context.Background(), assertion(func(claimsMap) {}), ao); err != nil {
		t.Fatalf("不查撤销时应通过：%v", err)
	}

	// 签名没过之前不看声明：claims 不合 schema 但签名也坏 → bad_signature。
	good := sign(t, e, gen.TokenTypeAccess, claimsMap{"nonsense": true})
	parts := strings.Split(good, ".")
	forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	expectFailure(t, forged, accessOptions(e), jose.FailBadSignature)
	expectFailure(t, good, accessOptions(e), jose.FailBadClaims)

	// typ 先于取钥：未知 kid 但 typ 不对 → wrong_type。
	other := jose.Header{Kid: "nobody", Typ: "armadra-relay"}
	tok, _ := jose.Sign(e.issuer.Private, other, validClaims())
	expectFailure(t, tok, accessOptions(e), jose.FailWrongType)
	other.Typ = gen.TokenTypeAccess
	tok, _ = jose.Sign(e.issuer.Private, other, validClaims())
	expectFailure(t, tok, accessOptions(e), jose.FailUnknownKey)
}

func TestRejections(t *testing.T) {
	e := newEnv(t)
	valid := sign(t, e, gen.TokenTypeAccess, validClaims())
	parts := strings.Split(valid, ".")

	flip := func(s string) string {
		b := []byte(s)
		if b[len(b)/2] == 'A' {
			b[len(b)/2] = 'B'
		} else {
			b[len(b)/2] = 'A'
		}
		return string(b)
	}
	b64 := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	sig := parts[2]
	o := accessOptions(e)

	for name, tok := range map[string]string{
		"空":             "",
		"两段":            parts[0] + "." + parts[1],
		"四段":            valid + ".x",
		"空段":            parts[0] + ".." + sig,
		"头不是 base64url": "!!!." + parts[1] + "." + sig,
		"头不是对象":         b64([]any{1}) + "." + parts[1] + "." + sig,
		"alg 是 none":    b64(map[string]any{"alg": "none", "kid": e.issuer.Kid, "typ": gen.TokenTypeAccess}) + "." + parts[1] + "." + sig,
		"alg 是 HS256":   b64(map[string]any{"alg": "HS256", "kid": e.issuer.Kid, "typ": gen.TokenTypeAccess}) + "." + parts[1] + "." + sig,
		"缺 alg":         b64(map[string]any{"kid": e.issuer.Kid, "typ": gen.TokenTypeAccess}) + "." + parts[1] + "." + sig,
		"缺 kid":         b64(map[string]any{"alg": "EdDSA", "typ": gen.TokenTypeAccess}) + "." + parts[1] + "." + sig,
		"typ 不是字符串":     b64(map[string]any{"alg": "EdDSA", "kid": e.issuer.Kid, "typ": 1}) + "." + parts[1] + "." + sig,
		"载荷不是对象":        parts[0] + "." + b64("x") + "." + sig,
		"签名带填充":         parts[0] + "." + parts[1] + "." + sig + "==",
		"签名太短":          parts[0] + "." + parts[1] + "." + sig[:40],
		"签名末尾多余位":       parts[0] + "." + parts[1] + "." + sig[:85] + "B",
		"头里夹着换行":        parts[0][:4] + "\n" + parts[0][4:] + "." + parts[1] + "." + sig,
	} {
		t.Run(name, func(t *testing.T) { expectFailure(t, tok, o, jose.FailMalformed) })
	}

	// 注意：签名最后一个字符改成末尾位非零的值会被严格解码拒绝（上面），所以篡改签名改中间。
	expectFailure(t, parts[0]+"."+parts[1]+"."+flip(sig), o, jose.FailBadSignature)
	changed := validClaims()
	changed["sub"] = "acct_other"
	expectFailure(t, parts[0]+"."+b64(changed)+"."+sig, o, jose.FailBadSignature)

	wrongKey := jose.StaticKeys{e.issuer.Kid: e.source.Public}
	o2 := o
	o2.Keys = wrongKey
	expectFailure(t, valid, o2, jose.FailBadSignature)

	short := jose.StaticKeys{e.issuer.Kid: ed25519.PublicKey("short")}
	o2.Keys = short
	expectFailure(t, valid, o2, jose.FailBadSignature)

	expectFailure(t, valid, e.options(gen.TokenTypeRelay, gen.AccessClaimsSchema, "cloud"), jose.FailWrongType)
	o3 := o
	o3.ExpectedIss = "https://other.test"
	expectFailure(t, valid, o3, jose.FailIssuerMismatch)
	o4 := o
	o4.ExpectedAud = "relay"
	expectFailure(t, valid, o4, jose.FailAudienceMismatch)

	// 缺必填字段、字段类型不对。
	missing := validClaims()
	delete(missing, "sid")
	expectFailure(t, sign(t, e, gen.TokenTypeAccess, missing), o, jose.FailBadClaims)
	badType := validClaims()
	badType["exp"] = "soon"
	expectFailure(t, sign(t, e, gen.TokenTypeAccess, badType), o, jose.FailBadClaims)
	badVer := validClaims()
	badVer["ver"] = 2
	expectFailure(t, sign(t, e, gen.TokenTypeAccess, badVer), o, jose.FailBadClaims)
	shortJti := validClaims()
	shortJti["jti"] = "short"
	expectFailure(t, sign(t, e, gen.TokenTypeAccess, shortJti), o, jose.FailBadClaims)
}

func TestTimeWindow(t *testing.T) {
	e := newEnv(t)
	iat := int64(testutil.FixedNow / 1000)
	claims := func(iatOff, expOff int64, nbf *int64) string {
		c := validClaims()
		c["iat"] = iat + iatOff
		c["exp"] = iat + expOff
		if nbf != nil {
			c["nbf"] = iat + *nbf
		}
		return sign(t, e, gen.TokenTypeAccess, c)
	}
	at := func(offset time.Duration) jose.Options {
		o := accessOptions(e)
		o.Now = now.Add(offset)
		return o
	}
	i64 := func(n int64) *int64 { return &n }

	tok := claims(0, 900, nil)
	expectFailure(t, tok, at(0), "")
	// exp 之后 5 分钟的容忍：now - skew >= exp 才算过期。
	expectFailure(t, tok, at(900*time.Second+5*time.Minute-time.Second), "")
	expectFailure(t, tok, at(900*time.Second+5*time.Minute), jose.FailExpired)
	// iat 在未来 5 分钟内容忍，超过就未生效。
	future := claims(300, 1200, nil)
	expectFailure(t, future, at(0), "")
	expectFailure(t, claims(301, 1200, nil), at(0), jose.FailNotYetValid)
	// nbf 同理。
	expectFailure(t, claims(0, 900, i64(300)), at(0), "")
	expectFailure(t, claims(0, 900, i64(301)), at(0), jose.FailNotYetValid)
	// 自定义容忍。
	o := at(0)
	o.Skew = time.Second
	expectFailure(t, claims(2, 900, nil), o, jose.FailNotYetValid)
}

func TestRevocationCallback(t *testing.T) {
	e := newEnv(t)
	tok := sign(t, e, gen.TokenTypeAccess, validClaims())
	var asked string
	o := accessOptions(e)
	o.IsRevoked = func(_ context.Context, jti string) (bool, error) { asked = jti; return false, nil }
	if _, err := jose.Verify(context.Background(), tok, o); err != nil || asked != "jti-access-9999" {
		t.Fatalf("应带着 jti 回调：%q %v", asked, err)
	}
	// 查不出来就当撤销（宁可拒绝），并带出底层错误。
	boom := errors.New("redis 挂了")
	o.IsRevoked = func(context.Context, string) (bool, error) { return false, boom }
	_, err := jose.Verify(context.Background(), tok, o)
	if jose.FailureOf(err) != jose.FailRevoked || !errors.Is(err, boom) {
		t.Fatalf("得到 %v", err)
	}
}

func TestKeyLookupErrorIsUnknownKey(t *testing.T) {
	e := newEnv(t)
	o := accessOptions(e)
	boom := errors.New("取钥失败")
	o.Keys = keysFunc(func(context.Context, string) (ed25519.PublicKey, bool, error) { return nil, false, boom })
	_, err := jose.Verify(context.Background(), sign(t, e, gen.TokenTypeAccess, validClaims()), o)
	if jose.FailureOf(err) != jose.FailUnknownKey || !errors.Is(err, boom) {
		t.Fatalf("得到 %v", err)
	}
}

type keysFunc func(context.Context, string) (ed25519.PublicKey, bool, error)

func (f keysFunc) Key(ctx context.Context, kid string) (ed25519.PublicKey, bool, error) {
	return f(ctx, kid)
}

func TestSignRequiresKid(t *testing.T) {
	e := newEnv(t)
	if _, err := jose.Sign(e.issuer.Private, jose.Header{}, validClaims()); err == nil {
		t.Fatal("缺 kid 应报错")
	}
}

// SignPayload 把载荷原样放进令牌（不转义 <>&，字段顺序由调用方定），签名照样验得过。
func TestSignPayloadKeepsBytes(t *testing.T) {
	_, source := testutil.Keys(t)
	payload := `{"z":1,"a":"<&>"}`
	token, err := jose.SignPayload(source.Private, jose.Header{Kid: "k", Typ: "t"}, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	parts, err := jose.Split(token)
	if err != nil {
		t.Fatal(err)
	}
	if string(parts.Payload) != payload {
		t.Fatalf("载荷应原样：%s", parts.Payload)
	}
	if !ed25519.Verify(source.Public, parts.SigningInput, parts.Signature) {
		t.Fatal("签名应验得过")
	}
	if _, err := jose.SignPayload(source.Private, jose.Header{}, []byte(payload)); err == nil {
		t.Fatal("头里缺 kid 应报错")
	}
}
