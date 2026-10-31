package identity

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// scryptCostOverride 非零时代替协议的 scrypt 成本 N；只有测试进程能设（UseScryptCostForTests）。
var scryptCostOverride atomic.Int64

// scryptCost 是这次派生用的成本 N：缺省取协议值。
func scryptCost() int {
	if n := scryptCostOverride.Load(); n != 0 {
		return int(n)
	}
	return gen.IdentityScryptCost
}

// UseScryptCostForTests 把新派生的口令记录的 scrypt 成本 N 降到 cost（2 的幂），给大量注册 / 登录的集成测试提速：
// 协议成本在 -race 下一次要 0.5 秒左右，整包测试的时间几乎都花在这里。
// 校验按记录里写的成本，所以降成本前后写出的记录都能验；协议成本本身由本包的向量测试守。
// 只许在测试二进制里调用（testing.Testing()），生产进程调用会 panic，生产路径永远走协议成本。
func UseScryptCostForTests(cost int) {
	if !testing.Testing() {
		panic("identity: UseScryptCostForTests 只能在测试里调用")
	}
	if cost < 2 || cost&(cost-1) != 0 {
		panic(fmt.Sprintf("identity: scrypt 成本必须是 2 的幂：%d", cost))
	}
	scryptCostOverride.Store(int64(cost))
}
