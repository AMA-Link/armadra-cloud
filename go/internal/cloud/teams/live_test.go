package teams

import (
	"context"
	"errors"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
)

type failingControl struct{ relayctl.Control }

func (failingControl) TeamConcurrent(context.Context, string) (int, error) {
	return 0, errors.New("redis 不可用")
}

// 没有 Relay（nil Live 或 nil Control）答 0 且不 panic；读失败答 0 并计数；正常时答中继的值。
func TestLiveConcurrent(t *testing.T) {
	ctx := context.Background()
	var nilLive *Live
	if nilLive.Concurrent(ctx, "t") != 0 || nilLive.Failures() != 0 {
		t.Fatal("nil Live 应答 0")
	}
	if NewLive(nil, nil).Concurrent(ctx, "t") != 0 {
		t.Fatal("没有 Relay 应答 0")
	}
	mem := relayctl.NewMemory(nil)
	mem.SetTeamConcurrent("t", 4)
	if got := NewLive(mem, nil).Concurrent(ctx, "t"); got != 4 {
		t.Fatalf("在线数：%d", got)
	}
	bad := NewLive(failingControl{}, nil)
	for i := 0; i < 3; i++ {
		if bad.Concurrent(ctx, "t") != 0 {
			t.Fatal("读失败应答 0")
		}
	}
	if bad.Failures() != 3 {
		t.Fatalf("失败计数：%d", bad.Failures())
	}
}
