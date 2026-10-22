// Package postgres 是控制面的 PostgreSQL 实现（多节点；对应
// packages/relay-runtime-node/src/control/postgres.ts，表与频道逐项一致）。
//
//	| 功能     | 实现                                                                                       |
//	| 路由     | relay_routes（UNLOGGED）：Get 每次读表，Set upsert，Touch 续期，Delete 按 tunnel_id CAS      |
//	| 在线事件 | 本节点立即投递 + NOTIFY relay_presence                                                       |
//	| 撤销     | relay_revoked_tokens + NOTIFY relay_revoked；各节点内存一份未过期的 jti，IsRevoked 只查内存     |
//	| 速率限流 | relay_limits（UNLOGGED）固定窗口 upsert 计数，各节点共用一个桶；库出错退回本节点内存             |
//	| 并发限流 | 本节点内存（节点崩溃不留占用）                                                                |
//	| 节点命令 | 本节点立即投递 + NOTIFY relay_ctl                                                            |
//
// LISTEN 用一条独立的常驻连接，断线后退避重连并重新 LISTEN；每次（重新）监听上都全量重读撤销表，
// 断线期间漏掉的撤销不会漏判。表由存储的迁移建（store/postgres），这里只核对表在不在，
// 不在就拒绝启动、提示先迁移。每 60 秒清一次过期的路由、限流桶与撤销记录（任一节点做都行）。
// 不写令牌明文（撤销只有 jti）。
package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
)

// 频道名（TypeScript 一侧写死的，Go 节点与 Node 节点共库时必须一致）。
const (
	ChannelPresence = "relay_presence"
	ChannelRevoked  = "relay_revoked"
	ChannelCommand  = "relay_ctl"
)

// SweepEvery 是清扫过期行的间隔。
const SweepEvery = 60 * time.Second

// ErrMissingSchema 表示库里没有中继控制面的表。
var ErrMissingSchema = errors.New("PostgreSQL 里没有中继控制面的表：先运行迁移（armadra-cloud migrate）")

// Options 配置 PostgreSQL 控制面。
type Options struct {
	cluster.Options
	// Pool 是查询用的连接池（调用方管它的生命周期，Close 不关它）；LISTEN 另开一条常驻连接，
	// 连接参数（含 search_path）取自它。与 URL 二选一。
	Pool *pgxpool.Pool
	// URL 与 Schema：给了就自建连接池，Close 时关。连接串含口令，不进日志与错误。
	URL    string
	Schema string
	// ChannelPrefix 加在三个频道名前面。缺省空，与 Node 版同名；同一个库里要跑互不相干的
	// 多套中继（或并行的测试）时给不同的前缀，NOTIFY 的频道是整个数据库共用的。
	ChannelPrefix string
	// ReconnectMin 与 ReconnectMax 是监听连接断线后的重连退避；缺省 200ms 到 5s。
	ReconnectMin, ReconnectMax time.Duration
	// SweepEvery 缺省 60 秒（测试可缩短）。
	SweepEvery time.Duration
}

// Plane 是 PostgreSQL 控制面。
type Plane struct {
	*cluster.Base
	pool     *pgxpool.Pool
	ownsPool bool
	listen   *pgx.ConnConfig
	chans    channels
	sweep    time.Duration

	stop     context.CancelFunc
	loopDone chan struct{}
	ready    chan struct{}
	readyErr chan error
	once     sync.Once
}

type channels struct{ presence, revoked, command string }

var _ control.Cluster = (*Plane)(nil)

// New 核对表、开始监听、全量读一遍撤销表，然后返回；表缺失或连不上就报错。
func New(ctx context.Context, o Options) (*Plane, error) {
	pool, owns := o.Pool, false
	if pool == nil {
		if o.URL == "" {
			return nil, errors.New("PostgreSQL 控制面缺 Pool 或 URL")
		}
		config, err := pgxpool.ParseConfig(o.URL)
		if err != nil {
			// 解析错误可能带连接串片段：不转述。
			return nil, errors.New("PostgreSQL 连接串不合格式")
		}
		if o.Schema != "" {
			config.ConnConfig.RuntimeParams["search_path"] = o.Schema
		}
		if pool, err = pgxpool.NewWithConfig(ctx, config); err != nil {
			return nil, errors.New("PostgreSQL 连不上")
		}
		owns = true
	}
	fail := func(err error) (*Plane, error) {
		if owns {
			pool.Close()
		}
		return nil, err
	}
	if _, err := pool.Exec(ctx, "SELECT 1 FROM relay_routes, relay_revoked_tokens, relay_limits LIMIT 0"); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return fail(ErrMissingSchema)
		}
		return fail(fmt.Errorf("PostgreSQL 查询失败：%s", errorClass(err)))
	}
	if o.ReconnectMin <= 0 {
		o.ReconnectMin = 200 * time.Millisecond
	}
	if o.ReconnectMax <= 0 {
		o.ReconnectMax = 5 * time.Second
	}
	if o.SweepEvery <= 0 {
		o.SweepEvery = SweepEvery
	}
	loopCtx, stop := context.WithCancel(context.Background())
	p := &Plane{
		Base: cluster.NewBase(o.Options), pool: pool, ownsPool: owns,
		listen: pool.Config().ConnConfig.Copy(), sweep: o.SweepEvery,
		chans: channels{
			presence: o.ChannelPrefix + ChannelPresence, revoked: o.ChannelPrefix + ChannelRevoked,
			command: o.ChannelPrefix + ChannelCommand,
		},
		stop: stop, loopDone: make(chan struct{}), ready: make(chan struct{}), readyErr: make(chan error, 1),
	}
	go p.listenLoop(loopCtx, o.ReconnectMin, o.ReconnectMax)
	select {
	case <-p.ready:
	case err := <-p.readyErr:
		_ = p.Close()
		return nil, err
	case <-ctx.Done():
		_ = p.Close()
		return nil, errors.New("PostgreSQL 控制面启动超时或被取消")
	}
	go p.sweepLoop(loopCtx)
	return p, nil
}

// errorClass 只报错误类别（码），不带连接串与服务端细节。
func errorClass(err error) string {
	var pgErr *pgconn.PgError
	var conn *pgconn.ConnectError
	switch {
	case errors.As(err, &pgErr):
		return pgErr.Code
	case errors.As(err, &conn):
		return "connect"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "unknown"
}

func (p *Plane) isReady() bool {
	select {
	case <-p.ready:
		return true
	default:
		return false
	}
}

// listenLoop 保持一条 LISTEN 连接：断线就退避重连，每次监听就绪都全量重读撤销。
func (p *Plane) listenLoop(ctx context.Context, minWait, maxWait time.Duration) {
	defer close(p.loopDone)
	wait := minWait
	for ctx.Err() == nil {
		err := p.listenOnce(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		if !p.isReady() {
			select {
			case p.readyErr <- fmt.Errorf("PostgreSQL 监听失败：%s", errorClass(err)):
			default:
			}
			return
		}
		p.Report("listener", fmt.Errorf("监听连接断开：%s", errorClass(err)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, maxWait)
	}
}

func (p *Plane) listenOnce(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, p.listen)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	for _, channel := range []string{p.chans.presence, p.chans.command, p.chans.revoked} {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
			return err
		}
	}
	// 监听已生效之后再读全量：读的过程中发生的撤销要么在表里、要么走通知，不会漏。
	if err := p.reloadRevoked(ctx); err != nil {
		if !p.isReady() {
			return err
		}
		p.Report("reload revoked", err)
	}
	if !p.isReady() {
		close(p.ready)
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		p.receive([]byte(n.Payload))
	}
}

func (p *Plane) receive(payload []byte) {
	m, err := cluster.ParseBroadcast(payload)
	if err != nil || m.Origin == p.Origin {
		return
	}
	switch m.Kind {
	case cluster.KindPresence:
		p.EmitPresence(m.Presence)
	case cluster.KindCommand:
		p.EmitCommand(m.Command)
	case cluster.KindRevoked:
		p.ReceiveRevoked(m.JTI, m.ExpMs)
	}
}

func (p *Plane) reloadRevoked(ctx context.Context) error {
	rows, err := p.pool.Query(ctx, "SELECT jti, exp_ms FROM relay_revoked_tokens WHERE exp_ms > $1", p.Now().UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	found := map[string]int64{}
	for rows.Next() {
		var jti string
		var exp int64
		if err := rows.Scan(&jti, &exp); err != nil {
			return err
		}
		found[jti] = exp
	}
	if err := rows.Err(); err != nil {
		return err
	}
	p.MergeRevoked(found)
	return nil
}

func (p *Plane) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(p.sweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Background("sweep", func() error { return p.Sweep(ctx) })
		}
	}
}

// Sweep 清一遍过期的路由、限流桶与撤销记录（也清内存副本）。
func (p *Plane) Sweep(ctx context.Context) error {
	at := p.Now().UnixMilli()
	for _, statement := range []string{
		"DELETE FROM relay_routes WHERE expires_at_ms <= $1",
		"DELETE FROM relay_limits WHERE reset_at_ms <= $1",
		"DELETE FROM relay_revoked_tokens WHERE exp_ms <= $1",
	} {
		if _, err := p.pool.Exec(ctx, statement, at); err != nil {
			return err
		}
	}
	p.PruneRevoked()
	return nil
}

func (p *Plane) notify(ctx context.Context, channel string, payload []byte) {
	_, err := p.pool.Exec(ctx, "SELECT pg_notify($1, $2)", channel, string(payload))
	p.Report("notify "+channel, err)
}

// ------------------------------------------------------------ control.Plane

// Routes 实现 control.Plane。
func (p *Plane) Routes() control.RouteTable { return routes{p} }

// Presence 实现 control.Plane。
func (p *Plane) Presence() control.Presence { return presence{p} }

// Limits 实现 control.Plane。
func (p *Plane) Limits() control.Limiter {
	return cluster.Limits{B: p.Base, Remote: p.remoteHit}
}

// Tokens 实现 control.Plane。
func (p *Plane) Tokens() control.TokenStore {
	return cluster.Tokens{
		B: p.Base,
		Persist: func(ctx context.Context, jti string, expMs int64) error {
			_, err := p.pool.Exec(ctx, `INSERT INTO relay_revoked_tokens (jti, exp_ms) VALUES ($1, $2)
				ON CONFLICT (jti) DO UPDATE SET exp_ms = GREATEST(relay_revoked_tokens.exp_ms, EXCLUDED.exp_ms)`, jti, expMs)
			return err
		},
		Broadcast: func(ctx context.Context, jti string, expMs int64) {
			p.notify(ctx, p.chans.revoked, cluster.EncodeRevoked(p.Origin, jti, expMs))
		},
	}
}

// Command 实现 control.Cluster：本节点立即收到，其余节点经 NOTIFY relay_ctl 收到。
func (p *Plane) Command(c control.NodeCommand) {
	p.EmitCommand(c)
	payload := cluster.EncodeCommand(p.Origin, c)
	p.Queue("notify "+p.chans.command, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := p.pool.Exec(ctx, "SELECT pg_notify($1, $2)", p.chans.command, string(payload))
		return err
	})
}

func (p *Plane) remoteHit(ctx context.Context, id string, window time.Duration) (int64, time.Duration, error) {
	at := p.Now().UnixMilli()
	var count, resetAt int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO relay_limits (id, count, reset_at_ms) VALUES ($1, 1, $3::bigint)
		ON CONFLICT (id) DO UPDATE SET
		  count = CASE WHEN relay_limits.reset_at_ms <= $2::bigint THEN 1 ELSE relay_limits.count + 1 END,
		  reset_at_ms = CASE WHEN relay_limits.reset_at_ms <= $2::bigint THEN $3::bigint ELSE relay_limits.reset_at_ms END
		RETURNING count, reset_at_ms`, id, at, at+window.Milliseconds()).Scan(&count, &resetAt)
	if err != nil {
		return 0, 0, err
	}
	return count, time.Duration(resetAt-at) * time.Millisecond, nil
}

// Close 停掉监听与清扫、等后台任务；自建的连接池一并关。重复调用无害。
func (p *Plane) Close() error {
	p.once.Do(func() {
		p.stop()
		<-p.loopDone
		p.Shutdown()
		if p.ownsPool {
			p.pool.Close()
		}
	})
	return nil
}

type routes struct{ p *Plane }

func (r routes) Get(ctx context.Context, sourceID string) (*control.Route, error) {
	var route control.Route
	err := r.p.pool.QueryRow(ctx, `SELECT node, tunnel_id, since_ms, core_version FROM relay_routes
		WHERE source_id = $1 AND expires_at_ms > $2`, sourceID, r.p.Now().UnixMilli()).
		Scan(&route.Node, &route.TunnelID, &route.SinceMs, &route.CoreVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}

func (r routes) Set(ctx context.Context, sourceID string, route control.Route, ttl time.Duration) error {
	var err error
	r.p.Run(func() {
		_, err = r.p.pool.Exec(ctx, `
			INSERT INTO relay_routes (source_id, node, tunnel_id, since_ms, core_version, expires_at_ms)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (source_id) DO UPDATE SET node = EXCLUDED.node,
			  tunnel_id = EXCLUDED.tunnel_id, since_ms = EXCLUDED.since_ms,
			  core_version = EXCLUDED.core_version, expires_at_ms = EXCLUDED.expires_at_ms`,
			sourceID, route.Node, route.TunnelID, route.SinceMs, route.CoreVersion, r.p.Now().Add(ttl).UnixMilli())
	})
	return err
}

// Touch 与 Delete 失败只报告（OnError），不返回：它们在心跳与收尾路径上，调用方无从补救。
func (r routes) Touch(ctx context.Context, sourceID string, ttl time.Duration) error {
	r.p.Run(func() {
		at := r.p.Now()
		_, err := r.p.pool.Exec(ctx, `UPDATE relay_routes SET expires_at_ms = $3
			WHERE source_id = $1 AND expires_at_ms > $2`, sourceID, at.UnixMilli(), at.Add(ttl).UnixMilli())
		r.p.Report("route touch", err)
	})
	return nil
}

func (r routes) Delete(ctx context.Context, sourceID, tunnelID string) error {
	r.p.Run(func() {
		_, err := r.p.pool.Exec(ctx, "DELETE FROM relay_routes WHERE source_id = $1 AND tunnel_id = $2", sourceID, tunnelID)
		r.p.Report("route delete", err)
	})
	return nil
}

type presence struct{ p *Plane }

func (pr presence) Publish(ctx context.Context, ev control.PresenceEvent) error {
	pr.p.EmitPresence(ev)
	pr.p.Run(func() { pr.p.notify(ctx, pr.p.chans.presence, cluster.EncodePresence(pr.p.Origin, ev)) })
	return nil
}

func (pr presence) Subscribe(fn func(control.PresenceEvent)) func() {
	return pr.p.SubscribePresence(fn)
}
