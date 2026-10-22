// Package core 是控制核的服务：账号、会话、签名、源、链接、me-stream、流量、挑战与撤销，
// 逐个对应 packages/control-core/src。服务只经 internal/store 的端口读写状态，不碰文件、
// 数据库驱动、HTTP 与中继内核（internal/tunnel、internal/edge）。
package core

import (
	"errors"
	"math"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// Error 是控制面服务抛的错误：只带注册表里的码。HTTP 层按码查状态码，Message 是固定英文短句，
// 界面按码取 i18n 文案。
type Error struct {
	Code gen.ErrorCode
	// RetryAfter 非零时，答复带 Retry-After 与 details.retryAfterMs。
	RetryAfter time.Duration
	// Details 是答复里 details 对象的附加字段（例如 {"setting": "memberHosts"}），不含口令与令牌；
	// 与 RetryAfter 同时存在时 retryAfterMs 以 RetryAfter 为准。
	Details map[string]any
}

func (e *Error) Error() string { return string(e.Code) }

// Message 是码对应的固定英文短句；没登记的码答码本身。
func (e *Error) Message() string { return MessageOf(e.Code) }

// Fail 造一个只带码的错误。
func Fail(code gen.ErrorCode) *Error { return &Error{Code: code} }

// FailWith 造一个带详情的错误；details 会被复制，之后改原 map 不影响错误。
func FailWith(code gen.ErrorCode, details map[string]any) *Error {
	e := &Error{Code: code}
	if len(details) > 0 {
		e.Details = make(map[string]any, len(details))
		for key, value := range details {
			e.Details[key] = value
		}
	}
	return e
}

// FailAfter 造一个带重试等待的错误（等待向上取整到毫秒，负数当 0）。
func FailAfter(code gen.ErrorCode, wait time.Duration) *Error {
	if wait < 0 {
		wait = 0
	}
	return &Error{Code: code, RetryAfter: time.Duration(math.Ceil(float64(wait)/float64(time.Millisecond))) * time.Millisecond}
}

// CodeOf 取错误里的码；不是 *Error 时 ok 为 false。
func CodeOf(err error) (gen.ErrorCode, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return "", false
}

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

// MessageOf 是错误码对应的固定英文短句（与 relay-kernel 的 errorMessage 逐条相同）。
// 没有专门文案的码（例如 not_implemented）答码本身。
func MessageOf(code gen.ErrorCode) string {
	if message, ok := messages[code]; ok {
		return message
	}
	return string(code)
}
