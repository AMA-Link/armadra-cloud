package relayctl

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// Durable 把签发索引与撤销表落到 PostgreSQL（设计 §7.2）：Redis 里的撤销与索引都能丢，PostgreSQL 里的不能。
//
//   - 签发：先 INSERT cloud_issued_tokens，再交给内层（Redis 的两个有序集合 / 内存实现）。
//   - 撤销：先在 PostgreSQL 里选出未到期的令牌、写 relay_revoked_tokens、从索引删掉，再交给内层撤销并踢人。
//     过滤以 PostgreSQL 为准；内层的索引只是快路径，所以两边都要撤（内层可能有 PostgreSQL 已经删掉的条目：
//     签发方在撤销与登记索引之间交错时，内层仍要被撤到）。
//
// 查询、清理一律带 tenant_id；core 的撤销钩子（登出、撤设备、改口令、撤链接与源）也经这里落盘。
// 重放（serve 启动与 Redis 重连时把撤销表与签发索引重写进 Redis）随 S8 的 Redis 实现。
type Durable struct {
	Control
	store  saas.Store
	tenant string
	now    Clock
	log    *obs.Logger
}

var _ Control = (*Durable)(nil)

// NewDurable 给内层加上落盘；now 缺省取内存时钟的缺省（time.Now）。
func NewDurable(inner Control, store saas.Store, tenantID string, now Clock, log *obs.Logger) *Durable {
	if log == nil {
		log = obs.Nop()
	}
	d := &Durable{Control: inner, store: store, tenant: tenantID, now: now, log: log}
	if d.now == nil {
		d.now = defaultClock
	}
	return d
}

func (d *Durable) nowMs() int64 { return d.now().UnixMilli() }

// IndexIssued 实现 Control：先落库再登记内层。
func (d *Durable) IndexIssued(ctx context.Context, issued Issued) error {
	err := d.store.Tx(ctx, func(tx saas.Tx) error {
		return tx.Issued().Insert(ctx, d.tenant, saas.IssuedToken{
			JTI: issued.JTI, AccountID: issued.Subject, SourceID: issued.SourceID, DeviceID: issued.DeviceID, ExpMs: issued.ExpMs,
		})
	})
	if err != nil {
		return err
	}
	return d.Control.IndexIssued(ctx, issued)
}

// persist 在 PostgreSQL 里撤销符合过滤的令牌（写撤销表、删索引），答撤销了几枚。
func (d *Durable) persist(ctx context.Context, filter Filter, keepDeviceID string) (int, error) {
	issued := saas.IssuedFilter{
		JTI: filter.JTI, AccountID: filter.Subject, AccountPrefix: filter.SubjectPrefix,
		SourceID: filter.SourceID, DeviceID: filter.DeviceID, KeepDeviceID: keepDeviceID,
	}
	if issued.Empty() {
		return 0, nil
	}
	var hit []saas.IssuedToken
	err := d.store.Tx(ctx, func(tx saas.Tx) error {
		var err error
		hit, err = tx.Issued().Revoke(ctx, d.tenant, issued, d.nowMs())
		return err
	})
	return len(hit), err
}

// RevokeWhere 实现 Control：PostgreSQL 与内层都撤；PostgreSQL 失败也要让内层撤（多撤不漏撤），再把错误交回去。
func (d *Durable) RevokeWhere(ctx context.Context, filter Filter) (int, error) {
	stored, storeErr := d.persist(ctx, filter, "")
	live, err := d.Control.RevokeWhere(ctx, filter)
	if storeErr != nil {
		return 0, storeErr
	}
	return max(stored, live), err
}

// Revocations 实现 Control：钩子先落盘再交给内层的钩子（撤令牌、按 (源, 设备) 踢人）；落盘失败只记日志。
func (d *Durable) Revocations() core.RevocationSink {
	return durableSink{d: d, inner: d.Control.Revocations()}
}

type durableSink struct {
	d     *Durable
	inner core.RevocationSink
}

func (s durableSink) persist(ctx context.Context, filter Filter, keepDeviceID string) {
	if _, err := s.d.persist(ctx, filter, keepDeviceID); err != nil {
		s.d.log.Warn("撤销落盘失败", obs.Operation("relayctl.persist"), obs.Error(err))
	}
}

func (s durableSink) DeviceRevoked(ctx context.Context, subject, deviceID string) {
	s.persist(ctx, Filter{Subject: subject, DeviceID: deviceID}, "")
	s.inner.DeviceRevoked(ctx, subject, deviceID)
}

func (s durableSink) DevicesRevoked(ctx context.Context, subject, keepDeviceID string) {
	s.persist(ctx, Filter{Subject: subject}, keepDeviceID)
	s.inner.DevicesRevoked(ctx, subject, keepDeviceID)
}

func (s durableSink) LinkRevoked(ctx context.Context, sourceID, linkID string) {
	s.persist(ctx, Filter{SourceID: sourceID, SubjectPrefix: core.GuestPrefix(linkID)}, "")
	s.inner.LinkRevoked(ctx, sourceID, linkID)
}

func (s durableSink) SourceRevoked(ctx context.Context, sourceID string) {
	s.persist(ctx, Filter{SourceID: sourceID}, "")
	s.inner.SourceRevoked(ctx, sourceID)
}
