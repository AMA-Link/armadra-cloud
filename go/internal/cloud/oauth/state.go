package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// StatePrefix 是 Redis 里授权状态的键前缀（设计 §2.4 的 cloud:oauth:*）；键里只放 state 的 sha256。
const StatePrefix = "cloud:oauth:"

// StateTTL 是授权状态的有效期。
const StateTTL = 10 * time.Minute

// 授权的目的（State.Purpose）：登录、提权（重新认证已绑定的身份）、给已登录账号绑定新身份。
// 三种目的共用回调地址，以服务端记录的目的为准；老记录没有这个字段，按登录处理。
const (
	PurposeLogin  = "login"
	PurposeReauth = "reauth"
	PurposeLink   = "link"
)

// State 是一次授权的服务端记录：只存发起时的上下文，不存访问令牌。
type State struct {
	Provider    string `json:"provider"`
	RedirectURI string `json:"redirectUri"`
	// Verifier 是 PKCE 的 code_verifier。
	Verifier string `json:"verifier"`
	// Nonce 是浏览器绑定随机数（Cookie 传输时才有，同时下发给浏览器的 __Host-armadra-oauth）；Bearer 传输为空。
	Nonce string `json:"nonce"`
	// Purpose 是目的（Purpose* 常量），空 = 登录。
	Purpose string `json:"purpose,omitempty"`
	// AccountID 与 DeviceID 是提权和绑定的发起者（会话的账号与设备）；登录没有。
	AccountID string `json:"accountId,omitempty"`
	DeviceID  string `json:"deviceId,omitempty"`
	// ExpiresAtMs 按控制面的时钟记；Redis 的 TTL 只负责清理。
	ExpiresAtMs int64 `json:"expiresAtMs"`
}

// PurposeOf 是记录的目的，老记录（没有目的）按登录。
func (s State) PurposeOf() string {
	if s.Purpose == "" {
		return PurposeLogin
	}
	return s.Purpose
}

// States 存授权状态。
type States interface {
	// Put 存一条记录（state 是答给客户端的明文，存储里只留哈希）。
	Put(ctx context.Context, state string, record State) error
	// Take 一次性取走记录：只有一个并发调用拿到；不存在（没有、已用、已被 TTL 清掉）答 nil。
	Take(ctx context.Context, state string) (*State, error)
}

// RedisStates 是 States 的 Redis 实现。
type RedisStates struct {
	Client goredis.UniversalClient
	// Prefix 缺省 StatePrefix；测试用随机前缀隔离。
	Prefix string
}

var _ States = (*RedisStates)(nil)

func (r *RedisStates) key(state string) string {
	prefix := r.Prefix
	if prefix == "" {
		prefix = StatePrefix
	}
	sum := sha256.Sum256([]byte(state))
	return prefix + hex.EncodeToString(sum[:])
}

// Put 实现 States。
func (r *RedisStates) Put(ctx context.Context, state string, record State) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	// TTL 比有效期多留一分钟：过期与否由记录里的时刻判，TTL 只清理。
	return r.Client.Set(ctx, r.key(state), data, StateTTL+time.Minute).Err()
}

// Take 实现 States：GETDEL 保证一次性。
func (r *RedisStates) Take(ctx context.Context, state string) (*State, error) {
	data, err := r.Client.GetDel(ctx, r.key(state)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record State
	if json.Unmarshal(data, &record) != nil || record.Provider == "" || record.Verifier == "" {
		return nil, nil
	}
	return &record, nil
}
