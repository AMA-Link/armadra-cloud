package httpx

import (
	"errors"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// MapError 把存储层按约束映射出的业务错误换成控制核的错误（线上形状 { code, message }），
// 处理器只返回 core.Fail / core.FailWith；其余错误原样返回，由 HTTP 层答 500 internal，日志只记类名。
func MapError(err error) error {
	var mapped *saas.Error
	if errors.As(err, &mapped) {
		return core.Fail(mapped.Code)
	}
	return err
}
