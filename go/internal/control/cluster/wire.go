// Package cluster 是多节点控制面（redis / postgres）共用的部分：广播消息的线上形状、
// 撤销表的内存副本、限流的「远端计数 + 本节点兜底」逻辑与后台任务跟踪。
//
// 线上形状与已删除的 TypeScript 版 relay-runtime-node/src/control 逐字段一致（JSON camelCase），
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

// commandWire 是节点命令（SaaS 实施设计 §2.4）。kick / closeTunnel 必须带 sourceId；G5 加的
// subject、subjectPrefix、deviceId 是 kick 的过滤，老节点忽略（退化为踢该源全部连接）；
// drain 带 node、不带 sourceId（老节点不认识这个种类，整条忽略）。
type commandWire struct {
	Type          string `json:"type"`
	SourceID      string `json:"sourceId,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Subject       string `json:"subject,omitempty"`
	SubjectPrefix string `json:"subjectPrefix,omitempty"`
	DeviceID      string `json:"deviceId,omitempty"`
	Node          string `json:"node,omitempty"`
}

// quotaWire 是 kind: quota 的 event，也是键 <前缀>quota:team:<teamId> 的值（值里不带 teamId）。
type quotaWire struct {
	TeamID     string `json:"teamId,omitempty"`
	Concurrent *int   `json:"concurrent"`
}

// incidentWire 是 kind: incident 的 event，也是键 <前缀>incident 的值。
type incidentWire struct {
	RejectNewConnections *bool `json:"rejectNewConnections"`
	AtMs                 int64 `json:"atMs"`
}

// nodeWire 是节点表 <前缀>nodes 的字段值（字段名是节点 id）。internodeUrl 是 G5 加的，可以缺。
type nodeWire struct {
	URL          string `json:"url"`
	Weight       int    `json:"weight"`
	Version      string `json:"version"`
	Draining     bool   `json:"draining"`
	Connections  int    `json:"connections"`
	Tunnels      int    `json:"tunnels"`
	InternodeURL string `json:"internodeUrl,omitempty"`
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

// Message 是解析后的一条广播；Kind 只会是 "presence"、"command"、"revoked"、"quota"、"incident"。
type Message struct {
	Origin   string
	Kind     string
	Presence control.PresenceEvent
	Command  control.NodeCommand
	JTI      string
	ExpMs    int64
	Quota    control.TeamQuota
	Incident control.Incident
}

// 广播种类。quota 与 incident 是 G5（N4）新增的，控制面发、SaaS 节点收；老节点忽略。
const (
	KindPresence = "presence"
	KindCommand  = "command"
	KindRevoked  = "revoked"
	KindQuota    = "quota"
	KindIncident = "incident"
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
	c2, _ := json.Marshal(commandWire{
		Type: string(c.Type), SourceID: c.SourceID, Reason: c.Reason,
		Subject: c.Subject, SubjectPrefix: c.SubjectPrefix, DeviceID: c.DeviceID, Node: c.Node,
	})
	b, _ := json.Marshal(broadcastWire{Origin: origin, Kind: KindCommand, Command: c2})
	return b
}

// EncodeQuota 编团队额度广播（kind: quota）。
func EncodeQuota(origin string, q control.TeamQuota) []byte {
	concurrent := q.Concurrent
	ev, _ := json.Marshal(quotaWire{TeamID: q.TeamID, Concurrent: &concurrent})
	b, _ := json.Marshal(broadcastWire{Origin: origin, Kind: KindQuota, Event: ev})
	return b
}

// EncodeIncident 编事故开关广播（kind: incident）。
func EncodeIncident(origin string, in control.Incident) []byte {
	b, _ := json.Marshal(broadcastWire{Origin: origin, Kind: KindIncident, Event: EncodeIncidentValue(in)})
	return b
}

// EncodeQuotaValue 编键 <前缀>quota:team:<teamId> 的值 {concurrent}。
func EncodeQuotaValue(q control.TeamQuota) []byte {
	concurrent := q.Concurrent
	b, _ := json.Marshal(quotaWire{Concurrent: &concurrent})
	return b
}

// DecodeQuotaValue 解键 <前缀>quota:team:<teamId> 的值；teamID 取自键名。
func DecodeQuotaValue(teamID string, data []byte) (control.TeamQuota, error) {
	var w quotaWire
	if err := json.Unmarshal(data, &w); err != nil || w.Concurrent == nil || *w.Concurrent < 0 {
		return control.TeamQuota{}, errUnknownShape
	}
	return control.TeamQuota{TeamID: teamID, Concurrent: *w.Concurrent}, nil
}

// EncodeIncidentValue 编键 <前缀>incident 的值 {rejectNewConnections, atMs}。
func EncodeIncidentValue(in control.Incident) []byte {
	reject := in.RejectNewConnections
	b, _ := json.Marshal(incidentWire{RejectNewConnections: &reject, AtMs: in.AtMs})
	return b
}

// DecodeIncidentValue 解键 <前缀>incident 的值。
func DecodeIncidentValue(data []byte) (control.Incident, error) {
	var w incidentWire
	if err := json.Unmarshal(data, &w); err != nil || w.RejectNewConnections == nil {
		return control.Incident{}, errUnknownShape
	}
	return control.Incident{RejectNewConnections: *w.RejectNewConnections, AtMs: w.AtMs}, nil
}

// EncodeNode 编节点表的字段值（字段名是 n.ID，不进值）。
func EncodeNode(n control.NodeInfo) []byte {
	b, _ := json.Marshal(nodeWire{
		URL: n.URL, Weight: n.Weight, Version: n.Version, Draining: n.Draining,
		Connections: n.Connections, Tunnels: n.Tunnels, InternodeURL: n.InternodeURL,
	})
	return b
}

// DecodeNode 解节点表的字段值；id 取自字段名。url 缺了就当坏记录。
func DecodeNode(id string, data []byte) (control.NodeInfo, error) {
	var w nodeWire
	if err := json.Unmarshal(data, &w); err != nil || w.URL == "" {
		return control.NodeInfo{}, errUnknownShape
	}
	return control.NodeInfo{
		ID: id, URL: w.URL, Weight: w.Weight, Version: w.Version, Draining: w.Draining,
		Connections: w.Connections, Tunnels: w.Tunnels, InternodeURL: w.InternodeURL,
	}, nil
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
		if json.Unmarshal(w.Command, &c) != nil {
			return Message{}, errUnknownShape
		}
		switch control.NodeCommandType(c.Type) {
		case control.CommandCloseTunnel, control.CommandKick:
			if c.SourceID == "" {
				return Message{}, errUnknownShape
			}
		case control.CommandDrain:
			if c.Node == "" {
				return Message{}, errUnknownShape
			}
		default:
			return Message{}, errUnknownShape
		}
		m.Command = control.NodeCommand{
			Type: control.NodeCommandType(c.Type), SourceID: c.SourceID, Reason: c.Reason,
			Subject: c.Subject, SubjectPrefix: c.SubjectPrefix, DeviceID: c.DeviceID, Node: c.Node,
		}
	case KindQuota:
		var q quotaWire
		if json.Unmarshal(w.Event, &q) != nil || q.TeamID == "" || q.Concurrent == nil || *q.Concurrent < 0 {
			return Message{}, errUnknownShape
		}
		m.Quota = control.TeamQuota{TeamID: q.TeamID, Concurrent: *q.Concurrent}
	case KindIncident:
		in, err := DecodeIncidentValue(w.Event)
		if err != nil {
			return Message{}, err
		}
		m.Incident = in
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
