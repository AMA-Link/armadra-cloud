package server_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// outage 是可开关的 Redis 故障：开着时客户端上的每条命令都失败。
type outage struct {
	down atomic.Bool
	// writesOnly 为真时只有写命令失败，PING 与读照常（Redis 拒写：OOM、READONLY、MISCONF）。
	writesOnly bool
}

func (o *outage) fails(cmd goredis.Cmder) bool {
	if !o.down.Load() {
		return false
	}
	if !o.writesOnly {
		return true
	}
	switch strings.ToLower(cmd.Name()) {
	case "set", "setex", "eval", "evalsha", "hset", "hincrby", "expire", "pexpire", "del", "zadd", "incr", "getdel", "publish":
		return true
	}
	return false
}

func (o *outage) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (o *outage) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		for _, cmd := range cmds {
			if o.fails(cmd) {
				return errors.New("redis down")
			}
		}
		return next(ctx, cmds)
	}
}
func (o *outage) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if o.fails(cmd) {
			return errors.New("redis down")
		}
		return next(ctx, cmd)
	}
}

// onAt 在另一个实例上发请求（同一个客户端 IP 之外每次换一个来源 IP，避开按 IP 的限流）。
func (r *rig) at(url string) *rig {
	peer := *r
	peer.url = url
	return &peer
}

// Redis 故障时登录失败关闭（设计 §4.3）：两个实例下，已注册与未注册的邮箱（已锁与未锁各一对）得到完全相同的答复
// （503 unavailable、同一个形状），不回落计数也不暴露谁注册了；故障期间已注册账号的失败计数不动；恢复后锁定照旧。
func TestLoginFailsClosedForBothKindsAcrossInstances(t *testing.T) {
	t.Parallel()
	failClosed(t, &outage{})
}

// 同一个场景，但 Redis 只是拒写（PING 与读都正常）：探活是写命令，所以仍然对两类邮箱一视同仁地答 503，
// 不会因为「PING 通了、只有未注册路径的记账写失败」而被区分。
func TestLoginFailsClosedWhenRedisRejectsWrites(t *testing.T) {
	t.Parallel()
	failClosed(t, &outage{writesOnly: true})
}

func failClosed(t *testing.T, hook *outage) {
	r := newRig(t, rigOptions{redisHook: hook})
	peer := r.at(r.startPeer().URL)
	instances := []*rig{r, peer}

	ghostLocked, ghostFresh := r.email(), r.email()
	regLocked, regFresh := r.email(), r.email()
	for _, email := range []string{regLocked, regFresh} {
		r.registerBearer(email)
	}
	ip := func(n int) headers { return hdr("X-Forwarded-For", fmt.Sprintf("10.9.%d.%d", n/250, n%250+1)) }
	n := 0
	attempt := func(rg *rig, email, pw string) response {
		n++
		return rg.login(email, pw, ip(n))
	}
	// 故障之前：各锁一对（6 次失败，两个实例交替记），其余一对不动。
	for i := 0; i < 6; i++ {
		for _, email := range []string{ghostLocked, regLocked} {
			attempt(instances[i%2], email, "wrong-wrong-wrong-1")
		}
	}
	for _, email := range []string{ghostLocked, regLocked} {
		for _, rg := range instances {
			r.expect(attempt(rg, email, "wrong-wrong-wrong-1"), 429, "account_locked")
		}
	}
	failuresOf := func(email string) int {
		t.Helper()
		return r.queryInt("SELECT failures::int FROM accounts WHERE email = $1", email)
	}
	freshBefore := failuresOf(regFresh)

	hook.down.Store(true)
	var seen []string
	for round := 0; round < 4; round++ {
		for _, email := range []string{ghostLocked, regLocked, ghostFresh, regFresh} {
			for _, rg := range instances {
				for _, pw := range []string{"wrong-wrong-wrong-1", password} {
					res := attempt(rg, email, pw)
					r.expect(res, 503, "unavailable")
					seen = append(seen, fmt.Sprintf("%d %v %v", res.status, res.json["code"], res.header.Get("Retry-After")))
				}
			}
		}
	}
	for _, line := range seen {
		if line != seen[0] {
			t.Fatalf("故障期间所有邮箱、所有实例的答复必须一致：%q 与 %q", line, seen[0])
		}
	}
	if got := failuresOf(regFresh); got != freshBefore {
		t.Fatalf("故障期间已注册账号的失败计数不能动：%d → %d", freshBefore, got)
	}

	hook.down.Store(false)
	time.Sleep(1200 * time.Millisecond) // 出错后的快速失败窗口（1 秒）过去
	// 恢复之后：已锁的仍然锁着（两个实例都看到）；没锁的照常（口令对了能登录，错的答 credentials_invalid）。
	for _, email := range []string{ghostLocked, regLocked} {
		for _, rg := range instances {
			r.expect(attempt(rg, email, "wrong-wrong-wrong-1"), 429, "account_locked")
		}
	}
	for _, rg := range instances {
		r.expect(attempt(rg, ghostFresh, "wrong-wrong-wrong-1"), 401, "credentials_invalid")
		r.expect(attempt(rg, regFresh, "wrong-wrong-wrong-1"), 401, "credentials_invalid")
	}
	r.expect(attempt(peer, regFresh, password), 200, "")
}
