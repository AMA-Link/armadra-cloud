// Package quota 是 M1 的额度表与超限错误（设计 §4.10）。额度表写在代码里（Plans），数值取能力设计 §3.10
// 初稿，改数只改代码；entitlements.overrides 可逐项放宽。M1 只有 Free，硬上限答 429 limit_reached，
// details 是协议包 limitReachedDetailsSchema 的 { quota, used, limit }。
package quota

import (
	"encoding/json"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 额度名（协议包 QUOTAS，契约 §21）：limit_reached 的 details.quota 与 teams.usage 的 metric 同一张表。
const (
	Teams      = "teams"
	Hosts      = "hosts"
	Members    = "members"
	Guests     = "guests"
	Concurrent = "concurrent"
	Traffic    = "traffic"
)

// Unlimited 是不限的额度值。
const Unlimited = -1

const gigabyte = int64(1) << 30

// Limits 是一个空间在某个计划下的额度。个人空间用 Teams 与 Hosts，团队空间用后面五项；
// 取 Unlimited 表示不限。TrafficBytes 是每月中继流量。
type Limits struct {
	Teams        int
	Hosts        int
	Members      int
	Guests       int
	Concurrent   int
	TrafficBytes int64
}

// Plan 是一个计划在两种空间里的额度。
type Plan struct {
	Personal Limits
	Team     Limits
}

// Plans 是计划表；M1 只有 free。数值是初稿（设计 §10 Q3），M2 前定。
// Free 团队的 Concurrent 与中继节点缺键时的最严缺省一致（RELAY_TEAM_CONCURRENT 缺省 10）。
var Plans = map[string]Plan{
	"free": {
		Personal: Limits{Teams: 1, Hosts: 2, TrafficBytes: 5 * gigabyte},
		Team:     Limits{Hosts: 3, Members: 3, Guests: 3, Concurrent: 10, TrafficBytes: 5 * gigabyte},
	},
}

// overrides 的键就是额度名；值是整数，-1 表示不限。
type overrides map[string]int64

func parse(raw json.RawMessage) overrides {
	var out overrides
	if len(raw) == 0 || json.Unmarshal(raw, &out) != nil {
		return nil // 坏了的 overrides 当作没有：只会更严，不会放宽
	}
	return out
}

func (o overrides) pick(name string, base int) int {
	if v, ok := o[name]; ok && v >= Unlimited {
		return int(v)
	}
	return base
}

func plan(code string) Plan {
	if p, ok := Plans[code]; ok {
		return p
	}
	return Plans["free"]
}

// ForPersonal 是个人空间的额度；plan 未知按 free。
func ForPersonal(code string, raw json.RawMessage) Limits {
	l, o := plan(code).Personal, parse(raw)
	l.Teams, l.Hosts = o.pick(Teams, l.Teams), o.pick(Hosts, l.Hosts)
	if v, ok := o[Traffic]; ok && v >= Unlimited {
		l.TrafficBytes = v
	}
	return l
}

// ForTeam 是团队空间的额度；plan 未知按 free。
func ForTeam(code string, raw json.RawMessage) Limits {
	l, o := plan(code).Team, parse(raw)
	l.Hosts, l.Members, l.Guests = o.pick(Hosts, l.Hosts), o.pick(Members, l.Members), o.pick(Guests, l.Guests)
	l.Concurrent = o.pick(Concurrent, l.Concurrent)
	if v, ok := o[Traffic]; ok && v >= Unlimited {
		l.TrafficBytes = v
	}
	return l
}

// Reached 报告再加一个是否会越过上限：used 是已有数量，limit 为 Unlimited 时永不越过。
func Reached(used, limit int) bool { return limit != Unlimited && used >= limit }

// Check 在 used 已经到上限时答 429 limit_reached（details = { quota, used, limit }）。
func Check(name string, used, limit int) error {
	if !Reached(used, limit) {
		return nil
	}
	return core.FailWith(gen.ErrLimitReached, map[string]any{"quota": name, "used": used, "limit": limit})
}
