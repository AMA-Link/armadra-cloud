package edge

import (
	"encoding/binary"
	"unicode/utf8"
)

// WebSocket 帧（RFC 6455）。边缘自己解析客户端帧、自己编服务端帧，因为事件驱动的连接不留协程。
// 严格程度与 Node 的 ws 库一致：保留位、掩码、控制帧长度与分片顺序出错一律 1002，
// 载荷超过上限 1009，关闭帧的码与原因不合法 1002 / 1007。

// 操作码。
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// 关闭码（RFC 6455 §7.4.1 里边缘会用到的）。
const (
	closeProtocolError   = 1002
	closePolicyViolation = 1008
	closeInvalidData     = 1007
	closeTooBig          = 1009
)

// clientFrame 是解出的一帧；Payload 已解掩码，指向输入缓冲（原地解掩码）。
type clientFrame struct {
	Fin     bool
	Op      byte
	Payload []byte
}

// frameError 是帧层面的协议错误，带该用的关闭码。
type frameError struct {
	code   int
	reason string
}

func (e *frameError) Error() string { return e.reason }

// parseClientFrame 从 d 开头解一帧，答用掉的字节数。不够一帧答 used=0、err=nil。
// maxPayload 是单帧载荷上限。
func parseClientFrame(d []byte, maxPayload int) (f clientFrame, used int, err *frameError) {
	if len(d) < 2 {
		return f, 0, nil
	}
	b0, b1 := d[0], d[1]
	if b0&0x70 != 0 {
		return f, 0, &frameError{closeProtocolError, "RSV 位必须为 0"}
	}
	if b1&0x80 == 0 {
		return f, 0, &frameError{closeProtocolError, "客户端帧必须带掩码"}
	}
	f.Fin = b0&0x80 != 0
	f.Op = b0 & 0x0f
	switch f.Op {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
	default:
		return f, 0, &frameError{closeProtocolError, "未知操作码"}
	}
	length := uint64(b1 & 0x7f)
	header := 2
	switch length {
	case 126:
		if len(d) < 4 {
			return f, 0, nil
		}
		length, header = uint64(binary.BigEndian.Uint16(d[2:4])), 4
	case 127:
		if len(d) < 10 {
			return f, 0, nil
		}
		length, header = binary.BigEndian.Uint64(d[2:10]), 10
		if length>>63 != 0 {
			return f, 0, &frameError{closeProtocolError, "长度的最高位必须为 0"}
		}
	}
	if f.Op >= opClose && (!f.Fin || length > 125) {
		return f, 0, &frameError{closeProtocolError, "控制帧不能分片且载荷不超过 125 字节"}
	}
	if length > uint64(maxPayload) {
		return f, 0, &frameError{closeTooBig, "载荷超过上限"}
	}
	total := header + 4 + int(length)
	if len(d) < total {
		return f, 0, nil
	}
	f.Payload = d[header+4 : total]
	unmask(f.Payload, d[header:header+4])
	return f, total, nil
}

// unmask 原地解掩码，8 字节一组。
func unmask(b, key []byte) {
	k := uint64(binary.LittleEndian.Uint32(key))
	k |= k << 32
	i := 0
	for ; i+8 <= len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], binary.LittleEndian.Uint64(b[i:])^k)
	}
	for ; i < len(b); i++ {
		b[i] ^= key[i&3]
	}
}

// serverFrame 编一个服务端帧（不带掩码、FIN 置位）。
func serverFrame(op byte, payload []byte) []byte {
	n := len(payload)
	var hdr [10]byte
	h := 2
	hdr[0] = 0x80 | op
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xffff:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
		h = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
		h = 10
	}
	out := make([]byte, h+n)
	copy(out, hdr[:h])
	copy(out[h:], payload)
	return out
}

// closePayload 是关闭帧的载荷：2 字节码加原因。
func closePayload(code int, reason string) []byte {
	b := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(b, uint16(code))
	return append(b, reason...)
}

// validCloseCode 按 RFC 6455 §7.4.2 判断关闭码能不能出现在关闭帧里。
func validCloseCode(code int) bool {
	switch {
	case code >= 1000 && code <= 1003, code >= 1007 && code <= 1014:
		return true
	case code >= 3000 && code <= 4999:
		return true
	}
	return false
}

// parseClose 解客户端关闭帧的载荷：空载荷答 code=1005（无码）；
// 一个字节、码不合法答 1002，原因不是 UTF-8 答 1007。
func parseClose(payload []byte) (code int, bad *frameError) {
	switch {
	case len(payload) == 0:
		return 1005, nil
	case len(payload) == 1:
		return 0, &frameError{closeProtocolError, "关闭帧载荷长度不合法"}
	}
	code = int(binary.BigEndian.Uint16(payload))
	if !validCloseCode(code) {
		return 0, &frameError{closeProtocolError, "关闭码不合法"}
	}
	if !utf8.Valid(payload[2:]) {
		return 0, &frameError{closeInvalidData, "关闭原因不是 UTF-8"}
	}
	return code, nil
}
