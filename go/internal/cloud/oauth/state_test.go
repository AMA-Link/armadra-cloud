package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/oauth"
)

func redisStates(t *testing.T) (*oauth.RedisStates, *goredis.Client) {
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
	prefix := "oauth-unit-test:" + t.Name() + ":"
	t.Cleanup(func() {
		ctx := context.Background()
		iter := client.Scan(ctx, 0, prefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			client.Del(ctx, iter.Val())
		}
	})
	return &oauth.RedisStates{Client: client, Prefix: prefix}, client
}

// 授权状态一次性：并发取只有一个拿到；坏记录当不存在。
func TestStatesTakeIsOneShot(t *testing.T) {
	states, client := redisStates(t)
	ctx := context.Background()
	record := oauth.State{Provider: "github", RedirectURI: "https://app.test/login/oauth/github", Verifier: "v", Nonce: "n", ExpiresAtMs: 5}
	if err := states.Put(ctx, "state-1", record); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := states.Take(ctx, "state-1")
			if err != nil {
				t.Error(err)
			} else if got != nil {
				wins.Add(1)
				if *got != record {
					t.Errorf("记录：%+v", got)
				}
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("恰好一个并发调用拿到：%d", wins.Load())
	}
	if got, err := states.Take(ctx, "never"); err != nil || got != nil {
		t.Fatalf("不存在：%v %v", got, err)
	}
	// 坏记录（不是 JSON、缺字段）当作不存在，并被取走。
	for i, raw := range []string{"not json", `{"provider":"github"}`} {
		state := "bad-" + string(rune('a'+i))
		sum := sha256.Sum256([]byte(state))
		if err := client.Set(ctx, states.Prefix+hex.EncodeToString(sum[:]), raw, 0).Err(); err != nil {
			t.Fatal(err)
		}
		if got, err := states.Take(ctx, state); err != nil || got != nil {
			t.Fatalf("坏记录 %q：%v %v", raw, got, err)
		}
		if n, _ := client.Exists(ctx, states.Prefix+hex.EncodeToString(sum[:])).Result(); n != 0 {
			t.Fatal("坏记录也应被取走")
		}
	}
	// 键长度固定：不管 state 多长，键里只有 sha256 的十六进制。
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'a'
	}
	if err := states.Put(ctx, string(long), record); err != nil {
		t.Fatal(err)
	}
	keys, _ := client.Keys(ctx, states.Prefix+"*").Result()
	for _, key := range keys {
		if len(key) > len(states.Prefix)+64 {
			t.Fatalf("键太长：%d", len(key))
		}
	}
}
