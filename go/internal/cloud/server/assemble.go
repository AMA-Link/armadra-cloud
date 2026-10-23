package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/clientip"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/accounts"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/billing"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/events"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/grants"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/hosts"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/invitations"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mestream"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/platform"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/teams"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	controlredis "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// assembly 是装配好的控制核与路由。
type assembly struct {
	handler http.Handler
	core    *core.Context
	auth    *core.Authenticator
	relay   *relayctl.Memory
	// lockouts 是未注册邮箱的 Redis 失败计数（因 Redis 不可用而拒绝的次数进指标）。
	lockouts *httpx.UnknownLockouts
	// jobs 是作业运行器（serve 在监听前启动、关闭时停止领取）。
	jobs *jobs.Runner
	// control 是对节点的约定（落盘包装之后的）；sync 是 Redis 实现的重放（内存实现时为 nil）。
	control relayctl.Control
	sync    *relayctl.Sync
	// presence 累计主机上下线的最近在线，serve 周期性落盘。
	presence *presenceRecorder
	// stream 是 /v1/me/stream 的 WebSocket 外壳（关闭时先发 1001）。
	stream *mestream.MeStream
	// closers 在关闭时按后进先出执行（Plane 的订阅等）。
	closers []func()
}

// newInstanceID 是控制面实例的随机标识，广播外壳的 origin 取 cloud/<实例 id>。
func newInstanceID() string {
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// assemble 把存储、Redis、控制核服务、闸与各批的过程接成一个 http.Handler：/health、/ready，
// 其余交给 /v1 与 /.well-known 的 API（还没实现的过程答 501 not_implemented）。
func assemble(ctx context.Context, cfg *config.Config, db *postgres.DB, redisClient goredis.UniversalClient, logger *obs.Logger, now func() time.Time, deps Deps) (built *assembly, err error) {
	saasStore := db.SaaS()
	out := &assembly{}
	instance := newInstanceID()
	defer func() {
		if err != nil {
			out.close()
		}
	}()
	// 对节点的约定：缺省包装 control/redis.Plane；测试可注入内存实现来断言发出的命令。
	var inner relayctl.Control
	var redisControl *relayctl.Redis
	if deps.Relay != nil {
		inner, out.relay = deps.Relay, deps.Relay
	} else {
		var syncer *relayctl.Sync
		redisControl, syncer, err = newRedisControl(ctx, cfg, instance, saasStore, redisClient, logger, now, deps, out)
		if err != nil {
			return nil, err
		}
		inner, out.sync = redisControl, syncer
	}
	// 签发索引与撤销落盘到 PostgreSQL（设计 §7.2）；Redis（或内存）只是快路径。
	durable := relayctl.NewDurable(inner, saasStore, TenantID, now, logger)
	out.control = durable
	limiter := deps.Limiter
	if limiter == nil {
		limiter = controlredis.NewLimiter(redisClient, httpx.LimitPrefix, httpx.Rules, func(err error) {
			logger.Warn("限流后台操作失败", obs.Operation("limit"), obs.Error(err))
		})
	}
	limits := httpx.NewLimits(limiter)
	relayOrigin := func(sourceID string) string { return "https://" + sourceID + ".src." + cfg.RelayDomain }

	coreCtx := &core.Context{
		TenantID: TenantID, Ports: db.SaaSPorts(), Signer: core.NewSigner(db.Keys(TenantID), now),
		Limits: limits, Relay: durable.Relay(), Revocations: durable.Revocations(),
		PasswordPolicy: core.DefaultPasswordPolicy, Now: now, Events: core.NewEventBus(),
		RelayOrigin: relayOrigin, RelayBaseURL: relayOrigin, TunnelNodes: hosts.TunnelNodes(durable),
	}
	if redisControl != nil {
		// 主机上下线（relay:presence）：记 last_seen / core_version，周期性落盘。
		out.presence = newPresenceRecorder(coreCtx.Ports, logger)
		out.closers = append(out.closers, redisControl.Plane().Presence().Subscribe(out.presence.Observe))
	}
	policy := grants.NewPolicy(grants.PolicyOptions{Core: coreCtx, Store: saasStore, Relay: durable, Log: logger})
	coreCtx.Policy = policy
	if redisControl != nil {
		coreCtx.Presence = presenceAdapter{redisControl.Plane().Presence()}
	}
	stream := core.NewMeStream(coreCtx)
	stream.UseTicketStore(&events.Tickets{Client: redisClient, Prefix: deps.TicketPrefix})
	shell := mestream.New(stream, now, deps.StreamPing)
	publisher := &events.Publisher{Core: coreCtx, Store: saasStore, Log: logger}
	hub := events.NewHub(events.HubOptions{
		Client: redisClient, Channel: deps.EventsChannel, Origin: "cloud/" + instance, Bus: coreCtx.Events,
		Prepare: accessPreparer(coreCtx, policy, stream, logger),
		// 事件订阅断线重连后，断线期间的事件丢了：让客户端重连并重新拉取。
		OnResubscribed: func() { shell.CloseAll(websocket.CloseServiceRestart, "resubscribed") },
		Log:            logger,
	})
	if err := hub.Start(ctx); err != nil {
		return nil, err
	}
	coreCtx.Events.SetPublisher(hub)
	out.closers = append(out.closers, func() { coreCtx.Events.SetPublisher(nil); hub.Close() })
	// me-stream 的连接在 HTTP 停收请求之前发 1001 关闭（关闭顺序，设计 §2.1）；这里登记的是兜底，Serve 另有显式的一步。
	out.stream = shell
	if cfg.Turnstile != nil {
		gate, err := challengeGate(cfg, logger, now, deps.Challenge)
		if err != nil {
			return nil, err
		}
		coreCtx.Challenge = gate
	}
	auth := core.NewAuthenticator(coreCtx)
	gate, err := httpx.NewGate(cfg.AppOrigin, cfg.MasterKey, auth, now)
	if err != nil {
		return nil, err
	}

	lockouts := httpx.NewUnknownLockouts(redisClient, func(err error) {
		logger.Warn("登录失败计数的 Redis 不可用，登录一律答 503（失败关闭）", obs.Operation("lockout"), obs.Error(err))
	})
	sender, err := mailSender(cfg, logger, now, deps)
	if err != nil {
		return nil, err
	}
	mailer, err := mail.New(mail.Options{
		Store: saasStore, TenantID: TenantID, Sender: sender, AppOrigin: cfg.AppOrigin, Now: now, Log: logger,
	})
	if err != nil {
		return nil, err
	}
	runner := jobs.New(jobs.Options{
		Store: saasStore, TenantID: TenantID, Workers: cfg.JobWorkers, Now: now, Log: logger,
	})
	mailer.Register(runner)
	if redisControl != nil {
		relayctl.RegisterJobs(runner, redisControl)
	}

	handlers := map[string]api.Handler{}
	for name, handler := range accounts.New(accounts.Options{
		Core: coreCtx, Store: saasStore, Gate: gate, Limits: limits, Mail: mailer, Stream: stream,
		Unknown: lockouts, LoginGuard: lockouts.Guard, AppOrigin: cfg.AppOrigin,
	}).Handlers() {
		handlers[name] = handler
	}
	teamService := teams.New(teams.Options{
		Core: coreCtx, Store: saasStore, Relay: durable, RequireEmailVerified: cfg.RequireEmailVerified, Log: logger,
		Events: publisher,
	})
	teamService.Register(runner)
	for name, handler := range teamService.Handlers() {
		handlers[name] = handler
	}
	inviteService := invitations.New(invitations.Options{
		Core: coreCtx, Store: saasStore, Limits: limits, Mail: mailer, RequireEmailVerified: cfg.RequireEmailVerified, Log: logger,
		Events: publisher,
	})
	inviteService.Register(runner)
	for name, handler := range inviteService.Handlers() {
		handlers[name] = handler
	}
	hostService := hosts.New(hosts.Options{
		Core: coreCtx, Store: saasStore, Limits: limits, Relay: durable, Directory: policy,
		RequireEmailVerified: cfg.RequireEmailVerified, Log: logger,
	})
	for name, handler := range hostService.Handlers() {
		handlers[name] = handler
	}
	grantService := grants.New(grants.Options{
		Core: coreCtx, Store: saasStore, Limits: limits, Relay: durable, Policy: policy,
		RequireEmailVerified: cfg.RequireEmailVerified, Log: logger, Hook: deps.GrantsHook, Events: publisher,
	})
	grantService.Register(runner)
	for name, handler := range grantService.Handlers() {
		handlers[name] = handler
	}
	auditService := audit.New(audit.Options{
		Core: coreCtx, Store: saasStore, Log: logger, Partitions: db,
		TrafficRetention: time.Duration(cfg.TrafficRetentionDays) * 24 * time.Hour,
		Authorize: func(ctx context.Context, r saas.Reader, tenantID, accountID, teamID string) (saas.TeamScope, error) {
			access, err := teams.Require(ctx, r, tenantID, accountID, teamID, teams.MinAdmin)
			return access.Scope, err
		},
	})
	auditService.Register(runner)
	for name, handler := range auditService.Handlers() {
		handlers[name] = handler
	}
	for name, handler := range billing.New(billing.Options{Core: coreCtx, Store: saasStore}).Handlers() {
		handlers[name] = handler
	}
	webApp := ""
	if cfg.WebRoot != "" {
		webApp = cfg.AppOrigin
	}
	for name, handler := range platform.Handlers(platform.Options{Core: coreCtx, Store: saasStore, Name: cfg.Name, WebApp: webApp}) {
		handlers[name] = handler
	}

	for name, handler := range deps.ExtraHandlers {
		handlers[name] = handler
	}

	clientIP, err := clientip.NewClientIP(cfg.TrustedProxy)
	if err != nil {
		return nil, err
	}
	apiServer, err := api.New(api.Options{
		Mode: api.ModeCloud, Handlers: handlers, Exclude: []string{"platform.health"},
		Before: gate.Before, Authenticate: gate.Authenticate,
		ClientIP: func(r *http.Request) string { return clientIP(r.RemoteAddr, r.Header) },
		OnError:  func(err error) { logger.Error("请求处理失败", obs.Error(err)) },
	})
	if err != nil {
		return nil, err
	}
	probes := map[string]obs.Probe{
		"postgres": func(ctx context.Context) error { return saasStore.Read().Ping(ctx) },
		"redis":    func(ctx context.Context) error { return redisClient.Ping(ctx).Err() },
	}
	mux := http.NewServeMux()
	mux.Handle("/ready", obs.ReadyHandler(probes))
	mux.Handle(mestream.MeStreamPath, shell)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { serveHealth(w, r, deps.Version) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if apiServer.Claims(r.URL.Path) {
			apiServer.ServeHTTP(w, r)
			return
		}
		api.WriteError(w, gen.ErrNotFound, "", 0, nil)
	})
	if out.sync != nil {
		probes["relay"] = out.sync.Ready
	}
	out.handler, out.core, out.auth, out.lockouts, out.jobs = mux, coreCtx, auth, lockouts, runner
	return out, nil
}

// close 按后进先出收尾。
func (a *assembly) close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
	a.closers = nil
}

// challengeGate 按配置建人机挑战（Turnstile）；校验的主机名只认控制台自己的。
func challengeGate(cfg *config.Config, logger *obs.Logger, now func() time.Time, verifier core.ChallengeVerifier) (*core.ChallengeGate, error) {
	if verifier == nil {
		secret, err := os.ReadFile(cfg.Turnstile.SecretFile)
		if err != nil {
			return nil, err
		}
		verifier = core.HTTPVerifier{Secret: strings.TrimSpace(string(secret))}
	}
	hostnames := []string{}
	if u, err := url.Parse(cfg.AppOrigin); err == nil && u.Hostname() != "" {
		hostnames = append(hostnames, strings.ToLower(u.Hostname()))
	}
	return core.NewChallengeGate(core.ChallengeGateOptions{
		SiteKey: cfg.Turnstile.SiteKey, Scope: cfg.Turnstile.Scope, Hostnames: hostnames, Verifier: verifier, Now: now,
		OnRejected: func(procedure core.ChallengeScope, reasons []string) {
			logger.Info("人机挑战没通过", obs.Procedure(string(procedure)), obs.Reasons(strings.Join(reasons, ",")))
		},
	})
}

var _ control.Limiter = (*controlredis.Limiter)(nil)

// mailSender 按配置建发信方：测试可注入；配了 SMTP 用 SMTP 适配器；都没有则每次发信都失败（启动时警告）。
func mailSender(cfg *config.Config, logger *obs.Logger, now func() time.Time, deps Deps) (mail.Sender, error) {
	switch {
	case deps.Mail != nil:
		return deps.Mail, nil
	case cfg.SMTP != nil:
		return mail.NewSMTP(mail.SMTPOptions{
			Host: cfg.SMTP.Host, Port: cfg.SMTP.Port, Username: cfg.SMTP.Username, Password: cfg.SMTP.Password,
			From: cfg.SMTP.From, TLS: cfg.SMTP.TLS, Now: now,
		})
	}
	logger.Warn("没有配置 SMTP，邮件留在发件箱里重试，不会发出")
	return mail.Disabled(), nil
}

// flushPresence 落盘累计的主机最近在线。
func (a *assembly) flushPresence(ctx context.Context) {
	if a.presence != nil {
		a.presence.Flush(ctx)
	}
}
