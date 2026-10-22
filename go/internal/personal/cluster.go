package personal

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
)

// Cluster 是装配好的控制面：控制面接口、向本节点发命令的入口、关闭与就绪探针。
type Cluster struct {
	Plane control.Plane
	// Command 向本节点发命令（关隧道）；多节点实现负责广播。
	Command func(control.NodeCommand)
	Close   func() error
	// Probes 是 /ready 里多出来的依赖探针（多节点时探外部服务）。
	Probes map[string]obs.Probe
}

// ClusterInput 是创建控制面的输入。
type ClusterInput struct {
	Kind     string
	NodeID   string
	Redis    string
	Database *DatabaseOptions
	Memory   control.MemoryOptions
	OnError  func(err error, operation string)
}

// ClusterFactory 创建多节点（redis / postgres）控制面。G3a 提供实现后接进来。
type ClusterFactory func(ctx context.Context, in ClusterInput) (*Cluster, error)

// clusterFactories 登记 --control 的多节点实现；没有登记的种类拒绝启动。
var clusterFactories = map[string]ClusterFactory{}

// RegisterCluster 登记一种多节点控制面的实现（redis、postgres）。
func RegisterCluster(kind string, f ClusterFactory) { clusterFactories[kind] = f }

// newCluster 按 --control 建控制面：memory 是内核的内存实现；其余走登记的实现。
func newCluster(ctx context.Context, in ClusterInput) (*Cluster, error) {
	if in.Kind == "" || in.Kind == "memory" {
		mem := control.NewMemory(in.Memory)
		return &Cluster{Plane: mem, Command: mem.Command, Close: func() error { return nil }}, nil
	}
	f, ok := clusterFactories[in.Kind]
	if !ok {
		return nil, Statef(Invalid, "--control %s 需要 G3a 的控制面实现，当前版本还没有", in.Kind)
	}
	return f(ctx, in)
}
