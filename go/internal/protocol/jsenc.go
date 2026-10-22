package protocol

import (
	"strconv"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

// appendJSString 按 JavaScript 的 JSON.stringify 写一个字符串，字节与 TypeScript 那一侧
// 逐一相同：只转义引号、反斜杠与 0x20 以下的控制字符；<、>、&、U+2028、U+2029 与 DEL 原样输出。
// s 必须是合法 UTF-8，调用方先查。
func appendJSString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			dst = append(dst, '\\', '"')
		case c == '\\':
			dst = append(dst, '\\', '\\')
		case c == '\b':
			dst = append(dst, '\\', 'b')
		case c == '\f':
			dst = append(dst, '\\', 'f')
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"')
}

func validUTF8(values ...string) bool {
	for _, v := range values {
		if !utf8.ValidString(v) {
			return false
		}
	}
	return true
}

func appendInt(dst []byte, n int64) []byte { return strconv.AppendInt(dst, n, 10) }
