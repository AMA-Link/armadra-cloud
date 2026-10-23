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
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
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
}

// Running 是在跑的控制面。
type Running struct {
	// URL 是实际监听的地址（http://host:port）。
	URL     string
	DB      *postgres.DB
	Redis   goredis.UniversalClient
	Handler http.Handler

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

	// 6. 路由与监听。
	handler, err := newHandler(cfg, db, redisClient, deps.Version)
	if err != nil {
		return fail(err)
	}
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
	go func() {
		defer close(watchDone)
		watchKeys(watchCtx, db, logger, every)
	}()
	undo.push(func() { stopWatch(); <-watchDone })

	url := "http://" + hostPort(listener.Addr())
	logger.Info("控制面已就绪", obs.Mode("saas"), obs.Listen(listener.Addr().String()), obs.URL(url), obs.Version(deps.Version))
	running := &Running{URL: url, DB: db, Redis: redisClient, Handler: handler}
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
func watchKeys(ctx context.Context, db *postgres.DB, logger *obs.Logger, every time.Duration) {
	var last int64
	if revision, ok, err := db.Revision(ctx, TenantID); err == nil && ok {
		last = revision
	}
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
		last = revision
		if err := db.RefreshKeys(ctx, TenantID); err != nil {
			logger.Warn("签名钥缓存重读失败", obs.Error(err))
		}
	}
}

// newHandler 是对外的路由：/health、/ready，其余交给 /v1 与 /.well-known 的 API（还没实现的过程答 501）。
func newHandler(cfg *config.Config, db *postgres.DB, redisClient goredis.UniversalClient, version string) (http.Handler, error) {
	apiServer, err := api.New(api.Options{
		Mode: api.ModeCloud, Handlers: map[string]api.Handler{},
		Exclude: []string{"platform.health"},
	})
	if err != nil {
		return nil, err
	}
	probes := map[string]obs.Probe{
		"postgres": func(ctx context.Context) error { return db.SaaS().Read().Ping(ctx) },
		"redis":    func(ctx context.Context) error { return redisClient.Ping(ctx).Err() },
	}
	mux := http.NewServeMux()
	mux.Handle("/ready", obs.ReadyHandler(probes))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { serveHealth(w, r, version) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if apiServer.Claims(r.URL.Path) {
			apiServer.ServeHTTP(w, r)
			return
		}
		api.WriteError(w, gen.ErrNotFound, "", 0, nil)
	})
	return mux, nil
}
