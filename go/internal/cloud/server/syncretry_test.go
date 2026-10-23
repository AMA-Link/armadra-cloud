package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
)

type fakeSync struct {
	errs  []error
	calls int
}

func (f *fakeSync) Run(context.Context) error {
	f.calls++
	if f.calls > len(f.errs) {
		return nil
	}
	return f.errs[f.calls-1]
}

func TestRunSyncWithRetry(t *testing.T) {
	unstable := relayctl.ErrQuotaUnstable
	cases := []struct {
		name  string
		errs  []error
		calls int
		fail  bool
	}{
		{"一次成功", nil, 1, false},
		{"交错两次后成功", []error{unstable, unstable}, 3, false},
		{"一直交错：试满 5 次放弃", []error{unstable, unstable, unstable, unstable, unstable, unstable}, 5, true},
		{"别的错误不重试", []error{errors.New("boom")}, 1, true},
	}
	for _, c := range cases {
		f := &fakeSync{errs: c.errs}
		err := runSyncWithRetry(context.Background(), f, obs.Nop(), time.Millisecond)
		if (err != nil) != c.fail || f.calls != c.calls {
			t.Errorf("%s：err=%v 调用 %d 次（应 %d）", c.name, err, f.calls, c.calls)
		}
	}
	// 取消时立即返回。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runSyncWithRetry(ctx, &fakeSync{errs: []error{unstable, unstable}}, obs.Nop(), time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("取消：%v", err)
	}
}
