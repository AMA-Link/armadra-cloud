package cluster

import (
	"fmt"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
)

func errUnknownKind(kind control.LimitKind) error {
	return fmt.Errorf("未知的限流种类 %q", kind)
}
