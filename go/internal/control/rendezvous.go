package control

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
)

// HintWeights 是 heartbeatHint 前三个节点的权重（SaaS 实施设计 §7.3）。
var HintWeights = []int{3, 2, 1}

// RendezvousScore 是节点 node 对源 sourceID 的加权 rendezvous 分数，越小越靠前：
// 取 sha256(sourceID + "\0" + node) 的前 8 字节作 64 位整数，换成 (0, 1) 里的 u，分数 = -ln(u) / weight。
// weight 不大于 0 时按 1 算。控制面（heartbeatHint）与节点共用这一个函数。
func RendezvousScore(sourceID, node string, weight int) float64 {
	sum := sha256.Sum256([]byte(sourceID + "\x00" + node))
	h := binary.BigEndian.Uint64(sum[:8])
	// 取高 53 位作尾数，加半个单位，u 落在开区间 (0, 1)，ln 不会是 0 或负无穷。
	u := (float64(h>>11) + 0.5) / (1 << 53)
	if weight <= 0 {
		weight = 1
	}
	return -math.Log(u) / float64(weight)
}

// RankNodes 按 sourceID 给节点排序（rendezvous 哈希，按 Weight 加权），去掉 Draining 的，
// 答前 len(HintWeights) 个，Weight 依次改成 3、2、1。同一个源在节点集合不变时总是同一个顺序；
// 加减一个节点只影响落在它上面的源。
func RankNodes(sourceID string, nodes []NodeInfo) []NodeInfo {
	type scored struct {
		node  NodeInfo
		score float64
	}
	list := make([]scored, 0, len(nodes))
	for _, n := range nodes {
		if n.Draining || n.ID == "" {
			continue
		}
		list = append(list, scored{node: n, score: RendezvousScore(sourceID, n.ID, n.Weight)})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score < list[j].score
		}
		return list[i].node.ID < list[j].node.ID
	})
	out := make([]NodeInfo, 0, min(len(list), len(HintWeights)))
	for i := 0; i < len(list) && i < len(HintWeights); i++ {
		n := list[i].node
		n.Weight = HintWeights[i]
		out = append(out, n)
	}
	return out
}
