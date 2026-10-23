package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// 运营登录的两步验证挑战（设计 §4.9、§2.4 的 cloud:mfa:*）：口令对了之后签发，5 分钟内有效，
// 每个挑战最多错 5 次（到次数就删），通过一次即作废。键里只放挑战 id 的哈希。
const (
	// MFAPrefix 是 Redis 里挑战的键前缀。
	MFAPrefix = "cloud:mfa:"
	// MFAChallengeTTL 是挑战的有效期。
	MFAChallengeTTL = 5 * time.Minute
	// MFAMaxAttempts 是每个挑战最多错几次。
	MFAMaxAttempts = 5
)

// Challenge 是一次待完成的运营登录：谁、用什么设备。
type Challenge struct {
	AccountID string
	Device    core.DeviceInput
	// ExpiresAtMs 按控制面的时钟记；Redis 的 TTL 只负责清理。
	ExpiresAtMs int64
}

// ChallengeStore 存运营登录的挑战。
type ChallengeStore interface {
	// Create 存一个挑战（id 是答给客户端的明文，存储里只留哈希）。
	Create(ctx context.Context, id string, c Challenge) error
	// Get 读挑战；没有（不存在、已用、已错满）答 nil。
	Get(ctx context.Context, id string) (*Challenge, error)
	// Fail 记一次错误，答累计错了几次（挑战已不存在答 -1，且不写任何东西）；到 MFAMaxAttempts 时挑战被删。
	Fail(ctx context.Context, id string) (int, error)
	// Consume 一次性取走挑战：只有一个并发调用拿到 true。
	Consume(ctx context.Context, id string) (bool, error)
}

// RedisChallenges 是 ChallengeStore 的 Redis 实现。
type RedisChallenges struct {
	Client goredis.UniversalClient
}

var _ ChallengeStore = (*RedisChallenges)(nil)

func mfaKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return MFAPrefix + hex.EncodeToString(sum[:])
}

// Create 实现 ChallengeStore。
func (r *RedisChallenges) Create(ctx context.Context, id string, c Challenge) error {
	device, err := json.Marshal(c.Device)
	if err != nil {
		return err
	}
	key := mfaKey(id)
	pipe := r.Client.TxPipeline()
	pipe.HSet(ctx, key, "accountId", c.AccountID, "device", string(device), "expiresAtMs", c.ExpiresAtMs, "attempts", 0)
	pipe.PExpire(ctx, key, MFAChallengeTTL+time.Minute)
	_, err = pipe.Exec(ctx)
	return err
}

// Get 实现 ChallengeStore。
func (r *RedisChallenges) Get(ctx context.Context, id string) (*Challenge, error) {
	values, err := r.Client.HGetAll(ctx, mfaKey(id)).Result()
	if err != nil {
		return nil, err
	}
	// 缺字段的键（例如只剩 attempts）不是一个完整的挑战：当作不存在，不报错。
	if values["accountId"] == "" || values["device"] == "" || values["expiresAtMs"] == "" {
		return nil, nil
	}
	c := Challenge{AccountID: values["accountId"]}
	if json.Unmarshal([]byte(values["device"]), &c.Device) != nil {
		return nil, nil
	}
	var perr error
	if c.ExpiresAtMs, perr = strconv.ParseInt(values["expiresAtMs"], 10, 64); perr != nil {
		return nil, nil
	}
	return &c, nil
}

// failScript 记一次错误：键不存在（已用、已错满、已过期）就什么都不写——直接 HINCRBY 会凭空造出一个只有
// attempts 字段、没有 TTL 的键；到次数就删。答累计次数，键不存在答 -1。
var failScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return -1 end
local n = redis.call('HINCRBY', KEYS[1], 'attempts', 1)
if n >= tonumber(ARGV[1]) then redis.call('DEL', KEYS[1]) end
return n`)

// Fail 实现 ChallengeStore。
func (r *RedisChallenges) Fail(ctx context.Context, id string) (int, error) {
	n, err := failScript.Run(ctx, r.Client, []string{mfaKey(id)}, MFAMaxAttempts).Int()
	return n, err
}

// Consume 实现 ChallengeStore。
func (r *RedisChallenges) Consume(ctx context.Context, id string) (bool, error) {
	n, err := r.Client.Del(ctx, mfaKey(id)).Result()
	return n == 1, err
}
