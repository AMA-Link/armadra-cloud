package personal

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	clusterpkg "github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/factory"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/dataplane"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/tlsx"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
	"github.com/AMA-Link/armadra-cloud/go/internal/trafficstore"
	"github.com/AMA-Link/armadra-cloud/go/internal/web"
)

// 个人中转装配（中继设计 §1、§3.5）：存储 → 签名钥 → 控制面 → TLS → 数据面（隧道 + /s/<id>/ 边缘）
// + /v1 子集 + /v1/me/stream + 托管页面。
//
// 日志只有启动 / 停止 / 证书 / 存储这类进程事件，不记请求、令牌、口令与连接串。

// 关闭与超时。
const (
	shutdownGrace   = 5 * time.Second
	httpIdleTimeout = 75 * time.Second
	httpHeaderWait  = 76 * time.Second
	httpReadTimeout = 5 * time.Minute
)

// ServeDeps 是 Serve 的可替换部分（测试用）。
type ServeDeps struct {
	// Log 缺省写到标准输出。
	Log *obs.Logger
	Now func() time.Time
	// Challenge 换掉人机挑战的提供方（缺省按 --challenge 调 siteverify）。
	Challenge core.ChallengeVerifier
	// StreamPing 缩短 me-stream 的心跳（测试用）。
	StreamPing time.Duration
	// WatchInterval 缩短存储变化的检查间隔（测试用，缺省 1 秒）。
	WatchInterval time.Duration
	// ACMEIssuer 换掉 ACME 的签发（测试用）。
	ACMEIssuer tlsx.Issuer
	// Output 是日志的去处，Log 为空时用；缺省标准输出。
	Output io.Writer
}

// Running 是在跑的个人中转。
type Running struct {
	URL         string
	Issuer      string
	MetricsURL  string
	Fingerprint string
	Store       Store
	Plane       *dataplane.Plane
	// Cluster、Core 与 Stream 给装配后的检查与测试用。
	Cluster *Cluster
	Core    *core.Context
	Stream  *MeStream
	// Meter 是流量计量器；--traffic-retention-days 0 时为 nil。
	Meter *traffic.Meter

	flush func(ctx context.Context)
	close func()
}

// Flush 立即把累计的设备与源的最近在线时间落库（平时每 5 分钟一次）。
func (r *Running) Flush(ctx context.Context) { r.flush(ctx) }

// Close 按顺序收尾（幂等）。
func (r *Running) Close() { r.close() }

// IssuerFor 是对外来源：--public-origin，否则 https://<host>:<port>。
func IssuerFor(o ServeOptions) string {
	if o.PublicOrigin != "" {
		return o.PublicOrigin
	}
	return DefaultIssuer(o.Host, o.Port)
}

// logLevel 把 --log-level 换成日志级别。
func logLevel(name string) obs.Level {
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

// openOrInit 打开存储；没有时若给了账号与口令就自动 init（compose 与 pnpm relay:personal 走这条）。
func openOrInit(ctx context.Context, o ServeOptions, issuer string, now time.Time, logger *obs.Logger) (Store, error) {
	kind, _, err := ResolveKind(o.DataDir, o.Store)
	if err != nil {
		return nil, err
	}
	// PostgreSQL：数据目录是空的不等于没初始化（别的节点可能已经建好），要问库。
	var missing bool
	if kind == StorePostgres && o.Database != nil {
		found, err := PostgresStoreExists(ctx, *o.Database)
		if err != nil {
			return nil, err
		}
		missing = !found
	} else {
		missing = !StoreExists(o.DataDir)
	}
	if missing && o.Account != "" && o.PasswordFile != "" {
		password, err := ReadPasswordFile(o.PasswordFile)
		if err != nil {
			return nil, err
		}
		st, err := CreateState(ctx, CreateStateInput{
			DataDir: o.DataDir, Account: o.Account, Password: password, Issuer: issuer,
			Store: kind, Database: o.Database, Now: now,
		})
		if err != nil {
			return nil, err
		}
		logger.Info("已初始化个人中转", obs.Store(string(kind)))
		return st, nil
	}
	return OpenStore(ctx, o.DataDir, o.Store, o.Database)
}

func set(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

// Serve 起个人中转：监听成功后返回，之后在后台跑；调 Close 停。
func Serve(ctx context.Context, o ServeOptions, deps ServeDeps) (*Running, error) {
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
		logger = obs.NewLogger(out, logLevel(o.LogLevel))
	}
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("数据目录建不出来：%s", o.DataDir)
	}
	issuer := IssuerFor(o)
	st, err := openOrInit(ctx, o, issuer, now(), logger)
	if err != nil {
		return nil, err
	}
	running, err := serving(ctx, o, deps, serveBase{log: logger, now: now, issuer: issuer, store: st})
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return running, nil
}

type serveBase struct {
	log    *obs.Logger
	now    func() time.Time
	issuer string
	store  Store
}

// onceFn 让收尾函数只跑一次。
type stack struct{ fns []func() }

func (s *stack) push(fn func()) { s.fns = append(s.fns, fn) }
func (s *stack) unwind() {
	for i := len(s.fns) - 1; i >= 0; i-- {
		s.fns[i]()
	}
	s.fns = nil
}

func serving(ctx context.Context, o ServeOptions, deps ServeDeps, base serveBase) (running *Running, err error) {
	logger, now, issuer, st := base.log, base.now, base.issuer, base.store
	var undo stack
	defer func() {
		if err != nil {
			undo.unwind()
		}
	}()
	if st.Tenant().Issuer != issuer {
		if err := st.SetIssuer(ctx, issuer); err != nil {
			return nil, err
		}
		logger.Warn("对外地址变了，已登记的源需要重新登记", obs.Issuer(issuer))
	}

	// TLS。
	var manager tlsx.Manager
	switch o.TLS {
	case "self-signed":
		if manager, err = tlsx.SelfSigned(o.DataDir, tlsx.Names(o.Host)); err != nil {
			return nil, err
		}
	case "file":
		if manager, err = tlsx.File(o.TLSCert, o.TLSKey); err != nil {
			return nil, err
		}
	case "acme":
		acmeOptions := tlsx.ACMEOptions{
			DataDir: o.DataDir, Email: o.ACME.Email, Names: []string{o.Host},
			DirectoryURL: o.ACME.Directory, HTTPHost: o.ACME.HTTPListen.Host, HTTPPort: o.ACME.HTTPListen.Port,
			RedirectOrigin: issuer, Issuer: deps.ACMEIssuer, Now: now,
			OnEvent: func(event string, failures int) {
				if event == "renewed" {
					logger.Info("ACME 证书已续期")
					return
				}
				logger.Warn("ACME 续期失败，继续用旧证书", obs.Failures(failures))
			},
		}
		if o.ACME.CABundle != "" {
			pem, err := os.ReadFile(o.ACME.CABundle)
			if err != nil {
				return nil, Usagef("--acme-ca-bundle 读不出来：%s", o.ACME.CABundle)
			}
			acmeOptions.CABundle = pem
		}
		acme, err := tlsx.StartACME(ctx, acmeOptions)
		if err != nil {
			return nil, err
		}
		undo.push(func() { _ = acme.Close() })
		manager = acme
	}

	// 签名钥与控制面。
	signer := core.NewSigner(st.Keys(), now)
	revokeCtx := &core.Context{TenantID: store.DefaultTenant, Ports: st.Ports()}
	stored := st.Tenant()
	var pool *pgxpool.Pool
	if owner, ok := st.(poolOwner); ok {
		pool = owner.PostgresPool()
	}
	cluster, err := openCluster(ctx, ClusterInput{
		Kind: factory.Kind(o.Control), NodeID: o.NodeID, RedisURL: o.RedisURL, Database: o.Database, Pool: pool,
		Options: clusterpkg.Options{
			Issuer: issuer, Mode: "personal", Now: now,
			JWKS: func(ctx context.Context) ([]byte, error) {
				set, err := signer.JWKS(ctx)
				if err != nil {
					return nil, err
				}
				return json.Marshal(set)
			},
			Limits: EdgeLimitRules(o.RateLimits, storedRateLimits(stored.RateLimits)),
			Revoked: func(ctx context.Context, jti string) (bool, error) {
				return core.IsRevokedJTI(ctx, revokeCtx, jti, now().UnixMilli())
			},
			OnError: func(operation string, err error) {
				logger.Warn("控制面后台操作失败", obs.Operation(operation), obs.Error(err))
			},
		},
	})
	if err != nil {
		return nil, err
	}
	undo.push(func() { _ = cluster.Close() })

	// 控制核。
	var plane *dataplane.Plane
	disconnect := func(f edge.ConnectionFilter) {
		if plane != nil {
			plane.Edge.Disconnect(f)
		}
	}
	events := core.NewEventBus()
	coreCtx := &core.Context{
		TenantID: store.DefaultTenant, Ports: st.Ports(), Signer: signer,
		Limits:   limiterAdapter{cluster.Limits()},
		Presence: presenceAdapter{cluster.Presence()},
		Relay: relayAdapter{
			routes: cluster.Routes(), command: cluster.Command, disconnect: disconnect,
		},
		PasswordPolicy: core.DefaultPasswordPolicy, Now: now, Events: events,
	}
	if o.Challenge != nil {
		secret, err := readTurnstileSecret(o.Challenge.SecretFile)
		if err != nil {
			return nil, err
		}
		verifier := deps.Challenge
		if verifier == nil {
			verifier = core.HTTPVerifier{Secret: secret}
		}
		gate, err := core.NewChallengeGate(core.ChallengeGateOptions{
			SiteKey: o.Challenge.SiteKey, Scope: o.Challenge.Scope, Hostnames: o.Challenge.Hostnames,
			Verifier: verifier, Now: now,
			OnRejected: func(procedure core.ChallengeScope, reasons []string) {
				logger.Info("人机挑战没通过", obs.Procedure(string(procedure)), obs.Reasons(strings.Join(reasons, ",")))
			},
		})
		if err != nil {
			return nil, err
		}
		coreCtx.Challenge = gate
		scope := make([]string, len(o.Challenge.Scope))
		for i, item := range o.Challenge.Scope {
			scope[i] = string(item)
		}
		logger.Info("已开启人机挑战", obs.Provider("turnstile"), obs.Scope(strings.Join(scope, ",")))
	}
	meCore := core.NewMeStream(coreCtx)
	stream := NewMeStream(meCore, now, deps.StreamPing)
	undo.push(stream.Close)

	webRoot := web.UsableRoot(o.WebRoot)
	if o.WebRoot != "" && webRoot == "" {
		logger.Warn("页面产物目录没有 index.html，/app 答错误页")
	}
	trusted := o.TrustedProxy
	if trusted == nil {
		trusted = storedTrustedProxy(stored.TrustedProxy)
	}
	clientIP, err := edge.NewClientIP(trusted)
	if err != nil {
		return nil, fmt.Errorf("trustedProxy 不合法：%v", err)
	}
	handlers, authenticator := api.PersonalHandlers(coreCtx, api.PersonalOptions{
		Stream: meCore,
		WebApp: func() string {
			if webRoot == "" {
				return ""
			}
			return st.Tenant().Issuer + "/app/"
		},
		// 每次现读：别的进程改了 displayName 也即时生效。
		Name: func() string {
			if o.Name != "" {
				return o.Name
			}
			if name := st.Tenant().DisplayName; name != nil {
				return *name
			}
			return o.Host
		},
	})
	apiServer, err := api.New(api.Options{
		Mode: api.ModePersonal, Handlers: handlers, Authenticator: authenticator, ClientIP: requestIP(clientIP),
		OnError: func(err error) { logger.Error("请求处理失败", obs.Error(err)) },
	})
	if err != nil {
		return nil, err
	}
	var challengeInfo func() *web.Challenge
	if o.Challenge != nil {
		challengeInfo = func() *web.Challenge {
			return &web.Challenge{EmbedOrigins: core.TrustedOriginsOf(st.Tenant().TenantSettings)}
		}
	}
	pages := web.New(web.Options{
		Root: webRoot,
		CAPEM: func() []byte {
			if manager == nil {
				return nil
			}
			return manager.Current().Anchor
		},
		Challenge: challengeInfo,
	})

	// 流量统计。
	retentionDays := o.TrafficRetentionDays
	var meter *traffic.Meter
	if retentionDays > 0 {
		meter = traffic.NewMeter(traffic.MeterOptions{
			Node: o.NodeID, Write: trafficstore.Writer(st.Ports().Traffic, store.DefaultTenant), Now: now,
			OnError: func(err error, dropped int) {
				logger.Warn("流量统计写入失败", obs.Error(err), obs.Dropped(dropped))
			},
		})
	}

	// 数据面。
	probes := map[string]obs.Probe{
		"dataDir": func(context.Context) error { return accessDir(o.DataDir) },
	}
	for name, probe := range cluster.Probes {
		probes[name] = probe
	}
	plane, err = dataplane.New(dataplane.Options{
		Mode: "personal", NodeID: o.NodeID, Version: Version, Control: cluster.Cluster,
		Addressing: edge.PathAddressing(), Log: logger,
		CORSOrigins: func(string) []string { return core.TrustedOriginsOf(st.Tenant().TenantSettings) },
		ClientIP:    clientIP, Traffic: meter, Probes: probes,
		UpgradeExtra: stream.HandleUpgrade,
		Fallback: func(w http.ResponseWriter, r *http.Request) bool {
			if apiServer.Claims(r.URL.Path) {
				apiServer.ServeHTTP(w, r)
				return true
			}
			return pages.Handle(w, r)
		},
	})
	if err != nil {
		return nil, err
	}

	// 监听。
	var dynamic *tlsx.Dynamic
	if manager != nil {
		dynamic = tlsx.NewDynamic(manager.Current())
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(o.Listen.Host, strconv.Itoa(o.Listen.Port)))
	if err != nil {
		return nil, fmt.Errorf("监听失败：%s", net.JoinHostPort(o.Listen.Host, strconv.Itoa(o.Listen.Port)))
	}
	scheme := "http"
	if dynamic != nil {
		scheme = "https"
		listener = tls.NewListener(listener, dynamic.Config())
	}
	server := &http.Server{
		Handler: plane, ReadHeaderTimeout: httpHeaderWait, ReadTimeout: httpReadTimeout, IdleTimeout: httpIdleTimeout,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("服务出错", obs.Error(err))
		}
	}()
	undo.push(func() { _ = listener.Close() })
	url := scheme + "://" + hostPort(listener.Addr())

	var metricsServer *http.Server
	var metricsURL string
	if o.MetricsListen != nil {
		addr := net.JoinHostPort(o.MetricsListen.Host, strconv.Itoa(o.MetricsListen.Port))
		mln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("指标监听失败：%s", addr)
		}
		metricsServer = &http.Server{
			Handler: obs.MetricsHandler(plane.Metrics.Render, o.MetricsToken), ReadHeaderTimeout: httpHeaderWait,
			ErrorLog: log.New(io.Discard, "", 0),
		}
		metricsURL = "http://" + hostPort(mln.Addr())
		go func() { _ = metricsServer.Serve(mln) }()
	}

	background, stopBackground := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	every := func(interval time.Duration, fn func(context.Context)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-background.Done():
					return
				case <-ticker.C:
					fn(background)
				}
			}
		}()
	}

	// 流量计量落库与保留期清理。
	var meterDone chan struct{}
	if meter != nil {
		meterDone = make(chan struct{})
		go func() { defer close(meterDone); meter.Run(background) }()
		prune := func(ctx context.Context) {
			cutoff := now().Add(-time.Duration(retentionDays) * day).UnixMilli()
			if _, err := st.Ports().Traffic.Prune(ctx, store.DefaultTenant, cutoff); err != nil {
				logger.Warn("流量统计清理失败", obs.Error(err))
			}
		}
		workers.Add(1)
		go func() { defer workers.Done(); prune(background) }()
		every(day, prune)
	}

	// 源上下线：记最近在线与版本（只在内存累计，和设备的 lastSeen 一起每 5 分钟落盘）。
	var seenMu sync.Mutex
	seen := map[string]SourceSeen{}
	offPresence := cluster.Presence().Subscribe(func(ev control.PresenceEvent) {
		seenMu.Lock()
		defer seenMu.Unlock()
		entry := SourceSeen{AtMs: ev.AtMs}
		if ev.Type == control.PresenceSourceOnline {
			entry.CoreVersion = ev.CoreVersion
		}
		seen[ev.SourceID] = entry
	})
	flush := func(ctx context.Context) {
		if err := authenticator.Flush(ctx); err != nil {
			logger.Warn("最近在线时间写入失败", obs.Error(err))
		}
		seenMu.Lock()
		batch := seen
		seen = map[string]SourceSeen{}
		seenMu.Unlock()
		if len(batch) > 0 {
			if err := st.RecordSourcesSeen(ctx, batch); err != nil {
				logger.Warn("最近在线时间写入失败", obs.Error(err))
			}
		}
	}
	every(core.LastSeenFlushEach, flush)

	// 别的进程（passwd / rotate-key / links revoke）或别的节点改了存储：新撤销的源与链接立即断开。
	revokedSources, revokedLinks, err := st.Revoked(ctx)
	if err != nil {
		stopBackground()
		workers.Wait()
		_ = server.Close()
		if metricsServer != nil {
			_ = metricsServer.Close()
		}
		return nil, err
	}
	previousSources, previousLinks := set(revokedSources), set(revokedLinks)
	stopWatch := st.Watch(func() {
		sources, links, err := st.Revoked(background)
		if err != nil {
			logger.Error("存储重新读取失败", obs.Error(err))
			return
		}
		for _, id := range sources {
			if !previousSources[id] {
				cluster.Command(control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: id})
				plane.Edge.Disconnect(edge.ConnectionFilter{SourceID: id})
			}
		}
		for _, id := range links {
			if !previousLinks[id] {
				plane.Edge.Disconnect(edge.ConnectionFilter{SubjectPrefix: core.GuestPrefix(id)})
			}
		}
		previousSources, previousLinks = set(sources), set(links)
		logger.Info("存储有外部改动，已重新读取", obs.Store(string(st.Kind())))
	}, func(err error) {
		logger.Error("存储重新读取失败", obs.Error(err))
	}, deps.WatchInterval)

	release := HoldRunLock(o.DataDir)

	var stopTLS func()
	if manager != nil {
		stopTLS = manager.Start(func(m tlsx.Material) {
			dynamic.Set(m)
			logger.Info("证书已更换", obs.NotAfter(m.NotAfter.UTC().Format(time.RFC3339)))
		}, func(err error) {
			logger.Warn("证书更新失败，继续用旧证书", obs.Error(err))
		})
	}

	fingerprint := ""
	if manager != nil && manager.Current().Source != "acme" {
		fingerprint = manager.Current().Fingerprint
	}
	webState := "missing"
	if webRoot != "" {
		webState = "ready"
	}
	fields := []obs.Field{
		obs.URL(url), obs.Issuer(issuer), obs.Host(o.Host), obs.TLS(o.TLS), obs.Store(string(st.Kind())),
		obs.Control(o.Control), obs.Web(webState),
	}
	if o.Control != "" && o.Control != "memory" {
		fields = append(fields, obs.Node(o.NodeID))
	}
	if fingerprint != "" {
		fields = append(fields, obs.Fingerprint(fingerprint))
	}
	if metricsURL != "" {
		fields = append(fields, obs.MetricsURL(metricsURL))
	}
	if retentionDays > 0 && st.Kind() == StoreFile {
		logger.Info("状态文件存储：流量统计只在内存里累计，重启归零", obs.Store(string(st.Kind())))
	}
	logger.Info("个人中转已启动", fields...)

	var closeOnce sync.Once
	running = &Running{
		URL: url, Issuer: issuer, MetricsURL: metricsURL, Fingerprint: fingerprint, Store: st, Plane: plane,
		Cluster: cluster, Core: coreCtx, Stream: stream, Meter: meter, flush: flush,
	}
	running.close = func() {
		closeOnce.Do(func() {
			// 先停后台任务与续期，再按设计的顺序收尾：
			// 隧道 GOAWAY → 断边缘连接 → 关服务 → 关指标 → 流量落库 → 释放 serve.pid → 关存储。
			stopWatch()
			if stopTLS != nil {
				stopTLS()
			}
			offPresence()
			stopBackground()
			workers.Wait()
			flushCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			flush(flushCtx)
			cancel()
			stream.Close()
			plane.Shutdown(shutdownGrace)
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
			_ = server.Shutdown(shutdownCtx)
			if metricsServer != nil {
				_ = metricsServer.Shutdown(shutdownCtx)
			}
			cancelShutdown()
			_ = server.Close()
			<-serveDone
			if meterDone != nil {
				<-meterDone
			}
			release()
			undo.unwind()
			_ = st.Close()
			logger.Info("个人中转已停止")
		})
	}
	return running, nil
}

// hostPort 是监听地址的写法：IPv6 带方括号。
func hostPort(addr net.Addr) string { return addr.String() }

// accessDir 数据目录可读可写（/ready 的探针）。
func accessDir(dir string) error {
	return syscall.Access(dir, 0x4|0x2)
}

// readTurnstileSecret 读 secret 文件（去首尾空白）；空文件当作没配。
func readTurnstileSecret(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", Statef(Invalid, "Turnstile secret 文件读不出来：%s", file)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", Statef(Invalid, "Turnstile secret 文件是空的：%s", file)
	}
	return secret, nil
}
