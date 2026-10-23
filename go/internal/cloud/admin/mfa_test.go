package admin_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/admin"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

func redisClient(t *testing.T) goredis.UniversalClient {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 里没有 REDIS_URL")
		}
		t.Skip("没有 Redis：设置 REDIS_URL，跳过")
	}
	opts, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	client := goredis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// Fail 只对存在的挑战记错误：已用 / 已错满 / 已过期的挑战上再 Fail 不会凭空造出一个只有 attempts、没有 TTL 的键；
// Get 遇到缺字段的键当作不存在。
func TestChallengeFailNeverResurrectsAKey(t *testing.T) {
	client := redisClient(t)
	ctx := context.Background()
	store := &admin.RedisChallenges{Client: client}
	id := "challenge-" + core.RandomToken(8)
	key := admin.MFAPrefix + hashHex(id)
	t.Cleanup(func() { client.Del(ctx, key) })

	if n, err := store.Fail(ctx, id); err != nil || n != -1 {
		t.Fatalf("不存在的挑战：%d %v", n, err)
	}
	if exists, _ := client.Exists(ctx, key).Result(); exists != 0 {
		t.Fatal("Fail 不能造出键")
	}

	if err := store.Create(ctx, id, admin.Challenge{AccountID: "a1", Device: core.DeviceInput{Platform: "web", Name: "n"}, ExpiresAtMs: 5}); err != nil {
		t.Fatal(err)
	}
	if ttl, _ := client.PTTL(ctx, key).Result(); ttl <= 0 {
		t.Fatalf("挑战带 TTL：%v", ttl)
	}
	for i := 1; i < admin.MFAMaxAttempts; i++ {
		if n, err := store.Fail(ctx, id); err != nil || n != i {
			t.Fatalf("第 %d 次：%d %v", i, n, err)
		}
	}
	if n, err := store.Fail(ctx, id); err != nil || n != admin.MFAMaxAttempts {
		t.Fatalf("错满：%d %v", n, err)
	}
	if got, err := store.Get(ctx, id); err != nil || got != nil {
		t.Fatalf("错满即删：%+v %v", got, err)
	}
	if n, _ := store.Fail(ctx, id); n != -1 {
		t.Fatal("删了之后再 Fail 不复活")
	}

	// 只剩 attempts 的残键（旧版本的 HINCRBY 会留下）：Get 当作不存在，不报错。
	client.HSet(ctx, key, "attempts", 3)
	if got, err := store.Get(ctx, id); err != nil || got != nil {
		t.Fatalf("残键当作不存在：%+v %v", got, err)
	}
}

func hashHex(id string) string {
	// 与 mfaKey 同一个算法：避免导出内部函数，只在测试里重算。
	return sha256Hex(id)
}

// failingRelay 的 SetIncident 前几次失败。
type failingRelay struct {
	*relayctl.Memory
	failures atomic.Int32
	calls    atomic.Int32
}

func (f *failingRelay) SetIncident(ctx context.Context, incident relayctl.Incident) error {
	f.calls.Add(1)
	if f.failures.Add(-1) >= 0 {
		return errors.New("redis 不可用")
	}
	return f.Memory.SetIncident(ctx, incident)
}

// 写节点失败不记「已写」：下一轮（缩短到 20 毫秒）照库再写，直到成功。
func TestIncidentSyncRetriesAfterAFailedPush(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateTenant(ctx, "cloud", store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://c.test", TrustedOrigins: []string{}}}); err != nil {
		t.Fatal(err)
	}
	relay := &failingRelay{Memory: relayctl.NewMemory(nil)}
	relay.failures.Store(3)
	svc := admin.New(admin.Options{
		Core: &core.Context{TenantID: "cloud", Now: time.Now}, Store: db.SaaS(), Relay: relay, IncidentEvery: 20 * time.Millisecond,
	})
	if _, err := db.Pool().Exec(ctx, `INSERT INTO platform_settings (tenant_id, key, value, updated_at_ms)
	  VALUES ('cloud', 'incident', '{"rejectNewConnections":true}', 77)`); err != nil {
		t.Fatal(err)
	}
	stop := svc.Start()
	defer stop()
	deadline := time.Now().Add(10 * time.Second)
	for !relay.CurrentIncident().RejectNewConnections {
		if time.Now().After(deadline) {
			t.Fatalf("失败之后没有重试：调用 %d 次", relay.calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if relay.calls.Load() < 4 || relay.CurrentIncident().AtMs != 77 {
		t.Fatalf("前 3 次失败、第 4 次成功：%d %+v", relay.calls.Load(), relay.CurrentIncident())
	}
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
