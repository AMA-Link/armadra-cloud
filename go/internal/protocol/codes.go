// Package protocol 是隧道线协议（协议 1.3）的 Go 实现：帧编解码、RST 与关闭码、限制和握手消息。
//
// 常量一律取自 gen（协议包 schema 的生成物）；约束与 packages/platform-protocol 的
// tunnel/frames.ts、handshake.ts 逐条对称：Decode 拒的，Encode 也不产出。
package protocol

import "github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"

// FrameType 是帧头第一个字节。
type FrameType uint8

// 八种帧。
const (
	FrameOpen   FrameType = gen.FrameTypeOpen
	FrameData   FrameType = gen.FrameTypeData
	FrameEnd    FrameType = gen.FrameTypeEnd
	FrameRst    FrameType = gen.FrameTypeRst
	FrameWindow FrameType = gen.FrameTypeWindow
	FramePing   FrameType = gen.FrameTypePing
	FramePong   FrameType = gen.FrameTypePong
	FrameGoaway FrameType = gen.FrameTypeGoaway
)

func (t FrameType) String() string {
	switch t {
	case FrameOpen:
		return "open"
	case FrameData:
		return "data"
	case FrameEnd:
		return "end"
	case FrameRst:
		return "rst"
	case FrameWindow:
		return "window"
	case FramePing:
		return "ping"
	case FramePong:
		return "pong"
	case FrameGoaway:
		return "goaway"
	}
	return "unknown"
}

// RstCode 是 RST 帧载荷里的 u16。
type RstCode uint16

// RST 码。
const (
	RstOK          RstCode = gen.RstOk
	RstProtocol    RstCode = gen.RstProtocol
	RstRefused     RstCode = gen.RstRefused
	RstSourceGone  RstCode = gen.RstSourceGone
	RstInternal    RstCode = gen.RstInternal
	RstFlowControl RstCode = gen.RstFlowControl
	RstClientGone  RstCode = gen.RstClientGone
	RstTimeout     RstCode = gen.RstTimeout
)

// CloseCode 是 WebSocket 关闭码。客户端只会看到 44xx；449x 只在隧道上。
type CloseCode int

// 关闭码。
const (
	CloseNormal              CloseCode = gen.CloseNormal
	CloseGoingAway           CloseCode = gen.CloseGoingAway
	CloseBadFrame            CloseCode = gen.CloseBadFrame
	CloseUnauthenticated     CloseCode = gen.CloseUnauthenticated
	CloseForbidden           CloseCode = gen.CloseForbidden
	CloseSourceOffline       CloseCode = gen.CloseSourceOffline
	CloseProtocolUnsupported CloseCode = gen.CloseProtocolUnsupported
	CloseFrameTooLarge       CloseCode = gen.CloseFrameTooLarge
	CloseLimitReached        CloseCode = gen.CloseLimitReached
	CloseTunnelAuthFailed    CloseCode = gen.CloseTunnelAuthFailed
	CloseTunnelReplaced      CloseCode = gen.CloseTunnelReplaced
	CloseTunnelGoaway        CloseCode = gen.CloseTunnelGoaway
)

// 隧道限制。WebSocket 的 maxPayload 取 MaxFrameBytes + 16。
const (
	// FrameHeaderBytes 是帧头：type u8 + streamId u32 BE。
	FrameHeaderBytes = 5
	// MaxFrameBytes 是整帧上限（含帧头）。
	MaxFrameBytes = gen.LimitMaxFrameBytes
	// MaxDataChunk 是单个 DATA 帧的载荷上限。
	MaxDataChunk = gen.LimitMaxDataChunk
	// StreamWindow 是每流初始信用。
	StreamWindow = gen.LimitStreamWindow
	// TunnelWindow 是隧道级初始信用。
	TunnelWindow = gen.LimitTunnelWindow
	// MaxStreams 是单条隧道同时开的流数上限。
	MaxStreams = gen.LimitMaxStreams
	// HeartbeatMs 是心跳间隔；连续 HeartbeatMisses 次没回应就断开。
	HeartbeatMs       = gen.LimitHeartbeatMs
	HeartbeatMisses   = gen.LimitHeartbeatMisses
	OpenHeadersMax    = gen.LimitOpenHeadersMaxBytes
	HandshakeTimeout  = gen.LimitHandshakeTimeoutMs
	GoawayGraceMs     = gen.LimitGoawayGraceMs
	maxSafeInteger    = 1<<53 - 1
	e2eOpenMethod     = "GET"
	frameTunnelStream = 0
)
