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
//   - 撤销：先在 PostgreSQL 里选出未到期的令牌、写 relay_revoked_tokens、从索引删掉，再把命中的令牌按 jti 逐个
//     交给内层撤销，并让内层撤它自己索引里符合过滤的。过滤以 PostgreSQL 为准；内层的索引只是快路径，两边都要撤
//     （内层可能缺 PostgreSQL 有的，也可能有 PostgreSQL 已经删掉的——签发方在撤销与登记索引之间交错时）。
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

// persist 在 PostgreSQL 里撤销符合过滤的令牌（写撤销表、删索引），答被撤销的条目。
func (d *Durable) persist(ctx context.Context, filter Filter, keepDeviceID string) ([]Issued, error) {
	issued := saas.IssuedFilter{
		JTI: filter.JTI, AccountID: filter.Subject, AccountPrefix: filter.SubjectPrefix,
		SourceID: filter.SourceID, DeviceID: filter.DeviceID, KeepDeviceID: keepDeviceID,
	}
	if issued.Empty() {
		return nil, nil
	}
	var hit []saas.IssuedToken
	err := d.store.Tx(ctx, func(tx saas.Tx) error {
		var err error
		hit, err = tx.Issued().Revoke(ctx, d.tenant, issued, d.nowMs())
		return err
	})
	out := make([]Issued, 0, len(hit))
	for _, token := range hit {
		out = append(out, Issued{Subject: token.AccountID, SourceID: token.SourceID, DeviceID: token.DeviceID, JTI: token.JTI, ExpMs: token.ExpMs})
	}
	return out, err
}

// Revoke 实现 Control：PostgreSQL 命中的令牌按 jti 逐个交给内层撤（内层索引缺了它们——重启、Redis 丢数据——也撤到），
// 再让内层按过滤撤它自己索引里的；答两边命中的并集（按 jti 去重）。PostgreSQL 失败也要让内层撤（多撤不漏撤），再把错误交回去。
func (d *Durable) Revoke(ctx context.Context, filter Filter, keepDeviceID string) ([]Issued, error) {
	stored, storeErr := d.persist(ctx, filter, keepDeviceID)
	seen := map[string]bool{}
	var hits []Issued
	add := func(entries []Issued) {
		for _, entry := range entries {
			if !seen[entry.JTI] {
				seen[entry.JTI] = true
				hits = append(hits, entry)
			}
		}
	}
	add(stored)
	for _, entry := range stored {
		// 内层索引可能缺这枚：先补登记再按 jti 撤，内层才有它的到期时间可记进撤销表。
		if err := d.Control.IndexIssued(ctx, entry); err != nil {
			return hits, err
		}
		live, err := d.Control.Revoke(ctx, Filter{JTI: entry.JTI}, "")
		if err != nil {
			return hits, err
		}
		add(live)
	}
	live, err := d.Control.Revoke(ctx, filter, keepDeviceID)
	add(live)
	if storeErr != nil {
		return hits, storeErr
	}
	return hits, err
}

// RevokeWhere 实现 Control。
func (d *Durable) RevokeWhere(ctx context.Context, filter Filter) (int, error) {
	hits, err := d.Revoke(ctx, filter, "")
	return len(hits), err
}

// Revocations 实现 Control：钩子先落盘再撤销；踢人用 PostgreSQL 命中与内层命中的并集，按 (源, subject, 设备) 去重发 Kick；
// 落盘失败只记日志（内层照撤）。
func (d *Durable) Revocations() core.RevocationSink { return durableSink{d: d} }

type durableSink struct{ d *Durable }

func (s durableSink) revoke(filter Filter, keepDeviceID, reason string, kick bool) {
	ctx := context.Background()
	hits, err := s.d.Revoke(ctx, filter, keepDeviceID)
	if err != nil {
		s.d.log.Warn("撤销落盘失败", obs.Operation("relayctl.persist"), obs.Error(err))
	}
	if !kick {
		return
	}
	type key struct{ source, subject, device string }
	seen := map[key]bool{}
	for _, entry := range hits {
		k := key{entry.SourceID, entry.Subject, entry.DeviceID}
		if entry.SourceID == "" || entry.Subject == NoAccount || seen[k] {
			continue
		}
		seen[k] = true
		if err := s.d.Control.Kick(ctx, KickFilter{SourceID: k.source, Subject: k.subject, DeviceID: k.device}, reason); err != nil {
			s.d.log.Warn("踢人失败", obs.Operation("relayctl.kick"), obs.SourceID(k.source), obs.Error(err))
		}
	}
}

func (s durableSink) DeviceRevoked(_ context.Context, subject, deviceID string) {
	s.revoke(Filter{Subject: subject, DeviceID: deviceID}, "", "device_revoked", true)
}

func (s durableSink) DevicesRevoked(_ context.Context, subject, keepDeviceID string) {
	s.revoke(Filter{Subject: subject}, keepDeviceID, "devices_revoked", true)
}

// LinkRevoked 与 SourceRevoked 只撤令牌：踢人与关隧道由 core 的 Disconnect / CloseTunnel 做了。
func (s durableSink) LinkRevoked(_ context.Context, sourceID, linkID string) {
	s.revoke(Filter{SourceID: sourceID, SubjectPrefix: core.GuestPrefix(linkID)}, "", "", false)
}

func (s durableSink) SourceRevoked(_ context.Context, sourceID string) {
	s.revoke(Filter{SourceID: sourceID}, "", "", false)
}
