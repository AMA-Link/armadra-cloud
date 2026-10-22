package personal

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/tlsx"
)

func levelsOf(findings []Finding) map[string]Level {
	out := map[string]Level{}
	for _, f := range findings {
		out[f.Item] = f.Level
	}
	return out
}

func sp(s string) *string { return &s }

// sqliteDataDir 是迁完的数据目录（目录 0700）。
func sqliteDataDir(t testing.TB) string {
	t.Helper()
	dir := legacyDataDir(t)
	if _, err := MigrateStore(context.Background(), dir, StoreSQLite, MigrateOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func doctorOptions(dir string) DoctorOptions {
	return DoctorOptions{
		Common: Common{DataDir: dir},
		Listen: ListenAddress{Host: "0.0.0.0", Port: 8443},
		TLS:    "self-signed",
	}
}

var doctorNow = time.UnixMilli(1760000000000)

func writeCA(t testing.TB, dir string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: notAfter.AddDate(-10, 0, 0), NotAfter: notAfter,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, tlsx.TLSDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tlsx.TLSDir, tlsx.CACert), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorCleanInstall(t *testing.T) {
	dir := sqliteDataDir(t)
	findings := Diagnose(context.Background(), doctorOptions(dir), doctorNow)
	for _, f := range findings {
		if f.Level != LevelPass {
			t.Fatalf("缺省配置应全部通过：%+v", f)
		}
	}
	got := levelsOf(findings)
	for _, item := range []string{"数据目录权限", "存储", "存储文件权限", "数据库完整性", "口令哈希", "受信代理", "监听地址"} {
		if got[item] != LevelPass {
			t.Fatalf("%s 应通过：%v", item, got)
		}
	}
}

func TestDoctorOutput(t *testing.T) {
	dir := sqliteDataDir(t)
	var lines []string
	code, err := RunDoctor(context.Background(), doctorOptions(dir), func(l string) { lines = append(lines, l) }, doctorNow)
	if err != nil || code != 0 {
		t.Fatalf("应通过：%d %v", code, err)
	}
	if !strings.HasPrefix(lines[0], "通过  数据目录权限：") {
		t.Fatalf("第一行不对：%q", lines[0])
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "检查完成：通过 ") || !strings.HasSuffix(last, "，警告 0，失败 0") {
		t.Fatalf("汇总不对：%q", last)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "通过  存储：SQLite，"+filepath.Join(dir, DatabaseFile)) {
		t.Fatalf("存储行不对：%q", lines)
	}

	opts := doctorOptions(dir)
	opts.TLS, opts.Listen.Host = "plain", "0.0.0.0"
	lines = nil
	code, _ = RunDoctor(context.Background(), opts, func(l string) { lines = append(lines, l) }, doctorNow)
	if code != 1 || !strings.Contains(strings.Join(lines, "\n"), "失败  监听地址：明文监听了非回环地址，serve 会拒绝启动：0.0.0.0:8443") {
		t.Fatalf("应失败：%d %q", code, lines)
	}
}

func TestDoctorBind(t *testing.T) {
	dir := sqliteDataDir(t)
	cases := []struct {
		host, tls string
		level     Level
		detail    string
	}{
		{"127.0.0.1", "plain", LevelPass, "明文只绑回环（在反向代理之后）：127.0.0.1:8443"},
		{"[::1]", "plain", LevelPass, "明文只绑回环（在反向代理之后）：[::1]:8443"},
		{"localhost", "plain", LevelPass, "明文只绑回环（在反向代理之后）：localhost:8443"},
		{"0.0.0.0", "plain", LevelFail, "明文监听了非回环地址，serve 会拒绝启动：0.0.0.0:8443"},
		{"10.0.0.5", "plain", LevelFail, "明文监听了非回环地址，serve 会拒绝启动：10.0.0.5:8443"},
		{"127.0.0.1", "acme", LevelPass, "只绑回环，TLS acme：127.0.0.1:8443"},
		{"0.0.0.0", "self-signed", LevelPass, "对外监听，TLS self-signed：0.0.0.0:8443"},
	}
	for _, c := range cases {
		o := doctorOptions(dir)
		o.Listen.Host, o.TLS = c.host, c.tls
		for _, f := range Diagnose(context.Background(), o, doctorNow) {
			if f.Item == "监听地址" && (f.Level != c.level || f.Detail != c.detail) {
				t.Errorf("%s/%s：%+v", c.host, c.tls, f)
			}
		}
	}
}

func TestDoctorTrustedProxy(t *testing.T) {
	dir := sqliteDataDir(t) // 存储里是 10.0.0.0/8
	cases := []struct {
		name   string
		tls    string
		flag   *string
		level  Level
		detail string
	}{
		{"存储里的", "self-signed", nil, LevelPass, "1 条"},
		{"清空 + 直连", "self-signed", sp(""), LevelPass, "未配置（直连）"},
		{"清空 + 明文", "plain", sp(""), LevelWarn, "明文模式在反向代理之后却没配受信代理：按 IP 的限流与锁定会合成一个桶"},
		{"IPv4 太宽", "self-signed", sp("0.0.0.0/0, 10.0.0.1"), LevelWarn, "网段太宽，任何人都能伪造客户端 IP：0.0.0.0/0"},
		{"IPv4 /7", "self-signed", sp("10.0.0.0/7"), LevelWarn, "网段太宽，任何人都能伪造客户端 IP：10.0.0.0/7"},
		{"IPv6 太宽", "self-signed", sp("::/0,fd00::/8"), LevelWarn, "网段太宽，任何人都能伪造客户端 IP：::/0, fd00::/8"},
		{"IPv6 /16", "self-signed", sp("2001::/16"), LevelPass, "1 条"},
		{"不是 IP", "self-signed", sp("not-an-ip"), LevelFail, "受信代理不是 IP 或 CIDR：not-an-ip"},
		{"前缀坏", "self-signed", sp("10.0.0.0/40"), LevelFail, "受信代理前缀不合法：10.0.0.0/40"},
	}
	for _, c := range cases {
		o := doctorOptions(dir)
		o.TLS, o.TrustedProxy = c.tls, c.flag
		if c.tls == "plain" {
			o.Listen.Host = "127.0.0.1"
		}
		var found *Finding
		for _, f := range Diagnose(context.Background(), o, doctorNow) {
			if f.Item == "受信代理" {
				found = &f
			}
		}
		if found == nil || found.Level != c.level || found.Detail != c.detail {
			t.Errorf("%s：%+v", c.name, found)
		}
	}
}

func TestDoctorPermissions(t *testing.T) {
	dir := sqliteDataDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(dir, DatabaseFile)
	if err := os.Chmod(database, 0o644); err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Join(dir, tlsx.TLSDir)
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"ca.key": 0o640, "leaf.key": 0o600, "leaf.crt": 0o644} {
		if err := os.WriteFile(filepath.Join(keyDir, name), []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(keyDir, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	acme := filepath.Join(dir, tlsx.ACMEDir)
	if err := os.MkdirAll(acme, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acme, "account-key.pem"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	details := map[string]string{}
	var warns int
	for _, f := range Diagnose(context.Background(), doctorOptions(dir), doctorNow) {
		details[f.Item+"|"+f.Detail] = string(f.Level)
		if f.Level == LevelWarn {
			warns++
		}
	}
	want := map[string]string{
		"数据目录权限|" + dir + "（0755，应为 0700）":                             "warn",
		"存储文件权限|" + database + "（0644，应为 0600）":                        "warn",
		"私钥文件权限|" + filepath.Join(keyDir, "ca.key") + "（0640，应为 0600）": "warn",
		"私钥文件权限|" + filepath.Join(keyDir, "leaf.key") + "（0600）":       "pass",
		"私钥文件权限|" + filepath.Join(acme, "account-key.pem") + "（0600）":  "pass",
	}
	for k, v := range want {
		if details[k] != v {
			t.Errorf("缺 %s = %s；实际 %v", k, v, details)
		}
	}
	for k := range details {
		if strings.Contains(k, "leaf.crt") {
			t.Errorf("不含 key 的文件不该检查：%s", k)
		}
	}
	if warns != 5 { // 数据目录、relay.db 与随它继承权限的 -wal / -shm、ca.key
		t.Errorf("应有 5 条警告，实际 %d：%v", warns, details)
	}
}

func TestDoctorPasswordFile(t *testing.T) {
	dir := sqliteDataDir(t)
	good := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(good, []byte(compatPassword(t)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(good, 0o644); err != nil {
		t.Fatal(err)
	}
	o := doctorOptions(dir)
	o.PasswordFile = good
	got := map[string]Finding{}
	for _, f := range Diagnose(context.Background(), o, doctorNow) {
		got[f.Item] = f
	}
	if got["口令文件权限"].Level != LevelWarn || !strings.HasSuffix(got["口令文件权限"].Detail, "（0644，应为 0600）") {
		t.Errorf("口令文件权限：%+v", got["口令文件权限"])
	}
	if f := got["口令文件"]; f.Level != LevelPass || f.Detail != "与当前口令一致，初始化之后不再读取，可以删掉："+good {
		t.Errorf("口令文件：%+v", f)
	}
	if _, ok := got["口令强度"]; ok {
		t.Errorf("口令过策略不该有口令强度：%+v", got["口令强度"])
	}

	// 口令不一致：不报「口令文件」。
	bad := filepath.Join(t.TempDir(), "pw2")
	if err := os.WriteFile(bad, []byte("something-else-entirely-77\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o.PasswordFile = bad
	for _, f := range Diagnose(context.Background(), o, doctorNow) {
		if f.Item == "口令文件" {
			t.Errorf("口令不一致不该报口令文件：%+v", f)
		}
	}

	// 不存在、"-"：略过。
	for _, name := range []string{filepath.Join(t.TempDir(), "nope"), "-"} {
		o.PasswordFile = name
		for _, f := range Diagnose(context.Background(), o, doctorNow) {
			if strings.HasPrefix(f.Item, "口令文件") || f.Item == "口令强度" {
				t.Errorf("%s 应略过：%+v", name, f)
			}
		}
	}
}

func TestDoctorLockedAccount(t *testing.T) {
	dir := sqliteDataDir(t)
	ctx := context.Background()
	s, err := OpenStore(ctx, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot(ctx)
	lockedUntil := doctorNow.UnixMilli() + 60_000
	if err := s.Ports().Accounts.Update(ctx, "default", snap.Accounts[0].AccountID, func(a *store.AccountRecord) error {
		a.LockedUntilMs = lockedUntil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	for _, f := range Diagnose(ctx, doctorOptions(dir), doctorNow) {
		if f.Item == "账号锁定" {
			want := "锁定到：" + time.UnixMilli(lockedUntil).UTC().Format("2006-01-02T15:04:05.000Z")
			if f.Level != LevelWarn || f.Detail != want {
				t.Fatalf("账号锁定：%+v", f)
			}
			return
		}
	}
	t.Fatal("应报账号锁定")
}

func TestDoctorStorageFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("数据目录不存在", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "none")
		got := Diagnose(ctx, doctorOptions(dir), doctorNow)
		if len(got) != 1 || got[0] != (Finding{LevelFail, "数据目录", "不存在：" + dir}) {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("只有 state.json", func(t *testing.T) {
		found := levelsOf(Diagnose(ctx, doctorOptions(legacyDataDir(t)), doctorNow))
		if found["存储"] != LevelFail {
			t.Fatalf("应失败：%v", found)
		}
		for _, f := range Diagnose(ctx, doctorOptions(legacyDataDir(t)), doctorNow) {
			if f.Item == "存储" && !strings.Contains(f.Detail, "personal migrate --to sqlite") {
				t.Fatalf("应带迁移提示：%+v", f)
			}
		}
	})

	t.Run("损坏的库：报失败而不是抛错，库不动", func(t *testing.T) {
		dir := t.TempDir()
		junk := []byte(strings.Repeat("definitely not sqlite", 64))
		if err := os.WriteFile(filepath.Join(dir, DatabaseFile), junk, 0o600); err != nil {
			t.Fatal(err)
		}
		found := levelsOf(Diagnose(ctx, doctorOptions(dir), doctorNow))
		if found["存储"] != LevelFail {
			t.Fatalf("应失败：%v", found)
		}
		after, _ := os.ReadFile(filepath.Join(dir, DatabaseFile))
		if string(after) != string(junk) {
			t.Fatal("库被改了")
		}
	})

	t.Run("两种存储同时在", func(t *testing.T) {
		dir := sqliteDataDir(t)
		if err := os.WriteFile(filepath.Join(dir, StateFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if levelsOf(Diagnose(ctx, doctorOptions(dir), doctorNow))["存储"] != LevelFail {
			t.Fatal("应失败")
		}
	})

	t.Run("没有存储时受信代理照样查", func(t *testing.T) {
		o := doctorOptions(t.TempDir())
		o.TLS, o.Listen.Host = "plain", "127.0.0.1"
		if got := levelsOf(Diagnose(ctx, o, doctorNow)); got["受信代理"] != LevelWarn {
			t.Fatalf("%v", got)
		}
	})
}

func TestDoctorCertificate(t *testing.T) {
	cases := []struct {
		name   string
		offset time.Duration
		level  Level
		detail string
	}{
		{"已过期", -time.Hour, LevelFail, "已过期"},
		{"不到一年", 100*day + time.Hour, LevelWarn, "剩余 100 天"},
		{"364 天", 364*day + time.Hour, LevelWarn, "剩余 364 天"},
		{"一年以上", 365*day + time.Hour, LevelPass, "剩余 365 天"},
	}
	for _, c := range cases {
		dir := sqliteDataDir(t)
		writeCA(t, dir, doctorNow.Add(c.offset))
		var found *Finding
		for _, f := range Diagnose(context.Background(), doctorOptions(dir), doctorNow) {
			if f.Item == "自签 CA" {
				found = &f
			}
		}
		if found == nil || found.Level != c.level || found.Detail != c.detail {
			t.Errorf("%s：%+v", c.name, found)
		}
	}

	dir := sqliteDataDir(t)
	if err := os.MkdirAll(filepath.Join(dir, tlsx.TLSDir), 0o700); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, tlsx.TLSDir, tlsx.CACert)
	if err := os.WriteFile(ca, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range Diagnose(context.Background(), doctorOptions(dir), doctorNow) {
		if f.Item == "自签 CA" && (f.Level != LevelFail || f.Detail != "读不出证书："+ca) {
			t.Errorf("坏证书：%+v", f)
		}
	}
}
