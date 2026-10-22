package auth

import (
	"context"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 比 jose.Failure 多出来的两种失败原因。
const (
	FailureMissing        = "missing"
	FailureSourceMismatch = "source_mismatch"
)

// RelayResult 是中继令牌的校验结果。
type RelayResult struct {
	OK     bool
	Claims gen.RelayTokenClaims
	// Error 在 !OK 时是 ErrRelayTokenInvalid 或 ErrRelaySourceMismatch。
	Error gen.ErrorCode
	// Failure 是 jose.Failure 的文本，或 "missing" / "source_mismatch"。
	Failure string
}

// VerifyRelayToken 校验中继令牌（平台设计 §8.1、中继设计 §4.2）：开端到端通道时在 WS 子协议
// armadra-relay.<jwt> 里。校验 typ、签名、aud == "relay"、iss、时间、jti 未撤销，最后 src == sourceID。
// 缺 / 坏 → relay_token_invalid（401）；src 不符 → relay_source_mismatch（403）。
func VerifyRelayToken(ctx context.Context, a *Context, token, sourceID string) RelayResult {
	if token == "" {
		return RelayResult{Error: gen.ErrRelayTokenInvalid, Failure: FailureMissing}
	}
	res, err := jose.Verify(ctx, token, a.Options(gen.TokenTypeRelay, gen.RelayTokenClaimsSchema, gen.TokenAudienceRelay))
	if err != nil {
		return RelayResult{Error: gen.ErrRelayTokenInvalid, Failure: string(jose.FailureOf(err))}
	}
	var claims gen.RelayTokenClaims
	if err := res.Into(&claims); err != nil {
		return RelayResult{Error: gen.ErrRelayTokenInvalid, Failure: string(jose.FailBadClaims)}
	}
	if claims.Src != sourceID {
		return RelayResult{Error: gen.ErrRelaySourceMismatch, Failure: FailureSourceMismatch}
	}
	return RelayResult{OK: true, Claims: claims}
}

// SplitRelaySubprotocol 从 Sec-WebSocket-Protocol 里拿出 armadra-relay.<jwt>，其余子协议原样留下
// （边缘再看里面有没有 armadra-e2e.1）。多个 relay 子协议只取第一个，后面的丢掉。
// 没有令牌时 token 为空串，rest 非 nil。
func SplitRelaySubprotocol(header string) (token string, rest []string) {
	rest = []string{}
	found := false
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, gen.SubprotocolRelay) {
			if !found {
				token = p[len(gen.SubprotocolRelay):]
				found = true
			}
			continue
		}
		rest = append(rest, p)
	}
	return token, rest
}
