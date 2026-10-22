package coretest

import (
	"errors"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

func asError(err error, target **core.Error) bool { return errors.As(err, target) }
