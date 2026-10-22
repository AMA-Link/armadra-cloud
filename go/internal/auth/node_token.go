package auth

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// NodeResult 是节点令牌的校验结果。
type NodeResult struct {
	OK     bool
	Claims gen.NodeTokenClaims
	// Failure 是 jose.Failure 的文本，或 "missing"。
	Failure string
}

// VerifyNodeToken 校验节点令牌（cloud-api §30，SaaS 实施设计 §2.4 的 N6）：节点之间 `/x/v1` 升级时
// 在 Authorization: Bearer 里出示。校验 typ = armadra-node、签名（JWKS）、声明形状、aud = relay-node、
// iss、时间与 jti 未撤销（`armadra-cloud nodes revoke` 走普通撤销表，jti 形如 n.<节点>.<随机>）。
// sub 是出示方的节点名，由声明形状（relayNodeIdSchema）约束。
func VerifyNodeToken(ctx context.Context, a *Context, token string) NodeResult {
	if token == "" {
		return NodeResult{Failure: FailureMissing}
	}
	res, err := jose.Verify(ctx, token, a.Options(gen.TokenTypeNode, gen.NodeTokenClaimsSchema, gen.TokenAudienceRelayNode))
	if err != nil {
		return NodeResult{Failure: string(jose.FailureOf(err))}
	}
	var claims gen.NodeTokenClaims
	if err := res.Into(&claims); err != nil {
		return NodeResult{Failure: string(jose.FailBadClaims)}
	}
	return NodeResult{OK: true, Claims: claims}
}
