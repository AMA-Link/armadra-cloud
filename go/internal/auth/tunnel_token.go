package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"

	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 握手 reject 码里与隧道令牌有关的两个（取值登记在 gen.RejectCodeValues）。
const (
	RejectTunnelTokenInvalid = "tunnel_token_invalid"
	RejectTunnelTokenExpired = "tunnel_token_expired"
)

// 比 jose.Failure 多出来的两种失败原因。
const (
	FailureSubjectMismatch = "subject_mismatch"
	FailureBadCnf          = "bad_cnf"
)

// TunnelResult 是隧道令牌的校验结果。
type TunnelResult struct {
	OK        bool
	Claims    gen.TunnelTokenClaims
	SourceKey ed25519.PublicKey
	// Reject 在 !OK 时是握手的 reject 码。
	Reject string
	// Failure 是 jose.Failure 的文本，或 "subject_mismatch" / "bad_cnf"。
	Failure string
}

// VerifyTunnelToken 校验隧道令牌（协议包 §4.4、中继设计 §3.1）：校验 typ、签名、aud == "relay-tunnel"、
// iss、时间、jti 未撤销、sub == sourceID，然后取出 cnf.jwk——握手第 3 步用它验源的签名。
func VerifyTunnelToken(ctx context.Context, a *Context, token, sourceID string) TunnelResult {
	invalid := func(failure string) TunnelResult {
		return TunnelResult{Reject: RejectTunnelTokenInvalid, Failure: failure}
	}
	res, err := jose.Verify(ctx, token, a.Options(gen.TokenTypeTunnel, gen.TunnelTokenClaimsSchema, gen.TokenAudienceTunnel))
	if err != nil {
		f := jose.FailureOf(err)
		if f == jose.FailExpired {
			return TunnelResult{Reject: RejectTunnelTokenExpired, Failure: string(f)}
		}
		return invalid(string(f))
	}
	var claims gen.TunnelTokenClaims
	if err := res.Into(&claims); err != nil {
		return invalid(string(jose.FailBadClaims))
	}
	if claims.Sub != sourceID {
		return invalid(FailureSubjectMismatch)
	}
	key, ok := parseOKP(claims.Cnf.JWK)
	if !ok {
		return invalid(FailureBadCnf)
	}
	return TunnelResult{OK: true, Claims: claims, SourceKey: key}
}

// parseOKP 取 OKP / Ed25519 公钥：x 是 base64url 的 32 字节。
func parseOKP(jwk gen.Ed25519PublicJWK) (ed25519.PublicKey, bool) {
	if jwk.Kty != "OKP" || jwk.Crv != "Ed25519" {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(jwk.X)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, false
	}
	return ed25519.PublicKey(raw), true
}
