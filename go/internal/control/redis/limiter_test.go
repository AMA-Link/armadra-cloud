package redis_test

import (
	"context"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	cr "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
)

var cloudRules = map[control.LimitKind]control.LimitRule{
	"register.ip": {Max: 2, Window: time.Hour},
	"stepup.dev":  {Max: 3, Window: 10 * time.Minute},
}

func TestStandaloneLimiterSharesBucketAcrossInstances(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	opts, _ := goredis.ParseURL(e.url)
	client := goredis.NewClient(opts)
	defer func() { _ = client.Close() }()
	a := cr.NewLimiter(client, e.prefix+"cloud:limit:", cloudRules, nil)
	b := cr.NewLimiter(client, e.prefix+"cloud:limit:", cloudRules, nil)
	hit := func(l *cr.Limiter, key string) control.LimitResult {
		t.Helper()
		result, err := l.Hit(ctx, "register.ip", key)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if !hit(a, "198.51.100.1").Allowed || !hit(b, "198.51.100.1").Allowed {
		t.Fatal("窗口内前两次放行（两个实例共用一个桶）")
	}
	denied := hit(a, "198.51.100.1")
	if denied.Allowed || denied.RetryAfter <= 0 || denied.RetryAfter > time.Hour {
		t.Fatalf("第三次被拒并带重试等待：%+v", denied)
	}
	if !hit(b, "198.51.100.2").Allowed {
		t.Fatal("别的键有自己的桶")
	}
	// 键按 <前缀><种类>:<sha256> 落库，IP 不以明文出现。
	keys, err := client.Keys(ctx, e.prefix+"cloud:limit:register.ip:*").Result()
	if err != nil || len(keys) != 2 {
		t.Fatalf("桶键：%v %v", keys, err)
	}
	for _, key := range keys {
		if strings.Contains(key, "198.51.100") {
			t.Fatalf("键里不该有明文 IP：%s", key)
		}
	}
	if _, err := a.Hit(ctx, "nope.kind", "x"); err == nil {
		t.Fatal("没登记的种类应报错")
	}
	if err := a.Release(ctx, "register.ip", "x"); err != nil {
		t.Fatal(err)
	}
}

func TestStandaloneLimiterFallsBackToLocalWindowOnRedisError(t *testing.T) {
	// 连不上的 Redis：不放行无限次，退回本进程的固定窗口。
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer func() { _ = client.Close() }()
	var reported int
	l := cr.NewLimiter(client, "x:", cloudRules, func(error) { reported++ })
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		result, err := l.Hit(ctx, "register.ip", "ip")
		if err != nil {
			t.Fatal(err)
		}
		if result.Allowed != (i <= 2) {
			t.Fatalf("第 %d 次：%+v", i, result)
		}
	}
	if reported != 3 {
		t.Fatalf("每次失败都报告：%d", reported)
	}
}

func TestLimiterPeekDoesNotCount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	opts, _ := goredis.ParseURL(e.url)
	client := goredis.NewClient(opts)
	defer func() { _ = client.Close() }()
	l := cr.NewLimiter(client, e.prefix+"cloud:limit:", cloudRules, nil)
	peek := func() control.LimitResult {
		t.Helper()
		result, err := l.Peek(ctx, "register.ip", "198.51.100.9")
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for i := 0; i < 5; i++ {
		if !peek().Allowed {
			t.Fatal("没计过数，只读不该被拒，也不该累加")
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := l.Hit(ctx, "register.ip", "198.51.100.9"); err != nil {
			t.Fatal(err)
		}
	}
	if denied := peek(); denied.Allowed || denied.RetryAfter <= 0 {
		t.Fatalf("计满（Max=2）后只读被拒并带等待：%+v", denied)
	}
	if _, err := l.Peek(ctx, "nope.kind", "x"); err == nil {
		t.Fatal("没登记的种类应报错")
	}
}
