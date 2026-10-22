package core_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

func TestRegistrationTokenLimitsAndExpiry(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := core.NewSources(e.h.Context)
	var first string
	for i := 0; i < core.MaxRegistrationTokens; i++ {
		token, err := sources.RegistrationToken(e.ctx, owner)
		e.must(err)
		if i == 0 {
			first = token.RegistrationToken
		}
		eq(t, token.ExpiresAtMs, e.h.Clock.Ms()+10*60_000, "注册令牌 10 分钟")
	}
	_, err := sources.RegistrationToken(e.ctx, owner)
	eq(t, code(err), "limit_reached", "第 21 个")
	// 过期的不算，满了之后过期的会让出位置。
	e.h.Clock.Set(e.h.Clock.Ms() + 10*60_000 + 1)
	_, err = sources.RegistrationToken(e.ctx, owner)
	eq(t, code(err), "ok", "旧的过期之后")
	// 过期的令牌不能登记。
	_, err = sources.Register(e.ctx, registerInput(t, first, sourceID, 1), "ip")
	eq(t, code(err), "registration_token_invalid", "过期的令牌")
	// 访客不能要注册令牌。
	guest := core.Caller{Subject: "guest:1111111111111111:ab", DeviceID: "d", Role: core.RoleGuest, LinkID: "1111111111111111"}
	_, err = sources.RegistrationToken(e.ctx, guest)
	eq(t, code(err), "forbidden", "访客")
}

func registerInput(t testing.TB, token, id string, major int64) core.RegisterInput {
	_, source := testutil.Keys(t)
	return core.RegisterInput{
		RegistrationToken: token, SourceID: id,
		PublicKey: gen.Ed25519PublicJWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(source.Public)},
		Name:      "mac", Kind: "desktop", CoreVersion: "0.9.2", Capabilities: []string{}, ProtocolMajor: major,
	}
}

func TestRegisterRules(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := core.NewSources(e.h.Context)
	fresh := func() string {
		token, err := sources.RegistrationToken(e.ctx, owner)
		e.must(err)
		return token.RegistrationToken
	}

	// 令牌一次性：登记一次之后同一个令牌不能再用。
	token := fresh()
	_, err := sources.Register(e.ctx, registerInput(t, token, sourceID, 1), "ip")
	e.must(err)
	_, err = sources.Register(e.ctx, registerInput(t, token, sourceID, 1), "ip")
	eq(t, code(err), "registration_token_invalid", "令牌已用过")

	// 协议主版本不对：令牌也已消耗，不能再用。
	token = fresh()
	_, err = sources.Register(e.ctx, registerInput(t, token, "ffffffffffffffffffffffffffffffff", 2), "ip")
	eq(t, code(err), "protocol_unsupported", "主版本不对")
	_, err = sources.Register(e.ctx, registerInput(t, token, "ffffffffffffffffffffffffffffffff", 1), "ip")
	eq(t, code(err), "registration_token_invalid", "拒绝之后令牌也已消耗")

	// 同一个 sourceId 换了公钥：source_taken；撤销之后可以重新登记。
	other := registerInput(t, fresh(), sourceID, 1)
	other.PublicKey.X = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	_, err = sources.Register(e.ctx, other, "ip")
	eq(t, code(err), "source_taken", "换了公钥")
	e.must(sources.Revoke(e.ctx, owner, sourceID))
	other = registerInput(t, fresh(), sourceID, 1)
	other.PublicKey.X = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	_, err = sources.Register(e.ctx, other, "ip")
	eq(t, code(err), "ok", "撤销之后重新登记")
	record, _ := e.h.Context.Ports.Sources.Get(e.ctx, "t1", sourceID)
	if record.RevokedAtMs != nil || record.PublicJWK.X != other.PublicKey.X {
		t.Fatalf("重新登记应清掉撤销标记：%+v", record)
	}
	// 重新登记同一把钥（未撤销）保留首次登记时间。
	e.h.Clock.Set(e.h.Clock.Ms() + 1000)
	same := registerInput(t, fresh(), sourceID, 1)
	same.PublicKey.X = other.PublicKey.X
	_, err = sources.Register(e.ctx, same, "ip")
	e.must(err)
	again, _ := e.h.Context.Ports.Sources.Get(e.ctx, "t1", sourceID)
	eq(t, again.RegisteredAtMs, record.RegisteredAtMs, "保留首次登记时间")
}

func TestAuthenticateSource(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	_, key := testutil.Keys(t)
	issuer := "https://cloud.armadra.test"
	sign := func(header jose.Header, claims map[string]any) string {
		token, err := jose.Sign(key.Private, header, claims)
		e.must(err)
		return "Source " + token
	}
	now := e.h.Clock.Ms() / 1000
	good := func(jti string) map[string]any {
		return map[string]any{"iss": sourceID, "aud": issuer, "iat": now, "exp": now + 60, "jti": jti}
	}
	header := jose.Header{Kid: sourceID}

	source, err := sources.AuthenticateSource(e.ctx, sign(header, good("jti-aaaa-0001")))
	e.must(err)
	eq(t, source.SourceID, sourceID, "认出源")

	cases := map[string]string{
		"没有头":             "",
		"不是 Source":       "Bearer x",
		"不是 JWS":          "Source abc",
		"jti 重放":          sign(header, good("jti-aaaa-0001")),
		"kid 不是已登记的源":     sign(jose.Header{Kid: "ffffffffffffffffffffffffffffffff"}, good("jti-aaaa-0002")),
		"typ 不对":          sign(jose.Header{Kid: sourceID, Typ: "armadra-access"}, good("jti-aaaa-0003")),
		"iss 不是 sourceId": sign(header, map[string]any{"iss": "ffffffffffffffffffffffffffffffff", "aud": issuer, "iat": now, "exp": now + 60, "jti": "jti-aaaa-0004"}),
		"aud 不是 issuer":   sign(header, map[string]any{"iss": sourceID, "aud": "https://evil.test", "iat": now, "exp": now + 60, "jti": "jti-aaaa-0005"}),
		"寿命超过 120 秒":      sign(header, map[string]any{"iss": sourceID, "aud": issuer, "iat": now, "exp": now + 121, "jti": "jti-aaaa-0006"}),
		"已过期":             sign(header, map[string]any{"iss": sourceID, "aud": issuer, "iat": now - 1000, "exp": now - 700, "jti": "jti-aaaa-0007"}),
		"iat 在未来":         sign(header, map[string]any{"iss": sourceID, "aud": issuer, "iat": now + 1000, "exp": now + 1060, "jti": "jti-aaaa-0008"}),
		"jti 太短":          sign(header, good("短")),
		"typ 是空串":         "Source " + signWithRawHeader(t, key.Private, `{"alg":"EdDSA","kid":"`+sourceID+`","typ":""}`, good("jti-aaaa-0011")),
		"nbf 在未来（比 TypeScript 更严）": sign(header, map[string]any{"iss": sourceID, "aud": issuer, "iat": now, "nbf": now + 1000, "exp": now + 60, "jti": "jti-aaaa-0012"}),
	}
	for name, header := range cases {
		_, err := sources.AuthenticateSource(e.ctx, header)
		eq(t, code(err), "unauthenticated", name)
	}
	// 别人的钥签的：验不过。
	issuerKey, _ := testutil.Keys(t)
	forged, err := jose.Sign(issuerKey.Private, header, good("jti-aaaa-0009"))
	e.must(err)
	_, err = sources.AuthenticateSource(e.ctx, "Source "+forged)
	eq(t, code(err), "unauthenticated", "别的钥签的")
	// 撤销之后：source_revoked。
	e.must(sources.Revoke(e.ctx, owner, sourceID))
	_, err = sources.AuthenticateSource(e.ctx, sign(header, good("jti-aaaa-0010")))
	eq(t, code(err), "source_revoked", "已撤销")
}

func TestRelayForSignsATunnelTokenBoundToTheSourceKey(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	sources := e.register(e.owner(), sourceID)
	source, _ := e.h.Context.Ports.Sources.Get(e.ctx, "t1", sourceID)
	info, err := sources.RelayFor(e.ctx, *source)
	e.must(err)
	claims := claimsOf(t, info.TunnelToken)
	eq(t, claims["sub"], any(sourceID), "sub")
	eq(t, claims["aud"], any("relay-tunnel"), "aud")
	if jwk := claims["cnf"].(map[string]any)["jwk"].(map[string]any); jwk["x"] != source.PublicJWK.X || jwk["kty"] != "OKP" {
		t.Fatalf("cnf.jwk 是登记的公钥：%+v", jwk)
	}
	eq(t, info.ExpiresAtMs-e.h.Clock.Ms(), int64(time.Hour/time.Millisecond), "隧道令牌 1 小时")
	if info.Limits.MaxStreams != 256 || info.Limits.StreamWindow != 262144 || info.Limits.TunnelWindow != 8388608 || info.Limits.MaxFrameBytes != 1048576 {
		t.Fatalf("限制取协议常量：%+v", info.Limits)
	}
}

func TestSourceVisibilityAndAssertionRules(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	_, err := sources.Get(e.ctx, owner, "ffffffffffffffffffffffffffffffff")
	eq(t, code(err), "not_found", "看不到的源")
	_, err = sources.Assertion(e.ctx, owner, "ffffffffffffffffffffffffffffffff", nil)
	eq(t, code(err), "source_access_denied", "没登记的源")
	grant, err := sources.Assertion(e.ctx, owner, sourceID, &core.DeviceInput{Platform: "ios", Name: "我的手机"})
	e.must(err)
	device := claimsOf(t, grant.Assertion)["device"].(map[string]any)
	eq(t, device["platform"], any("ios"), "请求里的设备信息覆盖")
	eq(t, claimsOf(t, grant.Assertion)["name"], any("alice"), "owner 的断言带账号名")
	eq(t, claimsOf(t, grant.Assertion)["aud"], any(sourceID), "aud 是源")
	// 撤销之后：source_revoked。
	e.must(sources.Revoke(e.ctx, owner, sourceID))
	_, err = sources.Assertion(e.ctx, owner, sourceID, nil)
	eq(t, code(err), "source_revoked", "已撤销")
	eq(t, code(sources.Revoke(e.ctx, owner, sourceID)), "not_found", "再撤一次")
}

// signWithRawHeader：头按原文写（jose.Sign 的 typ 为空时会省略，造不出 "typ":""）。
func signWithRawHeader(t *testing.T, private ed25519.PrivateKey, header string, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString([]byte(header)) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(input)))
}
