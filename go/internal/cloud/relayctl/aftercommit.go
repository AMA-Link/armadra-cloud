package relayctl

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// committer 是 saas.Tx 里登记「提交之后才执行」回调的那一个方法。
type committer interface {
	AfterCommit(fn func(ctx context.Context))
}

// Deferred 把撤销钩子包成事务感知的：每次回调都挂到 tx.AfterCommit，事务提交成功才投递给 inner，
// 回滚、重试时丢弃（设计 §4.2、§4.11）。在 SaaS 的绑定事务里调用 core 的撤销函数时，core.Context 的
// Revocations 必须换成它（core.Context.Bound）；否则库里还没提交令牌就先被撤了，或者回滚后令牌白撤。
func Deferred(tx committer, inner core.RevocationSink) core.RevocationSink {
	return deferred{tx: tx, inner: inner}
}

type deferred struct {
	tx    committer
	inner core.RevocationSink
}

func (d deferred) DeviceRevoked(_ context.Context, subject, deviceID string) {
	d.tx.AfterCommit(func(ctx context.Context) { d.inner.DeviceRevoked(ctx, subject, deviceID) })
}

func (d deferred) DevicesRevoked(_ context.Context, subject, keepDeviceID string) {
	d.tx.AfterCommit(func(ctx context.Context) { d.inner.DevicesRevoked(ctx, subject, keepDeviceID) })
}

func (d deferred) LinkRevoked(_ context.Context, sourceID, linkID string) {
	d.tx.AfterCommit(func(ctx context.Context) { d.inner.LinkRevoked(ctx, sourceID, linkID) })
}

func (d deferred) SourceRevoked(_ context.Context, sourceID string) {
	d.tx.AfterCommit(func(ctx context.Context) { d.inner.SourceRevoked(ctx, sourceID) })
}
