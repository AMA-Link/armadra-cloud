package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// streamConn 是一条 me-stream 连接：后台读事件（忽略 ping），按序收进切片。
type streamConn struct {
	t      *testing.T
	conn   *websocket.Conn
	mu     sync.Mutex
	events []map[string]any
	closed chan struct{}
	code   int
}

// ticket 在 r 指向的实例上发票。
func (r *rig) ticket(s session) string {
	r.t.Helper()
	res := r.do("POST", "/v1/me/stream/ticket", map[string]any{}, s.bearer())
	r.expect(res, 200, "")
	return res.json["ticket"].(string)
}

// dialStream 在 r 指向的实例上用票升级。
func (r *rig) dialStream(ticket string) (*streamConn, *http.Response, error) {
	r.t.Helper()
	dialer := websocket.Dialer{Subprotocols: []string{gen.SubprotocolTicket + ticket}, HandshakeTimeout: 3 * time.Second}
	conn, resp, err := dialer.Dial("ws"+strings.TrimPrefix(r.url, "http")+"/v1/me/stream", nil)
	if err != nil {
		return nil, resp, err
	}
	c := &streamConn{t: r.t, conn: conn, closed: make(chan struct{})}
	r.t.Cleanup(func() { _ = conn.Close() })
	go func() {
		defer close(c.closed)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				var closeErr *websocket.CloseError
				if ce, ok := err.(*websocket.CloseError); ok {
					closeErr = ce
					c.mu.Lock()
					c.code = closeErr.Code
					c.mu.Unlock()
				}
				return
			}
			var event map[string]any
			if json.Unmarshal(data, &event) != nil || event["type"] == "ping" || event["type"] == "pong" {
				continue
			}
			c.mu.Lock()
			c.events = append(c.events, event)
			c.mu.Unlock()
		}
	}()
	return c, resp, nil
}

func (r *rig) stream(s session) *streamConn {
	r.t.Helper()
	c, _, err := r.dialStream(r.ticket(s))
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// types 是收到的事件 type 序列。
func (c *streamConn) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.events))
	for _, e := range c.events {
		out = append(out, e["type"].(string))
	}
	return out
}

// waitFor 等到出现符合的事件，答它；超时失败。
func (c *streamConn) waitFor(what string, match func(map[string]any) bool) map[string]any {
	c.t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for {
		c.mu.Lock()
		snapshot := append([]map[string]any(nil), c.events...)
		c.mu.Unlock()
		for _, e := range snapshot {
			if match(e) {
				return e
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("等不到事件：%s（收到 %v）", what, c.types())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isType(typ string) func(map[string]any) bool {
	return func(e map[string]any) bool { return e["type"] == typ }
}

// quiet 在 d 内不该再出现符合的事件。
func (c *streamConn) quiet(d time.Duration, match func(map[string]any) bool, what string) {
	c.t.Helper()
	time.Sleep(d)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if match(e) {
			c.t.Fatalf("不该收到：%s（%v）", what, e)
		}
	}
}

// 票一次性、30 秒内有效，任一实例发的票在任一实例兑换；兑换要设备仍有效；没票或坏票被拒。
func TestMeStreamTicketIsSharedAcrossInstances(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	peer := r.at(r.startPeer().URL)
	s := r.verified()

	ticket := r.ticket(s)
	// 发在实例 1、兑换在实例 2。
	conn, _, err := peer.dialStream(ticket)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn
	if _, resp, err := r.dialStream(ticket); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("票只能用一次：%v %v", resp, err)
	}
	if _, resp, err := r.dialStream("not-a-ticket"); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("坏票：%v %v", resp, err)
	}
	// 过期（Redis 里 30 秒后消失）：直接删掉等价。
	expired := peer.ticket(s)
	rc := r.redisClient()
	iter := rc.Scan(context.Background(), 0, r.deps.TicketPrefix+"*", 100).Iterator()
	for iter.Next(context.Background()) {
		rc.Del(context.Background(), iter.Val())
	}
	if _, resp, err := r.dialStream(expired); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("过期的票：%v %v", resp, err)
	}
	// 设备失效之后票作废。
	s2 := r.verified()
	ticket2 := r.ticket(s2)
	r.expect(r.do("POST", "/v1/auth/logout", map[string]any{}, s2.bearer()), 200, "")
	if _, resp, err := peer.dialStream(ticket2); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("设备已登出：%v %v", resp, err)
	}
	// 票的哈希进 Redis、票本身不进。
	t3 := r.ticket(s)
	keys, _ := rc.Keys(context.Background(), r.deps.TicketPrefix+"*").Result()
	for _, key := range keys {
		if strings.Contains(key, t3) {
			t.Fatal("Redis 的键不能含票明文")
		}
	}
	if len(keys) != 1 {
		t.Fatalf("应有 1 张票：%v", keys)
	}
}

// 两个控制面实例 + Redis：邀请 → 接受 → 角色变化 → 移除，事件按受众到达各实例上的连接；
// 邀请只推给邮箱已验证的受邀账号；无关的账号什么也收不到。
func TestDomainEventsCrossInstancesByAudience(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	peer := r.at(r.startPeer().URL)
	owner, bob, carol, stranger := r.verified(), r.verified(), r.registerBearer(r.email()), r.verified()
	team := r.seedTeam("events", map[string]string{owner.accountID: "owner"})
	bobEmail := r.queryString("SELECT email FROM accounts WHERE account_id = $1", bob.accountID)
	carolEmail := r.queryString("SELECT email FROM accounts WHERE account_id = $1", carol.accountID) // 邮箱没验证

	// 连接分布在两个实例上：owner 在实例 1，bob、carol、stranger 在实例 2。
	ownerStream, bobStream, carolStream, strangerStream := r.stream(owner), peer.stream(bob), peer.stream(carol), peer.stream(stranger)

	// 邀请 bob（已验证）与 carol（未验证）：owner 收到两条，bob 收到自己的，carol 与陌生人什么也收不到。
	res := r.invite(owner, team, bobEmail, "member")
	r.expect(res, 200, "")
	bobInvite := res.json["invitationId"].(string)
	r.expect(r.invite(owner, team, carolEmail, "member"), 200, "")
	ev := bobStream.waitFor("bob 收到邀请", isType("invitationChanged"))
	if ev["invitationId"] != bobInvite || ev["teamId"] != team || ev["state"] != "pending" {
		t.Fatalf("invitationChanged：%v", ev)
	}
	ownerStream.waitFor("owner 收到邀请事件", isType("invitationChanged"))
	carolStream.quiet(300*time.Millisecond, isType("invitationChanged"), "邮箱没验证的账号不推 invitationChanged")
	strangerStream.quiet(0, isType("invitationChanged"), "无关账号")

	// bob 在实例 2 接受：owner（实例 1）收到 memberChanged added 与 invitationChanged accepted，bob 也收到。
	token := r.waitInvite(bobEmail, bobInvite)
	peer.expect(peer.do("POST", invitePath(bobInvite, "accept"), map[string]any{"token": token}, bob.bearer()), 200, "")
	added := ownerStream.waitFor("owner 收到 memberChanged added", func(e map[string]any) bool {
		return e["type"] == "memberChanged" && e["change"] == "added" && e["accountId"] == bob.accountID
	})
	if added["teamId"] != team {
		t.Fatalf("memberChanged：%v", added)
	}
	ownerStream.waitFor("owner 收到 accepted", func(e map[string]any) bool { return e["type"] == "invitationChanged" && e["state"] == "accepted" })
	bobStream.waitFor("bob 收到自己被加入", func(e map[string]any) bool { return e["type"] == "memberChanged" && e["change"] == "added" })

	// 改角色 → 双方都收到 roleChanged；移除 → bob 作为被移除的人仍收到 removed。
	r.expect(r.do("PATCH", "/v1/teams/"+team+"/members/"+bob.accountID, map[string]any{"role": "admin"}, owner.bearer()), 200, "")
	bobStream.waitFor("roleChanged", func(e map[string]any) bool { return e["type"] == "memberChanged" && e["change"] == "roleChanged" })
	r.expect(r.do("DELETE", "/v1/teams/"+team+"/members/"+bob.accountID, nil, owner.bearer()), 200, "")
	bobStream.waitFor("removed", func(e map[string]any) bool { return e["type"] == "memberChanged" && e["change"] == "removed" })
	ownerStream.waitFor("owner 也收到 removed", func(e map[string]any) bool { return e["type"] == "memberChanged" && e["change"] == "removed" })
	strangerStream.quiet(300*time.Millisecond, func(e map[string]any) bool { return e["type"] != "" }, "无关账号什么也收不到")
	carolStream.quiet(0, isType("memberChanged"), "未加入的账号收不到成员事件")
}

// 授予与可见集合：授予 bob 一台主机 → 他的连接（另一个实例上）收到 grantChanged，并开始收到这台主机的上下线；
// 撤销授予后不再收到上下线（可见集合缓存随 grantChanged 重算）；与此无关的账号收不到。
func TestGrantChangeUpdatesVisibleSetAcrossInstances(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{redisRelay: true})
	peer := r.at(r.startPeer().URL)
	node := r.newFakeNode("node-1")
	owner, friend, other := r.verified(), r.verified(), r.verified()
	h := r.addHost(owner, nil, "0.11.0")

	ownerStream, friendStream, otherStream := r.stream(owner), peer.stream(friend), peer.stream(other)
	online := func(at int64) {
		t.Helper()
		if err := node.plane.Presence().Publish(context.Background(), control.PresenceEvent{
			Type: control.PresenceSourceOnline, SourceID: h.id, Node: "node-1", TunnelID: "t", CoreVersion: "0.11.0", AtMs: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	atMs := r.clock.Ms()
	online(atMs)
	ownerStream.waitFor("所有者收到上线", func(e map[string]any) bool { return e["type"] == "sourceOnline" && e["sourceId"] == h.id })
	friendStream.quiet(300*time.Millisecond, isType("sourceOnline"), "没有授予时看不到这台主机")

	g := r.mustGrant(owner, h.id, guestOf(friend.accountID), "viewer", nil)
	ev := friendStream.waitFor("grantChanged", isType("grantChanged"))
	if ev["sourceId"] != h.id || ev["grantId"] != g {
		t.Fatalf("grantChanged：%v", ev)
	}
	online(atMs + 1)
	friendStream.waitFor("授予之后收到上线", func(e map[string]any) bool {
		return e["type"] == "sourceOnline" && e["sourceId"] == h.id && e["sinceMs"] == float64(atMs+1)
	})
	ownerStream.waitFor("所有者也收到 grantChanged", isType("grantChanged"))
	otherStream.quiet(0, func(e map[string]any) bool { return e["type"] == "grantChanged" || e["type"] == "sourceOnline" }, "无关账号")

	// 撤销授予：friend 收到 grantChanged，之后的上线事件看不到。
	r.expect(r.do("DELETE", "/v1/grants/"+g, nil, owner.bearer()), 200, "")
	friendStream.waitFor("撤销后的 grantChanged", func(e map[string]any) bool {
		return e["type"] == "grantChanged" && e["grantId"] == g && countType(friendStream, "grantChanged") >= 2
	})
	before := countType(friendStream, "sourceOnline")
	online(atMs + 2)
	ownerStream.waitFor("所有者收到新的上线", func(e map[string]any) bool { return e["type"] == "sourceOnline" && e["sinceMs"] == float64(atMs+2) })
	time.Sleep(200 * time.Millisecond)
	if after := countType(friendStream, "sourceOnline"); after != before {
		t.Fatalf("撤销后不该再收到这台主机的上线：%d → %d", before, after)
	}
	// 主机撤销经 cloud:events 到达另一个实例的连接：所有者收到 sourceRevoked。
	r.expect(r.do("DELETE", "/v1/sources/"+h.id, nil, owner.bearer()), 200, "")
	ownerStream.waitFor("sourceRevoked", isType("sourceRevoked"))
}

func countType(c *streamConn, typ string) int {
	n := 0
	for _, t := range c.types() {
		if t == typ {
			n++
		}
	}
	return n
}

// 关闭实例时 me-stream 以 1001 关闭连接。
func TestMeStreamClosesWithGoingAwayOnShutdown(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	peerRun := r.startPeer()
	peer := r.at(peerRun.URL)
	s := r.verified()
	c := peer.stream(s)
	peerRun.Close()
	select {
	case <-c.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("实例关闭后连接应被关掉")
	}
	c.mu.Lock()
	code := c.code
	c.mu.Unlock()
	if code != websocket.CloseGoingAway {
		t.Fatalf("关闭码：%d", code)
	}
}

// 漏更新后的兜底：库里的授予被撤了而事件没有处理（这里直接改库，等同于 Prepare / 发布失败），
// 连接的可见集合在一个心跳内收敛，不再收到这台主机的事件。
func TestVisibleSetConvergesOnHeartbeatAfterMissedEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{redisRelay: true, streamPing: 400 * time.Millisecond})
	node := r.newFakeNode("node-1")
	owner, friend := r.verified(), r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	r.mustGrant(owner, h.id, guestOf(friend.accountID), "viewer", nil)
	friendStream := r.stream(friend)
	time.Sleep(300 * time.Millisecond) // 服务端在升级之后才登记连接
	online := func(at int64) {
		t.Helper()
		if err := node.plane.Presence().Publish(context.Background(), control.PresenceEvent{
			Type: control.PresenceSourceOnline, SourceID: h.id, Node: "node-1", TunnelID: "t", CoreVersion: "0.11.0", AtMs: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	atMs := r.clock.Ms()
	online(atMs)
	friendStream.waitFor("授予有效时收到上线", func(e map[string]any) bool { return e["type"] == "sourceOnline" && e["sinceMs"] == float64(atMs) })
	// 绕过事件直接撤授予：可见集合此刻是过期的。
	r.exec("UPDATE source_grants SET revoked_at_ms = 1 WHERE subject_id = $1", friend.accountID)
	online(atMs + 1)
	friendStream.waitFor("过期窗口里仍收到", func(e map[string]any) bool { return e["sinceMs"] == float64(atMs+1) })
	// 一个心跳（加上重算）之后收敛。
	deadline := time.Now().Add(5 * time.Second)
	for n := int64(2); ; n++ {
		online(atMs + n)
		time.Sleep(300 * time.Millisecond)
		if countSince(friendStream, float64(atMs+n)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("可见集合没有在心跳内收敛")
		}
	}
}

func countSince(c *streamConn, sinceMs float64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e["type"] == "sourceOnline" && e["sinceMs"] == sinceMs {
			n++
		}
	}
	return n
}

// 被移除的原邀请人收不到他发出的邀请的事件（他不再是 owner / admin）；级联撤销的邀请发 invitationChanged revoked
// 给仍是管理者的人；恢复团队发 memberChanged 给全体成员。
func TestRemovedInviterGetsNoInvitationEventsAndCascadeAndRestoreEmit(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin := r.verified(), r.verified()
	team := r.seedTeam("inviter", map[string]string{owner.accountID: "owner", admin.accountID: "admin"})
	inviteeEmail := r.email()
	res := r.invite(admin, team, inviteeEmail, "member")
	r.expect(res, 200, "")
	invitationID := res.json["invitationId"].(string)

	ownerStream, adminStream := r.stream(owner), r.stream(admin)
	// 移除 admin：他发出的待处理邀请被级联撤销。
	r.expect(r.do("DELETE", "/v1/teams/"+team+"/members/"+admin.accountID, nil, owner.bearer()), 200, "")
	ev := ownerStream.waitFor("级联撤销的邀请发 revoked", func(e map[string]any) bool {
		return e["type"] == "invitationChanged" && e["state"] == "revoked" && e["invitationId"] == invitationID
	})
	if ev["teamId"] != team {
		t.Fatalf("事件：%v", ev)
	}
	adminStream.waitFor("被移除者收到 removed", func(e map[string]any) bool { return e["type"] == "memberChanged" && e["change"] == "removed" })
	adminStream.quiet(400*time.Millisecond, isType("invitationChanged"), "被移除的原邀请人不该收到邀请事件")

	// 删除再恢复团队：成员收到 memberChanged（恢复不带 accountId）。
	r.expect(r.do("DELETE", "/v1/teams/"+team, nil, owner.bearer()), 200, "")
	ownerStream.waitFor("删除事件", func(e map[string]any) bool {
		return e["type"] == "memberChanged" && e["change"] == "removed" && e["accountId"] == nil
	})
	r.expect(r.do("POST", "/v1/teams/"+team+"/restore", map[string]any{}, owner.bearer()), 200, "")
	ownerStream.waitFor("恢复事件", func(e map[string]any) bool {
		return e["type"] == "memberChanged" && e["change"] == "added" && e["accountId"] == nil
	})
}
