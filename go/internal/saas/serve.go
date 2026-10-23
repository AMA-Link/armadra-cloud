package saas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
	"github.com/AMA-Link/armadra-cloud/go/internal/dataplane"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/httperr"
	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

// Run 跑 `armadra-relay saas <args>`：只认 `serve`（与 `--help`）；收到 SIGINT / SIGTERM 后排空、停下并以 0 退出。
func Run(args []string, env map[string]string, version, usage string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "--help" {
			fmt.Fprintln(stdout, usage)
			return exitOK
		}
	}
	if len(args) != 1 || args[0] != "serve" {
		fmt.Fprintf(stderr, "未知命令：saas %s\n\n%s\n", strings.Join(args, " "), usage)
		return exitUsage
	}
	cfg, issues := LoadConfig(env, os.Hostname)
	if len(issues) > 0 {
		fmt.Fprintf(stderr, "配置无效：\n  - %s\n", strings.Join(issues, "\n  - "))
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := Serve(ctx, cfg, version, stdout, nil); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitFail
	}
	return exitOK
}

// Node 是一个起好的 SaaS 中继节点（测试从 ready 回调里拿到它）。
type Node struct {
	URL   string
	Plane *dataplane.Plane
	// Drain 开始排空（与收到 drain 命令相同）。
	Drain func()
}

// Serve 起节点，ctx 结束时排空并关掉；ready 非空时在监听就绪后收到节点（测试用）。
func Serve(ctx context.Context, cfg Config, version string, logOut io.Writer, ready func(*Node)) error {
	log := obs.NewLogger(logOut, cfg.LogLevel)
	// 驱动自己的日志不是一行一个 JSON，还带着主机名：关掉，失败由 OnError 与 /ready 报告。
	goredis.SetLogger(quietRedis{})
	prefix := cfg.RedisPrefix
	if prefix == "" {
		prefix = redis.DefaultPrefix
	}
	onError := func(operation string, err error) {
		log.Warn("控制面后台操作失败", obs.Operation(operation), obs.Error(err))
	}
	redisOptions, err := goredis.ParseURL(cfg.RedisURL)
	if err != nil {
		return errors.New("RELAY_REDIS_URL 不合格式")
	}
	redisOptions.DialTimeout = 5 * time.Second
	client := goredis.NewClient(redisOptions)
	defer func() { _ = client.Close() }()

	// 控制面：订阅本节点频道；quota / incident 交给准入闸。
	var ctl *redis.Plane
	g := newGate(client, prefix, cfg.NodeID, cfg.TeamConcurrent, func() control.Limiter { return ctl.Limits() }, onError)
	defer g.close()
	origin := jose.HTTPFetch(&http.Client{Timeout: 10 * time.Second}, cfg.JWKSURL)
	startCtx, cancelStart := context.WithTimeout(ctx, 10*time.Second)
	defer cancelStart()
	ctl, err = redis.New(startCtx, redis.Options{
		Options: cluster.Options{
			Issuer: cfg.CloudIssuer, Mode: "saas", NodeID: cfg.NodeID,
			JWKS:   sharedJWKS(client, prefix, cfg.CloudIssuer, origin),
			Limits: teamLimitRules(cfg), OnError: onError,
		},
		Client: client, Prefix: prefix, NodeChannel: true, OnSignal: g.signal, OnSubscribed: g.reload,
	})
	if err != nil {
		return fmt.Errorf("控制面（Redis）起不来：%s", err.Error())
	}
	defer func() { _ = ctl.Close() }()

	// 流量统计（可选）：只在入口节点计，带 team。
	var (
		meter     *traffic.Meter
		pool      *pgxpool.Pool
		meterDone = make(chan struct{})
	)
	meterCtx, stopMeter := context.WithCancel(context.Background())
	defer stopMeter()
	if cfg.DatabaseURL != "" {
		if pool, err = openTraffic(startCtx, cfg.DatabaseURL); err != nil {
			return err
		}
		defer pool.Close()
		meter = traffic.NewMeter(traffic.MeterOptions{
			Node: cfg.NodeID, Write: trafficWriter(pool),
			OnError: func(err error, dropped int) {
				log.Warn("流量统计写入失败", obs.Error(err), obs.Dropped(dropped))
			},
		})
		go func() { defer close(meterDone); meter.Run(meterCtx) }()
	} else {
		close(meterDone)
		log.Warn("没配 RELAY_DATABASE_URL，不记流量")
	}

	clientIP, err := edge.NewClientIP(cfg.TrustedProxy)
	if err != nil {
		return fmt.Errorf("RELAY_TRUSTED_PROXY 不合法：%v", err)
	}
	var draining sync.Once
	var drainingNow bool
	var drainMu sync.Mutex
	probes := map[string]obs.Probe{
		"redis": func(ctx context.Context) error { return client.Ping(ctx).Err() },
		// 排空中答不就绪：前面的负载均衡不再往这里送新连接。
		"node": func(context.Context) error {
			drainMu.Lock()
			defer drainMu.Unlock()
			if drainingNow {
				return errors.New("排空中")
			}
			return nil
		},
	}
	if pool != nil {
		probes["database"] = func(ctx context.Context) error { return pool.Ping(ctx) }
	}
	plane, err := dataplane.New(dataplane.Options{
		Mode: "saas", NodeID: cfg.NodeID, Version: version, Control: ctl,
		Addressing: edge.HostAddressing(cfg.Domain), Log: log, ClientIP: clientIP, Traffic: meter,
		Probes: probes, Gate: g, ClientKick: true,
	})
	if err != nil {
		return err
	}
	var planeDown sync.Once
	shutdownPlane := func(grace time.Duration) { planeDown.Do(func() { plane.Shutdown(grace) }) }
	defer shutdownPlane(0)

	// 节点表。
	reg := newRegistry(client, prefix, control.NodeInfo{
		ID: cfg.NodeID, URL: cfg.TunnelURL, Weight: cfg.Weight, Version: version, InternodeURL: cfg.InternodeURL,
	}, func() (int, int) {
		tracked, _ := plane.Edge.ConnCounts()
		tunnels, _ := plane.Hub.Counts()
		return tracked, tunnels
	}, onError)
	if err := reg.write(startCtx); err != nil {
		onError("node table", err)
	}
	bgCtx, stopBg := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { defer bg.Done(); reg.run(bgCtx) }()
	go func() { defer bg.Done(); g.run(bgCtx) }()

	// 排空：节点表标 draining、/ready 不就绪、隧道 GOAWAY rebalance、不再收新隧道。
	drain := func() {
		draining.Do(func() {
			drainMu.Lock()
			drainingNow = true
			drainMu.Unlock()
			wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			reg.drain(wctx)
			cancel()
			tunnels, _ := plane.Hub.Counts()
			log.Info("开始排空", obs.Node(cfg.NodeID), obs.Tunnels(tunnels))
			plane.Hub.Rebalance(cfg.DrainGrace)
		})
	}

	// 控制面命令：kick 只踢客户端连接（closeTunnel 由隧道表处理），drain 只认发给本节点的。
	unsubscribe := ctl.NodeCommands().Subscribe(func(c control.NodeCommand) {
		switch c.Type {
		case control.CommandKick:
			plane.Edge.Disconnect(edge.ConnectionFilter{
				SourceID: c.SourceID, Subject: c.Subject, SubjectPrefix: c.SubjectPrefix, DeviceID: c.DeviceID,
			})
			log.Info("按控制面命令断开客户端连接", obs.SourceID(c.SourceID), obs.Reason(c.Reason))
		case control.CommandDrain:
			if c.Node == cfg.NodeID {
				go drain()
			}
		}
	})
	defer unsubscribe()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 事故开关：拒绝新隧道（端到端通道由准入闸拒）。
		if r.Header.Get("Upgrade") != "" && g.Rejecting() && !plane.Edge.Addressed(r) && pathOf(r) == gen.PathTunnel {
			httperr.Write(w, gen.ErrLimitReached, httperr.Options{})
			return
		}
		plane.ServeHTTP(w, r)
	})
	// 字面量 IPv4 只监听 IPv4（同 Node：0.0.0.0 不变成 [::]，日志里的地址一致）。
	network := "tcp"
	if ip := net.ParseIP(cfg.Host); ip != nil && ip.To4() != nil {
		network = "tcp4"
	} else if ip != nil {
		network = "tcp6"
	}
	ln, err := net.Listen(network, net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		stopBg()
		bg.Wait()
		return fmt.Errorf("监听失败：%w", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 76 * time.Second, IdleTimeout: 75 * time.Second}
	var metricsSrv *http.Server
	if cfg.MetricsListen != "" {
		mln, err := net.Listen("tcp", cfg.MetricsListen)
		if err != nil {
			_ = ln.Close()
			stopBg()
			bg.Wait()
			return fmt.Errorf("指标监听失败：%w", err)
		}
		metricsSrv = &http.Server{Handler: obs.MetricsHandler(func() string { return plane.Metrics.Render() + g.render() }, cfg.MetricsToken), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = metricsSrv.Serve(mln) }()
	}
	addr := ln.Addr().(*net.TCPAddr)
	url := "http://" + net.JoinHostPort(addr.IP.String(), strconv.Itoa(addr.Port))
	log.Info("中继已启动", obs.URL(url), obs.Mode("saas"), obs.Node(cfg.NodeID))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if ready != nil {
		ready(&Node{URL: url, Plane: plane, Drain: drain})
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	}

	// 下线：先排空（源转到别的节点），等隧道走完或宽限到，再按数据面的顺序收尾。
	drain()
	deadline := time.Now().Add(cfg.DrainGrace + 2*time.Second)
	for time.Now().Before(deadline) {
		if tunnels, _ := plane.Hub.Counts(); tunnels == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	shutdownPlane(time.Second)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(sctx)
	}
	stopBg()
	bg.Wait()
	stopMeter()
	<-meterDone
	reg.remove(sctx)
	log.Info("中继已停止")
	return serveErr
}

func pathOf(r *http.Request) string {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	return path
}

// quietRedis 吞掉 go-redis 的内部日志（连接池重拨等）。
type quietRedis struct{}

func (quietRedis) Printf(context.Context, string, ...any) {}
