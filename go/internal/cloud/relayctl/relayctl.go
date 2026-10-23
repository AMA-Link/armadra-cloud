// Package relayctl 是控制面与中继节点之间的约定（SaaS 实现方案 §2.4、§4.8、§7.2）：签发索引、
// 撤销已签令牌、踢人、关隧道、节点表、团队额度与事故开关。
//
// S1b 只有内存实现 Memory：行为与以后包装 control/redis.Plane 的实现相同，测试直接断言它发出的
// 命令与撤销。Control 同时是 core 的撤销钩子（core.RevocationSink）：登出、撤销设备、改口令、
// 刷新令牌重放、链接与源撤销之后，按签发索引算出受影响的令牌与主机，撤销令牌并踢人。
package relayctl

import (
	"context"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// NoAccount 是隧道令牌在签发索引里的账号位（隧道令牌属于主机，不属于账号）。
const NoAccount = "-"

// Issued 是签发索引里的一条：一枚已签的断言、中继令牌或隧道令牌。
type Issued struct {
	// Subject 是令牌的 sub：账号 id、访客 guest:<linkId>:<随机>，隧道令牌记 NoAccount。
	Subject  string
	SourceID string
	DeviceID string
	JTI      string
	// ExpMs 是令牌到期（毫秒）；到期后索引与撤销记录都可以丢。
	ExpMs int64
}

// Filter 选出要撤销的已签令牌：给了的字段都要对上（取交集）；全空不选任何令牌。
type Filter struct {
	Subject       string
	SubjectPrefix string
	SourceID      string
	DeviceID      string
}

func (f Filter) empty() bool {
	return f.Subject == "" && f.SubjectPrefix == "" && f.SourceID == "" && f.DeviceID == ""
}

// KickFilter 选出要踢掉的客户端连接，字段含义同 core.ConnectionFilter，另有按设备。
// 老节点不认识 Subject / SubjectPrefix / DeviceID，会退化为踢该源全部连接（多踢不漏踢）。
type KickFilter struct {
	SourceID      string
	Subject       string
	SubjectPrefix string
	DeviceID      string
}

// CommandType 是发给节点的命令种类。
type CommandType string

// 命令种类。
const (
	CommandKick        CommandType = "kick"
	CommandCloseTunnel CommandType = "closeTunnel"
	CommandDrain       CommandType = "drain"
)

// Command 是一条发给节点的命令（所有命令都带 SourceID，drain 例外带 Node）。
type Command struct {
	Type          CommandType
	SourceID      string
	Subject       string
	SubjectPrefix string
	DeviceID      string
	Node          string
	Reason        string
}

// Revoked 是一条已写入撤销表的令牌。
type Revoked struct {
	JTI   string
	ExpMs int64
}

// Node 是 relay:nodes 里的一个节点。
type Node struct {
	ID          string
	URL         string
	Weight      int
	Version     string
	Draining    bool
	Connections int
	Tunnels     int
}

// Quota 是团队的并发额度（relay:quota:team:*）。
type Quota struct{ Concurrent int }

// Incident 是 relay:incident：开着时节点拒绝新的隧道与 e2e 升级。
type Incident struct {
	RejectNewConnections bool
	AtMs                 int64
}

// Control 是控制面对节点的全部依赖。
type Control interface {
	// IndexIssued 把一枚已签令牌记进签发索引（账号维度与主机维度）。
	IndexIssued(ctx context.Context, issued Issued) error
	// RevokeWhere 撤销签发索引里符合过滤的未到期令牌，答撤销了几枚。
	RevokeWhere(ctx context.Context, filter Filter) (int, error)
	// Kick 踢掉符合过滤的客户端连接。
	Kick(ctx context.Context, filter KickFilter, reason string) error
	// CloseTunnel 关掉主机的隧道。
	CloseTunnel(ctx context.Context, sourceID string) error
	// Route 答主机在线时隧道上报的 coreVersion；不在线 ok 为 false。
	Route(ctx context.Context, sourceID string) (coreVersion string, ok bool, err error)
	// Nodes 答节点表里没有在排空的节点。
	Nodes(ctx context.Context) ([]Node, error)
	// Drain 让节点排空。
	Drain(ctx context.Context, node string) error
	// SetTeamQuota 写团队的并发额度。
	SetTeamQuota(ctx context.Context, teamID string, quota Quota) error
	// SetIncident 写事故开关。
	SetIncident(ctx context.Context, incident Incident) error
	// Relay 是 core 要的中继端口（查在线、关隧道、断开客户端）。
	Relay() core.RelayPort
	// Revocations 是 core 的撤销钩子。
	Revocations() core.RevocationSink
}

// Clock 是时钟；缺省 time.Now。
type Clock func() time.Time
