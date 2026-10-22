// Package cluster 是多节点控制面（redis / postgres）共用的部分：广播消息的线上形状、
// 撤销表的内存副本、限流的「远端计数 + 本节点兜底」逻辑与后台任务跟踪。
//
// 线上形状与 TypeScript 的 relay-runtime-node/src/control 逐字段一致（JSON camelCase），
// 所以 Go 节点与 Node 节点可以共用同一个 Redis / 库，滚动切换。
package cluster

import (
	"encoding/json"
	"errors"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
)

// routeWire 是路由在 Redis 里的 JSON（Lua 脚本按 tunnelId 比较，字段名不能改）。
type routeWire struct {
	Node        string `json:"node"`
	TunnelID    string `json:"tunnelId"`
	SinceMs     int64  `json:"sinceMs"`
	CoreVersion string `json:"coreVersion"`
}

// EncodeRoute 把路由编成存储用的 JSON。
func EncodeRoute(r control.Route) []byte {
	b, _ := json.Marshal(routeWire{Node: r.Node, TunnelID: r.TunnelID, SinceMs: r.SinceMs, CoreVersion: r.CoreVersion})
	return b
}

// DecodeRoute 解存储里的路由 JSON。
func DecodeRoute(data []byte) (control.Route, error) {
	var w routeWire
	if err := json.Unmarshal(data, &w); err != nil {
		return control.Route{}, err
	}
	return control.Route{Node: w.Node, TunnelID: w.TunnelID, SinceMs: w.SinceMs, CoreVersion: w.CoreVersion}, nil
}

type presenceWire struct {
	Type        string `json:"type"`
	SourceID    string `json:"sourceId"`
	Node        string `json:"node"`
	TunnelID    string `json:"tunnelId"`
	CoreVersion string `json:"coreVersion,omitempty"`
	AtMs        int64  `json:"atMs"`
}

type commandWire struct {
	Type     string `json:"type"`
	SourceID string `json:"sourceId"`
	Reason   string `json:"reason,omitempty"`
}

// broadcastWire 是广播消息的外壳：Origin 是发送实例，自己发的不再投递一次。
type broadcastWire struct {
	Origin  string          `json:"origin"`
	Kind    string          `json:"kind"`
	Event   json.RawMessage `json:"event,omitempty"`
	Command json.RawMessage `json:"command,omitempty"`
	JTI     string          `json:"jti,omitempty"`
	ExpMs   *int64          `json:"expMs,omitempty"`
}

// Message 是解析后的一条广播；Kind 只会是 "presence"、"command"、"revoked"。
type Message struct {
	Origin   string
	Kind     string
	Presence control.PresenceEvent
	Command  control.NodeCommand
	JTI      string
	ExpMs    int64
}

// 广播种类。
const (
	KindPresence = "presence"
	KindCommand  = "command"
	KindRevoked  = "revoked"
)

// EncodePresence 编在线事件广播。
func EncodePresence(origin string, ev control.PresenceEvent) []byte {
	ev2, _ := json.Marshal(presenceWire{
		Type: string(ev.Type), SourceID: ev.SourceID, Node: ev.Node, TunnelID: ev.TunnelID,
		CoreVersion: ev.CoreVersion, AtMs: ev.AtMs,
	})
	b, _ := json.Marshal(broadcastWire{Origin: origin, Kind: KindPresence, Event: ev2})
	return b
}

// EncodeCommand 编节点命令广播。
func EncodeCommand(origin string, c control.NodeCommand) []byte {
	c2, _ := json.Marshal(commandWire{Type: string(c.Type), SourceID: c.SourceID, Reason: c.Reason})
	b, _ := json.Marshal(broadcastWire{Origin: origin, Kind: KindCommand, Command: c2})
	return b
}

// EncodeRevoked 编撤销广播。
func EncodeRevoked(origin, jti string, expMs int64) []byte {
	b, _ := json.Marshal(broadcastWire{Origin: origin, Kind: KindRevoked, JTI: jti, ExpMs: &expMs})
	return b
}

var errUnknownShape = errors.New("不认识的广播形状")

// ParseBroadcast 解析广播；不认识的形状答错误（别的版本的节点发的，忽略）。
func ParseBroadcast(data []byte) (Message, error) {
	var w broadcastWire
	if err := json.Unmarshal(data, &w); err != nil || w.Origin == "" {
		return Message{}, errUnknownShape
	}
	m := Message{Origin: w.Origin, Kind: w.Kind}
	switch w.Kind {
	case KindPresence:
		var p presenceWire
		if json.Unmarshal(w.Event, &p) != nil || p.SourceID == "" {
			return Message{}, errUnknownShape
		}
		switch control.PresenceType(p.Type) {
		case control.PresenceSourceOnline, control.PresenceSourceOffline:
		default:
			return Message{}, errUnknownShape
		}
		m.Presence = control.PresenceEvent{
			Type: control.PresenceType(p.Type), SourceID: p.SourceID, Node: p.Node, TunnelID: p.TunnelID,
			CoreVersion: p.CoreVersion, AtMs: p.AtMs,
		}
	case KindCommand:
		var c commandWire
		if json.Unmarshal(w.Command, &c) != nil || c.SourceID == "" {
			return Message{}, errUnknownShape
		}
		switch control.NodeCommandType(c.Type) {
		case control.CommandCloseTunnel, control.CommandKick:
		default:
			return Message{}, errUnknownShape
		}
		m.Command = control.NodeCommand{Type: control.NodeCommandType(c.Type), SourceID: c.SourceID, Reason: c.Reason}
	case KindRevoked:
		if w.JTI == "" || w.ExpMs == nil {
			return Message{}, errUnknownShape
		}
		m.JTI, m.ExpMs = w.JTI, *w.ExpMs
	default:
		return Message{}, errUnknownShape
	}
	return m, nil
}
