package events

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// TicketPrefix 是 me-stream 票的键前缀：cloud:ticket:<sha256>（设计 §2.4）。
const TicketPrefix = "cloud:ticket:"

// Tickets 是 core.TicketStore 的 Redis 实现：票 30 秒有效、GETDEL 一次性，任一实例发的票可以在任一实例兑换。
// 键是票的哈希（票本身不落 Redis），值只有 sub 与设备 id。
type Tickets struct {
	Client goredis.UniversalClient
	// Prefix 缺省 TicketPrefix；测试用随机前缀隔离。
	Prefix string
}

var _ core.TicketStore = (*Tickets)(nil)

type ticketValue struct {
	AccountID string `json:"accountId"`
	DeviceID  string `json:"deviceId"`
}

func (t *Tickets) key(hash string) string {
	if t.Prefix != "" {
		return t.Prefix + hash
	}
	return TicketPrefix + hash
}

// Put 实现 core.TicketStore。
func (t *Tickets) Put(ctx context.Context, key, subject, deviceID string, ttl time.Duration) error {
	value, _ := json.Marshal(ticketValue{AccountID: subject, DeviceID: deviceID})
	return redisError(t.Client.Set(ctx, t.key(key), value, ttl).Err())
}

// Take 实现 core.TicketStore。
func (t *Tickets) Take(ctx context.Context, key string) (string, string, bool, error) {
	raw, err := t.Client.GetDel(ctx, t.key(key)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, redisError(err)
	}
	var value ticketValue
	if json.Unmarshal(raw, &value) != nil || value.AccountID == "" || value.DeviceID == "" {
		return "", "", false, nil
	}
	return value.AccountID, value.DeviceID, true, nil
}

// redisError 只留类别，不带连接串、主机与服务端原文。
func redisError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New("操作失败（Redis）")
}
