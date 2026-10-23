package store

import (
	"errors"
	"fmt"
)

// ErrKind 是库的问题种类。调用方打印 Msg 后退出；Msg 不带库内容、连接串与口令。
type ErrKind string

// 库的问题。
const (
	// KindNotFound：库不存在（或没有这个租户）。
	KindNotFound ErrKind = "not_found"
	// KindExists：要新建的库（或租户）已存在。
	KindExists ErrKind = "exists"
	// KindUnknown：不是本程序的库、没有账本、账本不连续或对不上。
	KindUnknown ErrKind = "unknown"
	// KindCorrupt：打不开或校验不过。
	KindCorrupt ErrKind = "corrupt"
	// KindNewer：库是更新的版本写的。
	KindNewer ErrKind = "newer"
	// KindBehind：库的迁移落后于程序（需要先运行迁移命令）。
	KindBehind ErrKind = "behind"
	// KindUnreachable：连不上（PostgreSQL）。
	KindUnreachable ErrKind = "unreachable"
)

// Error 是库的问题。
type Error struct {
	Kind ErrKind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Errorf 造一个库的问题。
func Errorf(kind ErrKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// KindOf 取错误的种类；不是 *Error 时返回空串。
func KindOf(err error) ErrKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

// ErrNoTenant 表示没有这个租户。
var ErrNoTenant = errors.New("没有这个租户")
