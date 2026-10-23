// Package cli 是 armadra-cloud 的命令分派：解析命令、读配置、调用各命令的实现。
// 退出码：0 成功，64 用法 / 配置错误，1 运行出错；参数优先于环境变量，环境变量优先于缺省。
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/server"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

// 退出码。
const (
	ExitOK    = 0
	ExitUsage = 64
	ExitFail  = 1
)

// Options 是装配进来的可变部分。
type Options struct {
	Version string
	// Now 缺省 time.Now。
	Now func() time.Time
	// Signals 缺省监听 SIGINT / SIGTERM（测试里换成自己的通道）。
	Signals <-chan os.Signal
	// OnReady 在 serve 开始监听后调用（测试拿地址用）。
	OnReady func(*server.Running)
}

// Run 跑一条命令，答退出码。
func Run(args []string, env config.Env, stdout, stderr io.Writer, options Options) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, Usage)
		return ExitUsage
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	command, rest := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, Usage)
		return ExitOK
	case "version":
		fmt.Fprintln(stdout, options.Version)
		return ExitOK
	case "health":
		return health(rest, env, stderr)
	case "migrate":
		return migrate(rest, env, stdout, stderr)
	case "serve":
		return serve(rest, env, stdout, stderr, options)
	case "keys":
		if len(rest) > 0 && rest[0] == "rotate" {
			return keysRotate(rest[1:], env, stdout, stderr, options)
		}
	case "operators":
		if len(rest) > 0 && (rest[0] == "add" || rest[0] == "totp") {
			return operators(rest[0], rest[1:], env, stdout, stderr, options)
		}
	case "nodes":
		if len(rest) > 0 && (rest[0] == "token" || rest[0] == "revoke") {
			fmt.Fprintf(stderr, "这个命令还没有提供：%s %s\n", command, rest[0])
			return ExitUsage
		}
	}
	fmt.Fprintf(stderr, "未知命令：%s\n\n%s\n", strings.Join(args, " "), Usage)
	return ExitUsage
}

// noArgs 要求命令不带多余参数。
func noArgs(name string, args []string, stderr io.Writer) bool {
	if len(args) == 0 {
		return true
	}
	fmt.Fprintf(stderr, "%s 不收参数：%s\n\n%s\n", name, strings.Join(args, " "), Usage)
	return false
}

// fail 打印错误并换成退出码：配置错误 64，其余 1。
func fail(err error, stderr io.Writer) int {
	fmt.Fprintln(stderr, err.Error())
	var configErr *config.Error
	if errors.As(err, &configErr) {
		return ExitUsage
	}
	return ExitFail
}

func migrate(args []string, env config.Env, stdout, stderr io.Writer) int {
	if !noArgs("migrate", args, stderr) {
		return ExitUsage
	}
	databaseURL, err := config.LoadDatabaseURL(env)
	if err != nil {
		return fail(err, stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := postgres.Open(ctx, postgres.Options{URL: databaseURL, ApplicationName: "armadra-cloud-migrate"})
	if err != nil {
		return fail(err, stderr)
	}
	defer func() { _ = db.Close() }()
	migrations, err := postgres.Migrations()
	if err != nil {
		return fail(err, stderr)
	}
	fmt.Fprintf(stdout, "数据库已迁移到最新：共 %d 个迁移，本次新增 %d 个\n", len(migrations), db.Migrated())
	return ExitOK
}

func keysRotate(args []string, env config.Env, stdout, stderr io.Writer, options Options) int {
	if !noArgs("keys rotate", args, stderr) {
		return ExitUsage
	}
	databaseURL, masterKey, err := config.LoadKeys(env)
	if err != nil {
		return fail(err, stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := postgres.Open(ctx, postgres.Options{
		URL: databaseURL, ApplicationName: "armadra-cloud-keys", NoMigrate: true, MasterKey: masterKey,
	})
	if err != nil {
		return fail(err, stderr)
	}
	defer func() { _ = db.Close() }()
	tenant, err := db.ReadTenant(ctx, server.TenantID)
	if err != nil {
		return fail(err, stderr)
	}
	if tenant == nil {
		fmt.Fprintln(stderr, "还没有存储租户：先运行 armadra-cloud serve 完成引导")
		return ExitFail
	}
	kid, err := db.RotateKey(ctx, server.TenantID, options.Now().UnixMilli())
	if err != nil {
		return fail(err, stderr)
	}
	fmt.Fprintf(stdout, "已轮换签名钥：%s（旧钥 1 小时内仍在 JWKS 里）\n", kid)
	return ExitOK
}

func health(args []string, env config.Env, stderr io.Writer) int {
	if !noArgs("health", args, stderr) {
		return ExitUsage
	}
	port := config.ListenPort(env)
	if probeHealth(port) {
		return ExitOK
	}
	return ExitFail
}

func serve(args []string, env config.Env, stdout, stderr io.Writer, options Options) int {
	if !noArgs("serve", args, stderr) {
		return ExitUsage
	}
	cfg, err := config.Load(env)
	if err != nil {
		return fail(err, stderr)
	}
	signals := options.Signals
	if signals == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(ch)
		signals = ch
	}
	running, err := server.Serve(context.Background(), cfg, server.Deps{Output: stdout, Now: options.Now, Version: options.Version})
	if err != nil {
		return fail(err, stderr)
	}
	if options.OnReady != nil {
		options.OnReady(running)
	}
	<-signals
	running.Close()
	return ExitOK
}

func portText(port int) string { return strconv.Itoa(port) }
