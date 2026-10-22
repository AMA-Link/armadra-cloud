// Package proto 是压测客户端自带的隧道线协议实现：帧编解码、握手消息。
// 它独立于被测的 internal/tunnel（不 import 它）。
package proto

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 常量取自生成物 gen，免得与协议漂移。
const (
	FrameOpen   = gen.FrameTypeOpen
	FrameData   = gen.FrameTypeData
	FrameEnd    = gen.FrameTypeEnd
	FrameRst    = gen.FrameTypeRst
	FrameWindow = gen.FrameTypeWindow
	FramePing   = gen.FrameTypePing
	FramePong   = gen.FrameTypePong
	FrameGoaway = gen.FrameTypeGoaway

	HeaderBytes   = 5
	MaxFrameBytes = gen.LimitMaxFrameBytes
	MaxDataChunk  = gen.LimitMaxDataChunk
	StreamWindow  = gen.LimitStreamWindow
	TunnelWindow  = gen.LimitTunnelWindow
	MaxStreams    = gen.LimitMaxStreams
	HeartbeatMs   = gen.LimitHeartbeatMs

	RstFlowControl = gen.RstFlowControl
	RstClientGone  = gen.RstClientGone

	CloseNormal         = gen.CloseNormal
	CloseBadFrame       = gen.CloseBadFrame
	CloseUnauthorized   = gen.CloseUnauthenticated
	CloseSourceOffline  = gen.CloseSourceOffline
	CloseProtoUnsupp    = gen.CloseProtocolUnsupported
	CloseFrameTooLarge  = gen.CloseFrameTooLarge
	CloseLimitReached   = gen.CloseLimitReached
	CloseTunnelAuthFail = gen.CloseTunnelAuthFailed
	CloseTunnelReplaced = gen.CloseTunnelReplaced

	E2ESubprotocol      = gen.E2ESubprotocol
	RelaySubprotoPrefix = gen.SubprotocolRelay
	E2EPath             = gen.E2EPath
	E2ECapability       = gen.E2ECapability
	ProtoMajor          = gen.ProtocolMajor
	ProtoMinor          = gen.ProtocolMinor
)

type Frame struct {
	Type     byte
	StreamID uint32
	Payload  []byte
}

var ErrBadFrame = errors.New("bad_frame")

// Encode 产出一帧（新分配，调用方可以放心交给写协程）。
func Encode(t byte, streamID uint32, payload []byte) []byte {
	out := make([]byte, HeaderBytes+len(payload))
	out[0] = t
	binary.BigEndian.PutUint32(out[1:5], streamID)
	copy(out[HeaderBytes:], payload)
	return out
}

func Decode(b []byte) (Frame, error) {
	if len(b) < HeaderBytes || len(b) > MaxFrameBytes {
		return Frame{}, ErrBadFrame
	}
	f := Frame{Type: b[0], StreamID: binary.BigEndian.Uint32(b[1:5]), Payload: b[HeaderBytes:]}
	if f.Type < FrameOpen || f.Type > FrameGoaway {
		return Frame{}, ErrBadFrame
	}
	return f, nil
}

func U32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func U16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func F64(v float64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, math.Float64bits(v))
	return b
}

type Version struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

type Hello struct {
	Type         string   `json:"type"`
	Protocol     Version  `json:"protocol"`
	SourceID     string   `json:"sourceId"`
	CoreVersion  string   `json:"coreVersion"`
	Capabilities []string `json:"capabilities"`
	TunnelToken  string   `json:"tunnelToken"`
	Nonce        string   `json:"nonce"`
}

type Challenge struct {
	Type      string `json:"type"`
	Nonce2    string `json:"nonce2"`
	RelayNode string `json:"relayNode"`
}

type Auth struct {
	Type string `json:"type"`
	Sig  string `json:"sig"`
}

type Limits struct {
	MaxStreams    int `json:"maxStreams"`
	StreamWindow  int `json:"streamWindow"`
	TunnelWindow  int `json:"tunnelWindow"`
	MaxFrameBytes int `json:"maxFrameBytes"`
}

type Ready struct {
	Type        string  `json:"type"`
	TunnelID    string  `json:"tunnelId"`
	Limits      Limits  `json:"limits"`
	HeartbeatMs int     `json:"heartbeatMs"`
	Protocol    Version `json:"protocol"`
}

type Open struct {
	Kind         string      `json:"kind"`
	Method       string      `json:"method"`
	Path         string      `json:"path"`
	Headers      [][2]string `json:"headers"`
	ClientOrigin *string     `json:"clientOrigin"`
	RemoteIP     string      `json:"remoteIp"`
	RelayNode    string      `json:"relayNode"`
}

// SigningInput 是 auth 签名的输入：sourceId \n nonce \n nonce2 \n relayNode。
func SigningInput(sourceID, nonce, nonce2, relayNode string) []byte {
	return []byte(sourceID + "\n" + nonce + "\n" + nonce2 + "\n" + relayNode)
}
