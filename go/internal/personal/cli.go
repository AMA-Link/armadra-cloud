package personal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/tlsx"
)

// 退出码：0 成功，64 用法 / 配置错误（EX_USAGE），1 其它失败。
const (
	ExitOK    = 0
	ExitUsage = 64
	ExitFail  = 1
)

// exitFor 把错误换成提示和退出码（同 cli.ts 的 catch）。
func exitFor(err error, stderr io.Writer) int {
	var usage *UsageError
	var state *StateError
	var tlsErr *tlsx.Error
	var storeErr *store.Error
	switch {
	case errors.As(err, &usage):
		fmt.Fprintln(stderr, usage.Msg)
		return ExitUsage
	case errors.As(err, &state):
		fmt.Fprintln(stderr, state.Msg)
		if state.Code == NotInitialized || state.Code == Exists {
			return ExitUsage
		}
		return ExitFail
	case errors.As(err, &tlsErr), errors.As(err, &storeErr):
		fmt.Fprintln(stderr, err.Error())
		return ExitFail
	}
	fmt.Fprintln(stderr, err.Error())
	return ExitFail
}

// RunCLI 跑 `armadra-relay personal <子命令> …`；args 是 personal 之后的参数。
func RunCLI(args []string, env Env, stdout, stderr io.Writer) int {
	hasHelp := false
	for _, a := range args {
		if a == "--help" {
			hasHelp = true
		}
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || hasHelp {
		fmt.Fprintln(stdout, PersonalUsage)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	command, rest := args[0], args[1:]
	cwd, _ := os.Getwd()
	ctx := context.Background()
	print := func(line string) { fmt.Fprintln(stdout, line) }
	now := time.Now()
	var err error
	switch command {
	case "serve":
		var options ServeOptions
		if options, err = ParseServe(rest, env, cwd); err == nil {
			err = runServe(ctx, options, stdout)
		}
	case "init":
		var options InitOptions
		if options, err = ParseInit(rest, env, cwd); err == nil {
			err = RunInit(ctx, options, print)
		}
	case "status":
		var options Common
		if options, err = ParseDataDirOnly(rest, env, cwd); err == nil {
			err = RunStatus(ctx, options.DataDir, options.Database, print, now)
		}
	case "passwd":
		var options PasswdOptions
		if options, err = ParsePasswd(rest, env, cwd); err == nil {
			err = RunPasswd(ctx, options.DataDir, options.PasswordFile, options.Database, print)
		}
	case "rotate-key":
		var options Common
		if options, err = ParseDataDirOnly(rest, env, cwd); err == nil {
			err = RunRotateKey(ctx, options.DataDir, options.Database, print)
		}
	case "links":
		var options LinksOptions
		if options, err = ParseLinks(rest, env, cwd); err == nil {
			if options.Action == "list" {
				err = RunLinksList(ctx, options.DataDir, options.Database, print, now)
			} else {
				err = RunLinksRevoke(ctx, options.DataDir, options.LinkID, options.Database, print)
			}
		}
	case "traffic":
		var options TrafficOptions
		if options, err = ParseTraffic(rest, env, cwd); err == nil {
			err = RunTraffic(ctx, options, print, now)
		}
	case "migrate":
		var options MigrateOptions
		if options, err = ParseMigrate(rest, env, cwd); err == nil {
			err = RunMigrate(ctx, options.DataDir, options.To, options.Database, print, now)
		}
	case "backup":
		var options BackupOptions
		if options, err = ParseBackup(rest, env, cwd); err == nil {
			err = RunBackup(ctx, options.DataDir, options.Out, options.Database, print)
		}
	case "doctor":
		var options DoctorOptions
		if options, err = ParseDoctor(rest, env, cwd); err == nil {
			var code int
			if code, err = RunDoctor(ctx, options, print, now); err == nil {
				return code
			}
		}
	default:
		fmt.Fprintf(stderr, "未知子命令：personal %s\n\n%s\n", command, PersonalUsage)
		return ExitUsage
	}
	if err != nil {
		return exitFor(err, stderr)
	}
	return ExitOK
}

// runServe 起个人中转并等到收到 SIGINT / SIGTERM，再按顺序收尾。
func runServe(ctx context.Context, options ServeOptions, stdout io.Writer) error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	running, err := Serve(ctx, options, ServeDeps{Output: stdout})
	if err != nil {
		return err
	}
	<-signals
	running.Close()
	return nil
}

var listenPort = regexp.MustCompile(`:(\d+)$`)

// Health 是镜像的 HEALTHCHECK：打本机监听端口的 /health，先 HTTPS（自签证书，不验）再明文；
// 200 就是活着。端口取 RELAY_LISTEN，缺省 8443。
func Health(env Env) int {
	port := 8443
	if listen, ok := env.Get("RELAY_LISTEN"); ok {
		if m := listenPort.FindStringSubmatch(listen); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				port = n
			}
		}
	}
	probe := func(secure bool) int {
		transport := &http.Transport{DisableKeepAlives: true}
		scheme := "http"
		if secure {
			scheme = "https"
			transport.TLSClientConfig = tlsInsecure()
		}
		client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
		resp, err := client.Get(scheme + "://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/health")
		if err != nil {
			return 0
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	status := probe(true)
	if status == 0 {
		status = probe(false)
	}
	if status == http.StatusOK {
		return ExitOK
	}
	return ExitFail
}
