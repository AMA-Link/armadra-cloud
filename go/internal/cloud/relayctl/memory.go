package relayctl

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// Memory 是 Control 的内存实现：签发索引、撤销表、节点表都在进程内，发出的命令按序记下来给测试断言。
type Memory struct {
	now Clock

	mu       sync.Mutex
	issued   map[string]Issued
	revoked  map[string]int64
	commands []Command
	problems []string
	routes   map[string]string
	nodes    map[string]Node
	quotas   map[string]Quota
	live     map[string]int
	incident Incident
}

var _ Control = (*Memory)(nil)

// NewMemory 建内存实现；now 缺省 time.Now。
func NewMemory(now Clock) *Memory {
	if now == nil {
		now = time.Now
	}
	return &Memory{
		now: now, issued: map[string]Issued{}, revoked: map[string]int64{},
		routes: map[string]string{}, nodes: map[string]Node{}, quotas: map[string]Quota{}, live: map[string]int{},
	}
}

func (m *Memory) nowMs() int64 { return m.now().UnixMilli() }

// purge 丢掉已到期的索引与撤销记录（持锁调用）。
func (m *Memory) purge() {
	nowMs := m.nowMs()
	for jti, entry := range m.issued {
		if entry.ExpMs <= nowMs {
			delete(m.issued, jti)
		}
	}
	for jti, exp := range m.revoked {
		if exp <= nowMs {
			delete(m.revoked, jti)
		}
	}
}

func matches(entry Issued, filter Filter) bool {
	if filter.JTI != "" && entry.JTI != filter.JTI {
		return false
	}
	if filter.Subject != "" && entry.Subject != filter.Subject {
		return false
	}
	if filter.SubjectPrefix != "" && !strings.HasPrefix(entry.Subject, filter.SubjectPrefix) {
		return false
	}
	if filter.SourceID != "" && entry.SourceID != filter.SourceID {
		return false
	}
	if filter.DeviceID != "" && entry.DeviceID != filter.DeviceID {
		return false
	}
	return true
}

// IndexIssued 实现 Control。
func (m *Memory) IndexIssued(_ context.Context, issued Issued) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purge()
	m.issued[issued.JTI] = issued
	return nil
}

// revokeLocked 撤销符合过滤的令牌，答被撤销的条目（持锁调用）。
func (m *Memory) revokeLocked(filter Filter, keepDeviceID string) []Issued {
	if filter.empty() {
		return nil
	}
	m.purge()
	var hit []Issued
	for _, entry := range m.issued {
		if matches(entry, filter) && (keepDeviceID == "" || entry.DeviceID != keepDeviceID) {
			hit = append(hit, entry)
		}
	}
	sort.Slice(hit, func(i, j int) bool { return hit[i].JTI < hit[j].JTI })
	for _, entry := range hit {
		m.revoked[entry.JTI] = entry.ExpMs
		delete(m.issued, entry.JTI)
	}
	return hit
}

// RevokeWhere 实现 Control。
func (m *Memory) RevokeWhere(_ context.Context, filter Filter) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.revokeLocked(filter, "")), nil
}

// Revoke 实现 Control。
func (m *Memory) Revoke(_ context.Context, filter Filter, keepDeviceID string) ([]Issued, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revokeLocked(filter, keepDeviceID), nil
}

// Kick 实现 Control。
func (m *Memory) Kick(_ context.Context, filter KickFilter, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands = append(m.commands, Command{
		Type: CommandKick, SourceID: filter.SourceID, Subject: filter.Subject,
		SubjectPrefix: filter.SubjectPrefix, DeviceID: filter.DeviceID, Reason: reason,
	})
	return nil
}

// CloseTunnel 实现 Control。
func (m *Memory) CloseTunnel(_ context.Context, sourceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands = append(m.commands, Command{Type: CommandCloseTunnel, SourceID: sourceID})
	return nil
}

// Route 实现 Control。
func (m *Memory) Route(_ context.Context, sourceID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	version, ok := m.routes[sourceID]
	return version, ok, nil
}

// Nodes 实现 Control：去掉排空的，按 id 排序。
func (m *Memory) Nodes(context.Context) ([]Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Node
	for _, node := range m.nodes {
		if !node.Draining {
			out = append(out, node)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AllNodes 实现 Control：含排空的，按 id 排序。
func (m *Memory) AllNodes(context.Context) ([]Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Node, 0, len(m.nodes))
	for _, node := range m.nodes {
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Drain 实现 Control：记命令，并把内存里的节点标成排空（真实节点自己把 draining 写回节点表）。
func (m *Memory) Drain(_ context.Context, node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands = append(m.commands, Command{Type: CommandDrain, Node: node})
	if entry, ok := m.nodes[node]; ok {
		entry.Draining = true
		m.nodes[node] = entry
	}
	return nil
}

// SetTeamQuota 实现 Control。
func (m *Memory) SetTeamQuota(_ context.Context, teamID string, quota Quota) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotas[teamID] = quota
	return nil
}

// TeamConcurrent 实现 Control：内存实现没有节点，答 SetTeamConcurrent 设的值（测试用，缺省 0）。
func (m *Memory) TeamConcurrent(_ context.Context, teamID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[teamID], nil
}

// SetTeamConcurrent 设团队的在线连接数（测试用）。
func (m *Memory) SetTeamConcurrent(teamID string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.live[teamID] = n
}

// ClearTeamQuota 实现 Control。
func (m *Memory) ClearTeamQuota(_ context.Context, teamID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.quotas, teamID)
	return nil
}

// SetIncident 实现 Control。
func (m *Memory) SetIncident(_ context.Context, incident Incident) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.incident = incident
	return nil
}

// Relay 实现 Control。
func (m *Memory) Relay() core.RelayPort { return relayPort{m} }

// Revocations 实现 Control。
func (m *Memory) Revocations() core.RevocationSink { return sink{m} }

// ---- 测试与装配用的读写口 ----

// SetRoute 让一台主机在线（隧道上报了 coreVersion）；version 为空 = 下线。
func (m *Memory) SetRoute(sourceID, coreVersion string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if coreVersion == "" {
		delete(m.routes, sourceID)
		return
	}
	m.routes[sourceID] = coreVersion
}

// SetNode 写节点表的一个节点。
func (m *Memory) SetNode(node Node) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes[node.ID] = node
}

// Commands 是按序发出的节点命令。
func (m *Memory) Commands() []Command {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Command(nil), m.commands...)
}

// Problems 是没有发出的命令的原因（例如 kick 缺 sourceId）。
func (m *Memory) Problems() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.problems...)
}

// RevokedTokens 是撤销表里未到期的令牌，按 jti 排序。
func (m *Memory) RevokedTokens() []Revoked {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purge()
	out := make([]Revoked, 0, len(m.revoked))
	for jti, exp := range m.revoked {
		out = append(out, Revoked{JTI: jti, ExpMs: exp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JTI < out[j].JTI })
	return out
}

// IsRevoked 报告令牌是否已在撤销表里。
func (m *Memory) IsRevoked(jti string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purge()
	_, ok := m.revoked[jti]
	return ok
}

// Quota 读团队的并发额度。
func (m *Memory) Quota(teamID string) (Quota, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	quota, ok := m.quotas[teamID]
	return quota, ok
}

// CurrentIncident 读事故开关。
func (m *Memory) CurrentIncident() Incident {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.incident
}

// ---- core.RelayPort ----

type relayPort struct{ m *Memory }

func (p relayPort) Route(ctx context.Context, sourceID string) (string, bool, error) {
	return p.m.Route(ctx, sourceID)
}

func (p relayPort) CloseTunnel(sourceID string) { _ = p.m.CloseTunnel(context.Background(), sourceID) }

// Disconnect 踢客户端连接。节点命令必须带 sourceId（设计 §2.4，节点会丢弃不带的）：
// 没有 SourceID 的过滤不发，记一条问题（Problems）。
func (p relayPort) Disconnect(filter core.ConnectionFilter) {
	if filter.SourceID == "" {
		p.m.mu.Lock()
		p.m.problems = append(p.m.problems, "kick 缺 sourceId，没有发出")
		p.m.mu.Unlock()
		return
	}
	_ = p.m.Kick(context.Background(), KickFilter{SourceID: filter.SourceID, SubjectPrefix: filter.SubjectPrefix}, "disconnect")
}

// ---- core.RevocationSink（设计 §4.8 的撤销表）----

type sink struct{ m *Memory }

// expand 把被撤销的令牌展开成逐源的 kick：同一 (源, subject, 设备) 一条，节点按 subject / deviceId 精确踢人。
func (s sink) expand(hit []Issued, reason string) {
	type key struct{ source, subject, device string }
	seen := map[key]bool{}
	for _, entry := range hit {
		if entry.SourceID == "" || entry.Subject == NoAccount {
			continue
		}
		k := key{entry.SourceID, entry.Subject, entry.DeviceID}
		if seen[k] {
			continue
		}
		seen[k] = true
		s.m.commands = append(s.m.commands, Command{
			Type: CommandKick, SourceID: entry.SourceID, Subject: entry.Subject, DeviceID: entry.DeviceID, Reason: reason,
		})
	}
}

// DeviceRevoked：撤这台设备签出的全部令牌，并到每台有它令牌的主机去踢。
func (s sink) DeviceRevoked(_ context.Context, subject, deviceID string) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	s.expand(s.m.revokeLocked(Filter{Subject: subject, DeviceID: deviceID}, ""), "device_revoked")
}

// DevicesRevoked：撤账号的全部令牌（keepDeviceID 那台的除外），逐 (源, 设备) 踢。
func (s sink) DevicesRevoked(_ context.Context, subject, keepDeviceID string) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	s.expand(s.m.revokeLocked(Filter{Subject: subject}, keepDeviceID), "devices_revoked")
}

// LinkRevoked：撤这条链接访客在该源上的令牌；踢人由 core 的 Disconnect 做了。
func (s sink) LinkRevoked(_ context.Context, sourceID, linkID string) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	s.m.revokeLocked(Filter{SourceID: sourceID, SubjectPrefix: core.GuestPrefix(linkID)}, "")
}

// SourceRevoked：撤该源全部令牌（含隧道令牌）；关隧道与踢人由 core 的 CloseTunnel / Disconnect 做了。
func (s sink) SourceRevoked(_ context.Context, sourceID string) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	s.m.revokeLocked(Filter{SourceID: sourceID}, "")
}
