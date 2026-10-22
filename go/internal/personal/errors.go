package personal

import "fmt"

// UsageError 是用法 / 配置错误：CLI 打印后退出码 64。
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Usagef 造一个用法错误。
func Usagef(format string, args ...any) *UsageError {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

// StateCode 是存储 / 状态的问题种类。
type StateCode string

// 存储 / 状态的问题（对应 state.ts 的 StateErrorCode）。
const (
	// NotInitialized：还没有初始化；退出码 64。
	NotInitialized StateCode = "not_initialized"
	// Exists：已经初始化过 / 目标已存在；退出码 64。
	Exists StateCode = "exists"
	// Invalid：坏的、不认识的、对不上的；退出码 1。
	Invalid StateCode = "invalid"
)

// StateError 是存储 / 状态的问题。
type StateError struct {
	Code StateCode
	Msg  string
}

func (e *StateError) Error() string { return e.Msg }

// Statef 造一个存储 / 状态问题。
func Statef(code StateCode, format string, args ...any) *StateError {
	return &StateError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Print 是 CLI 的输出行。
type Print func(line string)
