package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// minRedis 是要求的最低 Redis 版本：HPEXPIRE（relay:nodes 的字段过期）要 7.4。
const minRedisMajor, minRedisMinor = 7, 4

// OpenRedis 连上 Redis、PING，并检查版本不低于 7.4。连不上或版本太低都拒绝；错误不带连接串。
func OpenRedis(ctx context.Context, url string) (goredis.UniversalClient, error) {
	options, err := goredis.ParseURL(url)
	if err != nil {
		// 解析错误可能带连接串片段：不转述。
		return nil, errors.New("连接串不合格式（Redis）")
	}
	client := goredis.NewClient(options)
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(check).Err(); err != nil {
		_ = client.Close()
		return nil, errors.New("连不上（Redis）")
	}
	info, err := client.InfoMap(check, "server").Result()
	if err != nil {
		_ = client.Close()
		return nil, errors.New("版本读不出来（Redis）")
	}
	version := info["Server"]["redis_version"]
	if !versionAtLeast(version, minRedisMajor, minRedisMinor) {
		_ = client.Close()
		return nil, fmt.Errorf("版本太低（Redis）：%s，要 %d.%d 以上", version, minRedisMajor, minRedisMinor)
	}
	return client, nil
}

// versionAtLeast 比较 "7.4.1" 形式的版本号；读不出来算不够。
func versionAtLeast(version string, major, minor int) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	have, err1 := strconv.Atoi(parts[0])
	haveMinor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return have > major || (have == major && haveMinor >= minor)
}
