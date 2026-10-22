package postgres

import (
	"context"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/storetest"
)

func keyOf(t *testing.T, kid string) store.SigningKeyRecord {
	t.Helper()
	k, err := store.GenerateSigningKey(kid, storetest.Now)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// 慢的旧读晚到，不能盖掉后发的新读。
func TestKeyCacheRejectsStaleRefresh(t *testing.T) {
	c := &keyCache{}
	older := c.begin()
	newer := c.begin()
	if !c.apply(newer, "iss-new", []store.SigningKeyRecord{keyOf(t, "k1"), keyOf(t, "k2")}) {
		t.Fatal("新读应写回")
	}
	if c.apply(older, "iss-old", []store.SigningKeyRecord{keyOf(t, "k1")}) {
		t.Fatal("旧读不该写回")
	}
	if c.issuer != "iss-new" || len(c.keys) != 2 {
		t.Fatalf("缓存被旧读盖掉了：%q %d", c.issuer, len(c.keys))
	}
}

// 本实例轮换让缓存失效之后，失效前发出的读不能再把旧数据写回。
func TestKeyCacheInvalidationBeatsInFlightRefresh(t *testing.T) {
	url, schema := pgtest.Schema(t)
	db, err := Open(context.Background(), Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	c := db.keyCache("t")
	inFlight := c.begin()
	db.dropKeyCache("t")
	if c.apply(inFlight, "iss", []store.SigningKeyRecord{keyOf(t, "k1")}) {
		t.Fatal("失效前发出的读不该写回")
	}
	if c.loaded {
		t.Fatal("失效后缓存应仍是未加载")
	}
}
