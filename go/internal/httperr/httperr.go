// Package httperr 是中继错误答复：`{ code, message, requestId? }`，状态码查协议注册表。
//
// 对应 TS 的 relay-kernel/http/errors.ts、已删除的 TypeScript 版 relay-runtime-node/errors.ts 与
// cloud-shared 的 sendError。code 只能是 gen 里登记的码；message 是固定英文短句。
package httperr

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

var messages = map[gen.ErrorCode]string{
	gen.ErrNotFound:                 "Not found",
	gen.ErrUnauthenticated:          "Unauthenticated",
	gen.ErrRateLimited:              "Too many requests",
	gen.ErrRelayTokenInvalid:        "Relay token missing or invalid",
	gen.ErrRelaySourceMismatch:      "Relay token is for another source",
	gen.ErrSourceOffline:            "Source is offline",
	gen.ErrLimitReached:             "Too many concurrent streams",
	gen.ErrBadGateway:               "Source sent an invalid response",
	gen.ErrStorageUnavailable:       "Relay storage is unavailable",
	gen.ErrNotInitialized:           "Relay is not initialized",
	gen.ErrChallengeRequired:        "Challenge token required",
	gen.ErrChallengeInvalid:         "Challenge token invalid or expired",
	gen.ErrBadRequest:               "Bad request",
	gen.ErrInternal:                 "Internal error",
	gen.ErrForbidden:                "Forbidden",
	gen.ErrUnavailable:              "Unavailable",
	gen.ErrCredentialsInvalid:       "Account or password is incorrect",
	gen.ErrAccountLocked:            "Too many failed sign-in attempts; try again later",
	gen.ErrSessionExpired:           "Session expired",
	gen.ErrSessionRevoked:           "Session revoked",
	gen.ErrPasswordTooShort:         "Password is shorter than the minimum",
	gen.ErrPasswordTooLong:          "Password is too long",
	gen.ErrPasswordContainsName:     "Password must not contain the account name",
	gen.ErrPasswordTooCommon:        "Password is on the list of common passwords",
	gen.ErrRegistrationTokenInvalid: "Registration token missing, used or expired",
	gen.ErrSourceTaken:              "Source id is registered with another key",
	gen.ErrSourceRevoked:            "Source revoked",
	gen.ErrSourceAccessDenied:       "No access to this source",
	gen.ErrProtocolUnsupported:      "Protocol version not supported",
	gen.ErrLinkInvalid:              "Link not found or revoked",
	gen.ErrLinkExpired:              "Link expired",
	gen.ErrLinkExhausted:            "Link has no uses left",
	gen.ErrLinkSecretInvalid:        "Link secret missing or wrong",
}

// Message 返回错误码的固定英文短句；没登记的回退为码本身。
func Message(code gen.ErrorCode) string {
	if m, ok := messages[code]; ok {
		return m
	}
	return string(code)
}

var statusText = map[int]string{
	101: "Switching Protocols",
	200: "OK",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	403: "Forbidden",
	404: "Not Found",
	409: "Conflict",
	410: "Gone",
	426: "Upgrade Required",
	428: "Precondition Required",
	429: "Too Many Requests",
	500: "Internal Server Error",
	501: "Not Implemented",
	502: "Bad Gateway",
	503: "Service Unavailable",
}

// StatusText 返回状态行的原因短语（与 Node 的 STATUS_CODES 逐字相同）；不认识的答空串。
func StatusText(status int) string { return statusText[status] }

// Options 是 Write 的可选项。
type Options struct {
	RequestID  string
	RetryAfter time.Duration
	// CORS 是名单内来源的 CORS 头（边缘算好），让跨源页面读得到错误码。
	CORS map[string]string
}

func statusOf(code gen.ErrorCode) int {
	if s, ok := code.HTTPStatus(); ok {
		return s
	}
	return http.StatusInternalServerError
}

// body 生成 JSON 体：键序 code、message、requestId，不转义 HTML 字符（同 JSON.stringify）。
func body(code gen.ErrorCode, requestID string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	type wire struct {
		Code      gen.ErrorCode `json:"code"`
		Message   string        `json:"message"`
		RequestID string        `json:"requestId,omitempty"`
	}
	_ = enc.Encode(wire{code, Message(code), requestID})
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// Write 把错误答复写进 w：先 CORS 头，再 retry-after（至少 1 秒，向上取整），
// 然后 content-type、content-length、cache-control: no-store 和 JSON 体。
func Write(w http.ResponseWriter, code gen.ErrorCode, o Options) {
	h := w.Header()
	for name, value := range o.CORS {
		h.Set(name, value)
	}
	if o.RetryAfter > 0 {
		secs := math.Max(1, math.Ceil(o.RetryAfter.Seconds()))
		h.Set("retry-after", strconv.FormatInt(int64(secs), 10))
	}
	payload := body(code, o.RequestID)
	h.Set("content-type", "application/json; charset=utf-8")
	h.Set("content-length", strconv.Itoa(len(payload)))
	h.Set("cache-control", "no-store")
	w.WriteHeader(statusOf(code))
	_, _ = w.Write(payload)
}

// Raw 返回原始 HTTP/1.1 错误响应的字节（升级请求被拒、或升级途中源下线时写给裸连接）。
// 头顺序与 TS 的 rawErrorResponse 相同；CORS 头按键名排序输出，保证字节确定
// （TS 按对象插入序，边缘只会给出固定的三个键）。
func Raw(code gen.ErrorCode, cors map[string]string) []byte {
	status := statusOf(code)
	payload := body(code, "")
	var b strings.Builder
	b.WriteString("HTTP/1.1 " + strconv.Itoa(status) + " " + StatusText(status) + "\r\n")
	b.WriteString("content-type: application/json; charset=utf-8\r\n")
	b.WriteString("content-length: " + strconv.Itoa(len(payload)) + "\r\n")
	b.WriteString("cache-control: no-store\r\nconnection: close\r\n")
	names := make([]string, 0, len(cors))
	for name := range cors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString(name + ": " + cors[name] + "\r\n")
	}
	b.WriteString("\r\n")
	return append([]byte(b.String()), payload...)
}
