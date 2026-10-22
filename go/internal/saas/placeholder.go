// Package saas 是 `armadra-relay saas serve` 的占位：与 Node 版 saas 的空壳（apps/relay/src/saas/main.ts）
// 逐项一致——同一套 RELAY_* 环境变量与校验、配置错误以 64 退出且只列变量名、明文监听
// RELAY_LISTEN（缺省 0.0.0.0:8443）、`/health` 答 `{ok,mode,node,version,tunnels,streams}`、
// `/ready` 报 Redis（PING，2 秒超时）、其余答 404 `not_found`、启动与停止各一行日志。
// 不接隧道与边缘；G5 用真正的 SaaS 中继节点替换它（go-relay-design.md §3.6）。
package saas

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/httperr"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 退出码与 personal 相同：0 成功，64 用法或配置错误，1 运行出错。
const (
	exitOK    = 0
	exitUsage = 64
	exitFail  = 1
)

// Config 是 saas 占位用到的环境变量（同 apps/relay/src/config.ts 的 saasSchema）。
type Config struct {
	Host, NodeID, Domain, CloudIssuer, RedisURL string
	Port                                        int
	AllowInsecureTunnel                         bool
	LogLevel                                    obs.Level
}

var (
	listenPattern = regexp.MustCompile(`^(?:\[([^\]]+)\]|([^:]+)):(\d{1,5})$`)
	nodePattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	nodeUnsafe    = regexp.MustCompile(`[^A-Za-z0-9._-]`)
)

// LoadConfig 解析环境变量；空串当没设。有问题时答全部问题（「变量名：说明」，不回显取值）。
func LoadConfig(env map[string]string, hostname func() (string, error)) (Config, []string) {
	get := func(key string) (string, bool) {
		v := env[key]
		return v, strings.TrimSpace(v) != ""
	}
	var issues []string
	bad := func(key, why string) { issues = append(issues, key+"："+why) }
	cfg := Config{Host: "0.0.0.0", Port: 8443, LogLevel: obs.LevelInfo}
	if v, ok := get("RELAY_LISTEN"); ok {
		m := listenPattern.FindStringSubmatch(v)
		port := -1
		if m != nil {
			port, _ = strconv.Atoi(m[3])
		}
		if m == nil || port < 0 || port > 65535 {
			bad("RELAY_LISTEN", "不是合法的 host:port")
		} else {
			cfg.Host, cfg.Port = m[1]+m[2], port
		}
	}
	if v, ok := get("RELAY_NODE_ID"); ok {
		if !nodePattern.MatchString(v) {
			bad("RELAY_NODE_ID", "只能是字母、数字与 . _ -，1–64 个字符")
		}
		cfg.NodeID = v
	} else {
		name, _ := hostname()
		name = nodeUnsafe.ReplaceAllString(name, "-")
		if len(name) > 64 {
			name = name[:64]
		}
		cfg.NodeID = name
	}
	if v, ok := get("RELAY_DOMAIN"); ok {
		cfg.Domain = v
	} else {
		bad("RELAY_DOMAIN", "必填")
	}
	for _, item := range []struct {
		key  string
		into *string
	}{{"RELAY_CLOUD_ISSUER", &cfg.CloudIssuer}, {"RELAY_REDIS_URL", &cfg.RedisURL}} {
		v, ok := get(item.key)
		switch {
		case !ok:
			bad(item.key, "必填")
		case !isURL(v):
			bad(item.key, "不是合法的 URL")
		default:
			*item.into = v
		}
	}
	if v, ok := get("RELAY_ALLOW_INSECURE_TUNNEL"); ok {
		switch v {
		case "1", "true":
			cfg.AllowInsecureTunnel = true
		case "0", "false":
		default:
			bad("RELAY_ALLOW_INSECURE_TUNNEL", "只能是 0、1、true 或 false")
		}
	}
	if v, ok := get("RELAY_LOG_LEVEL"); ok {
		levels := map[string]obs.Level{"debug": obs.LevelDebug, "info": obs.LevelInfo, "warn": obs.LevelWarn, "error": obs.LevelError}
		if level, known := levels[v]; known {
			cfg.LogLevel = level
		} else {
			bad("RELAY_LOG_LEVEL", "只能是 debug、info、warn 或 error")
		}
	}
	return cfg, issues
}

func isURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && u.Scheme != "" && (u.Host != "" || u.Opaque != "")
}

// Run 跑 `armadra-relay saas <args>`：只认 `serve`（与 `--help`）；收到 SIGINT / SIGTERM 后停下并以 0 退出。
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

// Serve 起占位服务，ctx 结束时关掉；ready 非空时在监听就绪后收到地址（测试用）。
func Serve(ctx context.Context, cfg Config, version string, logOut io.Writer, ready func(url string)) error {
	log := obs.NewLogger(logOut, cfg.LogLevel)
	var redis *goredis.Client
	if options, err := goredis.ParseURL(cfg.RedisURL); err == nil {
		options.DialTimeout = 5 * time.Second
		redis = goredis.NewClient(options)
		defer func() { _ = redis.Close() }()
	}
	ping := func(ctx context.Context) error {
		if redis == nil {
			return errors.New("redis url")
		}
		return redis.Ping(ctx).Err()
	}
	go func() {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if ping(pctx) != nil && ctx.Err() == nil {
			log.Warn("Redis 暂不可达，后台重连")
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/health", obs.HealthHandler(obs.HealthOptions{Mode: "saas", Node: cfg.NodeID, Version: version}))
	mux.Handle("/ready", obs.ReadyHandler(map[string]obs.Probe{"redis": ping}))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.URL.Path == "/health" || r.URL.Path == "/ready") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			mux.ServeHTTP(w, r)
			return
		}
		httperr.Write(w, gen.ErrNotFound, httperr.Options{RequestID: requestID()})
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
		return fmt.Errorf("监听失败：%w", err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 76 * time.Second,
		IdleTimeout:       75 * time.Second,
	}
	addr := ln.Addr().(*net.TCPAddr)
	url := "http://" + net.JoinHostPort(addr.IP.String(), strconv.Itoa(addr.Port))
	log.Info("中继已启动", obs.URL(url), obs.Mode("saas"), obs.Node(cfg.NodeID))
	if ready != nil {
		ready(url)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	_ = srv.Close()
	log.Info("中继已停止")
	return nil
}

// requestID 是 UUID v4（同 TS 的 randomUUID）。
func requestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
