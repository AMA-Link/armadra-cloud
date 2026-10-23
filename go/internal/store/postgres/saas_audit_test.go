package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

// auditSeed 写一批审计行：T1、T2 各有团队审计，a1 有安全日志，另有一条运营对账号的动作（没有团队）。
func auditSeed(t *testing.T, e *saasEnv, base int64) {
	t.Helper()
	if _, err := e.db.EnsureAuditPartitions(e.ctx, time.UnixMilli(base)); err != nil {
		t.Fatal(err)
	}
	rows := []saas.AuditEntry{
		{AtMs: base + 1, EventID: "e01", TeamID: "t1", ActorKind: saas.ActorAccount, ActorID: "a1", Action: "member.add", SourceID: "s1"},
		{AtMs: base + 2, EventID: "e02", TeamID: "t1", ActorKind: saas.ActorAccount, ActorID: "a2", Action: "grant.create", SourceID: "s2"},
		{AtMs: base + 3, EventID: "e03", TeamID: "t1", ActorKind: saas.ActorSystem, Action: "grant.expire", TargetKind: "grant", TargetID: "g1"},
		{AtMs: base + 3, EventID: "e04", TeamID: "t1", ActorKind: saas.ActorAccount, ActorID: "a1", Action: "assertion.issue", JTI: "j1", IPPrefix: "10.0.0.0/24"},
		{AtMs: base + 4, EventID: "e05", TeamID: "t2", ActorKind: saas.ActorAccount, ActorID: "a1", Action: "member.add"},
		{AtMs: base + 5, EventID: "e06", AccountID: "a1", ActorKind: saas.ActorAccount, ActorID: "a1", Action: "security.login", DeviceName: "笔记本", IPPrefix: "10.0.0.0/24"},
		{AtMs: base + 6, EventID: "e07", AccountID: "a2", ActorKind: saas.ActorAccount, ActorID: "a2", Action: "security.login"},
		{AtMs: base + 7, EventID: "e08", ActorKind: saas.ActorOperator, ActorID: "op", Action: "account.disable", TargetKind: "account", TargetID: "a1", Reason: "违规"},
	}
	err := e.db.SaaS().Tx(e.ctx, func(tx saas.Tx) error {
		for _, row := range rows {
			if err := tx.Audit().Append(e.ctx, saasTenant, row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func ids(rows []saas.AuditEntry) string {
	out := ""
	for _, row := range rows {
		out += row.EventID + ","
	}
	return out
}

func TestAuditListTeamFiltersAndCursor(t *testing.T) {
	e := openSaaS(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	auditSeed(t, e, base)
	s := e.db.SaaS()
	t1 := teamscope.Issue(saasTenant, "t1", teamscope.Admin)
	list := func(q saas.AuditQuery) string {
		t.Helper()
		var rows []saas.AuditEntry
		err := s.Tx(e.ctx, func(tx saas.Tx) (err error) {
			rows, err = tx.Audit().ListTeam(e.ctx, t1, q)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return ids(rows)
	}
	// 倒序，同一时刻按 event_id 倒序；只含 T1 的行（没有 T2、没有安全日志、没有运营动作）。
	if got := list(saas.AuditQuery{}); got != "e04,e03,e02,e01," {
		t.Fatalf("T1 的审计：%s", got)
	}
	if got := list(saas.AuditQuery{FromMs: base + 2, ToMs: base + 3}); got != "e02," {
		t.Fatalf("时间范围含起不含止：%s", got)
	}
	if got := list(saas.AuditQuery{Actor: "a1"}); got != "e04,e01," {
		t.Fatalf("按主体：%s", got)
	}
	if got := list(saas.AuditQuery{SourceID: "s2"}); got != "e02," {
		t.Fatalf("按主机：%s", got)
	}
	if got := list(saas.AuditQuery{ActionPrefix: "grant."}); got != "e03,e02," {
		t.Fatalf("按动作前缀：%s", got)
	}
	// 前缀里的 % 与 _ 是字面量。
	if got := list(saas.AuditQuery{ActionPrefix: "%"}); got != "" {
		t.Fatalf("通配符要转义：%s", got)
	}
	// 游标翻页：每页 2 条，两页取完，不重不漏。
	page1 := list(saas.AuditQuery{Limit: 2})
	if page1 != "e04,e03," {
		t.Fatalf("第一页：%s", page1)
	}
	cursor := saas.AuditCursor{AtMs: base + 3, EventID: "e03"}
	decoded, err := saas.DecodeAuditCursor(cursor.Encode())
	if err != nil || *decoded != cursor {
		t.Fatalf("游标往返：%v %v", decoded, err)
	}
	if got := list(saas.AuditQuery{Limit: 2, After: decoded}); got != "e02,e01," {
		t.Fatalf("第二页：%s", got)
	}
	if _, err := saas.DecodeAuditCursor("%%%"); err == nil {
		t.Fatal("坏游标要拒绝")
	}
}

func TestAuditListSecurityIsOwnerOnly(t *testing.T) {
	e := openSaaS(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	auditSeed(t, e, base)
	s := e.db.SaaS()
	var rows []saas.AuditEntry
	err := s.Tx(e.ctx, func(tx saas.Tx) (err error) {
		rows, err = tx.Audit().ListSecurity(e.ctx, saasTenant, "a1", saas.AuditQuery{})
		return err
	})
	// 只有 a1 本人的 security.*：不含 a2 的、团队审计、运营对 a1 的动作。
	if err != nil || ids(rows) != "e06," || rows[0].DeviceName != "笔记本" || rows[0].IPPrefix != "10.0.0.0/24" {
		t.Fatalf("a1 的安全日志：%s %v", ids(rows), err)
	}
}

func TestRetentionPrunesOnlyTheNamedScope(t *testing.T) {
	e := openSaaS(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	auditSeed(t, e, base)
	s := e.db.SaaS()
	before := base + 4 // 删 e01–e04（T1 全部）、e05 在 base+4 不删
	var n int
	err := s.Tx(e.ctx, func(tx saas.Tx) (err error) {
		teams, err := tx.Retention().TeamsWithAuditBefore(e.ctx, saasTenant, before+10, "", 10)
		if err != nil || fmt.Sprint(teams) != "[t1 t2]" {
			t.Fatalf("有旧行的团队：%v %v", teams, err)
		}
		if next, err := tx.Retention().TeamsWithAuditBefore(e.ctx, saasTenant, before+10, "t1", 10); err != nil || fmt.Sprint(next) != "[t2]" {
			t.Fatalf("翻页从 t1 之后开始：%v %v", next, err)
		}
		n, err = tx.Retention().PruneTeamAudit(e.ctx, saasTenant, "t1", before, 2)
		return err
	})
	if err != nil || n != 2 {
		t.Fatalf("每次最多删 limit 行：%d %v", n, err)
	}
	err = s.Tx(e.ctx, func(tx saas.Tx) (err error) {
		n, err = tx.Retention().PruneTeamAudit(e.ctx, saasTenant, "t1", before, 10)
		return err
	})
	if err != nil || n != 2 {
		t.Fatalf("T1 剩下的：%d %v", n, err)
	}
	if e.count("SELECT count(*)::int FROM audit_log WHERE team_id = 't1'") != 0 || e.count("SELECT count(*)::int FROM audit_log WHERE team_id = 't2'") != 1 {
		t.Fatal("只删 T1 的行，T2 的不动")
	}
	if e.count("SELECT count(*)::int FROM audit_log WHERE team_id IS NULL") != 3 {
		t.Fatal("团队清理不动安全日志与运营动作")
	}
	// 安全日志：只删 security.*。
	err = s.Tx(e.ctx, func(tx saas.Tx) (err error) {
		n, err = tx.Retention().PruneSecurityLog(e.ctx, saasTenant, base+100, 10)
		return err
	})
	if err != nil || n != 2 {
		t.Fatalf("安全日志：%d %v", n, err)
	}
	if e.count("SELECT count(*)::int FROM audit_log WHERE action = 'account.disable'") != 1 || e.count("SELECT count(*)::int FROM audit_log WHERE team_id = 't2'") != 1 {
		t.Fatal("安全日志清理不动团队审计与运营动作")
	}
}

func TestPruneTrafficAndUsageDays(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	e.team("t1", "a1")
	e.source("p1") // a1 的个人主机
	e.source("s1")
	e.must(`INSERT INTO source_spaces (tenant_id, source_id, team_id, owner_account_id, created_at_ms, updated_at_ms) VALUES ($1, 'p1', NULL, 'a1', 1, 1)`, saasTenant)
	e.must(`INSERT INTO source_spaces (tenant_id, source_id, team_id, owner_account_id, created_at_ms, updated_at_ms) VALUES ($1, 's1', 't1', 'a1', 1, 1)`, saasTenant)
	day := int64(24 * 60 * 60 * 1000)
	d0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	put := func(at int64, source, team, kind string, bytes int64) {
		e.must(`INSERT INTO relay_traffic_buckets (tenant_id, bucket_start_ms, node, source_id, subject, kind, team_id, bytes_to_source, bytes_to_client)
		  VALUES ($1, $2, 'n', $3, 'a1', $4, $5, $6, $6)`, saasTenant, at, source, kind, team, bytes)
	}
	put(d0, "s1", "t1", "e2e", 10)
	put(d0+3600000, "s1", "t1", "e2e", 5)
	put(d0+2*day, "s1", "t1", "e2e", 7)
	put(d0, "s1", "t1", "http", 1000) // 只计入口节点的 e2e
	put(d0, "p1", "", "e2e", 3)
	put(d0+day, "p1", "", "e2e", 4)
	put(d0, "p1", "", "http", 999)
	s := e.db.SaaS()
	scope := teamscope.Issue(saasTenant, "t1", teamscope.Owner)
	var team []saas.DayBytes
	err := s.Tx(e.ctx, func(tx saas.Tx) (err error) {
		team, err = tx.Teams().TrafficDays(e.ctx, scope, d0, d0+30*day)
		return err
	})
	if err != nil || fmt.Sprint(team) != fmt.Sprint([]saas.DayBytes{{StartMs: d0, Bytes: 30}, {StartMs: d0 + 2*day, Bytes: 14}}) {
		t.Fatalf("团队按天：%v %v", team, err)
	}
	personal, err := s.Read().PersonalTrafficDays(e.ctx, saasTenant, "a1", d0, d0+30*day)
	if err != nil || fmt.Sprint(personal) != fmt.Sprint([]saas.DayBytes{{StartMs: d0, Bytes: 6}, {StartMs: d0 + day, Bytes: 8}}) {
		t.Fatalf("个人按天：%v %v", personal, err)
	}
	total, err := s.Read().PersonalTraffic(e.ctx, saasTenant, "a1", d0, d0+30*day)
	if err != nil || total != 14 {
		t.Fatalf("个人合计：%d %v", total, err)
	}
	if other, err := s.Read().PersonalTraffic(e.ctx, saasTenant, "nobody", d0, d0+30*day); err != nil || other != 0 {
		t.Fatalf("别人的个人空间读不到：%d %v", other, err)
	}

	// 流量保留期：只删早于界线的，分批。
	var n int
	err = s.Tx(e.ctx, func(tx saas.Tx) (err error) {
		n, err = tx.Retention().PruneTraffic(e.ctx, saasTenant, d0+day, 2)
		return err
	})
	if err != nil || n != 2 {
		t.Fatalf("分批删：%d %v", n, err)
	}
	err = s.Tx(e.ctx, func(tx saas.Tx) (err error) {
		n, err = tx.Retention().PruneTraffic(e.ctx, saasTenant, d0+day, 10)
		return err
	})
	if err != nil || n != 3 || e.count("SELECT count(*)::int FROM relay_traffic_buckets") != 2 {
		t.Fatalf("早于界线的删光，之后的留着：%d %v", n, err)
	}
}

func TestDropAuditPartitionsBefore(t *testing.T) {
	e := openSaaS(t)
	for _, m := range []time.Month{time.January, time.August, time.September, time.October} {
		from := time.Date(2025, m, 1, 0, 0, 0, 0, time.UTC)
		e.must("SELECT audit_ensure_partition($1, $2, $3)", from.UnixMilli(), from.AddDate(0, 1, 0).UnixMilli(), fmt.Sprintf("audit_log_2025_%02d", m))
	}
	// now = 2026-10-23，13 个月前 = 2025-09-23：整月都早于它的只有 1 月与 8 月；9 月分区到 10 月初才满，留着。
	dropped, err := e.db.DropAuditPartitionsBefore(e.ctx, time.Date(2025, 9, 23, 0, 0, 0, 0, time.UTC))
	if err != nil || fmt.Sprint(dropped) != "[audit_log_2025_01 audit_log_2025_08]" {
		t.Fatalf("删的分区：%v %v", dropped, err)
	}
	if e.count("SELECT count(*)::int FROM pg_inherits i WHERE i.inhparent = to_regclass('audit_log')") != 3 {
		t.Fatal("剩 9 月、10 月和兜底分区")
	}
	again, err := e.db.DropAuditPartitionsBefore(e.ctx, time.Date(2025, 9, 23, 0, 0, 0, 0, time.UTC))
	if err != nil || len(again) != 0 {
		t.Fatalf("重复删应幂等：%v %v", again, err)
	}
}
