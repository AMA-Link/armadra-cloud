package admin

import (
	"context"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// accountStatus 是 admin.accountStatus：停用 = 拒绝登录与断言、撤销全部设备（经撤销钩子撤已签令牌并踢人，设计 §4.8），
// 恢复只改状态。写审计（actor.kind = operator，reason）。不许停用自己：否则最后一个运营账号会把自己锁在门外。
func (s *Service) accountStatus(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		AccountID string `json:"accountId"`
		Status    string `json:"status"`
		Reason    string `json:"reason"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	operator, err := s.operator(ctx, call)
	if err != nil {
		return nil, err
	}
	if in.AccountID == operator.AccountID && in.Status == saas.AccountDisabled {
		return nil, core.Fail(gen.ErrForbidden)
	}
	nowMs := s.nowMs()
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		found, err := tx.Accounts().LockAccount(ctx, s.tenant(), in.AccountID)
		if err != nil {
			return err
		}
		if !found {
			return core.Fail(gen.ErrNotFound)
		}
		reason := in.Reason
		var stored *string
		if in.Status == saas.AccountDisabled {
			stored = &reason
		}
		if _, err := tx.Ops().SetAccountStatus(ctx, s.tenant(), in.AccountID, in.Status, stored, nowMs); err != nil {
			return err
		}
		action := ActionAccountEnable
		if in.Status == saas.AccountDisabled {
			action = ActionAccountDisable
			if _, err := tx.Ops().RevokeDevices(ctx, s.tenant(), in.AccountID, nowMs); err != nil {
				return err
			}
			// 提交后：撤这个账号全部已签令牌，按签发索引到每台有它令牌的主机去踢（设计 §4.8「账号停用」一行）。
			sink := relayctl.Deferred(tx, s.relay.Revocations())
			sink.DevicesRevoked(ctx, in.AccountID, "")
		}
		return s.record(ctx, tx, call, operator.AccountID, "", action, "account", in.AccountID, in.Reason)
	})
	if err != nil {
		return nil, err
	}
	return s.accountDetail(ctx, in.AccountID)
}

// teamStatus 是 admin.teamStatus：停用 = 拒绝签断言与登记主机（S5a / S5b 的门已按团队状态判断），撤销团队每台主机的
// 已签令牌并踢掉全部连接、隧道保留（设计 §4.8「团队停用」一行）；恢复只改状态。
func (s *Service) teamStatus(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		TeamID string `json:"teamId"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	operator, err := s.operator(ctx, call)
	if err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		found, err := tx.Teams().LockTeam(ctx, s.tenant(), in.TeamID)
		if err != nil {
			return err
		}
		if !found {
			return core.Fail(gen.ErrNotFound)
		}
		reason := in.Reason
		var stored *string
		if in.Status == saas.TeamSuspended {
			stored = &reason
		}
		if _, err := tx.Ops().SetTeamStatus(ctx, s.tenant(), in.TeamID, in.Status, stored, nowMs); err != nil {
			return err
		}
		action := ActionTeamResume
		if in.Status == saas.TeamSuspended {
			action = ActionTeamSuspend
			hosts, err := tx.Ops().TeamHostIDs(ctx, s.tenant(), in.TeamID)
			if err != nil {
				return err
			}
			s.afterKickHosts(tx, hosts, "team_suspended")
		}
		return s.record(ctx, tx, call, operator.AccountID, in.TeamID, action, "team", in.TeamID, in.Reason)
	})
	if err != nil {
		return nil, err
	}
	return s.teamDetail(ctx, in.TeamID)
}

// afterKickHosts 在提交后撤销这些主机上的全部已签令牌并踢掉全部连接（按 sourceId，隧道保留）。
func (s *Service) afterKickHosts(tx saas.Tx, sourceIDs []string, reason string) {
	tx.AfterCommit(func(ctx context.Context) {
		for _, sourceID := range sourceIDs {
			if _, err := s.relay.RevokeWhere(ctx, relayctl.Filter{SourceID: sourceID}); err != nil {
				s.log.Warn("撤销主机的已签令牌失败", obs.Operation("admin.revoke"), obs.SourceID(sourceID), obs.Error(err))
			}
			if err := s.relay.Kick(ctx, relayctl.KickFilter{SourceID: sourceID}, reason); err != nil {
				s.log.Warn("踢掉主机的连接失败", obs.Operation("admin.kick"), obs.SourceID(sourceID), obs.Error(err))
			}
		}
	})
}

// ---- 中继节点 ----

type nodeOut struct {
	Node         string `json:"node"`
	Online       bool   `json:"online"`
	Tunnels      int    `json:"tunnels"`
	Connections  int    `json:"connections"`
	Draining     bool   `json:"draining"`
	Version      string `json:"version"`
	LastSeenAtMs *int64 `json:"lastSeenAtMs"`
}

// nodeView：节点表里的节点都是在线的（每个字段 60 秒过期，下线的节点自己就不在表里了），最后见到就是此刻。
func (s *Service) nodeView(n relayctl.Node) nodeOut {
	seen := s.nowMs()
	return nodeOut{
		Node: n.ID, Online: true, Tunnels: n.Tunnels, Connections: n.Connections, Draining: n.Draining, Version: n.Version, LastSeenAtMs: &seen,
	}
}

// relayNodes 是 admin.relayNodes：读 relay:nodes（含排空中的）。
func (s *Service) relayNodes(ctx context.Context, call *api.Call) (any, error) {
	if _, err := s.operator(ctx, call); err != nil {
		return nil, err
	}
	nodes, err := s.relay.AllNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]nodeOut, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, s.nodeView(n))
	}
	return map[string]any{"nodes": out}, nil
}

// relayNodeDrain 是 admin.relayNodeDrain：drain = true 发 drain 命令（节点收到 GOAWAY rebalance，自己把 draining 写回节点表）。
// 节点一旦排空只能重启回来，控制面没有「取消排空」的命令：drain = false 是空操作，答复里的 draining
// 如实反映节点此刻的状态（排空中仍是 true）。契约（§28）给这个过程的错误码只有 not_found / forbidden，所以不另答 conflict。
func (s *Service) relayNodeDrain(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Node  string `json:"node"`
		Drain bool   `json:"drain"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	operator, err := s.operator(ctx, call)
	if err != nil {
		return nil, err
	}
	nodes, err := s.relay.AllNodes(ctx)
	if err != nil {
		return nil, err
	}
	var node *relayctl.Node
	for i := range nodes {
		if nodes[i].ID == in.Node {
			node = &nodes[i]
		}
	}
	if node == nil {
		return nil, core.Fail(gen.ErrNotFound)
	}
	switch {
	case in.Drain && !node.Draining:
		if err := s.store.Tx(ctx, func(tx saas.Tx) error {
			return s.record(ctx, tx, call, operator.AccountID, "", ActionNodeDrain, "relay_node", in.Node, "")
		}); err != nil {
			return nil, err
		}
		if err := s.relay.Drain(ctx, in.Node); err != nil {
			return nil, err
		}
		node.Draining = true
	}
	return s.nodeView(*node), nil
}

// ---- 事故开关 ----

type incidentOut struct {
	PauseAssertions      bool   `json:"pauseAssertions"`
	PauseRegistrations   bool   `json:"pauseRegistrations"`
	RejectNewConnections bool   `json:"rejectNewConnections"`
	UpdatedAtMs          *int64 `json:"updatedAtMs"`
}

func incidentView(i saas.Incident, at *int64) incidentOut {
	return incidentOut{
		PauseAssertions: i.AssertionsPaused, PauseRegistrations: i.RegistrationsPaused, RejectNewConnections: i.RejectNewConnections, UpdatedAtMs: at,
	}
}

func (s *Service) incident(ctx context.Context, call *api.Call) (any, error) {
	if _, err := s.operator(ctx, call); err != nil {
		return nil, err
	}
	incident, at, err := s.store.Read().IncidentAt(ctx, s.tenant())
	if err != nil {
		return nil, err
	}
	return incidentView(incident, at), nil
}

// incidentSet 是 admin.incidentSet：只改给出的项。三项开关：停签断言（sources.assertion 答 503，读库）、停注册（auth.register 答
// registrations_paused）、中继拒绝新连接（写 relay:incident 并广播，节点拒绝新的隧道与端到端升级，在线连接保留）。
// 写库在事务里，提交后写节点；写节点失败只记日志，各实例每 5 秒按库重写一遍（Syncer）。
func (s *Service) incidentSet(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		PauseAssertions      *bool  `json:"pauseAssertions"`
		PauseRegistrations   *bool  `json:"pauseRegistrations"`
		RejectNewConnections *bool  `json:"rejectNewConnections"`
		Reason               string `json:"reason"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	operator, err := s.operator(ctx, call)
	if err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	var incident saas.Incident
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		var err error
		incident, err = tx.Ops().UpdateIncident(ctx, s.tenant(), saas.IncidentPatch{
			AssertionsPaused: in.PauseAssertions, RegistrationsPaused: in.PauseRegistrations, RejectNewConnections: in.RejectNewConnections,
		}, operator.AccountID, nowMs)
		if err != nil {
			return err
		}
		tx.AfterCommit(func(ctx context.Context) { s.pushIncident(ctx, incident, nowMs) })
		return s.record(ctx, tx, call, operator.AccountID, "", ActionIncidentSet, "incident", "incident", in.Reason)
	})
	if err != nil {
		return nil, err
	}
	return incidentView(incident, &nowMs), nil
}

// pushIncident 把库里的事故开关写给节点（relay:incident 与广播）。
func (s *Service) pushIncident(ctx context.Context, incident saas.Incident, atMs int64) {
	if err := s.relay.SetIncident(ctx, relayctl.Incident{RejectNewConnections: incident.RejectNewConnections, AtMs: atMs}); err != nil {
		s.log.Warn("写中继事故开关失败，下一轮同步再写", obs.Operation("admin.incident"), obs.Error(err))
	}
}

// Start 开始每 5 秒按库同步事故开关（控制面各实例各一个，不依赖 Redis 保活）：启动时先写一遍（清掉过期的键），
// 之后开关变了立刻写，没变也每隔一分钟重写一遍（节点那边的键丢了也能自愈）。答停止函数。
func (s *Service) Start() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.syncIncident(ctx)
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

func (s *Service) syncIncident(ctx context.Context) {
	var (
		last     saas.Incident
		lastAt   int64
		applied  bool
		sinceAck int
	)
	tick := func() {
		incident, at, err := s.store.Read().IncidentAt(ctx, s.tenant())
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("读事故开关失败", obs.Operation("admin.incident"), obs.Error(err))
			}
			return
		}
		var atMs int64
		if at != nil {
			atMs = *at
		}
		sinceAck++
		if applied && incident == last && atMs == lastAt && sinceAck < incidentRepushTicks {
			return
		}
		s.pushIncident(ctx, incident, atMs)
		last, lastAt, applied, sinceAck = incident, atMs, true, 0
	}
	tick()
	ticker := time.NewTicker(s.incidentEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}
