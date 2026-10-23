package httpx_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
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
	options, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	client := goredis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// 未注册邮箱的失败计数在 Redis 里，与账号行同一套 NextLockout：5 次后锁 1 分钟，到期后再失败翻倍，
// 最后一次失败超过窗口则清零；多个实例共享。
func TestUnknownLockoutsMatchAccountThrottle(t *testing.T) {
	client := redisClient(t)
	var buf [6]byte
	_, _ = rand.Read(buf[:])
	name := "ghost-" + hex.EncodeToString(buf[:]) + "@example.com"
	a, b := httpx.NewUnknownLockouts(client, nil), httpx.NewUnknownLockouts(client, nil)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	fail := func(u *httpx.UnknownLockouts, at int64) (int64, bool) {
		t.Helper()
		until, locked, err := u.Fail(ctx, name, at)
		if err != nil {
			t.Fatal(err)
		}
		return until, locked
	}
	for i := 0; i < 5; i++ {
		store := a
		if i%2 == 1 {
			store = b // 两个实例交替记
		}
		if _, locked := fail(store, now); locked {
			t.Fatalf("第 %d 次不该锁", i+1)
		}
	}
	until, locked := fail(a, now+1000)
	if !locked || until != now+60_000 {
		t.Fatalf("第 6 次被锁到 +1 分钟：%d %v", until-now, locked)
	}
	if until2, locked := fail(b, now+2000); !locked || until2 != until {
		t.Fatal("锁着不再计数，另一个实例看到同一把锁")
	}
	// 锁到期后再失败：第 6 次起锁 2 分钟（翻倍）。
	at := now + 61_000
	if _, locked := fail(a, at); locked {
		t.Fatal("锁到期后这一次记账、不拒绝")
	}
	if until3, locked := fail(b, at+1); !locked || until3 != at+120_000 {
		t.Fatalf("翻倍到 2 分钟：%d %v", until3-at, locked)
	}
	// 过了失败窗口且不在锁中：清零。
	far := at + 24*3600*1000
	if _, locked := fail(a, far); locked {
		t.Fatal("窗口外清零")
	}
}

// Redis 连不上：失败关闭。Fail 与 Guard 都答 503 unavailable（带重试等待），不回落到进程内计数；
// 出错后一小段时间内 Guard 快速失败，恢复后放行；每次拒绝都报告并计数。
func TestUnknownLockoutsFailClosedWhenRedisIsDown(t *testing.T) {
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer func() { _ = client.Close() }()
	var reported int
	u := httpx.NewUnknownLockouts(client, func(error) { reported++ })
	clock := time.Unix(1_000, 0)
	u.Now = func() time.Time { return clock }
	var guard core.UnknownAccounts
	guard.Store = u
	now := func() int64 { return 1_000_000 }
	// 未注册路径：记账出错 → 503，永远不会因为回落计数而答 account_locked。
	for i := 0; i < 8; i++ {
		err := guard.Login(context.Background(), now, "x@example.com", "pw")
		code, _ := core.CodeOf(err)
		var e *core.Error
		if code != "unavailable" || !errors.As(err, &e) || e.RetryAfter != httpx.RetryAfterUnavailable {
			t.Fatalf("第 %d 次：%v %v", i+1, code, err)
		}
	}
	if reported != 8 || u.Unavailable() != 8 {
		t.Fatalf("每次都报告并计数：报告 %d 次，计数 %d", reported, u.Unavailable())
	}
	// 探活：第一次真去连（出错并进入快速失败窗口），窗口内不再连。
	if code, _ := core.CodeOf(u.Guard(context.Background())); code != "unavailable" {
		t.Fatalf("探活：%v", code)
	}
	before := reported
	if code, _ := core.CodeOf(u.Guard(context.Background())); code != "unavailable" || reported != before {
		t.Fatalf("窗口内快速失败、不再连 Redis：%v 报告 %d→%d", code, before, reported)
	}
	// 窗口过后重新探活（这里仍连不上，仍 503）。
	clock = clock.Add(2 * time.Second)
	if code, _ := core.CodeOf(u.Guard(context.Background())); code != "unavailable" || reported != before+1 {
		t.Fatalf("窗口过后重新探活：%v 报告 %d", code, reported)
	}
}

// Redis 可用时探活放行。
func TestGuardPassesWhenRedisIsUp(t *testing.T) {
	u := httpx.NewUnknownLockouts(redisClient(t), nil)
	if err := u.Guard(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u.Unavailable() != 0 {
		t.Fatal("没有拒绝过")
	}
}
