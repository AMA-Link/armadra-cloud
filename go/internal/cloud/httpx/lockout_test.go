package httpx_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// Redis 连不上：退回进程内计数，不答 500。
func TestUnknownLockoutsFallBackWhenRedisIsDown(t *testing.T) {
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer func() { _ = client.Close() }()
	var reported int
	u := httpx.NewUnknownLockouts(client, func(error) { reported++ })
	var guard core.UnknownAccounts
	guard.Store = u
	now := func() int64 { return 1_000_000 }
	for i := 0; i < 5; i++ {
		if code, _ := core.CodeOf(guard.Login(context.Background(), now, "x@example.com", "pw")); code != "credentials_invalid" {
			t.Fatalf("第 %d 次：%v", i+1, code)
		}
	}
	if code, _ := core.CodeOf(guard.Login(context.Background(), now, "x@example.com", "pw")); code != "account_locked" {
		t.Fatalf("退路也要锁：%v", code)
	}
	if reported == 0 {
		t.Fatal("Redis 失败要报告")
	}
}
