// loadgen：对中继做一套压测（对 `armadra-relay dev --debug` 装配）。
//
// 它同时扮演两端：S 个「源」经 /t/v1 握手后把收到的 e2e 字节原样回显（遵守中继给的信用，
// 发出回显后才补信用，背压一路传回客户端）；S×C 个「客户端」经 /s/<id>/_relay/e2e 开通道。
//
// 阶段：建隧道 → 建通道 → 空闲（内存、CPU）→ 全体心跳式小消息（RTT）→ 少数大流量通道 +
// 其余通道里抽样探测延迟（吞吐与负载下延迟）。中继进程的 RSS 与 CPU 时间用 ps 每 500 ms 采样。
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/bench/proto"
)

var b64 = base64.RawURLEncoding

// ------------------------------------------------------------------ 记录格式
// 客户端发出的每条记录：[0:8] 发送时刻 ns，[8:12] 记录总长，[12] 种类（1 探测 / 2 大流量），[13] 阶段号，
// [14:22] 本通道的序号（回显必须按序、不丢、不重）。
const recHeader = 22

var corrupt atomic.Int64

const (
	kindProbe = 1
	kindBulk  = 2
)

type recorder struct {
	mu      sync.Mutex
	samples map[byte][]int64 // 阶段 → RTT 微秒
	sent    [8]atomic.Int64
	got     [8]atomic.Int64
}

var rec = &recorder{samples: map[byte][]int64{}}
var bulkRecv atomic.Int64

func (r *recorder) add(phase byte, us int64) {
	r.mu.Lock()
	r.samples[phase] = append(r.samples[phase], us)
	r.mu.Unlock()
	r.got[phase].Add(1)
}

// ------------------------------------------------------------------ 源

type cred struct {
	SourceID    string `json:"sourceId"`
	Seed        string `json:"seed"`
	TunnelToken string `json:"tunnelToken"`
	RelayToken  string `json:"relayToken"`
}

type simStream struct {
	id      uint32
	credit  int
	pending [][]byte
	unacked int
	ended   bool
}

type simSource struct {
	cred
	conn          *websocket.Conn
	out           chan []byte
	streams       map[uint32]*simStream
	tunnelCredit  int
	tunnelUnacked int
	limits        proto.Limits
}

func dialer() *websocket.Dialer {
	return &websocket.Dialer{HandshakeTimeout: 60 * time.Second, ReadBufferSize: 4096,
		WriteBufferPool: &sync.Pool{}}
}

func (s *simSource) connect(base string) error {
	conn, _, err := dialer().Dial(base+"/t/v1", nil)
	if err != nil {
		return err
	}
	s.conn = conn
	conn.SetReadLimit(proto.MaxFrameBytes + 16)
	seed, err := b64.DecodeString(s.Seed)
	if err != nil {
		return err
	}
	key := ed25519.NewKeyFromSeed(seed)
	nonce := b64.EncodeToString([]byte(fmt.Sprintf("%016d", rand.IntN(1e9))))
	if err := conn.WriteJSON(proto.Hello{Type: "hello", Protocol: proto.Version{Major: 1, Minor: 3},
		SourceID: s.SourceID, CoreVersion: "bench", Capabilities: []string{proto.E2ECapability},
		TunnelToken: s.TunnelToken, Nonce: nonce}); err != nil {
		return err
	}
	var ch proto.Challenge
	if err := conn.ReadJSON(&ch); err != nil {
		return fmt.Errorf("challenge: %w", err)
	}
	if ch.Type != "challenge" {
		return fmt.Errorf("期望 challenge，收到 %q", ch.Type)
	}
	sig := ed25519.Sign(key, proto.SigningInput(s.SourceID, nonce, ch.Nonce2, ch.RelayNode))
	if err := conn.WriteJSON(proto.Auth{Type: "auth", Sig: b64.EncodeToString(sig)}); err != nil {
		return err
	}
	var ready proto.Ready
	if err := conn.ReadJSON(&ready); err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	if ready.Type != "ready" {
		return fmt.Errorf("期望 ready，收到 %q", ready.Type)
	}
	s.limits = ready.Limits
	s.tunnelCredit = ready.Limits.TunnelWindow
	s.streams = map[uint32]*simStream{}
	s.out = make(chan []byte, 1024)
	go func() {
		for f := range s.out {
			if err := conn.WriteMessage(websocket.BinaryMessage, f); err != nil {
				return
			}
		}
	}()
	go s.reader()
	return nil
}

func (s *simSource) ack(st *simStream, n int) {
	s.tunnelUnacked += n
	if s.tunnelUnacked >= s.limits.TunnelWindow/2 {
		s.out <- proto.Encode(proto.FrameWindow, 0, proto.U32(uint32(s.tunnelUnacked)))
		s.tunnelUnacked = 0
	}
	if st != nil && !st.ended {
		st.unacked += n
		if st.unacked >= s.limits.StreamWindow/2 {
			s.out <- proto.Encode(proto.FrameWindow, st.id, proto.U32(uint32(st.unacked)))
			st.unacked = 0
		}
	}
}

func (s *simSource) flush(st *simStream) {
	for len(st.pending) > 0 && st.credit > 0 && s.tunnelCredit > 0 {
		chunk := st.pending[0]
		n := min(len(chunk), st.credit, s.tunnelCredit, proto.MaxDataChunk)
		s.out <- proto.Encode(proto.FrameData, st.id, chunk[:n])
		st.credit -= n
		s.tunnelCredit -= n
		s.ack(st, n)
		if n == len(chunk) {
			st.pending = st.pending[1:]
		} else {
			st.pending[0] = chunk[n:]
		}
	}
	if st.ended && len(st.pending) == 0 {
		s.out <- proto.Encode(proto.FrameEnd, st.id, nil)
		delete(s.streams, st.id)
	}
}

func (s *simSource) reader() {
	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		f, err := proto.Decode(data)
		if err != nil {
			continue
		}
		switch f.Type {
		case proto.FrameOpen:
			s.streams[f.StreamID] = &simStream{id: f.StreamID, credit: s.limits.StreamWindow}
		case proto.FrameData:
			if st := s.streams[f.StreamID]; st != nil {
				st.pending = append(st.pending, f.Payload)
				s.flush(st)
			} else {
				s.ack(nil, len(f.Payload))
			}
		case proto.FrameEnd:
			if st := s.streams[f.StreamID]; st != nil {
				st.ended = true
				s.flush(st)
			}
		case proto.FrameRst:
			delete(s.streams, f.StreamID)
		case proto.FrameWindow:
			c := int(binary.BigEndian.Uint32(f.Payload))
			if f.StreamID == 0 {
				s.tunnelCredit += c
				for _, st := range s.streams {
					if len(st.pending) > 0 {
						s.flush(st)
					}
				}
			} else if st := s.streams[f.StreamID]; st != nil {
				st.credit += c
				s.flush(st)
			}
		case proto.FramePing:
			s.out <- proto.Encode(proto.FramePong, 0, f.Payload)
		}
	}
}

// ------------------------------------------------------------------ 客户端

type client struct {
	ws      *websocket.Conn
	seqSend uint64
	seqRecv uint64
	sent    atomic.Int64
	recv    atomic.Int64
	wake    chan struct{}
	wbuf    []byte
}

func (c *client) reader() {
	var buf []byte
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		buf = append(buf, data...)
		now := time.Now().UnixNano()
		off := 0
		for len(buf)-off >= recHeader {
			l := int(binary.BigEndian.Uint32(buf[off+8 : off+12]))
			if l < recHeader || l > 4<<20 {
				corrupt.Add(1)
				buf, off = buf[:0], 0
				break
			}
			if len(buf)-off < l {
				break
			}
			ts := int64(binary.BigEndian.Uint64(buf[off : off+8]))
			kind, phase := buf[off+12], buf[off+13]
			seq := binary.BigEndian.Uint64(buf[off+14 : off+22])
			if l < recHeader || (kind != kindProbe && kind != kindBulk) || seq != c.seqRecv {
				corrupt.Add(1)
				buf = buf[:0]
				off = 0
				break
			}
			c.seqRecv++
			if kind == kindProbe {
				rec.add(phase, (now-ts)/1000)
			} else {
				bulkRecv.Add(int64(l))
			}
			c.recv.Add(int64(l))
			off += l
		}
		if off > 0 {
			buf = append(buf[:0], buf[off:]...)
		}
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

func (c *client) send(kind, phase byte, size int) error {
	if cap(c.wbuf) < size {
		c.wbuf = make([]byte, size)
	}
	b := c.wbuf[:size]
	binary.BigEndian.PutUint64(b[0:8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint32(b[8:12], uint32(size))
	b[12], b[13] = kind, phase
	binary.BigEndian.PutUint64(b[14:22], c.seqSend)
	c.seqSend++
	_ = c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := c.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return err
	}
	c.sent.Add(int64(size))
	return nil
}

// ------------------------------------------------------------------ 采样

type sample struct {
	atMs  int64
	rssKB int64
	cpuMs int64
}

type sampler struct {
	pid    int
	docker string // 非空：中继在这个容器里（PID 1），经 docker exec 读 /proc
	mu     sync.Mutex
	list   []sample
	stop   chan struct{}
}

func parseCPU(s string) int64 {
	// macOS ps time：[[dd-]hh:]mm:ss.cc
	parts := strings.Split(strings.TrimSpace(s), ":")
	var total float64
	for _, p := range parts {
		v, _ := strconv.ParseFloat(p, 64)
		total = total*60 + v
	}
	return int64(total * 1000)
}

func (s *sampler) take() (sample, bool) {
	if s.docker != "" {
		var out []byte
		var err error
		if pid, ok := strings.CutPrefix(s.docker, "proc:"); ok {
			// 与中继同一 PID 命名空间：直接读 /proc/<pid>
			stat, e1 := os.ReadFile("/proc/" + pid + "/stat")
			status, e2 := os.ReadFile("/proc/" + pid + "/status")
			if e1 != nil || e2 != nil {
				return sample{}, false
			}
			var rssLine string
			for _, l := range strings.Split(string(status), "\n") {
				if strings.HasPrefix(l, "VmRSS") {
					rssLine = l
				}
			}
			out = []byte(strings.TrimSpace(string(stat)) + "\n" + rssLine)
		} else {
			out, err = exec.Command("docker", "exec", s.docker, "sh", "-c",
				"cat /proc/1/stat; grep VmRSS /proc/1/status").Output()
			if err != nil {
				return sample{}, false
			}
		}
		lines := strings.Split(string(out), "\n")
		if len(lines) < 2 {
			return sample{}, false
		}
		stat := lines[0][strings.LastIndex(lines[0], ")")+2:]
		f := strings.Fields(stat) // f[0] 是第 3 个字段 state；utime=14、stime=15
		if len(f) < 13 {
			return sample{}, false
		}
		ut, _ := strconv.ParseInt(f[11], 10, 64)
		st, _ := strconv.ParseInt(f[12], 10, 64)
		rf := strings.Fields(lines[1])
		if len(rf) < 2 {
			return sample{}, false
		}
		rss, _ := strconv.ParseInt(rf[1], 10, 64)
		return sample{atMs: time.Now().UnixMilli(), rssKB: rss, cpuMs: (ut + st) * 10}, true
	}
	out, err := exec.Command("ps", "-o", "rss=,time=", "-p", strconv.Itoa(s.pid)).Output()
	if err != nil {
		return sample{}, false
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return sample{}, false
	}
	rss, _ := strconv.ParseInt(f[0], 10, 64)
	return sample{atMs: time.Now().UnixMilli(), rssKB: rss, cpuMs: parseCPU(f[1])}, true
}

func (s *sampler) run() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if x, ok := s.take(); ok {
				s.mu.Lock()
				s.list = append(s.list, x)
				s.mu.Unlock()
			}
		case <-s.stop:
			return
		}
	}
}

type window struct {
	RssStartMB float64 `json:"rssStartMB"`
	RssEndMB   float64 `json:"rssEndMB"`
	RssMaxMB   float64 `json:"rssMaxMB"`
	CPUPct     float64 `json:"cpuPct"`
	CPUMs      int64   `json:"cpuMs"`
	WallMs     int64   `json:"wallMs"`
}

func (s *sampler) window(from, to int64) window {
	s.mu.Lock()
	defer s.mu.Unlock()
	var in []sample
	for _, x := range s.list {
		if x.atMs >= from && x.atMs <= to {
			in = append(in, x)
		}
	}
	if len(in) < 2 {
		return window{}
	}
	a, b := in[0], in[len(in)-1]
	w := window{RssStartMB: float64(a.rssKB) / 1024, RssEndMB: float64(b.rssKB) / 1024}
	for _, x := range in {
		w.RssMaxMB = max(w.RssMaxMB, float64(x.rssKB)/1024)
	}
	w.CPUMs, w.WallMs = b.cpuMs-a.cpuMs, b.atMs-a.atMs
	if b.atMs > a.atMs {
		w.CPUPct = float64(b.cpuMs-a.cpuMs) / float64(b.atMs-a.atMs) * 100
	}
	return w
}

// ------------------------------------------------------------------ 统计

type pct struct {
	N    int     `json:"n"`
	P50  float64 `json:"p50ms"`
	P90  float64 `json:"p90ms"`
	P99  float64 `json:"p99ms"`
	P999 float64 `json:"p999ms"`
	Max  float64 `json:"maxms"`
}

func percentiles(us []int64) pct {
	if len(us) == 0 {
		return pct{}
	}
	s := slices.Clone(us)
	slices.Sort(s)
	at := func(q float64) float64 { return float64(s[min(len(s)-1, int(q*float64(len(s))))]) / 1000 }
	return pct{N: len(s), P50: at(0.5), P90: at(0.9), P99: at(0.99), P999: at(0.999), Max: float64(s[len(s)-1]) / 1000}
}

// ------------------------------------------------------------------ 主流程

type result struct {
	Label        string             `json:"label"`
	Sources      int                `json:"sources"`
	Channels     int                `json:"channels"`
	BaselineMB   float64            `json:"baselineRssMB"`
	TunnelSetup  map[string]any     `json:"tunnelSetup"`
	ChannelSetup map[string]any     `json:"channelSetup"`
	Idle         window             `json:"idle"`
	PerConnKB    float64            `json:"perChannelKB"`
	Rtt          map[string]any     `json:"rtt"`
	Bulk         map[string]any     `json:"bulk"`
	RelayStats   json.RawMessage    `json:"relayStats,omitempty"`
	Errors       map[string]int     `json:"errors"`
	Extra        map[string]float64 `json:"extra"`
}

func main() {
	base := flag.String("url", "ws://127.0.0.1:18443", "中继地址（ws://）")
	credsPath := flag.String("creds", "", "bench/mint 的输出")
	nSources := flag.Int("sources", 100, "源（隧道）数")
	per := flag.Int("per", 4, "每个源的通道数")
	pid := flag.Int("pid", 0, "中继进程 pid（采样 RSS 与 CPU）")
	docker := flag.String("docker", "", "中继所在容器名（PID 1；与 -pid 二选一）")
	label := flag.String("label", "", "结果标签")
	outPath := flag.String("out", "", "结果 JSON 输出文件")
	statsURL := flag.String("stats", "", "中继的 /debug/stats（可选，dev 装配加 --debug 才有）")
	conc := flag.Int("conc", 128, "建连并发")
	idleDur := flag.Duration("idle", 10*time.Second, "空闲阶段时长")
	rttDur := flag.Duration("rtt", 20*time.Second, "全体小消息阶段时长")
	rttEvery := flag.Duration("rtt-interval", time.Second, "每个通道发探测的间隔")
	rttSize := flag.Int("rtt-size", 1024, "探测记录字节数")
	bulkN := flag.Int("bulk", 32, "大流量通道数")
	bulkDur := flag.Duration("bulk-dur", 15*time.Second, "大流量阶段时长")
	bulkSize := flag.Int("bulk-size", 65536, "大流量记录字节数")
	inflight := flag.Int64("inflight", 1<<20, "每个大流量通道最多在途字节")
	probeN := flag.Int("probe", 200, "大流量阶段同时探测延迟的通道数")
	probeEvery := flag.Duration("probe-interval", 100*time.Millisecond, "大流量阶段探测间隔")
	probeSame := flag.Bool("probe-same", false, "探测通道取大流量所在的源（同一条隧道，测队头阻塞）")
	flag.Parse()

	raw, err := os.ReadFile(*credsPath)
	if err != nil {
		log.Fatal(err)
	}
	var creds struct{ Sources []cred }
	if err := json.Unmarshal(raw, &creds); err != nil {
		log.Fatal(err)
	}
	if len(creds.Sources) < *nSources {
		log.Fatalf("凭据只有 %d 个源，要 %d", len(creds.Sources), *nSources)
	}
	res := result{Label: *label, Sources: *nSources, Channels: *nSources * *per,
		Errors: map[string]int{}, Extra: map[string]float64{}}
	var errMu sync.Mutex
	addErr := func(where string, err error) {
		errMu.Lock()
		key := where + ": " + err.Error()
		if len(key) > 120 {
			key = key[:120]
		}
		res.Errors[key]++
		errMu.Unlock()
	}

	smp := &sampler{pid: *pid, docker: *docker, stop: make(chan struct{})}
	if *pid > 0 || *docker != "" {
		go smp.run()
	}
	time.Sleep(1200 * time.Millisecond)
	if x, ok := smp.take(); ok {
		res.BaselineMB = float64(x.rssKB) / 1024
	}

	// ---- 建隧道
	sources := make([]*simSource, *nSources)
	var setupUs []int64
	var setupMu sync.Mutex
	runPool := func(n int, fn func(i int)) time.Duration {
		start := time.Now()
		var wg sync.WaitGroup
		next := atomic.Int64{}
		for w := 0; w < *conc; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := int(next.Add(1) - 1)
					if i >= n {
						return
					}
					fn(i)
				}
			}()
		}
		wg.Wait()
		return time.Since(start)
	}
	var okTunnels atomic.Int64
	took := runPool(*nSources, func(i int) {
		s := &simSource{cred: creds.Sources[i]}
		t0 := time.Now()
		if err := s.connect(*base); err != nil {
			addErr("tunnel", err)
			return
		}
		setupMu.Lock()
		setupUs = append(setupUs, time.Since(t0).Microseconds())
		setupMu.Unlock()
		sources[i] = s
		okTunnels.Add(1)
	})
	res.TunnelSetup = map[string]any{"ok": okTunnels.Load(), "seconds": took.Seconds(),
		"perSec": float64(okTunnels.Load()) / took.Seconds(), "latency": percentiles(setupUs)}
	log.Printf("[%s] 隧道 %d/%d，%.1fs", *label, okTunnels.Load(), *nSources, took.Seconds())

	// ---- 建通道
	clients := make([]*client, *nSources**per)
	setupUs = nil
	var okCh atomic.Int64
	took = runPool(len(clients), func(i int) {
		s := sources[i / *per]
		if s == nil {
			return
		}
		t0 := time.Now()
		ws, resp, err := dialer().Dial(fmt.Sprintf("%s/s/%s/_relay/e2e", *base, s.SourceID), http.Header{
			"Sec-WebSocket-Protocol": {proto.E2ESubprotocol + ", " + proto.RelaySubprotoPrefix + s.RelayToken},
		})
		if err != nil {
			if resp != nil {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
				err = fmt.Errorf("%v %s", err, body)
			}
			addErr("channel", err)
			return
		}
		if ws.Subprotocol() != proto.E2ESubprotocol {
			addErr("channel", fmt.Errorf("子协议 %q", ws.Subprotocol()))
			ws.Close()
			return
		}
		ws.SetReadLimit(proto.MaxFrameBytes)
		c := &client{ws: ws, wake: make(chan struct{}, 1)}
		setupMu.Lock()
		setupUs = append(setupUs, time.Since(t0).Microseconds())
		setupMu.Unlock()
		clients[i] = c
		okCh.Add(1)
		go c.reader()
	})
	res.ChannelSetup = map[string]any{"ok": okCh.Load(), "seconds": took.Seconds(),
		"perSec": float64(okCh.Load()) / took.Seconds(), "latency": percentiles(setupUs)}
	log.Printf("[%s] 通道 %d/%d，%.1fs", *label, okCh.Load(), len(clients), took.Seconds())
	live := make([]*client, 0, len(clients))
	for _, c := range clients {
		if c != nil {
			live = append(live, c)
		}
	}

	// ---- 空闲
	time.Sleep(3 * time.Second)
	from := time.Now().UnixMilli()
	time.Sleep(*idleDur)
	res.Idle = smp.window(from, time.Now().UnixMilli())
	if len(live) > 0 {
		res.PerConnKB = (res.Idle.RssEndMB - res.BaselineMB) * 1024 / float64(len(live)+int(okTunnels.Load()))
	}
	if *statsURL != "" {
		if r, err := http.Get(*statsURL); err == nil {
			res.RelayStats, _ = io.ReadAll(r.Body)
			r.Body.Close()
		}
	}
	log.Printf("[%s] 空闲 RSS %.1f MB，CPU %.1f%%", *label, res.Idle.RssEndMB, res.Idle.CPUPct)

	// ---- 全体小消息
	const phRtt, phBulk = 1, 2
	from = time.Now().UnixMilli()
	deadline := time.Now().Add(*rttDur)
	var wg sync.WaitGroup
	for _, c := range live {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(rand.Int64N(int64(*rttEvery))))
			for time.Now().Before(deadline) {
				if err := c.send(kindProbe, phRtt, *rttSize); err != nil {
					addErr("rtt-send", err)
					return
				}
				rec.sent[phRtt].Add(1)
				time.Sleep(*rttEvery)
			}
		}()
	}
	wg.Wait()
	rttWin := smp.window(from, time.Now().UnixMilli())
	time.Sleep(2 * time.Second)
	rec.mu.Lock()
	rttP := percentiles(rec.samples[phRtt])
	rec.mu.Unlock()
	res.Rtt = map[string]any{"sent": rec.sent[phRtt].Load(), "got": rec.got[phRtt].Load(),
		"msgPerSec": float64(rec.sent[phRtt].Load()) / rttDur.Seconds(), "latency": rttP, "resource": rttWin}
	log.Printf("[%s] 小消息 %d 条/s，p50 %.2f ms，p99 %.2f ms，CPU %.1f%%", *label,
		int(float64(rec.sent[phRtt].Load())/rttDur.Seconds()), rttP.P50, rttP.P99, rttWin.CPUPct)

	// ---- 大流量 + 探测
	if *bulkN > 0 && len(live) > *bulkN {
		// 大流量通道分散到不同的源；探测通道只取没有大流量的源（不和大流量共用隧道）
		step := max(1, *nSources / *bulkN)
		var bulk, probes []*client
		busy := map[int]bool{}
		for i := 0; i < *bulkN; i++ {
			src := (i * step) % *nSources
			if c := clients[src**per]; c != nil {
				bulk = append(bulk, c)
				busy[src] = true
			}
		}
		for k := 0; k < len(clients) && len(probes) < *probeN; k++ {
			idx := (k*7919 + 3) % len(clients)
			if c := clients[idx]; c != nil && busy[idx / *per] == *probeSame && idx%*per == 1 {
				probes = append(probes, c)
			}
		}
		bulkRecv.Store(0)
		from = time.Now().UnixMilli()
		deadline = time.Now().Add(*bulkDur)
		for _, c := range bulk {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(deadline) {
					if c.sent.Load()-c.recv.Load() > *inflight {
						select {
						case <-c.wake:
						case <-time.After(50 * time.Millisecond):
						}
						continue
					}
					if err := c.send(kindBulk, phBulk, *bulkSize); err != nil {
						addErr("bulk-send", err)
						return
					}
				}
			}()
		}
		for _, c := range probes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Duration(rand.Int64N(int64(*probeEvery))))
				for time.Now().Before(deadline) {
					if err := c.send(kindProbe, phBulk, *rttSize); err != nil {
						addErr("probe-send", err)
						return
					}
					rec.sent[phBulk].Add(1)
					time.Sleep(*probeEvery)
				}
			}()
		}
		wg.Wait()
		to := time.Now().UnixMilli()
		echoed := bulkRecv.Load()
		bulkWin := smp.window(from, to)
		time.Sleep(2 * time.Second)
		rec.mu.Lock()
		bp := percentiles(rec.samples[phBulk])
		rec.mu.Unlock()
		secs := float64(to-from) / 1000
		res.Bulk = map[string]any{"channels": len(bulk), "echoMBps": float64(echoed) / secs / 1e6,
			"relayForwardMBps": 2 * float64(echoed) / secs / 1e6,
			"probeSent":        rec.sent[phBulk].Load(), "probeGot": rec.got[phBulk].Load(),
			"probeLatency": bp, "resource": bulkWin}
		log.Printf("[%s] 大流量 回显 %.0f MB/s（中继双向 %.0f MB/s），探测 p99 %.2f ms，CPU %.1f%%，RSS 峰值 %.0f MB",
			*label, float64(echoed)/secs/1e6, 2*float64(echoed)/secs/1e6, bp.P99, bulkWin.CPUPct, bulkWin.RssMaxMB)
	}

	close(smp.stop)
	if n := corrupt.Load(); n > 0 {
		res.Errors["corrupt_or_reordered_records"] = int(n)
	}
	res.Extra["corruptRecords"] = float64(corrupt.Load())
	out, _ := json.MarshalIndent(res, "", "  ")
	if *outPath != "" {
		_ = os.WriteFile(*outPath, out, 0o644)
	} else {
		fmt.Println(string(out))
	}
	for _, c := range live {
		_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
		c.ws.Close()
	}
	for _, s := range sources {
		if s != nil {
			s.conn.Close()
		}
	}
}
