package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

// FrameErrorCode 是帧错误的类别，与 TypeScript 一侧的 FrameErrorCode 同名。
type FrameErrorCode string

// 帧错误类别。
const (
	ErrBadFrame      FrameErrorCode = "bad_frame"
	ErrFrameTooLarge FrameErrorCode = "frame_too_large"
	ErrBadOpen       FrameErrorCode = "bad_open"
)

// FrameError 是编码或解码时拒绝一帧的原因。
type FrameError struct {
	Code FrameErrorCode
	Msg  string
}

func (e *FrameError) Error() string { return string(e.Code) + "：" + e.Msg }

func frameErr(code FrameErrorCode, format string, args ...any) error {
	return &FrameError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// CodeOf 取错误的帧错误类别；不是 FrameError 时返回空串。
func CodeOf(err error) FrameErrorCode {
	var fe *FrameError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

// Frame 是一帧。按 Type 只有对应的字段有意义：
//
//	open   StreamID、Open
//	data   StreamID、Data
//	end    StreamID
//	rst    StreamID、Code
//	window StreamID（0 为隧道级）、Credit
//	ping、pong  Ts（StreamID 恒为 0）
//	goaway Goaway（StreamID 恒为 0）
type Frame struct {
	Type     FrameType
	StreamID uint32
	Open     *gen.OpenPayload
	Data     []byte
	Code     RstCode
	Credit   uint32
	Ts       float64
	Goaway   *gen.GoawayPayload
}

func knownType(t FrameType) bool { return t >= FrameOpen && t <= FrameGoaway }

func checkStreamID(t FrameType, streamID uint32) error {
	tunnelLevel := t == FramePing || t == FramePong || t == FrameGoaway
	if tunnelLevel && streamID != frameTunnelStream {
		return frameErr(ErrBadFrame, "%s 只能是隧道级（streamId 0）", t)
	}
	streamLevel := t == FrameOpen || t == FrameData || t == FrameEnd || t == FrameRst
	if streamLevel && streamID == frameTunnelStream {
		return frameErr(ErrBadFrame, "%s 不能用 streamId 0", t)
	}
	return nil
}

func headersBytes(headers [][2]string) int {
	total := 0
	for _, h := range headers {
		total += len(h[0]) + len(h[1])
	}
	return total
}

// utf16Len 按 UTF-16 码元计长，与 zod 的字符串长度一致。
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// validateOpen 检查 OPEN 载荷：schema 里的约束，加上 e2e 与 headers 总字节数。
func validateOpen(o *gen.OpenPayload) error {
	bad := func(format string, args ...any) error { return frameErr(ErrBadOpen, format, args...) }
	strs := []string{o.Kind, o.Method, o.Path, o.RemoteIP, o.RelayNode}
	if o.ClientOrigin != nil {
		strs = append(strs, *o.ClientOrigin)
	}
	for _, h := range o.Headers {
		strs = append(strs, h[0], h[1])
	}
	if !validUTF8(strs...) {
		return bad("OPEN 载荷不是合法 UTF-8")
	}
	if !contains(gen.OpenPayloadKindValues, o.Kind) {
		return bad("OPEN 的 kind 不在枚举里")
	}
	if n := utf16Len(o.Method); n < 1 || n > 32 {
		return bad("OPEN 的 method 长度应在 1 到 32 之间")
	}
	if len(o.Path) == 0 || o.Path[0] != '/' {
		return bad("OPEN 的 path 须以 / 开头")
	}
	for _, h := range o.Headers {
		if h[0] == "" {
			return bad("OPEN 的头名不能为空")
		}
	}
	if o.Kind == "e2e" && (o.Method != e2eOpenMethod || o.Path != gen.E2EPath || len(o.Headers) > 0) {
		return bad("e2e 的 OPEN 只能是 GET %s、不带头", gen.E2EPath)
	}
	if headersBytes(o.Headers) > OpenHeadersMax {
		return bad("OPEN 的 headers 超过上限")
	}
	return nil
}

func validateGoaway(g *gen.GoawayPayload) error {
	if !contains(gen.GoawayPayloadReasonValues, g.Reason) {
		return frameErr(ErrBadFrame, "GOAWAY 载荷不符合 schema")
	}
	if g.GraceMs < 0 || g.GraceMs > maxSafeInteger {
		return frameErr(ErrBadFrame, "GOAWAY 载荷不符合 schema")
	}
	return nil
}

func appendOpen(dst []byte, o *gen.OpenPayload) []byte {
	dst = append(dst, `{"kind":`...)
	dst = appendJSString(dst, o.Kind)
	dst = append(dst, `,"method":`...)
	dst = appendJSString(dst, o.Method)
	dst = append(dst, `,"path":`...)
	dst = appendJSString(dst, o.Path)
	dst = append(dst, `,"headers":[`...)
	for i, h := range o.Headers {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '[')
		dst = appendJSString(dst, h[0])
		dst = append(dst, ',')
		dst = appendJSString(dst, h[1])
		dst = append(dst, ']')
	}
	dst = append(dst, `],"clientOrigin":`...)
	if o.ClientOrigin == nil {
		dst = append(dst, "null"...)
	} else {
		dst = appendJSString(dst, *o.ClientOrigin)
	}
	dst = append(dst, `,"remoteIp":`...)
	dst = appendJSString(dst, o.RemoteIP)
	dst = append(dst, `,"relayNode":`...)
	dst = appendJSString(dst, o.RelayNode)
	return append(dst, '}')
}

func appendGoaway(dst []byte, g *gen.GoawayPayload) []byte {
	dst = append(dst, `{"reason":`...)
	dst = appendJSString(dst, g.Reason)
	dst = append(dst, `,"graceMs":`...)
	dst = appendInt(dst, g.GraceMs)
	return append(dst, '}')
}

// Encode 把一帧编成字节（新分配，调用方可以放心交给写协程）。
func Encode(f Frame) ([]byte, error) {
	if !knownType(f.Type) {
		return nil, frameErr(ErrBadFrame, "未知帧类型 %d", f.Type)
	}
	if err := checkStreamID(f.Type, f.StreamID); err != nil {
		return nil, err
	}
	var payload []byte
	switch f.Type {
	case FrameOpen:
		if f.Open == nil {
			return nil, frameErr(ErrBadOpen, "OPEN 缺载荷")
		}
		if err := validateOpen(f.Open); err != nil {
			return nil, err
		}
		payload = appendOpen(nil, f.Open)
	case FrameData:
		if len(f.Data) > MaxDataChunk {
			return nil, frameErr(ErrFrameTooLarge, "DATA %d 字节超过 maxDataChunk", len(f.Data))
		}
		payload = f.Data
	case FrameEnd:
	case FrameRst:
		payload = binary.BigEndian.AppendUint16(nil, uint16(f.Code))
	case FrameWindow:
		if f.Credit == 0 {
			return nil, frameErr(ErrBadFrame, "WINDOW 的 credit 不能为 0")
		}
		payload = binary.BigEndian.AppendUint32(nil, f.Credit)
	case FramePing, FramePong:
		if math.IsNaN(f.Ts) || math.IsInf(f.Ts, 0) {
			return nil, frameErr(ErrBadFrame, "ts 不是有限数")
		}
		payload = binary.BigEndian.AppendUint64(nil, math.Float64bits(f.Ts))
	case FrameGoaway:
		if f.Goaway == nil {
			return nil, frameErr(ErrBadFrame, "GOAWAY 缺载荷")
		}
		if err := validateGoaway(f.Goaway); err != nil {
			return nil, err
		}
		payload = appendGoaway(nil, f.Goaway)
	}
	total := FrameHeaderBytes + len(payload)
	if total > MaxFrameBytes {
		return nil, frameErr(ErrFrameTooLarge, "帧 %d 字节超过上限", total)
	}
	out := make([]byte, total)
	out[0] = byte(f.Type)
	binary.BigEndian.PutUint32(out[1:5], f.StreamID)
	copy(out[FrameHeaderBytes:], payload)
	return out, nil
}

func decodeOpen(payload []byte) (*gen.OpenPayload, error) {
	var open gen.OpenPayload
	if err := schema.Decode(gen.OpenPayloadSchema, payload, &open); err != nil {
		return nil, frameErr(ErrBadOpen, "OPEN 载荷不符合 schema")
	}
	if err := validateOpen(&open); err != nil {
		return nil, err
	}
	return &open, nil
}

func decodeGoaway(payload []byte) (*gen.GoawayPayload, error) {
	var g gen.GoawayPayload
	if err := schema.Decode(gen.GoawayPayloadSchema, payload, &g); err != nil {
		return nil, frameErr(ErrBadFrame, "GOAWAY 载荷不符合 schema")
	}
	return &g, nil
}

// Decode 解一帧。b 必须恰好是一帧（WebSocket 的一条二进制消息）。
// 返回的 Data 是拷贝，调用方可以复用 b。
func Decode(b []byte) (Frame, error) { return decode(b, true) }

// DecodeView 与 Decode 校验完全相同，但返回的 Data 直接指向 b（不拷贝）。
// 数据面的热路径用它：调用方在下一次读取前必须用完 Data。
func DecodeView(b []byte) (Frame, error) { return decode(b, false) }

func decode(b []byte, copyData bool) (Frame, error) {
	if len(b) < FrameHeaderBytes {
		return Frame{}, frameErr(ErrBadFrame, "帧只有 %d 字节", len(b))
	}
	if len(b) > MaxFrameBytes {
		return Frame{}, frameErr(ErrFrameTooLarge, "帧 %d 字节超过上限", len(b))
	}
	t := FrameType(b[0])
	if !knownType(t) {
		return Frame{}, frameErr(ErrBadFrame, "未知帧类型 %d", b[0])
	}
	streamID := binary.BigEndian.Uint32(b[1:5])
	if err := checkStreamID(t, streamID); err != nil {
		return Frame{}, err
	}
	payload := b[FrameHeaderBytes:]
	expect := func(n int) error {
		if len(payload) != n {
			return frameErr(ErrBadFrame, "%s 载荷应为 %d 字节，实为 %d", t, n, len(payload))
		}
		return nil
	}
	f := Frame{Type: t, StreamID: streamID}
	switch t {
	case FrameOpen:
		open, err := decodeOpen(payload)
		if err != nil {
			return Frame{}, err
		}
		f.Open = open
	case FrameData:
		if len(payload) > MaxDataChunk {
			return Frame{}, frameErr(ErrFrameTooLarge, "DATA %d 字节超过 maxDataChunk", len(payload))
		}
		if copyData {
			f.Data = append([]byte{}, payload...)
		} else {
			f.Data = payload
		}
	case FrameEnd:
		if err := expect(0); err != nil {
			return Frame{}, err
		}
	case FrameRst:
		if err := expect(2); err != nil {
			return Frame{}, err
		}
		f.Code = RstCode(binary.BigEndian.Uint16(payload))
	case FrameWindow:
		if err := expect(4); err != nil {
			return Frame{}, err
		}
		f.Credit = binary.BigEndian.Uint32(payload)
		if f.Credit == 0 {
			return Frame{}, frameErr(ErrBadFrame, "WINDOW 的 credit 不能为 0")
		}
	case FramePing, FramePong:
		if err := expect(8); err != nil {
			return Frame{}, err
		}
		f.Ts = math.Float64frombits(binary.BigEndian.Uint64(payload))
		if math.IsNaN(f.Ts) || math.IsInf(f.Ts, 0) {
			return Frame{}, frameErr(ErrBadFrame, "ts 不是有限数")
		}
	case FrameGoaway:
		g, err := decodeGoaway(payload)
		if err != nil {
			return Frame{}, err
		}
		f.Goaway = g
	}
	return f, nil
}
