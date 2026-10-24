package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/storetest"
)

// 请求取消不能打断写入（#73）：并发地用随时会被取消的 context 写（插入 + 修订号加一在同一个事务里），
// 取消只在事务开始前生效——成功返回的写一定落了库，失败返回的一定没落；修订号与行数始终一致（没有半写）；
// 取消产生的 interrupt 也不会落到复用这条连接的下一条语句上（取消风暴之后立刻用干净的 context 读写都成功）。
func TestCancelledRequestsNeverTearWrites(t *testing.T) {
	db := create(t)
	defer func() { _ = db.Close() }()
	bg := context.Background()
	ports := db.Ports()
	if err := db.CreateTenant(bg, "t", store.Tenant{TenantSettings: storetest.Settings()}); err != nil {
		t.Fatal(err)
	}
	if _, err := ports.Sources.Put(bg, "t", "s", func(*store.SourceRecord) (store.SourceRecord, error) {
		return store.SourceRecord{
			SourceID: "s", Name: "s", Kind: "desktop",
			PublicJWK:      gen.Ed25519PublicJWK{Kty: "OKP", Crv: "Ed25519", X: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
			CoreVersion:    "0.3.0",
			Capabilities:   []string{"terminal"},
			RegisteredAtMs: storetest.Now, TrustedOrigins: []string{},
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	startRevision, ok, err := db.Revision(bg, "t")
	if err != nil || !ok {
		t.Fatalf("修订号：%v %v", ok, err)
	}

	const rounds = 400
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make(map[string]error, rounds)
	)
	for i := 0; i < rounds; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithCancel(bg)
			// 取消的时机错开：有的在写之前，有的在写进行中。
			time.AfterFunc(time.Duration(i%40)*100*time.Microsecond, cancel)
			defer cancel()
			id := fmt.Sprintf("l%03d", i)
			err := ports.Links.Insert(ctx, "t", store.LinkRecord{
				LinkID: id, SourceID: "s", InvitationID: "inv-" + id, Role: "viewer",
				SecretHash: "h", CreatedAtMs: storetest.Now, ExpiresAtMs: storetest.Now + 60_000,
			})
			mu.Lock()
			results[id] = err
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// 取消风暴之后立刻用干净的 context：读写都不该带着上一条语句的 interrupt。
	rows, err := ports.Links.List(bg, "t", "s")
	if err != nil {
		t.Fatalf("取消之后的读：%v", err)
	}
	stored := make(map[string]bool, len(rows))
	for _, row := range rows {
		stored[row.LinkID] = true
	}
	succeeded := 0
	for id, err := range results {
		switch {
		case err == nil:
			succeeded++
			if !stored[id] {
				t.Fatalf("写成功返回却没落库：%s", id)
			}
		case errors.Is(err, context.Canceled):
			if stored[id] {
				t.Fatalf("写失败返回却落了库（半写）：%s %v", id, err)
			}
		default:
			t.Fatalf("意外的错误：%s %v", id, err)
		}
	}
	if len(stored) != succeeded {
		t.Fatalf("落库 %d 行，成功 %d 次", len(stored), succeeded)
	}
	revision, _, err := db.Revision(bg, "t")
	if err != nil || revision-startRevision != int64(succeeded) {
		t.Fatalf("修订号应随成功的写逐个加一：%d -> %d，成功 %d，%v", startRevision, revision, succeeded, err)
	}
	if err := ports.Links.Insert(bg, "t", store.LinkRecord{
		LinkID: "after", SourceID: "s", InvitationID: "inv-after", Role: "viewer",
		SecretHash: "h", CreatedAtMs: storetest.Now, ExpiresAtMs: storetest.Now + 60_000,
	}); err != nil {
		t.Fatalf("取消之后的写：%v", err)
	}
	t.Logf("成功 %d / %d", succeeded, rounds)
}

// 已取消的 context 不开始任何工作；锁等待期间被取消同理。
func TestCancelledBeforeStartDoesNothing(t *testing.T) {
	db := create(t)
	defer func() { _ = db.Close() }()
	bg := context.Background()
	if err := db.CreateTenant(bg, "t", store.Tenant{TenantSettings: storetest.Settings()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, _, err := db.Revision(ctx, "t"); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的读：%v", err)
	}
	if err := db.CreateTenant(ctx, "t2", store.Tenant{TenantSettings: storetest.Settings()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的写：%v", err)
	}
	if _, ok, err := db.Revision(bg, "t2"); err != nil || ok {
		t.Fatalf("已取消的写不该落库：%v %v", ok, err)
	}
}
