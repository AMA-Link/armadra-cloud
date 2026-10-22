// Package obs 是中继的可观测性：日志、指标与 /health /ready /metrics 处理器。
//
// 日志只记元数据（AGENTS.md）：字段白名单由编译期封闭的构造函数保证，调用方拿不到
// Field 的内部，也就没法把路径、query、头、令牌、正文塞进日志。
package obs

import (
	"io"
	"reflect"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// Level 是日志级别。
type Level int

// 四个级别，数值与 TS 的 ORDER 同序。
const (
	LevelDebug Level = 10
	LevelInfo  Level = 20
	LevelWarn  Level = 30
	LevelError Level = 40
)

func (l Level) String() string {
	switch {
	case l >= LevelError:
		return "error"
	case l >= LevelWarn:
		return "warn"
	case l >= LevelInfo:
		return "info"
	}
	return "debug"
}

type fieldKind uint8

const (
	kindNone fieldKind = iota
	kindString
	kindInt
)

// Field 是白名单里的一个日志字段；只能由本包的构造函数产生。
type Field struct {
	key  string
	kind fieldKind
	s    string
	n    int64
}

func str(key, v string) Field { return Field{key: key, kind: kindString, s: v} }
func num(key string, v int64) Field {
	return Field{key: key, kind: kindInt, n: v}
}

// 字段构造函数：与 TS 的 RelayLogFields 白名单一一对应，顺序即输出顺序由调用方决定。
func SourceID(v string) Field  { return str("sourceId", v) }
func TunnelID(v string) Field  { return str("tunnelId", v) }
func StreamID(v uint32) Field  { return num("streamId", int64(v)) }
func Node(v string) Field      { return str("node", v) }
func Kind(v string) Field      { return str("kind", v) }
func Result(v string) Field    { return str("result", v) }
func Reason(v string) Field    { return str("reason", v) }
func BytesIn(v int64) Field    { return num("bytesIn", v) }
func BytesOut(v int64) Field   { return num("bytesOut", v) }
func DurationMs(v int64) Field { return num("durationMs", v) }
func Tunnels(v int) Field      { return num("tunnels", int64(v)) }
func Streams(v int) Field      { return num("streams", int64(v)) }
func Mode(v string) Field      { return str("mode", v) }
func URL(v string) Field       { return str("url", v) }
func Listen(v string) Field    { return str("listen", v) }

// Code 记关闭码或错误码；只认整数与字符串，别的类型丢弃（同 TS 丢非标量）。
func Code(v any) Field {
	switch c := v.(type) {
	case string:
		return str("code", c)
	case int:
		return num("code", int64(c))
	case int8:
		return num("code", int64(c))
	case int16:
		return num("code", int64(c))
	case int32:
		return num("code", int64(c))
	case int64:
		return num("code", c)
	case uint8:
		return num("code", int64(c))
	case uint16:
		return num("code", int64(c))
	case uint32:
		return num("code", int64(c))
	}
	return Field{}
}

// Error 只记错误的类型名（如 `*net.OpError`），不记 message：message 里可能带着对端给的内容。
// err 为 nil 时不写这个字段。
func Error(err error) Field {
	if err == nil {
		return Field{}
	}
	return str("error", reflect.TypeOf(err).String())
}

// Logger 一行一个 JSON：{"time","level","name":"relay","msg",...字段}。并发安全。
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
	now   func() time.Time
	buf   []byte
}

// NewLogger 创建写到 w 的日志，低于 level 的丢弃。
func NewLogger(w io.Writer, level Level) *Logger {
	return NewLoggerWithClock(w, level, time.Now)
}

// NewLoggerWithClock 同 NewLogger，时钟可注入（测试用）。
func NewLoggerWithClock(w io.Writer, level Level, now func() time.Time) *Logger {
	return &Logger{w: w, level: level, now: now}
}

// Nop 返回什么都不写的日志。
func Nop() *Logger { return &Logger{level: LevelError + 1} }

// Debug 记调试信息。
func (l *Logger) Debug(msg string, fields ...Field) { l.emit(LevelDebug, msg, fields) }

// Info 记一般信息。
func (l *Logger) Info(msg string, fields ...Field) { l.emit(LevelInfo, msg, fields) }

// Warn 记警告。
func (l *Logger) Warn(msg string, fields ...Field) { l.emit(LevelWarn, msg, fields) }

// Error 记错误。
func (l *Logger) Error(msg string, fields ...Field) { l.emit(LevelError, msg, fields) }

func (l *Logger) emit(level Level, msg string, fields []Field) {
	if l == nil || l.w == nil || level < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buf[:0]
	b = append(b, `{"time":"`...)
	b = l.now().UTC().AppendFormat(b, "2006-01-02T15:04:05.000Z")
	b = append(b, `","level":"`...)
	b = append(b, level.String()...)
	b = append(b, `","name":"relay","msg":`...)
	b = appendJSONString(b, msg)
	for _, f := range fields {
		switch f.kind {
		case kindString:
			b = append(b, ',', '"')
			b = append(b, f.key...)
			b = append(b, '"', ':')
			b = appendJSONString(b, f.s)
		case kindInt:
			b = append(b, ',', '"')
			b = append(b, f.key...)
			b = append(b, '"', ':')
			b = strconv.AppendInt(b, f.n, 10)
		}
	}
	b = append(b, '}', '\n')
	_, _ = l.w.Write(b)
	l.buf = b
}

const hexDigits = "0123456789abcdef"

// appendJSONString 按 JSON.stringify 的规则转义：不转义 < > & 与 U+2028/2029，控制字符用 \uXXXX。
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				b = append(b, '\\', c)
			case c == '\n':
				b = append(b, '\\', 'n')
			case c == '\r':
				b = append(b, '\\', 'r')
			case c == '\t':
				b = append(b, '\\', 't')
			case c == '\b':
				b = append(b, '\\', 'b')
			case c == '\f':
				b = append(b, '\\', 'f')
			case c < 0x20:
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			default:
				b = append(b, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, "�"...)
		} else {
			b = append(b, s[i:i+size]...)
		}
		i += size
	}
	return append(b, '"')
}
