package main

import (
	"strings"
	"unicode"
)

// initialisms 是 Go 里习惯全大写的词。
var initialisms = map[string]string{
	"id": "ID", "url": "URL", "ip": "IP", "jwk": "JWK", "jws": "JWS",
	"http": "HTTP", "ws": "WS", "e2e": "E2E", "ttl": "TTL",
}

// words 把 camelCase、kebab-case、snake_case 与 SCREAMING_CASE 拆成小写词。
func words(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '/' || r == ' '
	}) {
		// 全大写的词（SCRYPT）整体当一个词；否则按小写到大写的边界拆。
		if part == strings.ToUpper(part) {
			out = append(out, strings.ToLower(part))
			continue
		}
		start := 0
		runes := []rune(part)
		for i := 1; i < len(runes); i++ {
			if unicode.IsUpper(runes[i]) && !unicode.IsUpper(runes[i-1]) {
				out = append(out, strings.ToLower(string(runes[start:i])))
				start = i
			}
		}
		out = append(out, strings.ToLower(string(runes[start:])))
	}
	return out
}

// pascal 把名字转成导出的 Go 标识符：sourceId → SourceID，TOKEN_HASH_KINDS → TokenHashKinds。
func pascal(s string) string {
	var b strings.Builder
	for _, w := range words(s) {
		if up, ok := initialisms[w]; ok {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(w[:1]) + w[1:])
	}
	return b.String()
}
