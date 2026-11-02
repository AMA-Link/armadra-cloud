// Package netpoll 是边缘连接的事件驱动层。
//
// 同一个接口两种实现：
//
//   - Linux：每核一个 epoll 循环，连接升级后不留协程。epoll fd 设成非阻塞、交给 Go 的
//     netpoller 等可读，醒来后用超时 0 的 epoll_wait 取事件；一批最多 64 个，处理完让出。
//     读进循环共用的暂存区；下行由调用方的协程非阻塞直写，写不完的排队、等 EPOLLOUT；
//     上行没信用时 Pause 停读。
//   - 其他平台与非 TCP 连接（如 TLS）：每连接一个读协程、一个写协程，行为相同。
//
// 字节真正交给内核之后才通过 OnSent 报「已写出」，边缘据此补信用。
package netpoll

import (
	"errors"
	"net"
	"time"
)

// ErrClosed 表示 Poller 已关闭，不再接管连接。
var ErrClosed = errors.New("netpoll：已关闭")

// ErrQueueOverflow 是写队列超限时 OnClose 收到的原因：对端读得太慢，连接被断开（#191）。
var ErrQueueOverflow = errors.New("netpoll：写队列超限")

// MaxCtlBytes 是单条连接写队列里不带信用的帧（ack 为 0：PONG、关闭帧等控制帧，已接受、还没写进内核）
// 的字节上限（#191）。带信用的下行数据受调用方的流控约束（边缘是每流 256 KiB 窗口），队列不会无限长；
// 控制帧不受流控约束，对端不读 socket 又不停触发回复就会无限增长，所以单独限。64 KiB 够几百条 PONG，
// 正常客户端读得动，队列恒为空；超了就断开慢读的对端（OnClose 收到 ErrQueueOverflow）。
const MaxCtlBytes = 64 << 10

// ctlBytes 记写队列里不带信用的字节数；调用方持连接的锁。
type ctlBytes int

// add 记入一帧，答是否超限。
func (q *ctlBytes) add(n, ack int) bool {
	if ack == 0 {
		*q += ctlBytes(n)
	}
	return *q > MaxCtlBytes
}

// sub 记出一帧（写完了）。
func (q *ctlBytes) sub(n, ack int) {
	if ack == 0 {
		*q -= ctlBytes(n)
	}
}

// Handler 接住一条连接的事件。OnRead 对同一条连接串行调用；OnSent、OnClose 可能来自任意协程。
type Handler interface {
	// OnRead 是读到的字节；p 在返回后会被复用，要留就拷贝。
	OnRead(c Conn, p []byte)
	// OnSent 报告之前 Write 的若干帧已全部交给内核（或连接关闭时被丢弃），ack 是它们的 ack 之和。
	OnSent(c Conn, ack int)
	// OnClose 在连接关闭后调用恰好一次；err 为空表示本地主动关闭。
	OnClose(c Conn, err error)
}

// Conn 是一条受管理的连接。所有方法可以从任意协程调用，且不阻塞。
type Conn interface {
	// Write 排队写一帧，答是否接受。接受后恰好会有一次 OnSent 报告它的 ack；
	// 连接已关闭或正在关闭时答 false，ack 由调用方自己处理。
	// 队列里不带信用的帧超过 MaxCtlBytes 时连接以 ErrQueueOverflow 关闭（这一帧算已接受、随队列作废）。
	Write(b []byte, ack int) bool
	// Pause 停止读（上行没信用时用）；Resume 恢复。
	Pause()
	Resume()
	// Close 立即关闭，没写出的帧作废（ack 经 OnSent 报告）。
	Close()
	// CloseAfterFlush 等队列写完再关；超过 timeout 仍没写完就强关。之后的 Write 被拒。
	CloseAfterFlush(timeout time.Duration)
	// RemoteAddr 是对端地址。
	RemoteAddr() string
}

// Poller 管理一组连接。
type Poller struct {
	impl poller
}

type poller interface {
	adopt(nc net.Conn, h Handler) (Conn, error)
	close()
	size() int
}

// New 建 Poller。loops 是 epoll 循环数（Linux），小于 1 取 GOMAXPROCS。
func New(loops int) (*Poller, error) {
	impl, err := newPoller(loops)
	if err != nil {
		return nil, err
	}
	return &Poller{impl: impl}, nil
}

// Adopt 接管 nc（调用方不要再读写它）。读从下一刻开始。
func (p *Poller) Adopt(nc net.Conn, h Handler) (Conn, error) { return p.impl.adopt(nc, h) }

// Size 是当前受管理的连接数。
func (p *Poller) Size() int { return p.impl.size() }

// Close 停掉所有循环；连接由调用方各自关闭。
func (p *Poller) Close() { p.impl.close() }

// Native 是否用了 epoll 实现。
func Native() bool { return native }
