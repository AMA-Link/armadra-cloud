package core

import (
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 口令的策略与派生由 internal/identity 实现（参数全部来自协议包生成物）；这里只是服务用到的薄封装。

func violation(password string, names []string) gen.ErrorCode {
	return identity.Violation(password, names, nil)
}
