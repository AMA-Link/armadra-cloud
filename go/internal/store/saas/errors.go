package saas

import (
	"errors"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// Error 是存储层把约束冲突映射出的业务错误；Code 取自 gen，服务层原样换成 core 的错误。
type Error struct {
	Code gen.ErrorCode
	// Constraint 是触发它的约束或索引名，只给日志与测试看，不进响应。
	Constraint string
}

func (e *Error) Error() string { return string(e.Code) }

// Is 让 errors.Is(err, saas.ErrConflict) 按错误码比较，不看约束名。
func (e *Error) Is(target error) bool {
	var other *Error
	return errors.As(target, &other) && other.Code == e.Code
}

// 可以用 errors.Is 判断的映射结果。
var (
	ErrTeamSlugTaken = &Error{Code: gen.ErrTeamSlugTaken}
	ErrEmailTaken    = &Error{Code: gen.ErrEmailTaken}
	ErrConflict      = &Error{Code: gen.ErrConflict}
	ErrTeamLastOwner = &Error{Code: gen.ErrTeamLastOwner}
	ErrMemberExists  = &Error{Code: gen.ErrMemberExists}
)

// 约束与索引名（迁移 0004–0009）。
const (
	ConstraintTeamSlug      = "teams_slug"
	ConstraintAccountsEmail = "accounts_email"
	ConstraintTeamHasOwner  = "team_has_owner"
	ConstraintTeamMembersPK = "team_members_pkey"
)
