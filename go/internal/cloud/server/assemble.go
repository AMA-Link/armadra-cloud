package server

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/clientip"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/accounts"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/platform"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	controlredis "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

// assembly 是装配好的控制核与路由。
type assembly struct {
	handler http.Handler
	core    *core.Context
	auth    *core.Authenticator
	relay   *relayctl.Memory
	// lockouts 是未注册邮箱的 Redis 失败计数（回落次数进指标）。
	lockouts *httpx.UnknownLockouts
	// jobs 是作业运行器（serve 在监听前启动、关闭时停止领取）。
	jobs *jobs.Runner
}

// assemble 把存储、Redis、控制核服务、闸与各批的过程接成一个 http.Handler：/health、/ready，
// 其余交给 /v1 与 /.well-known 的 API（还没实现的过程答 501 not_implemented）。
func assemble(cfg *config.Config, db *postgres.DB, redisClient goredis.UniversalClient, logger *obs.Logger, now func() time.Time, deps Deps) (*assembly, error) {
	relay := relayctl.NewMemory(now)
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
		Limits: limits, Relay: relay.Relay(), Revocations: relay.Revocations(),
		PasswordPolicy: core.DefaultPasswordPolicy, Now: now, Events: core.NewEventBus(),
		RelayOrigin: relayOrigin, RelayBaseURL: relayOrigin,
	}
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
	saasStore := db.SaaS()

	lockouts := httpx.NewUnknownLockouts(redisClient, func(err error) {
		logger.Warn("登录失败计数回落到进程内计数，已注册与未注册邮箱的锁定可能不一致", obs.Operation("lockout"), obs.Error(err))
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

	handlers := map[string]api.Handler{}
	for name, handler := range accounts.New(accounts.Options{
		Core: coreCtx, Store: saasStore, Gate: gate, Limits: limits, Mail: mailer,
		Unknown: lockouts, AppOrigin: cfg.AppOrigin,
	}).Handlers() {
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
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { serveHealth(w, r, deps.Version) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if apiServer.Claims(r.URL.Path) {
			apiServer.ServeHTTP(w, r)
			return
		}
		api.WriteError(w, gen.ErrNotFound, "", 0, nil)
	})
	return &assembly{handler: mux, core: coreCtx, auth: auth, relay: relay, lockouts: lockouts, jobs: runner}, nil
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
