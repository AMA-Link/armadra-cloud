package relayctl

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	controlredis "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
)

// 控制面自己的键（设计 §2.4）：签发索引。节点不读不写。
const (
	// IssuedPrefix 是签发索引的键前缀：cloud:issued:acct:<账号>、cloud:issued:src:<主机>。
	IssuedPrefix = "cloud:issued:"
	// issuedKeepMs 是索引键的保留期：每次写入时续，空闲一小时后整把键过期（PostgreSQL 里有同一份）。
	issuedKeepMs = int64(time.Hour / time.Millisecond)
	// issuedSep 分隔成员 <账号>|<主机>|<设备>|<jti>。
	issuedSep = "|"
	// batchSize 是重放与额度重写的 pipeline 批大小。
	batchSize = 500
	// revokeAttempts 是撤销写 Redis 的尝试次数（设计 §4.8：失败重试 3 次后交给 relay.revoke 作业兜底）。
	revokeAttempts = 3
)

// Redis 是 Control 的 Redis 实现：包装 control/redis.Plane（前缀 relay:，Origin = cloud/<实例 id>），
// 路由读取、撤销、命令、在线事件订阅都经它；签发索引放在 cloud:issued:* 的两个有序集合里（快路径，
// 过滤以 PostgreSQL 为准，见 Durable）。线上格式一律取 cluster 包，这里不手写。
type Redis struct {
	plane  *controlredis.Plane
	client goredis.UniversalClient
	issued string
	now    Clock
	log    *obs.Logger
	// revokeFailed 在撤销写 Redis 重试用尽时调用（装配方挂 relay.revoke 作业）；可为 nil。
	revokeFailed func(ctx context.Context, jti string, expMs int64)
	// retryWait 是撤销重试之间的等待；测试可缩短。
	retryWait time.Duration
}

var _ Control = (*Redis)(nil)

// RedisOptions 配置 Redis 实现。
type RedisOptions struct {
	// Plane 是已建好的控制面（调用方管生命周期）。
	Plane *controlredis.Plane
	Now   Clock
	Log   *obs.Logger
	// IssuedPrefix 是签发索引的键前缀；缺省 IssuedPrefix（测试用随机前缀隔离）。
	IssuedPrefix string
	// RevokeFailed 在撤销写 Redis 重试 3 次仍失败时调用；PostgreSQL 里已有这条撤销，作业重放它。
	RevokeFailed func(ctx context.Context, jti string, expMs int64)
}

// NewRedis 建 Redis 实现。
func NewRedis(o RedisOptions) *Redis {
	r := &Redis{plane: o.Plane, client: o.Plane.Client(), issued: o.IssuedPrefix, now: o.Now, log: o.Log, revokeFailed: o.RevokeFailed, retryWait: 100 * time.Millisecond}
	if r.issued == "" {
		r.issued = IssuedPrefix
	}
	if r.now == nil {
		r.now = defaultClock
	}
	if r.log == nil {
		r.log = obs.Nop()
	}
	return r
}

func (r *Redis) nowMs() int64 { return r.now().UnixMilli() }

func (r *Redis) acctKey(subject string) string { return r.issued + "acct:" + subject }
func (r *Redis) srcKey(sourceID string) string { return r.issued + "src:" + sourceID }

// jtiKey 记一枚令牌的索引成员，让只知道 jti 的撤销（签发方复核）也找得到它；到期自动消失。
func (r *Redis) jtiKey(jti string) string { return r.issued + "jti:" + jti }

func (r *Redis) quotaKey(teamID string) string { return r.plane.Prefix() + "quota:team:" + teamID }
func (r *Redis) incidentKey() string           { return r.plane.Prefix() + "incident" }

func memberOf(i Issued) string {
	return strings.Join([]string{i.Subject, i.SourceID, i.DeviceID, i.JTI}, issuedSep)
}

func parseMember(member string, expMs int64) (Issued, bool) {
	parts := strings.SplitN(member, issuedSep, 4)
	if len(parts) != 4 || parts[3] == "" {
		return Issued{}, false
	}
	return Issued{Subject: parts[0], SourceID: parts[1], DeviceID: parts[2], JTI: parts[3], ExpMs: expMs}, true
}

// queueIndex 把一条索引写进 pipeline：写前先清掉已到期的成员，键续一小时。
func (r *Redis) queueIndex(ctx context.Context, pipe goredis.Pipeliner, issued Issued, nowMs int64) {
	member := memberOf(issued)
	z := goredis.Z{Score: float64(issued.ExpMs), Member: member}
	keys := []string{r.srcKey(issued.SourceID)}
	if issued.Subject != NoAccount {
		keys = append(keys, r.acctKey(issued.Subject))
	}
	for _, key := range keys {
		pipe.ZRemRangeByScore(ctx, key, "-inf", itoa(nowMs))
		pipe.ZAdd(ctx, key, z)
		pipe.PExpire(ctx, key, time.Duration(issuedKeepMs)*time.Millisecond)
	}
	pipe.Do(ctx, "SET", r.jtiKey(issued.JTI), member, "PXAT", issued.ExpMs)
}

// IndexIssued 实现 Control。已到期的令牌不记。
func (r *Redis) IndexIssued(ctx context.Context, issued Issued) error {
	nowMs := r.nowMs()
	if issued.ExpMs <= nowMs || issued.JTI == "" || issued.SourceID == "" {
		return nil
	}
	_, err := r.client.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		r.queueIndex(ctx, pipe, issued, nowMs)
		return nil
	})
	return sanitize(err)
}

// candidates 取过滤可能命中的索引条目：给了 Subject 走账号集合，否则给了 SourceID 走主机集合，
// 否则只给 JTI 走 jti 键。只给设备或只给 subject 前缀（没有主机）时 Redis 里选不出来——那种撤销以
// PostgreSQL 为准（Durable 逐枚交给 Redis 撤）。
func (r *Redis) candidates(ctx context.Context, filter Filter, nowMs int64) ([]Issued, error) {
	var key string
	switch {
	case filter.Subject != "":
		key = r.acctKey(filter.Subject)
	case filter.SourceID != "":
		key = r.srcKey(filter.SourceID)
	case filter.JTI != "":
		member, err := r.client.Get(ctx, r.jtiKey(filter.JTI)).Result()
		if errors.Is(err, goredis.Nil) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		ttl, err := r.client.PExpireTime(ctx, r.jtiKey(filter.JTI)).Result()
		if err != nil {
			return nil, err
		}
		entry, ok := parseMember(member, int64(ttl/time.Millisecond))
		if !ok || entry.ExpMs <= nowMs {
			return nil, nil
		}
		return []Issued{entry}, nil
	default:
		return nil, nil
	}
	rows, err := r.client.ZRangeByScoreWithScores(ctx, key, &goredis.ZRangeBy{Min: "(" + itoa(nowMs), Max: "+inf"}).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Issued, 0, len(rows))
	for _, row := range rows {
		member, _ := row.Member.(string)
		if entry, ok := parseMember(member, int64(row.Score)); ok {
			out = append(out, entry)
		}
	}
	return out, nil
}

// revokeToken 把一枚令牌写进撤销表（relay:revoked:<jti> + 广播），失败重试 revokeAttempts 次；
// 仍失败时记错误日志并交给 revokeFailed（relay.revoke 作业重放；PostgreSQL 里已落盘）。
func (r *Redis) revokeToken(ctx context.Context, jti string, expMs int64) error {
	var err error
	for attempt := 0; attempt < revokeAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return sanitize(ctx.Err())
			case <-time.After(r.retryWait << (attempt - 1)):
			}
		}
		if err = r.plane.Tokens().Revoke(ctx, jti, time.UnixMilli(expMs)); err == nil {
			return nil
		}
	}
	r.log.Error("撤销写 Redis 失败，已交给作业重放", obs.Operation("relayctl.revoke"), obs.Error(sanitize(err)))
	if r.revokeFailed != nil {
		r.revokeFailed(ctx, jti, expMs)
	}
	return sanitize(err)
}

// Revoke 实现 Control：选出命中的索引条目，逐枚写撤销表并从两个集合里删掉；写撤销失败的条目也算命中
// （调用方据此踢人），错误合并返回。
func (r *Redis) Revoke(ctx context.Context, filter Filter, keepDeviceID string) ([]Issued, error) {
	if filter.empty() {
		return nil, nil
	}
	nowMs := r.nowMs()
	found, err := r.candidates(ctx, filter, nowMs)
	if err != nil {
		return nil, sanitize(err)
	}
	var hit []Issued
	for _, entry := range found {
		if matches(entry, filter) && (keepDeviceID == "" || entry.DeviceID != keepDeviceID) {
			hit = append(hit, entry)
		}
	}
	sort.Slice(hit, func(i, j int) bool { return hit[i].JTI < hit[j].JTI })
	var errs []error
	for _, entry := range hit {
		if err := r.revokeToken(ctx, entry.JTI, entry.ExpMs); err != nil {
			errs = append(errs, err)
		}
	}
	if len(hit) > 0 {
		_, err := r.client.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
			for _, entry := range hit {
				member := memberOf(entry)
				pipe.ZRem(ctx, r.srcKey(entry.SourceID), member)
				if entry.Subject != NoAccount {
					pipe.ZRem(ctx, r.acctKey(entry.Subject), member)
				}
				pipe.Del(ctx, r.jtiKey(entry.JTI))
			}
			return nil
		})
		if err != nil {
			errs = append(errs, sanitize(err))
		}
	}
	return hit, errors.Join(errs...)
}

// RevokeWhere 实现 Control。
func (r *Redis) RevokeWhere(ctx context.Context, filter Filter) (int, error) {
	hit, err := r.Revoke(ctx, filter, "")
	return len(hit), err
}

// command 发一条节点命令：总是经 relay:ctl 广播；对已知持有节点（路由表里的 node）再额外发 relay:node:<id>。
func (r *Redis) command(ctx context.Context, c control.NodeCommand) error {
	node := ""
	if c.Type != control.CommandDrain {
		switch route, err := r.plane.Routes().Get(ctx, c.SourceID); {
		case err != nil:
			// 路由读不到不挡命令：广播照发（所有节点都收得到）。
			r.log.Warn("读路由失败，命令只广播", obs.Operation("relayctl.command"), obs.SourceID(c.SourceID), obs.Error(sanitize(err)))
		case route != nil:
			node = route.Node
		}
	} else {
		node = c.Node
	}
	return r.plane.CommandContext(ctx, c, node)
}

// Kick 实现 Control。命令必须带 sourceId（节点会丢弃不带的）。
func (r *Redis) Kick(ctx context.Context, filter KickFilter, reason string) error {
	if filter.SourceID == "" {
		return errors.New("kick 缺 sourceId")
	}
	return r.command(ctx, control.NodeCommand{
		Type: control.CommandKick, SourceID: filter.SourceID, Reason: reason,
		Subject: filter.Subject, SubjectPrefix: filter.SubjectPrefix, DeviceID: filter.DeviceID,
	})
}

// CloseTunnel 实现 Control。
func (r *Redis) CloseTunnel(ctx context.Context, sourceID string) error {
	if sourceID == "" {
		return errors.New("closeTunnel 缺 sourceId")
	}
	return r.command(ctx, control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: sourceID})
}

// Route 实现 Control：读 relay:route:src:<id>；不在线（键不在或已过期）ok 为 false。
func (r *Redis) Route(ctx context.Context, sourceID string) (string, bool, error) {
	route, err := r.plane.Routes().Get(ctx, sourceID)
	if err != nil {
		return "", false, sanitize(err)
	}
	if route == nil {
		return "", false, nil
	}
	return route.CoreVersion, true, nil
}

// Nodes 实现 Control：读 relay:nodes（过期字段由 Redis 自己丢），去掉排空的，按 id 排序。
func (r *Redis) Nodes(ctx context.Context) ([]Node, error) {
	all, err := r.client.HGetAll(ctx, r.plane.Prefix()+"nodes").Result()
	if err != nil {
		return nil, sanitize(err)
	}
	out := make([]Node, 0, len(all))
	for id, value := range all {
		n, err := cluster.DecodeNode(id, []byte(value))
		if err != nil || n.Draining {
			continue // 坏记录跳过
		}
		out = append(out, Node{
			ID: n.ID, URL: n.URL, Weight: n.Weight, Version: n.Version, Draining: n.Draining,
			Connections: n.Connections, Tunnels: n.Tunnels,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Drain 实现 Control：drain 命令带 node、不带 sourceId，经广播与该节点自己的频道发出。
func (r *Redis) Drain(ctx context.Context, node string) error {
	if node == "" {
		return errors.New("drain 缺 node")
	}
	return r.command(ctx, control.NodeCommand{Type: control.CommandDrain, Node: node})
}

// unlimitedConcurrent 是「不限」写到线上的值：节点的解码器不收负数。
const unlimitedConcurrent = math.MaxInt32

func wireQuota(teamID string, q Quota) control.TeamQuota {
	if q.Concurrent < 0 {
		q.Concurrent = unlimitedConcurrent
	}
	return q.Wire(teamID)
}

// SetTeamQuota 实现 Control：先 SET 键（节点订阅就绪时全量读它），再广播 kind: quota（在线节点增量更新）。
func (r *Redis) SetTeamQuota(ctx context.Context, teamID string, quota Quota) error {
	wire := wireQuota(teamID, quota)
	if err := r.client.Set(ctx, r.quotaKey(teamID), cluster.EncodeQuotaValue(wire), 0).Err(); err != nil {
		return sanitize(err)
	}
	return r.plane.PublishQuota(ctx, wire)
}

// ClearTeamQuota 实现 Control：删键。在线节点保留旧值直到下次订阅就绪时全量重读（团队清除后没有流量，无碍）。
func (r *Redis) ClearTeamQuota(ctx context.Context, teamID string) error {
	return sanitize(r.client.Del(ctx, r.quotaKey(teamID)).Err())
}

// SetIncident 实现 Control：先 SET relay:incident，再广播 kind: incident。
func (r *Redis) SetIncident(ctx context.Context, incident Incident) error {
	in := control.Incident{RejectNewConnections: incident.RejectNewConnections, AtMs: incident.AtMs}
	if err := r.client.Set(ctx, r.incidentKey(), cluster.EncodeIncidentValue(in), 0).Err(); err != nil {
		return sanitize(err)
	}
	return r.plane.PublishIncident(ctx, in)
}

// ---- 重放（设计 §7.2）----

// Restore 把撤销表与签发索引从 PostgreSQL 重写进 Redis：撤销用 SET … PXAT … NX（已有的不动），索引用 pipeline
// 分批 ZADD。幂等；Redis 被清空、重启丢了最后一秒、订阅重新就绪时都会跑。
func (r *Redis) Restore(ctx context.Context, revoked []Revoked, issued []Issued) error {
	nowMs := r.nowMs()
	for start := 0; start < len(revoked); start += batchSize {
		batch := revoked[start:min(start+batchSize, len(revoked))]
		_, err := r.client.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
			for _, token := range batch {
				if token.ExpMs > nowMs {
					pipe.Do(ctx, "SET", r.plane.Prefix()+"revoked:"+token.JTI, "1", "PXAT", token.ExpMs, "NX")
				}
			}
			return nil
		})
		// NX 命中已有的键时 go-redis 报 Nil：正常。
		if err != nil && !errors.Is(err, goredis.Nil) {
			return sanitize(err)
		}
		// 本实例的内存副本也并进来（节点自己靠订阅就绪时的 SCAN 与定时重读，不用广播）。
		for _, token := range batch {
			r.plane.ReceiveRevoked(token.JTI, token.ExpMs)
		}
	}
	for start := 0; start < len(issued); start += batchSize {
		batch := issued[start:min(start+batchSize, len(issued))]
		_, err := r.client.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
			for _, entry := range batch {
				if entry.ExpMs > nowMs && entry.JTI != "" && entry.SourceID != "" {
					r.queueIndex(ctx, pipe, entry, nowMs)
				}
			}
			return nil
		})
		if err != nil {
			return sanitize(err)
		}
	}
	return nil
}

// TeamQuotas 是各团队的并发额度（团队 id → 额度）。
type TeamQuotas map[string]Quota

// WriteQuotas 把全部团队的额度键重写一遍（pipeline 分批 SET）。键没有 TTL；已不在表里的团队的旧键由
// ClearTeamQuota（团队清除）管，这里不扫描删除。
func (r *Redis) WriteQuotas(ctx context.Context, quotas TeamQuotas) error {
	ids := make([]string, 0, len(quotas))
	for id := range quotas {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for start := 0; start < len(ids); start += batchSize {
		batch := ids[start:min(start+batchSize, len(ids))]
		_, err := r.client.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
			for _, id := range batch {
				pipe.Set(ctx, r.quotaKey(id), cluster.EncodeQuotaValue(wireQuota(id, quotas[id])), 0)
			}
			return nil
		})
		if err != nil {
			return sanitize(err)
		}
	}
	return nil
}

// ---- core 接口 ----

// Relay 实现 Control。
func (r *Redis) Relay() core.RelayPort { return redisRelay{r} }

// Revocations 实现 Control。
func (r *Redis) Revocations() core.RevocationSink { return redisSink{r} }

type redisRelay struct{ r *Redis }

func (p redisRelay) Route(ctx context.Context, sourceID string) (string, bool, error) {
	return p.r.Route(ctx, sourceID)
}

func (p redisRelay) CloseTunnel(sourceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.r.CloseTunnel(ctx, sourceID); err != nil {
		p.r.log.Warn("关闭隧道失败", obs.Operation("relayctl.closeTunnel"), obs.SourceID(sourceID), obs.Error(err))
	}
}

// Disconnect 踢客户端连接；节点命令必须带 sourceId，没有就不发（记警告）。
func (p redisRelay) Disconnect(filter core.ConnectionFilter) {
	if filter.SourceID == "" {
		p.r.log.Warn("kick 缺 sourceId，没有发出", obs.Operation("relayctl.disconnect"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.r.Kick(ctx, KickFilter{SourceID: filter.SourceID, SubjectPrefix: filter.SubjectPrefix}, "disconnect"); err != nil {
		p.r.log.Warn("踢人失败", obs.Operation("relayctl.disconnect"), obs.SourceID(filter.SourceID), obs.Error(err))
	}
}

type redisSink struct{ r *Redis }

func (s redisSink) revoke(filter Filter, keepDeviceID, reason string, kick bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hit, err := s.r.Revoke(ctx, filter, keepDeviceID)
	if err != nil {
		s.r.log.Warn("撤销失败", obs.Operation("relayctl.revoke"), obs.Error(err))
	}
	if !kick {
		return
	}
	for _, k := range kicksOf(hit) {
		if err := s.r.Kick(ctx, k, reason); err != nil {
			s.r.log.Warn("踢人失败", obs.Operation("relayctl.kick"), obs.SourceID(k.SourceID), obs.Error(err))
		}
	}
}

func (s redisSink) DeviceRevoked(_ context.Context, subject, deviceID string) {
	s.revoke(Filter{Subject: subject, DeviceID: deviceID}, "", "device_revoked", true)
}

func (s redisSink) DevicesRevoked(_ context.Context, subject, keepDeviceID string) {
	s.revoke(Filter{Subject: subject}, keepDeviceID, "devices_revoked", true)
}

func (s redisSink) LinkRevoked(_ context.Context, sourceID, linkID string) {
	s.revoke(Filter{SourceID: sourceID, SubjectPrefix: core.GuestPrefix(linkID)}, "", "", false)
}

func (s redisSink) SourceRevoked(_ context.Context, sourceID string) {
	s.revoke(Filter{SourceID: sourceID}, "", "", false)
}

// kicksOf 把被撤销的令牌展开成逐 (源, subject, 设备) 的踢人过滤（隧道令牌与没有源的不踢）。
func kicksOf(hit []Issued) []KickFilter {
	type key struct{ source, subject, device string }
	seen := map[key]bool{}
	var out []KickFilter
	for _, entry := range hit {
		k := key{entry.SourceID, entry.Subject, entry.DeviceID}
		if entry.SourceID == "" || entry.Subject == NoAccount || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, KickFilter{SourceID: k.source, Subject: k.subject, DeviceID: k.device})
	}
	return out
}

// sanitize 只留错误类别：不带连接串、主机与服务端的原文。
func sanitize(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	var redisErr goredis.Error
	if errors.As(err, &redisErr) {
		first, _, _ := strings.Cut(err.Error(), " ")
		return errors.New("Redis 答复错误：" + first)
	}
	return errors.New("Redis 操作失败")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// ReplayRevoke 重新写一枚撤销（单次尝试，不再交给作业）：relay.revoke 作业用它，失败由作业运行器退避重试。
// 重新走 Plane.Tokens().Revoke，所以撤销键与广播都补上。
func (r *Redis) ReplayRevoke(ctx context.Context, jti string, expMs int64) error {
	return sanitize(r.plane.Tokens().Revoke(ctx, jti, time.UnixMilli(expMs)))
}

// Plane 是包装的控制面（装配方订阅在线事件用）。
func (r *Redis) Plane() *controlredis.Plane { return r.plane }
