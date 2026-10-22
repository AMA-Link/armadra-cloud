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
