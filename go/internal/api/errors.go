package api

import (
	"fmt"
	"net/url"
)

func errUnsupportedInput(procedure string) error {
	return fmt.Errorf("过程 %s 的入参 schema 用了生成器还不认识的关键字，不能启用", procedure)
}

func errUnknownProcedure(procedure string) error {
	return fmt.Errorf("契约里没有这个过程：%s", procedure)
}

// unescape 解路径参数的百分号编码；解不开就原样用（后面的入参校验会拒绝不合格式的值）。
func unescape(segment string) string {
	if decoded, err := url.PathUnescape(segment); err == nil {
		return decoded
	}
	return segment
}
