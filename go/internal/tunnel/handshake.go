package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"

	"github.com/AMA-Link/armadra-cloud/go/internal/auth"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 隧道握手状态机（协议包 §3.1）：hello → challenge → auth → ready。
//
//  1. hello：协议 major 相同（否则 reject protocol_unsupported + 4409）；隧道令牌验签与声明，
//     sub 等于 hello.sourceId；之后按 sourceId 限流 tunnel.connect.src（reject rate_limited + 4429）。
//     令牌错 → reject + 4490。
//  2. 发 challenge { nonce2, relayNode }。
//  3. auth.sig 用 cnf.jwk 验 sourceId、nonce、nonce2、relayNode；错 → reject signature_invalid + 4490。
//  4. 通过后由 Hub 处理同源替换、发 ready、写路由。
//
// 任何格式错（不是 JSON、不符 schema、顺序不对）不发 reject，直接关 4400。
// 本类型不碰套接字与定时器：输入一条文本消息，输出下一步动作；超时由调用方计时。

// StepKind 是握手下一步的类别。
type StepKind int

// 握手下一步。
const (
	// StepSend 发 Challenge。
	StepSend StepKind = iota
	// StepReject 发 Reject，再以 CloseCode 关闭。
	StepReject
	// StepFail 不发 reject，直接以 CloseCode 关闭。
	StepFail
	// StepReady 握手完成。
	StepReady
)

// Step 是握手的输出。
type Step struct {
	Kind      StepKind
	Challenge gen.Challenge
	Reject    gen.Reject
	CloseCode protocol.CloseCode
	// Reason 是 StepFail 的关闭原因。
	Reason string
	Hello  gen.Hello
}

// 握手拒绝码（取自 gen.RejectCodeValues）。
const (
	RejectProtocolUnsupported = "protocol_unsupported"
	RejectSignatureInvalid    = "signature_invalid"
	RejectSourceRevoked       = "source_revoked"
	RejectRateLimited         = "rate_limited"
	RejectReplaced            = "replaced"
)

var rejectMessages = map[string]string{
	RejectProtocolUnsupported:     "protocol major not supported",
	auth.RejectTunnelTokenInvalid: "tunnel token invalid",
	auth.RejectTunnelTokenExpired: "tunnel token expired",
	RejectSignatureInvalid:        "signature invalid",
	RejectSourceRevoked:           "source revoked",
	RejectRateLimited:             "too many tunnel connections",
	RejectReplaced:                "replaced by a newer tunnel",
}

var rejectClose = map[string]protocol.CloseCode{
	RejectProtocolUnsupported:     protocol.CloseProtocolUnsupported,
	auth.RejectTunnelTokenInvalid: protocol.CloseTunnelAuthFailed,
	auth.RejectTunnelTokenExpired: protocol.CloseTunnelAuthFailed,
	RejectSignatureInvalid:        protocol.CloseTunnelAuthFailed,
	RejectSourceRevoked:           protocol.CloseTunnelAuthFailed,
	RejectRateLimited:             protocol.CloseLimitReached,
	RejectReplaced:                protocol.CloseTunnelReplaced,
}

// RejectStep 构造某个拒绝码对应的 StepReject。
func RejectStep(code string) Step {
	return Step{
		Kind:      StepReject,
		Reject:    gen.Reject{Type: "reject", Code: code, Message: rejectMessages[code]},
		CloseCode: rejectClose[code],
	}
}

func failStep(reason string) Step {
	return Step{Kind: StepFail, CloseCode: protocol.CloseBadFrame, Reason: reason}
}

type handshakeState int

const (
	stateHello handshakeState = iota
	stateAuth
	stateDone
)

// HandshakeDeps 是握手要用的东西。
type HandshakeDeps struct {
	Auth      *auth.Context
	Limits    control.Limiter
	RelayNode string
	// Nonce 产生 16 字节随机数的 base64url；测试注入固定值。
	Nonce func() string
}

// Handshake 是一次握手。
type Handshake struct {
	deps      HandshakeDeps
	state     handshakeState
	hello     gen.Hello
	sourceKey ed25519.PublicKey
	challenge gen.Challenge
}

// NewHandshake 建一次握手。
func NewHandshake(deps HandshakeDeps) *Handshake {
	if deps.Nonce == nil {
		deps.Nonce = RandomNonce
	}
	return &Handshake{deps: deps}
}

// SourceID 是 hello 里的源（hello 通过之前为空）。
func (h *Handshake) SourceID() string { return h.hello.SourceID }

// OnText 输入一条文本消息，答下一步。err 非空表示控制面出错（调用方记日志并断开）。
func (h *Handshake) OnText(ctx context.Context, text []byte) (Step, error) {
	switch h.state {
	case stateHello:
		return h.onHello(ctx, text)
	case stateAuth:
		return h.onAuth(text), nil
	}
	return h.done(failStep("unexpected_text")), nil
}

func (h *Handshake) onHello(ctx context.Context, text []byte) (Step, error) {
	hello, err := protocol.ParseHello(text)
	if err != nil {
		return h.done(failStep("bad_hello")), nil
	}
	if hello.Protocol.Major != gen.ProtocolMajor {
		return h.done(RejectStep(RejectProtocolUnsupported)), nil
	}
	token := auth.VerifyTunnelToken(ctx, h.deps.Auth, hello.TunnelToken, hello.SourceID)
	if !token.OK {
		return h.done(RejectStep(token.Reject)), nil
	}
	limit, err := h.deps.Limits.Hit(ctx, control.LimitTunnelConnectSrc, hello.SourceID)
	if err != nil {
		return Step{}, err
	}
	if !limit.Allowed {
		return h.done(RejectStep(RejectRateLimited)), nil
	}
	h.hello = hello
	h.sourceKey = token.SourceKey
	h.challenge = gen.Challenge{Type: "challenge", Nonce2: h.deps.Nonce(), RelayNode: h.deps.RelayNode}
	h.state = stateAuth
	return Step{Kind: StepSend, Challenge: h.challenge}, nil
}

func (h *Handshake) onAuth(text []byte) Step {
	msg, err := protocol.ParseAuth(text)
	if err != nil {
		return h.done(failStep("bad_auth"))
	}
	input := protocol.SigningInput(h.hello.SourceID, h.hello.Nonce, h.challenge.Nonce2, h.challenge.RelayNode)
	sig, err := base64.RawURLEncoding.DecodeString(msg.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(h.sourceKey, input, sig) {
		return h.done(RejectStep(RejectSignatureInvalid))
	}
	return h.done(Step{Kind: StepReady, Hello: h.hello})
}

func (h *Handshake) done(s Step) Step {
	h.state = stateDone
	return s
}

// RandomNonce 返回 16 字节随机数的 base64url。
func RandomNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("读取随机数失败：" + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
