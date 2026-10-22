package postgres_test

import (
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
)

// 服务合同（16 例）：控制核服务跑在 PostgreSQL 端口上（无库时跳过）。
func TestServicesContract(t *testing.T) { coretest.ServicesContract(t, create) }
