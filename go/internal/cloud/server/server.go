// Package server 是 armadra-cloud serve 的装配与生命周期（SaaS 实现方案 §2.1）。
//
// 启动检查依次：账本（落后 / 比程序新 / 对不上都拒绝）→ 租户引导 → 审计分区 → Redis 连通与版本 →
// 开监听。任何一步失败都不开监听、退出码 1。日志只有进程事件，不记请求、令牌、口令与连接串。
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

// TenantID 是 SaaS 的存储租户（设计 §3.1）。
const TenantID = "cloud"

// 关闭与超时。
const (
	// requestGrace 是关闭时等在途请求的上限。
	requestGrace    = 10 * time.Second
	httpIdleTimeout = 75 * time.Second
	httpHeaderWait  = 20 * time.Second
	httpReadTimeout = 2 * time.Minute
	// keyWatchEvery 是察觉别的实例轮换签名钥的间隔（读租户修订号）。
	keyWatchEvery = 5 * time.Second
)

// Deps 是 Serve 可替换的部分（测试用）。
type Deps struct {
	// Log 缺省写到 Output（缺省标准输出）。
	Log    *obs.Logger
	Output io.Writer
	Now    func() time.Time
	// Version 是二进制版本（/health 报）。
	Version string
	// KeyWatchEvery 缩短察觉签名钥变化的间隔（测试用）。
	KeyWatchEvery time.Duration
	// Challenge 换掉人机挑战的提供方（缺省调 Turnstile 的 siteverify）。
	Challenge core.ChallengeVerifier
	// Limiter 换掉限流器（缺省 Redis 的 cloud:limit: 桶）。
	Limiter control.Limiter
	// ExtraHandlers 给测试挂额外的过程处理器（例如用 admin.* 检查凭据闸）；生产装配不设。
	ExtraHandlers map[string]api.Handler
}

// Running 是在跑的控制面。
type Running struct {
	// URL 是实际监听的地址（http://host:port）。
	URL     string
	DB      *postgres.DB
	Redis   goredis.UniversalClient
	Handler http.Handler
	// Core 与 Relay 给装配后的检查与测试用。
	Core  *core.Context
	Relay *relayctl.Memory

	closeOnce sync.Once
	closeFn   func()
}

// Close 按顺序收尾（幂等）：停收新请求 → 等在途请求 → 关 Redis → 关 PostgreSQL。
func (r *Running) Close() { r.closeOnce.Do(r.closeFn) }

// LogLevel 把配置里的级别名换成日志级别。
func LogLevel(name string) obs.Level {
	switch name {
	case "debug":
		return obs.LevelDebug
	case "warn":
		return obs.LevelWarn
	case "error":
		return obs.LevelError
	}
	return obs.LevelInfo
}

// Serve 做完启动检查、开监听，之后在后台跑；调 Close 停。
func Serve(ctx context.Context, cfg *config.Config, deps Deps) (*Running, error) {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	logger := deps.Log
	if logger == nil {
		out := deps.Output
		if out == nil {
			out = os.Stdout
		}
		logger = obs.NewNamedLogger(out, LogLevel(cfg.LogLevel), "cloud")
	}
	for _, warning := range cfg.Warnings {
		logger.Warn(warning)
	}

	var undo stack
	var failure error
	defer func() {
		if failure != nil {
			undo.unwind()
		}
	}()
	fail := func(err error) (*Running, error) { failure = err; return nil, err }

	// 1. 账本：不自动迁移。
	db, err := postgres.Open(ctx, postgres.Options{
		URL: cfg.DatabaseURL, ApplicationName: "armadra-cloud", NoMigrate: true, MasterKey: cfg.MasterKey,
		OnError: func(operation string, err error) {
			logger.Warn("存储后台操作失败", obs.Operation(operation), obs.Error(err))
		},
	})
	if err != nil {
		return fail(err)
	}
	undo.push(func() { _ = db.Close() })

	// 2. 租户引导。
	created, err := db.BootstrapTenant(ctx, TenantID, cfg.Issuer, now().UnixMilli())
	if err != nil {
		return fail(err)
	}
	if created {
		logger.Info("已引导存储租户", obs.Issuer(cfg.Issuer))
	}

	// 3. 审计分区：当月与后两个月，失败拒绝启动。
	partitions, err := db.EnsureAuditPartitions(ctx, now())
	if err != nil {
		return fail(err)
	}
	logger.Info("审计分区已就绪", obs.Count(len(partitions)))

	// 4. Redis 连通与版本。
	redisClient, err := OpenRedis(ctx, cfg.RedisURL)
	if err != nil {
		return fail(err)
	}
	undo.push(func() { _ = redisClient.Close() })

	// 5. 撤销与签发索引的重放：随 relayctl 的 Redis 实现在 S8 加入。

	// 6. 装配、路由与监听。
	assembly, err := assemble(cfg, db, redisClient, logger, now, deps)
	if err != nil {
		return fail(err)
	}
	handler := assembly.handler
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fail(fmt.Errorf("监听失败：%s", cfg.Listen))
	}
	httpServer := &http.Server{
		Handler: handler, ReadHeaderTimeout: httpHeaderWait, ReadTimeout: httpReadTimeout, IdleTimeout: httpIdleTimeout,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("服务出错", obs.Error(err))
		}
	}()
	undo.push(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), requestGrace)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
		<-serveDone
	})

	var metricsServer *http.Server
	if cfg.MetricsListen != "" {
		mln, err := net.Listen("tcp", cfg.MetricsListen)
		if err != nil {
			return fail(fmt.Errorf("指标监听失败：%s", cfg.MetricsListen))
		}
		render := func() string {
			return "# HELP armadra_cloud_up 控制面在跑。\n# TYPE armadra_cloud_up gauge\narmadra_cloud_up 1\n"
		}
		metricsServer = &http.Server{
			Handler: obs.MetricsHandler(render, cfg.MetricsToken), ReadHeaderTimeout: httpHeaderWait,
			ErrorLog: log.New(io.Discard, "", 0),
		}
		go func() { _ = metricsServer.Serve(mln) }()
		undo.push(func() { _ = metricsServer.Close() })
	}

	// 别的实例轮换了签名钥（租户修订号变了）：重读本实例的钥缓存。
	watchCtx, stopWatch := context.WithCancel(context.Background())
	watchDone := make(chan struct{})
	every := deps.KeyWatchEvery
	if every <= 0 {
		every = keyWatchEvery
	}
	baseline, _, err := db.Revision(ctx, TenantID)
	if err != nil {
		stopWatch()
		return fail(err)
	}
	if err := db.RefreshKeys(ctx, TenantID); err != nil {
		stopWatch()
		return fail(err)
	}
	go func() {
		defer close(watchDone)
		watchKeys(watchCtx, db, logger, every, baseline)
	}()
	undo.push(func() { stopWatch(); <-watchDone })

	url := "http://" + hostPort(listener.Addr())
	logger.Info("控制面已就绪", obs.Mode("saas"), obs.Listen(listener.Addr().String()), obs.URL(url), obs.Version(deps.Version))
	// 设备「最近使用」每 5 分钟批量落盘，关闭时再落一次。
	flushCtx, stopFlush := context.WithCancel(context.Background())
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		ticker := time.NewTicker(core.LastSeenFlushEach)
		defer ticker.Stop()
		for {
			select {
			case <-flushCtx.Done():
				return
			case <-ticker.C:
				if err := assembly.auth.Flush(flushCtx); err != nil {
					logger.Warn("最近使用时间落盘失败", obs.Error(err))
				}
			}
		}
	}()
	undo.push(func() {
		stopFlush()
		<-flushDone
		ctx, cancel := context.WithTimeout(context.Background(), requestGrace)
		defer cancel()
		if err := assembly.auth.Flush(ctx); err != nil {
			logger.Warn("最近使用时间落盘失败", obs.Error(err))
		}
	})

	running := &Running{URL: url, DB: db, Redis: redisClient, Handler: handler, Core: assembly.core, Relay: assembly.relay}
	running.closeFn = func() {
		logger.Info("控制面正在关闭")
		undo.unwind()
		logger.Info("控制面已关闭")
	}
	return running, nil
}

func hostPort(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if host == "::" || host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// watchKeys 每隔一段时间读一次租户修订号，变了就让签名钥缓存重读。
// 基线（baseline）由调用方在开始监视之前同步取好并先刷新一次缓存：基线之后的轮换一定被察觉；
// 不能在后台协程里才取基线，那样协程启动前发生的轮换会被当成基线而永远不重读。
func watchKeys(ctx context.Context, db *postgres.DB, logger *obs.Logger, every time.Duration, baseline int64) {
	last := baseline
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		revision, ok, err := db.Revision(ctx, TenantID)
		if err != nil || !ok || revision == last {
			continue
		}
		// 重读成功才记下新基线；失败下一轮再试。
		if err := db.RefreshKeys(ctx, TenantID); err != nil {
			logger.Warn("签名钥缓存重读失败", obs.Error(err))
			continue
		}
		last = revision
	}
}
