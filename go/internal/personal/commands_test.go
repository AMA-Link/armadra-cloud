package personal

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/tlsx"
)

type collector struct{ lines []string }

func (c *collector) print(line string) { c.lines = append(c.lines, line) }

func (c *collector) take() []string {
	out := c.lines
	c.lines = nil
	return out
}

func eq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("输出不对：\n%s\n--- 要：\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func passwordFile(t *testing.T, password string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(file, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// TestCommandsEndToEnd：init → status → passwd → rotate-key → links → traffic。
func TestCommandsEndToEnd(t *testing.T) {
	eachStore(t, func(t *testing.T, kind StoreKind, dataDir string, db *DatabaseOptions) {
		ctx := context.Background()
		out := &collector{}
		now := baseTime
		common := Common{DataDir: dataDir, Database: db}

		// init
		err := RunInit(ctx, InitOptions{
			Common: common, Account: "alice", PasswordFile: passwordFile(t, testPassword),
			Host: "127.0.0.1", Port: 8100, TLS: "self-signed", Store: kind,
		}, out.print)
		if err != nil {
			t.Fatal(err)
		}
		lines := out.take()
		label := StoreLabel(kind)
		if len(lines) != 5 || lines[0] != "已初始化："+dataDir || lines[1] != "存储："+label || lines[2] != "账号：alice" ||
			lines[3] != "对外地址：https://127.0.0.1:8100" || !regexp.MustCompile(`^CA 指纹：[0-9a-f]{64}$`).MatchString(lines[4]) {
			t.Fatalf("init 输出 = %q", lines)
		}
		fingerprint, ok := tlsxFingerprint(dataDir)
		if !ok || lines[4] != "CA 指纹："+fingerprint {
			t.Fatalf("指纹不一致：%q vs %q", lines[4], fingerprint)
		}
		// 再来一次：已初始化。
		err = RunInit(ctx, InitOptions{Common: common, Account: "alice", PasswordFile: passwordFile(t, testPassword), Host: "h", Port: 443, Store: kind}, out.print)
		stateErr(t, err, Exists, "已经初始化过")
		eq(t, out.take())

		// 造一个源、一条链接、两行流量。
		var st Store
		st, err = OpenStore(ctx, dataDir, "", db)
		if err != nil {
			t.Fatal(err)
		}
		seen := now.Add(-time.Minute).UnixMilli()
		src := testSource("s1", &seen)
		if _, err := st.Ports().Sources.Put(ctx, store.DefaultTenant, "s1", func(*store.SourceRecord) (store.SourceRecord, error) { return src, nil }); err != nil {
			t.Fatal(err)
		}
		maxUses := int64(3)
		expires := now.Add(24 * time.Hour).UnixMilli()
		link := store.LinkRecord{
			LinkID: "aaaaaaaaaaaaaaaa", SourceID: "s1", InvitationID: "i1", Label: "", Role: "viewer",
			SecretHash: strings.Repeat("a", 64), MaxUses: &maxUses, CreatedAtMs: now.UnixMilli(), ExpiresAtMs: expires,
		}
		if err := st.Ports().Links.Insert(ctx, store.DefaultTenant, link); err != nil {
			t.Fatal(err)
		}
		bucket := now.UnixMilli() - now.UnixMilli()%store.HourMs - 2*store.HourMs
		if err := st.Ports().Traffic.Upsert(ctx, store.DefaultTenant, []store.TrafficRow{
			{BucketStartMs: bucket, Node: "n1", SourceID: "s1", Subject: "acct:alice", Kind: "web", Connections: 2, BytesToSource: 2048, BytesToClient: 1048576},
			{BucketStartMs: bucket + store.HourMs, Node: "n2", SourceID: "s1", Subject: "acct:alice", Kind: "web", Connections: 1, BytesToSource: 0, BytesToClient: 1024},
			{BucketStartMs: bucket, Node: "n1", SourceID: "s1", Subject: "guest:aaaaaaaaaaaaaaaa:ff", DeviceID: "d1", Kind: "api", Connections: 1, BytesToSource: 100, BytesToClient: 200},
		}); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()

		// status
		if err := RunStatus(ctx, dataDir, db, out.print, now); err != nil {
			t.Fatal(err)
		}
		eq(t, out.take(),
			"存储："+label,
			"账号：alice",
			"对外地址：https://127.0.0.1:8100",
			"CA 指纹："+fingerprint,
			"签名钥：k1",
			"源：1",
			"  s1  笔记本-s1  desktop  0.9.0  最近在线 2026-10-10T11:59:00Z",
			"链接：1",
			"设备：0",
		)

		// links list / revoke
		if err := RunLinksList(ctx, dataDir, db, out.print, now); err != nil {
			t.Fatal(err)
		}
		eq(t, out.take(), "  aaaaaaaaaaaaaaaa  有效  —  viewer  0/3  到期 2026-10-11T12:00:00Z  源 s1")
		if err := RunLinksList(ctx, dataDir, db, out.print, now.Add(48*time.Hour)); err != nil {
			t.Fatal(err)
		}
		eq(t, out.take(), "  aaaaaaaaaaaaaaaa  已过期  —  viewer  0/3  到期 2026-10-11T12:00:00Z  源 s1")
		if err := RunLinksRevoke(ctx, dataDir, "aaaaaaaaaaaaaaaa", db, out.print); err != nil {
			t.Fatal(err)
		}
		eq(t, out.take(), "已撤销链接：aaaaaaaaaaaaaaaa")
		if err := RunLinksList(ctx, dataDir, db, out.print, now); err != nil {
			t.Fatal(err)
		}
		eq(t, out.take(), "  aaaaaaaaaaaaaaaa  已撤销  —  viewer  0/3  到期 2026-10-11T12:00:00Z  源 s1")
		err = RunLinksRevoke(ctx, dataDir, "aaaaaaaaaaaaaaaa", db, out.print)
		usageErr(t, err, "没有这条有效链接：aaaaaaaaaaaaaaaa")
		if err := RunStatus(ctx, dataDir, db, out.print, now); err != nil {
			t.Fatal(err)
		}
		if got := out.take(); got[7] != "链接：0" {
			t.Fatalf("撤销后 status = %q", got)
		}

		// passwd：口令不过 → 用法错误；通过 → 新口令生效。
		err = RunPasswd(ctx, dataDir, passwordFile(t, "short"), db, out.print)
		usageErr(t, err, "口令太短：至少 12 个字符")
		newPassword := "another long passphrase 42"
		if err := RunPasswd(ctx, dataDir, passwordFile(t, newPassword), db, out.print); err != nil {
			t.Fatal(err)
		}
		eq(t, out.take(), "口令已更新，账号的全部设备需要重新登录")
		_ = WithStore(ctx, dataDir, db, func(s Store) error {
			snap, err := s.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !identity.VerifyPassword(newPassword, snap.Accounts[0].Password) || identity.VerifyPassword(testPassword, snap.Accounts[0].Password) {
				t.Fatal("口令没换")
			}
			return nil
		})

		// rotate-key
		if err := RunRotateKey(ctx, dataDir, db, out.print); err != nil {
			t.Fatal(err)
		}
		eq(t, out.take(), "已启用新签名钥：k2")
		if err := RunStatus(ctx, dataDir, db, out.print, now); err != nil {
			t.Fatal(err)
		}
		if got := out.take(); !regexp.MustCompile(`^签名钥：k1（退役于 \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ）、k2$`).MatchString(got[4]) {
			t.Fatalf("轮换后签名钥行 = %q", got[4])
		}

		// traffic：合并同一源 × 账号 × 设备 × 类别，按总字节从多到少。
		if err := RunTraffic(ctx, TrafficOptions{Common: common, SinceMs: 7 * 24 * 3_600_000}, out.print, now); err != nil {
			t.Fatal(err)
		}
		eq(t, out.take(),
			"源    账号                       设备  类别  连接  上行     下行",
			"s1    acct:alice                 —    web   3     2.0 KiB  1.0 MiB",
			"s1    guest:aaaaaaaaaaaaaaaa:ff  d1    api   1     100 B    200 B",
			"合计                                         4     2.1 KiB  1.0 MiB",
		)
		if err := RunTraffic(ctx, TrafficOptions{Common: common, SinceMs: 7 * 24 * 3_600_000, SourceID: "none"}, out.print, now); err != nil {
			t.Fatal(err)
		}
		eq(t, out.take(), "没有流量记录")
	})
}

func TestRunCommandsOnEmptyDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	out := &collector{}
	const hint = "还没有初始化：先运行 armadra-relay personal init"
	stateErr(t, RunStatus(ctx, dir, nil, out.print, baseTime), NotInitialized, hint)
	stateErr(t, RunRotateKey(ctx, dir, nil, out.print), NotInitialized, hint)
	stateErr(t, RunLinksList(ctx, dir, nil, out.print, baseTime), NotInitialized, hint)
	stateErr(t, RunLinksRevoke(ctx, dir, "x", nil, out.print), NotInitialized, hint)
	stateErr(t, RunTraffic(ctx, TrafficOptions{Common: Common{DataDir: dir}}, out.print, baseTime), NotInitialized, hint)
	stateErr(t, RunPasswd(ctx, dir, passwordFile(t, testPassword), nil, out.print), NotInitialized, hint)
	eq(t, out.take())
}

func TestRunStatusLockedAndNoLinks(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "d")
	st := createTestStore(t, StoreSQLite, dir, nil)
	locked := baseTime.Add(time.Hour).UnixMilli()
	if err := st.Ports().Accounts.Update(ctx, store.DefaultTenant, "acct:alice", func(a *store.AccountRecord) error {
		a.LockedUntilMs = locked
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	out := &collector{}
	if err := RunStatus(ctx, dir, nil, out.print, baseTime); err != nil {
		t.Fatal(err)
	}
	eq(t, out.take(),
		"存储：SQLite", "账号：alice", "对外地址：https://127.0.0.1:8100", "CA 指纹：—",
		"锁定到：2026-10-10T13:00:00Z", "签名钥：k1", "源：0", "链接：0", "设备：0")
	if err := RunLinksList(ctx, dir, nil, out.print, baseTime); err != nil {
		t.Fatal(err)
	}
	eq(t, out.take(), "没有链接")
	if err := RunTraffic(ctx, TrafficOptions{Common: Common{DataDir: dir}, SinceMs: 1000}, out.print, baseTime); err != nil {
		t.Fatal(err)
	}
	eq(t, out.take(), "没有流量记录")
	// 口令策略仍然是 core 的缺省：改口令包含账号名。
	err := RunPasswd(ctx, dir, passwordFile(t, "alice-and-more-text"), nil, out.print)
	usageErr(t, err, "口令不能包含账号名")
}

func TestFormatBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 1048576: "1.0 MiB",
		5 * 1024 * 1024 * 1024: "5.0 GiB", 3 * 1024 * 1024 * 1024 * 1024: "3.0 TiB", 5000 * 1024 * 1024 * 1024 * 1024: "5000.0 TiB",
	} {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q，要 %q", in, got, want)
		}
	}
}

func TestFormatTableWidth(t *testing.T) {
	lines := formatTable([][]string{{"源", "账号", "x"}, {"abcd", "a", "yy"}, {"合计", "", ""}})
	want := []string{"源    账号  x", "abcd  a     yy", "合计"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", lines)
	}
	if displayWidth("中a") != 3 || displayWidth("—") != 2 {
		t.Fatal("宽度：中文与破折号算两格")
	}
	if len(formatTable(nil)) != 0 {
		t.Fatal("空表")
	}
}

func tlsxFingerprint(dataDir string) (string, bool) { return tlsx.ExistingCAFingerprint(dataDir) }
