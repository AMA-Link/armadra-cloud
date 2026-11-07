package budget

import (
	"context"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// ReasonDailyBudgetExceeded 是被每日配额拒绝时 WS 关闭帧里的原因。错误码表是封闭的，所以码仍是 rate_limited。
const ReasonDailyBudgetExceeded = "daily_budget_exceeded"

// Gate 把每日配额接到边缘的准入（edge.Gate）：令牌验过、通道要开成之前占一个名额，超额答
// rate_limited（4429），关闭原因 daily_budget_exceeded。Refuse 不拦：未认证的请求不碰配额。
type Gate struct {
	Budget *Budget
	// OnExceeded 每个 UTC 日第一次拒绝时调用（记日志），可为空。
	OnExceeded func(retryAfter time.Duration)
}

var _ edge.ReasonedGate = (*Gate)(nil)

// Refuse 不拦：配额只在令牌验过之后计，匿名请求烧不掉它。
func (g *Gate) Refuse() gen.ErrorCode { return "" }

// Admit 占一个名额；超额答 rate_limited。
func (g *Gate) Admit(context.Context, edge.GateInput) (func(), gen.ErrorCode, error) {
	v := g.Budget.Admit()
	if !v.OK {
		if v.First && g.OnExceeded != nil {
			g.OnExceeded(v.RetryAfter)
		}
		return nil, gen.ErrRateLimited, nil
	}
	return func() {}, "", nil
}

// RefusalReason 实现 edge.ReasonedGate。
func (g *Gate) RefusalReason(code gen.ErrorCode) string {
	if code == gen.ErrRateLimited {
		return ReasonDailyBudgetExceeded
	}
	return ""
}
