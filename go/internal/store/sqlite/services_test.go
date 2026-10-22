package sqlite_test

import (
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
)

// 服务合同（16 例）：控制核服务跑在 SQLite 端口上。
func TestServicesContract(t *testing.T) { coretest.ServicesContract(t, create) }
