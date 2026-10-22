// armadra-relay 是中继的命令行：personal（个人中转的全部子命令）、health、version、help，
// 另有给测试与 bench 用的 dev（数据面 + 内存控制面 + 指定的 JWKS 文件 + 回环明文监听）。
// saas serve 现在是与 Node 版空壳一致的占位（internal/saas），G5 换成真正的 SaaS 中继节点。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/dataplane"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/personal"
	"github.com/AMA-Link/armadra-cloud/go/internal/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

// version 由构建注入（-ldflags "-X main.version=…"）。
var version = "dev"

func main() {
	tuneGC()
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// tuneGC 把 GC 触发点从「存活堆翻倍」收到「涨一半」（环境里设了 GOGC 就听环境的）。
// 中继的存活堆主要是连接，建连高峰留下的垃圾会让常驻内存浮到两倍；压测里每连接内存
// 从约 16 KB 降到约 13.5 KB，代价是多一点 GC 次数（存活堆百兆级时可以忽略）。
func tuneGC() {
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}
}

// 退出码：0 成功，64 用法错误，1 运行出错。
func run(args []string, stdout, stderr io.Writer) int {
	personal.Version = version
	if len(args) == 0 {
		fmt.Fprintln(stdout, personal.Usage)
		return personal.ExitUsage
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "dev":
		return runDev(args[1:], stdout, stderr)
	case "health":
		return personal.Health(personal.EnvFromOS())
	case "personal":
		return personal.RunCLI(args[1:], personal.EnvFromOS(), stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, personal.Usage)
		return 0
	case "saas":
		return saas.Run(args[1:], personal.EnvFromOS(), version, personal.Usage, stdout, stderr)
	}
	fmt.Fprintf(stderr, "未知命令：%s\n\n%s\n", strings.Join(args, " "), personal.Usage)
	return personal.ExitUsage
}

func runDev(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:0", "监听地址，只许回环")
	jwksPath := fs.String("jwks", "", "签发者公钥集（JWKS JSON 文件）")
	issuer := fs.String("issuer", "", "令牌的 iss")
	nodeID := fs.String("node-id", "", "节点名，缺省取主机名")
	metricsListen := fs.String("metrics-listen", "", "指标监听地址（回环）")
	metricsToken := fs.String("metrics-token", "", "指标的 Bearer 令牌")
	debug := fs.Bool("debug", false, "开 /debug/pprof 与 /debug/stats")
	metered := fs.Bool("traffic", false, "开流量计量（写到丢弃的存储，量计数本身的开销）")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *jwksPath == "" || *issuer == "" {
		fmt.Fprintln(stderr, "用法: armadra-relay dev --jwks <文件> --issuer <iss> [--listen <地址>]")
		return 64
	}
	if err := requireLoopback(*listen); err != nil {
		fmt.Fprintln(stderr, err)
		return 64
	}
	if *metricsListen != "" {
		if err := requireLoopback(*metricsListen); err != nil {
			fmt.Fprintln(stderr, err)
			return 64
		}
	}
	if *nodeID == "" {
		host, _ := os.Hostname()
		*nodeID = host
	}
	log := obs.NewLogger(stdout, obs.LevelInfo)

	ctl := control.NewMemory(control.MemoryOptions{
		Issuer: *issuer,
		JWKS:   func(context.Context) ([]byte, error) { return os.ReadFile(*jwksPath) },
	})
	opts := dataplane.Options{
		Mode: "personal", NodeID: *nodeID, Version: version, Control: ctl,
		Addressing: edge.PathAddressing(), Log: log,
	}
	var meterDone chan struct{}
	meterCtx, stopMeter := context.WithCancel(context.Background())
	defer stopMeter()
	if *metered {
		meter := traffic.NewMeter(traffic.MeterOptions{
			Node:  *nodeID,
			Write: func(context.Context, []traffic.Row) error { return nil },
		})
		opts.Traffic = meter
		meterDone = make(chan struct{})
		go func() { defer close(meterDone); meter.Run(meterCtx) }()
	}
	var plane *dataplane.Plane
	if *debug {
		opts.Fallback = func(w http.ResponseWriter, r *http.Request) bool { return serveDebug(plane, w, r) }
	}
	plane, err := dataplane.New(opts)
	if err != nil {
		log.Error("数据面启动失败", obs.Error(err))
		return 1
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error("监听失败", obs.Error(err))
		return 1
	}
	srv := &http.Server{Handler: plane, ReadHeaderTimeout: 10 * time.Second}
	var metricsSrv *http.Server
	if *metricsListen != "" {
		mln, err := net.Listen("tcp", *metricsListen)
		if err != nil {
			log.Error("指标监听失败", obs.Error(err))
			return 1
		}
		metricsSrv = &http.Server{Handler: obs.MetricsHandler(plane.Metrics.Render, *metricsToken), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = metricsSrv.Serve(mln) }()
	}
	url := "http://" + ln.Addr().String()
	log.Info("数据面已就绪", obs.Mode("dev"), obs.Listen(ln.Addr().String()), obs.URL(url))
	// 机器可读的就绪行，bench 脚本据此等待启动。
	fmt.Fprintf(stdout, `{"ready":true,"url":%q,"pid":%d}`+"\n", url, os.Getpid())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("服务出错", obs.Error(err))
			return 1
		}
	}
	plane.Shutdown(5 * time.Second)
	if meterDone != nil {
		stopMeter()
		<-meterDone
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(ctx)
	}
	return 0
}

// requireLoopback 明文监听只许回环。
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("监听地址不合法：%s", addr)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("明文监听只许回环地址：%s", addr)
	}
	return nil
}

// serveDebug 是 bench 用的运行时快照与 pprof，只在 --debug 时挂上。
func serveDebug(plane *dataplane.Plane, w http.ResponseWriter, r *http.Request) bool {
	switch p := r.URL.Path; {
	case p == "/debug/pprof/profile":
		pprof.Profile(w, r)
	case p == "/debug/pprof/trace":
		pprof.Trace(w, r)
	case p == "/debug/pprof/symbol":
		pprof.Symbol(w, r)
	case strings.HasPrefix(p, "/debug/pprof/"):
		pprof.Index(w, r)
	case p == "/debug/stats":
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		tunnels, streams := plane.Hub.Counts()
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"goroutines":%d,"tunnels":%d,"channels":%d,"heapInuse":%d,"stackInuse":%d,"sys":%d,"numGC":%d,"gomaxprocs":%d}`+"\n",
			runtime.NumGoroutine(), tunnels, streams, m.HeapInuse, m.StackInuse, m.Sys, m.NumGC, runtime.GOMAXPROCS(0))
	default:
		return false
	}
	return true
}
