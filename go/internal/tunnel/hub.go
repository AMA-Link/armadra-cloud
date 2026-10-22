package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/auth"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// Socket 是运行时交给隧道层的一条已升级的 WebSocket。
type Socket interface {
	Transport
	// ReadMessage 阻塞读下一条消息；data 只在下一次调用之前有效。
	ReadMessage() (binary bool, data []byte, err error)
	// SendText 发一条文本消息（握手用）。
	SendText(b []byte) error
}

// HubOptions 配置隧道表。
type HubOptions struct {
	Control          control.Plane
	Auth             *auth.Context
	NodeID           string
	Config           Config
	HandshakeTimeout time.Duration
	RouteTTL         time.Duration
	Log              *obs.Logger
	Metrics          *obs.Metrics
	// Nonce 产生握手的 nonce2；测试注入固定值。
	Nonce func() string
}

// Hub 是本节点的隧道表：握手 → Tunnel，每源一条，新的替换旧的；
// 路由与在线事件写进 ControlPlane。
//
// 替换：同一源在本节点已有隧道，旧的发 GOAWAY replaced，宽限期后关 4491，路由 Set 覆盖。
// 关闭：只有「当前」隧道（没被替换）才按 tunnelId 删路由并发 sourceOffline。
type Hub struct {
	opts HubOptions

	mu           sync.Mutex
	current      map[string]*Tunnel
	all          map[*Tunnel]struct{}
	shuttingDown bool
	unsubscribe  func()
}

// NewHub 建隧道表，并订阅控制面发给本节点的命令。
func NewHub(o HubOptions) *Hub {
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = time.Duration(protocol.HandshakeTimeout) * time.Millisecond
	}
	if o.RouteTTL <= 0 {
		o.RouteTTL = control.RouteTTL
	}
	if o.Log == nil {
		o.Log = obs.Nop()
	}
	if o.Metrics == nil {
		o.Metrics = obs.NewMetrics(func() (int, int) { return 0, 0 })
	}
	h := &Hub{opts: o, current: map[string]*Tunnel{}, all: map[*Tunnel]struct{}{}}
	if cmds := o.Control.NodeCommands(); cmds != nil {
		h.unsubscribe = cmds.Subscribe(h.onCommand)
	}
	return h
}

func (h *Hub) onCommand(c control.NodeCommand) {
	h.mu.Lock()
	t := h.current[c.SourceID]
	h.mu.Unlock()
	if t == nil {
		return
	}
	if c.Type == control.CommandKick {
		t.Goaway("replaced", DefaultGrace)
		return
	}
	t.Close(protocol.CloseTunnelGoaway, "closed_by_control")
}

// Accepting 还收新隧道（没在关闭）。
func (h *Hub) Accepting() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.shuttingDown
}

// Counts 返回当前隧道数与流数。
func (h *Hub) Counts() (tunnels, streams int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for t := range h.all {
		streams += t.StreamCount()
	}
	return len(h.all), streams
}

// Lookup 边缘用：本节点上持有这个源、路由指向它、还能开流的隧道；没有答 nil。
func (h *Hub) Lookup(ctx context.Context, sourceID string) (*Tunnel, error) {
	route, err := h.opts.Control.Routes().Get(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if route == nil || route.Node != h.opts.NodeID {
		return nil, nil
	}
	h.mu.Lock()
	t := h.current[sourceID]
	h.mu.Unlock()
	if t == nil || t.ID() != route.TunnelID || !t.Accepting() {
		return nil, nil
	}
	return t, nil
}

// Shutdown 所有隧道发 GOAWAY shutdown，等它们排空（或宽限期到）。
func (h *Hub) Shutdown(grace time.Duration) {
	h.mu.Lock()
	h.shuttingDown = true
	if h.unsubscribe != nil {
		h.unsubscribe()
		h.unsubscribe = nil
	}
	list := make([]*Tunnel, 0, len(h.all))
	for t := range h.all {
		list = append(list, t)
	}
	h.mu.Unlock()
	for _, t := range list {
		t.Goaway("shutdown", grace)
	}
	deadline := time.After(grace + 5*time.Second)
	for _, t := range list {
		select {
		case <-t.Closed():
		case <-deadline:
			return
		}
	}
}

// Serve 处理一条刚升级好的隧道 WebSocket：握手、就绪后转交 Tunnel。阻塞到连接结束。
func (h *Hub) Serve(sock Socket) {
	o := h.opts
	defer sock.Terminate()
	startedAt := time.Now()
	hs := NewHandshake(HandshakeDeps{Auth: o.Auth, Limits: o.Control.Limits(), RelayNode: o.NodeID, Nonce: o.Nonce})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		smu     sync.Mutex
		settled bool
	)
	settle := func(result string) bool {
		smu.Lock()
		if settled {
			smu.Unlock()
			return false
		}
		settled = true
		smu.Unlock()
		if result == "ok" {
			o.Metrics.HandshakeDone(time.Since(startedAt))
		} else {
			o.Metrics.Handshake(result)
			o.Log.Warn("隧道握手失败", obs.Result(result), obs.SourceID(hs.SourceID()))
		}
		return true
	}
	isSettled := func() bool {
		smu.Lock()
		defer smu.Unlock()
		return settled
	}
	timer := time.AfterFunc(o.HandshakeTimeout, func() {
		if settle("timeout") {
			cancel() // 握手卡在令牌校验或限流上时一并取消
			sock.Close(protocol.CloseBadFrame, "handshake_timeout")
		}
	})
	defer timer.Stop()

	var tun *Tunnel
	code := protocol.CloseCode(1006)
	for {
		binary, data, err := sock.ReadMessage()
		if err != nil {
			code = closeCodeOf(err)
			break
		}
		if tun != nil {
			if binary {
				tun.Receive(data)
			} else {
				tun.Close(protocol.CloseBadFrame, "text_after_ready")
			}
			continue
		}
		if isSettled() {
			continue
		}
		if binary {
			settle("binary_before_ready")
			sock.Close(protocol.CloseBadFrame, "binary_before_ready")
			continue
		}
		step, err := hs.OnText(ctx, data)
		if err != nil {
			o.Log.Error("隧道握手异常", obs.Error(err))
			return
		}
		switch step.Kind {
		case StepSend:
			sock.SendText(mustJSON(step.Challenge))
		case StepReject:
			settle(step.Reject.Code)
			sock.SendText(mustJSON(step.Reject))
			sock.Close(step.CloseCode, step.Reject.Code)
		case StepFail:
			settle(step.Reason)
			sock.Close(step.CloseCode, step.Reason)
		case StepReady:
			t, err := h.ready(ctx, sock, step.Hello)
			if err != nil {
				o.Log.Error("隧道握手异常", obs.Error(err))
				return
			}
			if t != nil {
				tun = t
				settle("ok")
			}
		}
	}
	timer.Stop()
	settle("aborted")
	if tun != nil {
		tun.TransportClosed(code, "")
	}
}

func closeCodeOf(err error) protocol.CloseCode {
	var ce interface{ CloseCode() int }
	if errors.As(err, &ce) {
		return protocol.CloseCode(ce.CloseCode())
	}
	return 1006
}

func (h *Hub) ready(ctx context.Context, sock Socket, hello gen.Hello) (*Tunnel, error) {
	o := h.opts
	tunnelID := randomHex(16)
	var tunnel *Tunnel
	tunnel = New(tunnelID, hello.SourceID, hello.CoreVersion, sock, o.Config, Hooks{
		OnPong: func() {
			if !h.isCurrent(tunnel) {
				return
			}
			go func() {
				c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = o.Control.Routes().Touch(c, hello.SourceID, o.RouteTTL)
			}()
		},
		OnPeerGoaway: func(string) {
			if h.isCurrent(tunnel) {
				c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = o.Control.Routes().Delete(c, hello.SourceID, tunnelID)
			}
		},
		OnHeartbeatMiss: o.Metrics.HeartbeatMiss,
		OnBytes:         o.Metrics.AddBytes,
		OnClose:         func(info CloseInfo) { h.closed(tunnel, info) },
	})
	tunnel.SetCapabilities(hello.Capabilities)

	h.mu.Lock()
	previous := h.current[hello.SourceID]
	h.current[hello.SourceID] = tunnel
	h.all[tunnel] = struct{}{}
	h.mu.Unlock()
	if previous != nil {
		previous.Goaway("replaced", DefaultGrace)
	}
	if err := o.Control.Routes().Set(ctx, hello.SourceID, control.Route{
		Node:        o.NodeID,
		TunnelID:    tunnelID,
		SinceMs:     time.Now().UnixMilli(),
		CoreVersion: hello.CoreVersion,
	}, o.RouteTTL); err != nil {
		tunnel.TransportClosed(protocol.CloseGoingAway, "route_failed")
		return nil, err
	}
	ready := gen.Ready{
		Type:     "ready",
		TunnelID: tunnelID,
		Limits: gen.ReadyLimits{
			MaxStreams:    int64(o.Config.MaxStreams),
			StreamWindow:  int64(o.Config.StreamWindow),
			TunnelWindow:  int64(o.Config.TunnelWindow),
			MaxFrameBytes: protocol.MaxFrameBytes,
		},
		HeartbeatMs: o.Config.Heartbeat.Milliseconds(),
		Protocol:    gen.HelloProtocol{Major: gen.ProtocolMajor, Minor: gen.ProtocolMinor},
	}
	if err := sock.SendText(mustJSON(ready)); err != nil {
		tunnel.TransportClosed(protocol.CloseGoingAway, "closed_during_handshake")
		return nil, nil
	}
	tunnel.Start()
	_ = o.Control.Presence().Publish(ctx, control.PresenceEvent{
		Type:        control.PresenceSourceOnline,
		SourceID:    hello.SourceID,
		Node:        o.NodeID,
		TunnelID:    tunnelID,
		CoreVersion: hello.CoreVersion,
		AtMs:        time.Now().UnixMilli(),
	})
	o.Log.Info("隧道已就绪", obs.SourceID(hello.SourceID), obs.TunnelID(tunnelID))
	return tunnel, nil
}

func (h *Hub) isCurrent(t *Tunnel) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current[t.SourceID()] == t
}

func (h *Hub) closed(t *Tunnel, info CloseInfo) {
	o := h.opts
	h.mu.Lock()
	delete(h.all, t)
	isCurrent := h.current[t.SourceID()] == t
	if isCurrent {
		delete(h.current, t.SourceID())
	}
	h.mu.Unlock()
	// TODO(G3)：下面的删路由与发下线事件现在同步跑在关闭它的协程上（可能是 epoll 循环），
	// 内存控制面没问题；redis / postgres 实现必须改成异步。
	o.Log.Info("隧道已关闭", obs.SourceID(t.SourceID()), obs.TunnelID(t.ID()), obs.Code(int(info.Code)), obs.Reason(info.Reason))
	if !isCurrent || info.Replaced {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = o.Control.Routes().Delete(ctx, t.SourceID(), t.ID())
	_ = o.Control.Presence().Publish(ctx, control.PresenceEvent{
		Type:     control.PresenceSourceOffline,
		SourceID: t.SourceID(),
		Node:     o.NodeID,
		TunnelID: t.ID(),
		AtMs:     time.Now().UnixMilli(),
	})
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("读取随机数失败：" + err.Error())
	}
	return hex.EncodeToString(b)
}
