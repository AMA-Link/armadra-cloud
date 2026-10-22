// Package storetest 是存储端口的合同测试：packages/control-core/src/testing/ports-contract.ts
// 的 15 个用例逐例翻译成 Go，SQLite 与 PostgreSQL 各跑一遍。只在 _test.go 里用。
//
// 用法：storetest.PortsContract(t, func(t *testing.T) store.Backend { 新建后端 })。
// 每个用例拿一份新后端，建好 t1、t2 两个租户。
package storetest

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// Now 是合同里固定的「当前」：2026-10-10 12:00 UTC。
var Now = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC).UnixMilli()

const (
	hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	d1    = "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"
	d2    = "d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2"
	d3    = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
	s1    = "51515151515151515151515151515151"
	s2    = "52525252525252525252525252525252"
	l1    = "1111111111111111"
	l2    = "2222222222222222"
)

// Settings 是合同里 t1 的租户设置。
func Settings() store.TenantSettings {
	return store.TenantSettings{
		Issuer:         "https://relay.armadra.test",
		AllowGuests:    true,
		TrustedOrigins: []string{"https://app.armadra.test"},
	}
}

func password() identity.PasswordRecord {
	return identity.PasswordRecord{KDF: "scrypt", Cost: 16384, Block: 8, Parallel: 1, Length: 32, Salt: "c2FsdA==", Hash: "aGFzaA=="}
}

// P 取值的指针。
func P[T any](v T) *T { return &v }

func account(name, accountID string) store.AccountRecord {
	if accountID == "" {
		accountID = "acct:" + name
	}
	return store.AccountRecord{
		AccountID: accountID, Name: name, Password: password(), CreatedAtMs: 1, UpdatedAtMs: 1,
	}
}

func device(deviceID, subject string, mutate ...func(*store.DeviceRecord)) store.DeviceRecord {
	d := store.DeviceRecord{
		DeviceID: deviceID, Subject: subject, Platform: "desktop", Name: "d-" + deviceID,
		RefreshHash: hashA, Rotation: 1, CreatedAtMs: Now, ExpiresAtMs: Now + 86_400_000,
	}
	for _, m := range mutate {
		m(&d)
	}
	return d
}

func source(sourceID, name string) store.SourceRecord {
	return store.SourceRecord{
		SourceID: sourceID, Name: name, Kind: "desktop",
		PublicJWK:      gen.Ed25519PublicJWK{Kty: "OKP", Crv: "Ed25519", X: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
		CoreVersion:    "0.3.0",
		Capabilities:   []string{"terminal"},
		RegisteredAtMs: Now,
		TrustedOrigins: []string{},
	}
}

func link(linkID, sourceID string, mutate ...func(*store.LinkRecord)) store.LinkRecord {
	l := store.LinkRecord{
		LinkID: linkID, SourceID: sourceID, InvitationID: "inv-" + linkID, Label: "", Role: "viewer",
		SecretHash: hashA, CreatedAtMs: Now, ExpiresAtMs: Now + 60_000,
	}
	for _, m := range mutate {
		m(&l)
	}
	return l
}

type env struct {
	t     *testing.T
	ctx   context.Context
	b     store.Backend
	ports *store.Ports
}

func fresh(t *testing.T, create func(t *testing.T) store.Backend) *env {
	t.Helper()
	ctx := context.Background()
	b := create(t)
	t.Cleanup(func() { _ = b.Close() })
	e := &env{t: t, ctx: ctx, b: b, ports: b.Ports()}
	e.must(b.CreateTenant(ctx, "t1", store.Tenant{TenantSettings: Settings()}))
	other := Settings()
	other.AllowGuests = false
	e.must(b.CreateTenant(ctx, "t2", store.Tenant{TenantSettings: other}))
	return e
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func eq(t *testing.T, got, want any, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s：\n得到 %+v\n应为 %+v", what, got, want)
	}
}

func (e *env) device(tenant, id string) *store.DeviceRecord {
	e.t.Helper()
	d, err := e.ports.Devices.Get(e.ctx, tenant, id)
	e.must(err)
	return d
}

func (e *env) insertDevice(tenant string, d store.DeviceRecord, nowMs int64) {
	e.t.Helper()
	e.must(e.ports.Devices.Insert(e.ctx, tenant, d, nowMs))
}

func revokedAt(d *store.DeviceRecord) any {
	if d == nil {
		return "missing"
	}
	if d.RevokedAtMs == nil {
		return nil
	}
	return *d.RevokedAtMs
}

// PortsContract 对一个存储实现跑端口合同（15 例：10 个端口例 + 5 个流量统计例）。
func PortsContract(t *testing.T, create func(t *testing.T) store.Backend) {
	t.Run("端口合同", func(t *testing.T) {
		t.Run("租户设置按租户答；没有的租户抛错", func(t *testing.T) { // 例 1
			e := fresh(t, create)
			got, err := e.ports.Tenants.Get(e.ctx, "t1")
			e.must(err)
			eq(t, got, Settings(), "t1 设置")
			t2, err := e.ports.Tenants.Get(e.ctx, "t2")
			e.must(err)
			if t2.AllowGuests {
				t.Fatal("t2 不该允许访客")
			}
			if _, err := e.ports.Tenants.Get(e.ctx, "nope"); err == nil {
				t.Fatal("没有的租户应该报错")
			}
		})

		t.Run("账号：按名 / id 读、读答副本、租户隔离", func(t *testing.T) { // 例 2
			e := fresh(t, create)
			e.must(e.b.AddAccount(e.ctx, "t1", account("alice", "u-1")))
			e.must(e.b.AddAccount(e.ctx, "t2", account("alice", "u-2")))
			byName, err := e.ports.Accounts.FindByName(e.ctx, "t1", "alice")
			e.must(err)
			want := account("alice", "u-1")
			eq(t, byName, &want, "按名读")
			other, err := e.ports.Accounts.FindByName(e.ctx, "t2", "alice")
			e.must(err)
			if other == nil || other.AccountID != "u-2" {
				t.Fatalf("t2 的 alice：%+v", other)
			}
			if got, err := e.ports.Accounts.Get(e.ctx, "t1", "u-2"); err != nil || got != nil {
				t.Fatalf("t1 不该看到 u-2：%+v %v", got, err)
			}
			if got, err := e.ports.Accounts.FindByName(e.ctx, "t1", "bob"); err != nil || got != nil {
				t.Fatalf("bob 不该存在：%+v %v", got, err)
			}
			copied, _ := e.ports.Accounts.Get(e.ctx, "t1", "u-1")
			copied.Failures = 99
			again, _ := e.ports.Accounts.Get(e.ctx, "t1", "u-1")
			if again.Failures != 0 {
				t.Fatal("读应答副本")
			}
		})

		t.Run("update：改副本才写；fn 抛错什么也不写；不存在时 fn 拿到 nil", func(t *testing.T) { // 例 3
			e := fresh(t, create)
			e.must(e.b.AddAccount(e.ctx, "t1", account("alice", "u-1")))
			e.must(e.ports.Accounts.Update(e.ctx, "t1", "u-1", func(d *store.AccountRecord) error {
				d.Failures = 2
				return nil
			}))
			got, _ := e.ports.Accounts.Get(e.ctx, "t1", "u-1")
			if got.Failures != 2 {
				t.Fatalf("failures = %d", got.Failures)
			}
			boom := errBoom{}
			err := e.ports.Accounts.Update(e.ctx, "t1", "u-1", func(d *store.AccountRecord) error {
				d.Failures = 7
				return boom
			})
			if err != boom {
				t.Fatalf("应原样返回 fn 的错误：%v", err)
			}
			got, _ = e.ports.Accounts.Get(e.ctx, "t1", "u-1")
			if got.Failures != 2 {
				t.Fatalf("fn 出错后不该写：failures = %d", got.Failures)
			}
			sawNil := false
			e.must(e.ports.Accounts.Update(e.ctx, "t1", "u-9", func(d *store.AccountRecord) error {
				sawNil = d == nil
				return nil
			}))
			if !sawNil {
				t.Fatal("账号不存在时 fn 应拿到 nil")
			}
			if got, _ := e.ports.Accounts.Get(e.ctx, "t1", "u-9"); got != nil {
				t.Fatal("不该凭空造出账号")
			}
		})

		t.Run("resetPassword：换口令、清锁，只撤销本人未撤销的设备", func(t *testing.T) { // 例 4
			e := fresh(t, create)
			locked := account("alice", "u-1")
			locked.Failures, locked.LockedUntilMs = 5, Now+1
			e.must(e.b.AddAccount(e.ctx, "t1", locked))
			e.insertDevice("t1", device(d1, "u-1"), Now)
			e.insertDevice("t1", device(d2, "u-1", func(d *store.DeviceRecord) { d.RevokedAtMs = P(Now - 5) }), Now)
			e.insertDevice("t1", device(d3, "guest:"+l1+":ab"), Now)
			e.insertDevice("t2", device(d1, "u-1"), Now)
			next := password()
			next.Salt, next.Hash = "bmV3", "bmV3aGFzaA=="
			ok, err := e.ports.Accounts.ResetPassword(e.ctx, "t1", "u-1", next, Now, "")
			e.must(err)
			if !ok {
				t.Fatal("账号存在应答 true")
			}
			got, _ := e.ports.Accounts.Get(e.ctx, "t1", "u-1")
			if !reflect.DeepEqual(got.Password, next) || got.UpdatedAtMs != Now || got.Failures != 0 || got.LockedUntilMs != 0 {
				t.Fatalf("口令没换干净：%+v", got)
			}
			eq(t, revokedAt(e.device("t1", d1)), any(Now), "d1 撤销")
			eq(t, revokedAt(e.device("t1", d2)), any(Now-5), "d2 保持原撤销时间")
			eq(t, revokedAt(e.device("t1", d3)), nil, "访客设备不受影响")
			eq(t, revokedAt(e.device("t2", d1)), nil, "别的租户不受影响")
			ok, err = e.ports.Accounts.ResetPassword(e.ctx, "t1", "nobody", next, Now, "")
			e.must(err)
			if ok {
				t.Fatal("账号不存在应答 false")
			}
		})

		t.Run("resetPassword：keepDeviceId 留下当前设备，其余本人设备照样撤销", func(t *testing.T) { // 例 5
			e := fresh(t, create)
			e.must(e.b.AddAccount(e.ctx, "t1", account("alice", "u-1")))
			e.insertDevice("t1", device(d1, "u-1"), Now)
			e.insertDevice("t1", device(d2, "u-1"), Now)
			e.insertDevice("t1", device(d3, "guest:"+l1+":ab"), Now)
			next := password()
			next.Salt, next.Hash = "bmV3", "bmV3aGFzaA=="
			ok, err := e.ports.Accounts.ResetPassword(e.ctx, "t1", "u-1", next, Now, d2)
			e.must(err)
			if !ok {
				t.Fatal("应答 true")
			}
			eq(t, revokedAt(e.device("t1", d1)), any(Now), "d1 撤销")
			eq(t, revokedAt(e.device("t1", d2)), nil, "d2 保留")
			eq(t, revokedAt(e.device("t1", d3)), nil, "访客保留")
			got, _ := e.ports.Accounts.Get(e.ctx, "t1", "u-1")
			eq(t, got.Password, next, "口令记录")
		})

		t.Run("设备：按创建顺序列出；新增时清掉早已失效的；update 与 touch 只往后推", func(t *testing.T) { // 例 6
			e := fresh(t, create)
			refresh := int64(gen.IdentitySessionRefreshTTLMs)
			stale := device(d3, "u-1", func(d *store.DeviceRecord) { d.ExpiresAtMs = Now - refresh - 1 })
			e.insertDevice("t1", stale, Now-refresh*3)
			e.insertDevice("t1", device(d2, "u-1"), Now)
			e.insertDevice("t1", device(d1, "u-1"), Now)
			list, err := e.ports.Devices.ListBySubject(e.ctx, "t1", "u-1")
			e.must(err)
			var ids []string
			for _, d := range list {
				ids = append(ids, d.DeviceID)
			}
			eq(t, ids, []string{d2, d1}, "按创建顺序")
			if e.device("t1", d3) != nil {
				t.Fatal("早已失效的设备应被清掉")
			}
			other, _ := e.ports.Devices.ListBySubject(e.ctx, "t2", "u-1")
			if len(other) != 0 {
				t.Fatal("租户隔离")
			}

			e.must(e.ports.Devices.Update(e.ctx, "t1", d1, func(d *store.DeviceRecord) error {
				prev := d.RefreshHash
				d.PreviousRefreshHash = &prev
				d.RefreshHash = hashB
				d.Rotation++
				return nil
			}))
			got := e.device("t1", d1)
			if got.RefreshHash != hashB || got.PreviousRefreshHash == nil || *got.PreviousRefreshHash != hashA || got.Rotation != 2 {
				t.Fatalf("update 结果：%+v", got)
			}
			e.must(e.ports.Devices.Touch(e.ctx, "t1", map[string]int64{d1: Now + 10, d2: Now + 20, "ffffffffffffffffffffffffffffffff": Now}))
			e.must(e.ports.Devices.Touch(e.ctx, "t1", map[string]int64{d1: Now + 5}))
			if v := e.device("t1", d1).LastSeenAtMs; v == nil || *v != Now+10 {
				t.Fatalf("d1 lastSeen = %v", v)
			}
			if v := e.device("t1", d2).LastSeenAtMs; v == nil || *v != Now+20 {
				t.Fatalf("d2 lastSeen = %v", v)
			}
		})

		t.Run("源：put 新增与替换（替换后排到最后）、update、按顺序列出、租户隔离", func(t *testing.T) { // 例 7
			e := fresh(t, create)
			first, err := e.ports.Sources.Put(e.ctx, "t1", s1, func(existing *store.SourceRecord) (store.SourceRecord, error) {
				if existing != nil {
					t.Fatal("首次 put 时 existing 应为 nil")
				}
				return source(s1, "one"), nil
			})
			e.must(err)
			eq(t, first, source(s1, "one"), "put 的返回值")
			_, err = e.ports.Sources.Put(e.ctx, "t1", s2, func(*store.SourceRecord) (store.SourceRecord, error) {
				return source(s2, "two"), nil
			})
			e.must(err)
			_, err = e.ports.Sources.Put(e.ctx, "t1", s1, func(existing *store.SourceRecord) (store.SourceRecord, error) {
				next := source(s1, "mac")
				if existing != nil {
					next = *existing
				}
				next.CoreVersion = "0.4.0"
				return next, nil
			})
			e.must(err)
			list, _ := e.ports.Sources.List(e.ctx, "t1")
			var ids []string
			for _, s := range list {
				ids = append(ids, s.SourceID)
			}
			eq(t, ids, []string{s2, s1}, "替换后排到最后")
			got, _ := e.ports.Sources.Get(e.ctx, "t1", s1)
			if got.CoreVersion != "0.4.0" {
				t.Fatalf("coreVersion = %s", got.CoreVersion)
			}
			e.must(e.ports.Sources.Update(e.ctx, "t1", s2, func(d *store.SourceRecord) error {
				d.RevokedAtMs = P(Now)
				return nil
			}))
			got, _ = e.ports.Sources.Get(e.ctx, "t1", s2)
			if got.RevokedAtMs == nil || *got.RevokedAtMs != Now {
				t.Fatalf("revokedAtMs = %v", got.RevokedAtMs)
			}
			if list, _ := e.ports.Sources.List(e.ctx, "t2"); len(list) != 0 {
				t.Fatal("t2 不该有源")
			}
			if got, _ := e.ports.Sources.Get(e.ctx, "t2", s1); got != nil {
				t.Fatal("t2 不该看到 t1 的源")
			}
		})

		t.Run("注册令牌：有效的满了答 false；过期的不算并被清掉；一次性消费，答签发账号", func(t *testing.T) { // 例 8
			e := fresh(t, create)
			add := func(hash string, expires int64, nowMs int64) bool {
				ok, err := e.ports.Registrations.Add(e.ctx, "t1",
					store.RegistrationToken{TokenHash: hash, ExpiresAtMs: expires, AccountID: "u-1"}, nowMs, 2)
				e.must(err)
				return ok
			}
			h := func(pair string) string { return repeat(pair, 32) }
			if !add(h("01"), Now+10, Now) || !add(h("02"), Now+1000, Now) {
				t.Fatal("前两条应加入")
			}
			if add(h("03"), Now+1000, Now) {
				t.Fatal("有效的已满应答 false")
			}
			if !add(h("03"), Now+1000, Now+20) {
				t.Fatal("第一条过期之后又能加")
			}
			consume := func(tenant, hash string, nowMs int64) (string, bool) {
				id, ok, err := e.ports.Registrations.Consume(e.ctx, tenant, hash, nowMs)
				e.must(err)
				return id, ok
			}
			if _, ok := consume("t1", h("01"), Now+20); ok {
				t.Fatal("过期的不能消费")
			}
			if _, ok := consume("t2", h("02"), Now); ok {
				t.Fatal("租户隔离")
			}
			if id, ok := consume("t1", h("02"), Now); !ok || id != "u-1" {
				t.Fatalf("消费 02：%q %v", id, ok)
			}
			if _, ok := consume("t1", h("02"), Now); ok {
				t.Fatal("一次性")
			}
			if _, ok := consume("t1", h("03"), Now+2000); ok {
				t.Fatal("过期的不能消费")
			}
		})

		t.Run("链接：范围原样往返（整台时没有 scope 键）、按源过滤、按创建顺序、update", func(t *testing.T) { // 例 9
			e := fresh(t, create)
			e.must(e.ports.Links.Insert(e.ctx, "t1", link(l1, s1)))
			e.must(e.ports.Links.Insert(e.ctx, "t1", link(l2, s2, func(l *store.LinkRecord) {
				l.MaxUses = P(int64(3))
				l.Scope = &store.LinkScope{WorkspaceID: P("ws_01"), ReadOnly: P(true)}
			})))
			plain, _ := e.ports.Links.Get(e.ctx, "t1", l1)
			want := link(l1, s1)
			eq(t, plain, &want, "整台链接")
			if plain.Scope != nil {
				t.Fatal("整台时不该有 scope")
			}
			scoped, _ := e.ports.Links.Get(e.ctx, "t1", l2)
			eq(t, scoped.Scope, &store.LinkScope{WorkspaceID: P("ws_01"), ReadOnly: P(true)}, "范围")
			all, _ := e.ports.Links.List(e.ctx, "t1", "")
			eq(t, []string{all[0].LinkID, all[1].LinkID}, []string{l1, l2}, "按创建顺序")
			bySource, _ := e.ports.Links.List(e.ctx, "t1", s2)
			if len(bySource) != 1 || bySource[0].LinkID != l2 {
				t.Fatalf("按源过滤：%+v", bySource)
			}
			if other, _ := e.ports.Links.List(e.ctx, "t2", ""); len(other) != 0 {
				t.Fatal("租户隔离")
			}
			e.must(e.ports.Links.Update(e.ctx, "t1", l2, func(d *store.LinkRecord) error {
				d.Uses++
				return nil
			}))
			scoped, _ = e.ports.Links.Get(e.ctx, "t1", l2)
			if scoped.Uses != 1 {
				t.Fatalf("uses = %d", scoped.Uses)
			}
		})

		t.Run("撤销链接：连同它名下的访客设备；答源；再撤一次答 null", func(t *testing.T) { // 例 10
			e := fresh(t, create)
			e.must(e.ports.Links.Insert(e.ctx, "t1", link(l1, s1)))
			e.must(e.ports.Links.Insert(e.ctx, "t1", link(l2, s1)))
			e.insertDevice("t1", device(d1, "guest:"+l1+":aa"), Now)
			e.insertDevice("t1", device(d2, "guest:"+l2+":bb"), Now)
			e.insertDevice("t1", device(d3, "u-1"), Now)
			source, ok, err := e.ports.Links.Revoke(e.ctx, "t1", l1, Now)
			e.must(err)
			if !ok || source != s1 {
				t.Fatalf("撤销应答源：%q %v", source, ok)
			}
			got, _ := e.ports.Links.Get(e.ctx, "t1", l1)
			if got.RevokedAtMs == nil || *got.RevokedAtMs != Now {
				t.Fatal("链接应记撤销时间")
			}
			eq(t, revokedAt(e.device("t1", d1)), any(Now), "名下访客设备撤销")
			eq(t, revokedAt(e.device("t1", d2)), nil, "别的链接的访客不受影响")
			eq(t, revokedAt(e.device("t1", d3)), nil, "本人设备不受影响")
			if _, ok, _ := e.ports.Links.Revoke(e.ctx, "t1", l1, Now+1); ok {
				t.Fatal("再撤一次应答 null")
			}
			if _, ok, _ := e.ports.Links.Revoke(e.ctx, "t2", l2, Now); ok {
				t.Fatal("别的租户撤不了")
			}
		})
	})

	t.Run("流量统计", func(t *testing.T) { trafficContract(t, create) })
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
