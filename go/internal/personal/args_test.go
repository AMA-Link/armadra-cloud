package personal

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
)

func args(s ...string) []string { return s }

func mustServe(t *testing.T, argv []string, env Env) ServeOptions {
	t.Helper()
	o, err := ParseServe(argv, env, "/w")
	if err != nil {
		t.Fatalf("ParseServe(%v): %v", argv, err)
	}
	return o
}

func wantUsage(t *testing.T, err error, contains string) {
	t.Helper()
	var u *UsageError
	if !errors.As(err, &u) {
		t.Fatalf("want UsageError, got %v", err)
	}
	if !strings.Contains(u.Msg, contains) {
		t.Fatalf("message %q lacks %q", u.Msg, contains)
	}
}

func TestArgsServeRateLimits(t *testing.T) {
	base := args("--data-dir", "d", "--host", "127.0.0.1")
	if mustServe(t, base, Env{}).RateLimits != nil {
		t.Fatal("缺省不给")
	}
	got := mustServe(t, append(append([]string{}, base...), "--limit-token", "900"),
		Env{"RELAY_LIMIT_TOKEN": "100", "RELAY_LIMIT_PREFLIGHT": "50"}).RateLimits
	if want := (&RateLimits{Token: 900, Preflight: 50}); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"0", "-1", "1.5", "abc", "", "1000001"} {
		_, err := ParseServe(append(append([]string{}, base...), "--limit-anonymous", bad), Env{}, "/w")
		wantUsage(t, err, "--limit-anonymous 要是 1 到 1000000 的整数：")
	}
	_, err := ParseServe(base, Env{"RELAY_LIMIT_ACCOUNT": ""}, "/w")
	wantUsage(t, err, "--limit-account")
}

func TestArgsServeDefaults(t *testing.T) {
	o := mustServe(t, args("--data-dir", "d", "--host", "127.0.0.1"), Env{})
	if o.DataDir != "/w/d" || o.Host != "127.0.0.1" || o.Port != 8443 || o.TLS != "self-signed" ||
		o.Listen != (ListenAddress{"127.0.0.1", 8443}) || o.LogLevel != "info" ||
		o.Control != "memory" || o.NodeID != "127.0.0.1" || o.TrafficRetentionDays != 90 {
		t.Fatalf("%+v", o)
	}
}

func TestArgsServeListen(t *testing.T) {
	if l := mustServe(t, args("--data-dir", "d", "--host", "203.0.113.5"), Env{}).Listen; l != (ListenAddress{"0.0.0.0", 8443}) {
		t.Fatal(l)
	}
	if l := mustServe(t, args("--data-dir", "d", "--host", "h", "--port", "9000"), Env{"RELAY_LISTEN": "0.0.0.0:8443"}).Listen; l != (ListenAddress{"0.0.0.0", 8443}) {
		t.Fatal(l)
	}
	if l := mustServe(t, args("--data-dir", "d", "--host", "h", "--listen", "[::1]:1"), Env{}).Listen; l != (ListenAddress{"::1", 1}) {
		t.Fatal(l)
	}
	// --listen 给了空串：不回退缺省，直接报不是地址。
	_, err := ParseServe(args("--data-dir", "d", "--host", "h", "--listen", "x"), Env{}, "/w")
	if err == nil || err.Error() != "--listen 不是一个地址：x" {
		t.Fatal(err)
	}
	_, err = ParseServe(args("--data-dir", "d", "--host", "h"), Env{"RELAY_LISTEN": ""}, "/w")
	wantUsage(t, err, "--listen 不是一个地址：")
}

func TestArgsServeEnvFallback(t *testing.T) {
	o := mustServe(t, nil, Env{
		"RELAY_DATA_DIR": "/data", "RELAY_HOST": "relay.example", "RELAY_TLS": "self-signed",
		"RELAY_WEB_ROOT": "/app/web", "RELAY_PERSONAL_ACCOUNT": "me",
		"RELAY_PERSONAL_PASSWORD_FILE": "/run/secrets/pw", "RELAY_PUBLIC_ORIGIN": "https://relay.example/ignored-path",
	})
	if o.DataDir != "/data" || o.Host != "relay.example" || o.WebRoot != "/app/web" || o.Account != "me" ||
		o.PasswordFile != "/run/secrets/pw" || o.PublicOrigin != "https://relay.example" {
		t.Fatalf("%+v", o)
	}
}

func TestArgsServePlain(t *testing.T) {
	if mustServe(t, args("--data-dir", "d", "--host", "127.0.0.1", "--tls", "plain"), Env{}).TLS != "plain" {
		t.Fatal("plain")
	}
	_, err := ParseServe(args("--data-dir", "d", "--host", "relay.example", "--tls", "plain"), Env{}, "/w")
	wantUsage(t, err, "--tls plain 只允许监听回环地址")
	_, err = ParseServe(args("--data-dir", "d", "--host", "127.0.0.1", "--tls", "plain", "--listen", "0.0.0.0:8443"), Env{}, "/w")
	if err == nil || err.Error() != "--tls plain 只允许监听回环地址（给同机的反向代理用）：0.0.0.0" {
		t.Fatal(err)
	}
}

func TestArgsServeTLSFileAndACME(t *testing.T) {
	_, err := ParseServe(args("--data-dir", "d", "--host", "h", "--tls", "file", "--tls-cert", "c"), Env{}, "/w")
	wantUsage(t, err, "--tls-key")
	_, err = ParseServe(args("--data-dir", "d", "--host", "h", "--tls", "acme"), Env{}, "/w")
	wantUsage(t, err, "--acme-email")
	o := mustServe(t, args("--data-dir", "d", "--host", "h", "--tls", "file", "--tls-cert", "c.pem", "--tls-key", "/abs/k.pem"), Env{})
	if o.TLSCert != "/w/c.pem" || o.TLSKey != "/abs/k.pem" {
		t.Fatalf("%+v", o)
	}
	a := mustServe(t, args("--data-dir", "d", "--host", "relay.example", "--tls", "acme", "--acme-email", "ops@example.com"), Env{}).ACME
	if want := (&ACMEArgs{Email: "ops@example.com", HTTPListen: ListenAddress{"0.0.0.0", 80}}); !reflect.DeepEqual(a, want) {
		t.Fatalf("%+v", a)
	}
	a = mustServe(t, args("--data-dir", "d", "--host", "h", "--tls", "acme", "--acme-email", "e", "--acme-directory", "https://d", "--acme-ca-bundle", "ca.pem", "--acme-http-listen", "127.0.0.1:5002"), Env{}).ACME
	if a.Directory != "https://d" || a.CABundle != "/w/ca.pem" || a.HTTPListen != (ListenAddress{"127.0.0.1", 5002}) {
		t.Fatalf("%+v", a)
	}
}

func TestArgsServeErrors(t *testing.T) {
	cases := []struct {
		argv    []string
		mention string
	}{
		{nil, "--data-dir"},
		{args("--data-dir", "d"), "--host"},
		{args("--data-dir", "d", "--host", "h", "--port", "0"), "--port"},
		{args("--data-dir", "d", "--host", "h", "--tls", "none"), "--tls"},
		{args("--data-dir", "d", "--host", "h", "--bogus", "1"), "--bogus"},
		{args("--data-dir"), "--data-dir"},
		{args("stray"), "stray"},
		{args("--data-dir", "d", "--host", "h", "--log-level", "loud"), "--log-level 不合法：loud"},
	}
	for _, c := range cases {
		_, err := ParseServe(c.argv, Env{}, "/w")
		wantUsage(t, err, c.mention)
	}
}

func TestArgsFlagsMessages(t *testing.T) {
	known := FlagSet{"a": true, "b": true}
	_, _, err := ParseFlags(args("--x"), known, FlagOptions{})
	if err == nil || err.Error() != "未知选项：--x" {
		t.Fatal(err)
	}
	_, _, err = ParseFlags(args("--a"), known, FlagOptions{})
	if err == nil || err.Error() != "--a 缺少取值" {
		t.Fatal(err)
	}
	_, _, err = ParseFlags(args("--a", "--b"), known, FlagOptions{})
	if err == nil || err.Error() != "--a 缺少取值" {
		t.Fatal(err)
	}
	_, _, err = ParseFlags(args("stray"), known, FlagOptions{})
	if err == nil || err.Error() != "多余的参数：stray" {
		t.Fatal(err)
	}
	v, pos, err := ParseFlags(args("--a", "-", "p", "--b", "2"), known, FlagOptions{Positional: 1})
	if err != nil || v["a"] != "-" || v["b"] != "2" || !reflect.DeepEqual(pos, []string{"p"}) {
		t.Fatal(v, pos, err)
	}
	v, _, err = ParseFlags(args("--b", "--a", "1"), known, FlagOptions{Booleans: FlagSet{"b": true}})
	if err != nil || v["b"] != "1" || v["a"] != "1" {
		t.Fatal(v, err)
	}
	_, _, err = ParseFlags(args("--"), known, FlagOptions{})
	if err == nil || err.Error() != "未知选项：--" {
		t.Fatal(err)
	}
}

func TestArgsInit(t *testing.T) {
	o, err := ParseInit(args("--data-dir", "d", "--account", "me", "--host", "h", "--password-file", "-"), Env{}, "/work")
	if err != nil {
		t.Fatal(err)
	}
	if o.DataDir != "/work/d" || o.Account != "me" || o.Host != "h" || o.Port != 8443 || o.PasswordFile != "-" || o.TLS != "self-signed" {
		t.Fatalf("%+v", o)
	}
	_, err = ParseInit(args("--data-dir", "d", "--host", "h"), Env{}, "/w")
	wantUsage(t, err, "--account")
	_, err = ParseInit(args("--data-dir", "d", "--host", "h", "--account", "a b"), Env{}, "/w")
	wantUsage(t, err, "--account 只能是")
	_, err = ParseInit(args("--data-dir", "d", "--account", "me"), Env{}, "/w")
	wantUsage(t, err, "缺少 --host：公网 IP 或域名（或 RELAY_HOST）")
	// 环境变量里的空账号：给了就校验，不当作没给。
	_, err = ParseInit(args("--data-dir", "d", "--host", "h"), Env{"RELAY_PERSONAL_ACCOUNT": ""}, "/w")
	wantUsage(t, err, "--account 只能是")
	o, err = ParseInit(args("--data-dir", "d", "--account", "me", "--host", "h", "--password-file", "pw", "--public-origin", "http://h:80/x", "--store", "file"), Env{}, "/w")
	if err != nil || o.PasswordFile != "/w/pw" || o.PublicOrigin != "http://h" || o.Store != StoreFile {
		t.Fatalf("%+v %v", o, err)
	}
	_, err = ParseInit(args("--data-dir", "d", "--account", "me", "--host", "h", "--store", "postgres"), Env{}, "/w")
	wantUsage(t, err, "--store postgres 要给 --database-url")
}

func TestArgsLinks(t *testing.T) {
	o, err := ParseLinks(args("list", "--data-dir", "d"), Env{}, "/w")
	if err != nil || o.Action != "list" || o.DataDir != "/w/d" || o.LinkID != "" {
		t.Fatalf("%+v %v", o, err)
	}
	o, err = ParseLinks(args("revoke", "0123456789abcdef", "--data-dir", "d"), Env{}, "/w")
	if err != nil || o.Action != "revoke" || o.LinkID != "0123456789abcdef" || o.DataDir != "/w/d" {
		t.Fatalf("%+v %v", o, err)
	}
	_, err = ParseLinks(args("revoke", "x", "--data-dir", "d"), Env{}, "/w")
	if err == nil || err.Error() != "links revoke 要一个链接 id：x" {
		t.Fatal(err)
	}
	_, err = ParseLinks(args("revoke", "--data-dir", "d"), Env{}, "/w")
	if err == nil || err.Error() != "links revoke 要一个链接 id：" {
		t.Fatal(err)
	}
	_, err = ParseLinks(args("drop"), Env{}, "/w")
	if err == nil || err.Error() != "links 只有 list 与 revoke：drop" {
		t.Fatal(err)
	}
	_, err = ParseLinks(nil, Env{}, "/w")
	if err == nil || err.Error() != "links 只有 list 与 revoke：" {
		t.Fatal(err)
	}
}

func TestArgsDefaultIssuerAndLoopback(t *testing.T) {
	for _, c := range []struct {
		host string
		port int
		want string
	}{
		{"relay.example", 443, "https://relay.example"},
		{"203.0.113.5", 8443, "https://203.0.113.5:8443"},
		{"2001:db8::1", 8443, "https://[2001:db8::1]:8443"},
		{"[::1]", 8443, "https://[::1]:8443"},
	} {
		if got := DefaultIssuer(c.host, c.port); got != c.want {
			t.Errorf("%s: %s", c.host, got)
		}
	}
	for _, h := range []string{"127.0.0.1", "127.1.2.3", "::1", "[::1]", "localhost"} {
		if !IsLoopback(h) {
			t.Errorf("%s", h)
		}
	}
	for _, h := range []string{"0.0.0.0", "203.0.113.5", "relay.example", "127.0.0.1.evil"} {
		if IsLoopback(h) {
			t.Errorf("%s", h)
		}
	}
}

func TestArgsTrustedProxy(t *testing.T) {
	base := args("--data-dir", "d", "--host", "127.0.0.1")
	if mustServe(t, base, Env{}).TrustedProxy != nil {
		t.Fatal("缺省不给")
	}
	got := mustServe(t, append(append([]string{}, base...), "--trusted-proxy", "127.0.0.1/32, ::1/128", "--trusted-proxy-prefer", "real-ip"),
		Env{"RELAY_TRUSTED_PROXY": "10.0.0.0/8"}).TrustedProxy
	if want := (&edge.TrustedProxyConfig{Proxies: []string{"127.0.0.1/32", "::1/128"}, Prefer: "real-ip"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	got = mustServe(t, base, Env{"RELAY_TRUSTED_PROXY": "10.0.0.0/8"}).TrustedProxy
	if want := (&edge.TrustedProxyConfig{Proxies: []string{"10.0.0.0/8"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	for _, extra := range [][]string{
		{"--trusted-proxy", "nope"},
		{"--trusted-proxy", "10.0.0.0/40"},
		{"--trusted-proxy", "127.0.0.1", "--trusted-proxy-prefer", "x"},
		{"--trusted-proxy-prefer", "real-ip"},
	} {
		_, err := ParseServe(append(append([]string{}, base...), extra...), Env{}, "/w")
		wantUsage(t, err, "--trusted-proxy")
	}
	_, err := ParseServe(append(append([]string{}, base...), "--trusted-proxy", "nope"), Env{}, "/w")
	if err == nil || err.Error() != "--trusted-proxy 受信代理不是 IP 或 CIDR：nope" {
		t.Fatal(err)
	}
}

func TestArgsName(t *testing.T) {
	base := args("--data-dir", "d", "--host", "127.0.0.1")
	if mustServe(t, base, Env{}).Name != "" {
		t.Fatal("缺省不给")
	}
	if n := mustServe(t, append(append([]string{}, base...), "--name", "  家里  "), Env{"RELAY_NAME": "公司"}).Name; n != "家里" {
		t.Fatal(n)
	}
	if n := mustServe(t, base, Env{"RELAY_NAME": "公司"}).Name; n != "公司" {
		t.Fatal(n)
	}
	if n := mustServe(t, base, Env{"RELAY_NAME": ""}).Name; n != "" {
		t.Fatal(n)
	}
	for _, v := range []string{"   ", "", strings.Repeat("x", 129)} {
		_, err := ParseServe(append(append([]string{}, base...), "--name", v), Env{}, "/w")
		wantUsage(t, err, "--name 要是 1 到 128 个字符：")
	}
	if _, err := ParseServe(append(append([]string{}, base...), "--name", strings.Repeat("x", 128)), Env{}, "/w"); err != nil {
		t.Fatal(err)
	}
	// UTF-16 长度：BMP 外字符占两个单元。
	if _, err := ParseServe(append(append([]string{}, base...), "--name", strings.Repeat("\U0001F600", 65)), Env{}, "/w"); err == nil {
		t.Fatal("65 个代理对 = 130 单元，应拒绝")
	}
	if _, err := ParseServe(append(append([]string{}, base...), "--name", strings.Repeat("\U0001F600", 64)), Env{}, "/w"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseServe(append(append([]string{}, base...), "--name", strings.Repeat("家", 128)), Env{}, "/w"); err != nil {
		t.Fatal(err)
	}
}

func TestArgsStore(t *testing.T) {
	serve := args("--data-dir", "d", "--host", "relay.example")
	if mustServe(t, serve, Env{}).Store != "" {
		t.Fatal("缺省空")
	}
	if mustServe(t, serve, Env{"RELAY_STORE": "file"}).Store != StoreFile {
		t.Fatal("env")
	}
	if mustServe(t, append(append([]string{}, serve...), "--store", "sqlite"), Env{"RELAY_STORE": "file"}).Store != StoreSQLite {
		t.Fatal("flag 优先")
	}
	_, err := ParseServe(append(append([]string{}, serve...), "--store", "pg"), Env{}, "/w")
	if err == nil || err.Error() != "--store 只能是 file | sqlite | postgres：pg" {
		t.Fatal(err)
	}
	_, err = ParseServe(serve, Env{"RELAY_STORE": ""}, "/w")
	wantUsage(t, err, "--store 只能是")
	_, err = ParseServe(append(append([]string{}, serve...), "--store", "postgres"), Env{}, "/w")
	wantUsage(t, err, "--store postgres 要给 --database-url")
}

func TestArgsMigrateBackup(t *testing.T) {
	m, err := ParseMigrate(args("--data-dir", "d", "--to", "sqlite"), Env{}, "/w")
	if err != nil || m.DataDir != "/w/d" || m.To != StoreSQLite || m.Database != nil {
		t.Fatalf("%+v %v", m, err)
	}
	_, err = ParseMigrate(args("--data-dir", "d"), Env{}, "/w")
	wantUsage(t, err, "--to")
	_, err = ParseMigrate(args("--data-dir", "d", "--to", "pg"), Env{}, "/w")
	wantUsage(t, err, "--to 只能是")
	_, err = ParseMigrate(args("--data-dir", "d", "--to", "postgres"), Env{}, "/w")
	wantUsage(t, err, "--store postgres 要给")
	m, err = ParseMigrate(args("--data-dir", "d", "--to", "postgres", "--database-url", "postgres://u:p@h/db", "--database-schema", "s1"), Env{}, "/w")
	if err != nil || m.Database == nil || m.Database.Schema != "s1" {
		t.Fatalf("%+v %v", m, err)
	}
	b, err := ParseBackup(args("--data-dir", "d", "--out", "b.db"), Env{}, "/w")
	if err != nil || b.DataDir != "/w/d" || b.Out != "/w/b.db" {
		t.Fatalf("%+v %v", b, err)
	}
	_, err = ParseBackup(args("--data-dir", "d"), Env{}, "/w")
	wantUsage(t, err, "--out")
}

func TestArgsDoctor(t *testing.T) {
	o, err := ParseDoctor(args("--data-dir", "d", "--tls", "plain"), Env{}, "/w")
	want := DoctorOptions{Common: Common{DataDir: "/w/d"}, Listen: ListenAddress{"0.0.0.0", 8443}, TLS: "plain"}
	if err != nil || !reflect.DeepEqual(o, want) {
		t.Fatalf("%+v %v", o, err)
	}
	o, err = ParseDoctor(args("--data-dir", "d"), Env{"RELAY_HOST": "127.0.0.1", "RELAY_TRUSTED_PROXY": "127.0.0.1/32", "RELAY_PERSONAL_PASSWORD_FILE": "/run/pw"}, "/w")
	if err != nil || o.Listen != (ListenAddress{"127.0.0.1", 8443}) || o.TLS != "self-signed" ||
		o.TrustedProxy == nil || *o.TrustedProxy != "127.0.0.1/32" || o.PasswordFile != "/run/pw" {
		t.Fatalf("%+v %v", o, err)
	}
	o, err = ParseDoctor(args("--data-dir", "d", "--password-file", "-"), Env{"RELAY_TRUSTED_PROXY": ""}, "/w")
	if err != nil || o.PasswordFile != "" || o.TrustedProxy == nil || *o.TrustedProxy != "" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestArgsTrafficRetention(t *testing.T) {
	base := args("--data-dir", "d", "--host", "127.0.0.1")
	if mustServe(t, base, Env{}).TrafficRetentionDays != 90 {
		t.Fatal("缺省 90")
	}
	if mustServe(t, base, Env{"RELAY_TRAFFIC_RETENTION_DAYS": "30"}).TrafficRetentionDays != 30 {
		t.Fatal("env")
	}
	if mustServe(t, append(append([]string{}, base...), "--traffic-retention-days", "0"), Env{"RELAY_TRAFFIC_RETENTION_DAYS": "30"}).TrafficRetentionDays != 0 {
		t.Fatal("flag 0")
	}
	if mustServe(t, base, Env{"RELAY_TRAFFIC_RETENTION_DAYS": ""}).TrafficRetentionDays != 90 {
		t.Fatal("空串当没给")
	}
	for _, bad := range []string{"-1", "1.5", "abc", "3651", ""} {
		_, err := ParseServe(append(append([]string{}, base...), "--traffic-retention-days", bad), Env{}, "/w")
		wantUsage(t, err, "--traffic-retention-days 要是 0 到 3650 的整数：")
	}
}

func TestArgsTraffic(t *testing.T) {
	o, err := ParseTraffic(args("--data-dir", "d"), Env{}, "/w")
	if err != nil || o.DataDir != "/w/d" || o.SinceMs != 7*86_400_000 || o.SourceID != "" {
		t.Fatalf("%+v %v", o, err)
	}
	src := strings.Repeat("a", 32)
	o, err = ParseTraffic(args("--data-dir", "d", "--since", "24h", "--source", src), Env{}, "/w")
	if err != nil || o.SinceMs != 86_400_000 || o.SourceID != src {
		t.Fatalf("%+v %v", o, err)
	}
	if v, ok := ParseDuration("30m"); !ok || v != 1_800_000 {
		t.Fatal(v, ok)
	}
	for _, bad := range []string{"0d", "7", "7w", "-1d", "123456d", ""} {
		if _, ok := ParseDuration(bad); ok {
			t.Errorf("%q", bad)
		}
	}
	_, err = ParseTraffic(args("--data-dir", "d", "--since", "week"), Env{}, "/w")
	if err == nil || err.Error() != "--since 要是天、小时或分钟，例如 7d、24h：week" {
		t.Fatal(err)
	}
	_, err = ParseTraffic(args("--data-dir", "d", "--source", "nope"), Env{}, "/w")
	if err == nil || err.Error() != "--source 要是一个源 id：nope" {
		t.Fatal(err)
	}
}

func TestArgsPasswdAndDataDirOnly(t *testing.T) {
	p, err := ParsePasswd(args("--data-dir", "d", "--password-file", "-"), Env{}, "/w")
	if err != nil || p.PasswordFile != "-" || p.DataDir != "/w/d" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = ParsePasswd(args("--data-dir", "d", "--password-file", "pw"), Env{}, "/w")
	if err != nil || p.PasswordFile != "/w/pw" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = ParsePasswd(args("--data-dir", "d"), Env{"RELAY_PERSONAL_PASSWORD_FILE": "x"}, "/w")
	if err != nil || p.PasswordFile != "" {
		t.Fatalf("passwd 不读环境变量：%+v %v", p, err)
	}
	c, err := ParseDataDirOnly(nil, Env{"RELAY_DATA_DIR": "rel/../x/"}, "/w")
	if err != nil || c.DataDir != "/w/x" {
		t.Fatalf("%+v %v", c, err)
	}
	_, err = ParseDataDirOnly(nil, Env{"RELAY_DATA_DIR": ""}, "/w")
	if err == nil || err.Error() != "缺少 --data-dir（或 RELAY_DATA_DIR）" {
		t.Fatal(err)
	}
}

func TestArgsDatabase(t *testing.T) {
	url := "postgres://u:secret@h/db"
	c, err := ParseDataDirOnly(args("--data-dir", "d", "--database-url", url), Env{}, "/w")
	if err != nil || c.Database == nil || c.Database.URL != url || c.Database.Schema != "" {
		t.Fatalf("%+v %v", c, err)
	}
	// 命令行优先于环境变量；环境变量空串当没给。
	c, err = ParseDataDirOnly(args("--data-dir", "d"), Env{"RELAY_DATABASE_URL": "", "RELAY_DATABASE_SCHEMA": ""}, "/w")
	if err != nil || c.Database != nil {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = ParseDataDirOnly(args("--data-dir", "d", "--database-url", url), Env{"RELAY_DATABASE_URL": "postgres://other/x", "RELAY_DATABASE_SCHEMA": "mine"}, "/w")
	if err != nil || c.Database.URL != url || c.Database.Schema != "mine" {
		t.Fatalf("%+v %v", c, err)
	}
	_, err = ParseDataDirOnly(args("--data-dir", "d", "--database-schema", "s"), Env{}, "/w")
	wantUsage(t, err, "--database-schema 要和 --database-url 一起给")
	_, err = ParseDataDirOnly(args("--data-dir", "d", "--database-url", "mysql://secret@x"), Env{}, "/w")
	if err == nil || err.Error() != "--database-url 要是 postgres:// 开头的连接串" || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
	_, err = ParseDataDirOnly(args("--data-dir", "d", "--database-url", url, "--database-schema", "Bad"), Env{}, "/w")
	wantUsage(t, err, "--database-schema 只能是小写字母、数字与下划线：Bad")
	_, err = ParseDataDirOnly(args("--data-dir", "d", "--database-url", ""), Env{}, "/w")
	wantUsage(t, err, "--database-url 要是")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "url"), []byte(" "+url+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = ParseDataDirOnly(args("--data-dir", "d", "--database-url-file", "url"), Env{}, dir)
	if err != nil || c.Database.URL != url {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = ParseDataDirOnly(args("--data-dir", "d"), Env{"RELAY_DATABASE_URL_FILE": "url"}, dir)
	if err != nil || c.Database.URL != url {
		t.Fatalf("%+v %v", c, err)
	}
	_, err = ParseDataDirOnly(args("--data-dir", "d", "--database-url-file", "nope"), Env{}, dir)
	if err == nil || err.Error() != "--database-url-file 读不出来：nope" {
		t.Fatal(err)
	}
	// 连接串优先于文件，文件不读。
	if _, err = ParseDataDirOnly(args("--data-dir", "d", "--database-url", url, "--database-url-file", "nope"), Env{}, dir); err != nil {
		t.Fatal(err)
	}
}

func TestArgsControl(t *testing.T) {
	base := args("--data-dir", "d", "--host", "h")
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	_, err := ParseServe(with("--control", "x"), Env{}, "/w")
	if err == nil || err.Error() != "--control 只能是 memory | redis | postgres：x" {
		t.Fatal(err)
	}
	_, err = ParseServe(with("--control", "redis"), Env{}, "/w")
	wantUsage(t, err, "--control redis 要给 --redis-url")
	_, err = ParseServe(with("--redis-url", "http://x"), Env{}, "/w")
	wantUsage(t, err, "--redis-url 要是 redis://")
	_, err = ParseServe(with("--control", "postgres"), Env{}, "/w")
	wantUsage(t, err, "--control postgres 要给 --database-url")
	_, err = ParseServe(with("--node-id", "a b"), Env{}, "/w")
	wantUsage(t, err, "--node-id 只能是")
	o := mustServe(t, with("--control", "redis", "--redis-url", "rediss://r:6379", "--node-id", "n1"), Env{})
	if o.Control != "redis" || o.RedisURL != "rediss://r:6379" || o.NodeID != "n1" {
		t.Fatalf("%+v", o)
	}
	// 控制面不是 redis 时不带 redis 地址；环境变量空串当没给。
	o = mustServe(t, with("--redis-url", "redis://r"), Env{"RELAY_CONTROL": "", "RELAY_NODE_ID": ""})
	if o.Control != "memory" || o.RedisURL != "" || o.NodeID != "h" {
		t.Fatalf("%+v", o)
	}
	long := strings.Repeat("a", 70)
	if o = mustServe(t, args("--data-dir", "d", "--host", long), Env{}); len(o.NodeID) != 64 {
		t.Fatal(len(o.NodeID))
	}
	o = mustServe(t, with("--control", "postgres", "--database-url", "postgresql://u@h/d"), Env{})
	if o.Control != "postgres" || o.Database == nil {
		t.Fatalf("%+v", o)
	}
}

func TestArgsChallenge(t *testing.T) {
	base := args("--data-dir", "d", "--host", "h")
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	if mustServe(t, base, Env{"RELAY_CHALLENGE": "", "RELAY_TURNSTILE_SITE_KEY": ""}).Challenge != nil {
		t.Fatal("缺省不开")
	}
	_, err := ParseServe(with("--turnstile-site-key", "k"), Env{}, "/w")
	wantUsage(t, err, "要和 --challenge turnstile 一起给")
	_, err = ParseServe(with("--challenge", "x"), Env{}, "/w")
	wantUsage(t, err, "--challenge 只能是 turnstile 或 off：x")
	_, err = ParseServe(with("--challenge", "turnstile", "--turnstile-site-key", " "), Env{}, "/w")
	wantUsage(t, err, "--challenge turnstile 要同时给")
	c := mustServe(t, with("--challenge", "turnstile", "--turnstile-site-key", " key ", "--turnstile-secret-file", "s",
		"--turnstile-hostnames", "Relay.Example, localhost", "--challenge-scope", "auth.login,links.accept"), Env{}).Challenge
	if c == nil || c.SiteKey != "key" || c.SecretFile != "/w/s" || len(c.Scope) != 2 ||
		!reflect.DeepEqual(c.Hostnames, []string{"relay.example", "localhost"}) {
		t.Fatalf("%+v", c)
	}
	c = mustServe(t, base, Env{"RELAY_CHALLENGE": "turnstile", "RELAY_TURNSTILE_SITE_KEY": "k", "RELAY_TURNSTILE_SECRET_FILE": "/s"}).Challenge
	if c == nil || len(c.Scope) != 1 || string(c.Scope[0]) != "auth.login" || len(c.Hostnames) != 0 {
		t.Fatalf("%+v", c)
	}
	_, err = ParseServe(with("--challenge", "turnstile", "--turnstile-site-key", "k", "--turnstile-secret-file", "s", "--challenge-scope", "nope"), Env{}, "/w")
	wantUsage(t, err, "--challenge-scope 只能是 auth.login、links.accept、auth.changePassword，逗号分隔：nope")
	// https 公网地址下主机名不得含本机；无公网地址时放行。
	_, err = ParseServe(with("--challenge", "turnstile", "--turnstile-site-key", "k", "--turnstile-secret-file", "s",
		"--turnstile-hostnames", "localhost", "--public-origin", "https://relay.example"), Env{}, "/w")
	wantUsage(t, err, "--turnstile-hostnames ")
	if _, err = ParseServe(with("--challenge", "turnstile", "--turnstile-site-key", "k", "--turnstile-secret-file", "s",
		"--turnstile-hostnames", "localhost", "--public-origin", "http://relay.example"), Env{}, "/w"); err != nil {
		t.Fatal(err)
	}
}

func TestArgsPublicOrigin(t *testing.T) {
	base := args("--data-dir", "d", "--host", "h")
	cases := map[string]string{
		"https://Relay.Example:8443/a?b#c": "https://relay.example:8443",
		"https://relay.example:443":        "https://relay.example",
		"http://relay.example:80/":         "http://relay.example",
		"http://[::1]:8100":                "http://[::1]:8100",
	}
	for in, want := range cases {
		if got := mustServe(t, append(append([]string{}, base...), "--public-origin", in), Env{}).PublicOrigin; got != want {
			t.Errorf("%s -> %s", in, got)
		}
	}
	for in, msg := range map[string]string{
		"relay.example":    "--public-origin 不是一个地址：relay.example",
		"ftp://relay.test": "--public-origin 只能是 https 或 http：ftp://relay.test",
	} {
		_, err := ParseServe(append(append([]string{}, base...), "--public-origin", in), Env{}, "/w")
		if err == nil || err.Error() != msg {
			t.Errorf("%s: %v", in, err)
		}
	}
	// 环境变量空串当没给。
	if mustServe(t, base, Env{"RELAY_PUBLIC_ORIGIN": ""}).PublicOrigin != "" {
		t.Fatal("空串")
	}
}

func TestArgsMetricsAndEnvPrecedence(t *testing.T) {
	base := args("--data-dir", "d", "--host", "h")
	o := mustServe(t, append(append([]string{}, base...), "--metrics-listen", "127.0.0.1:9100"), Env{"RELAY_METRICS_LISTEN": "0.0.0.0:1", "RELAY_METRICS_TOKEN": "tok"})
	if o.MetricsListen == nil || *o.MetricsListen != (ListenAddress{"127.0.0.1", 9100}) || o.MetricsToken != "tok" {
		t.Fatalf("%+v", o)
	}
	o = mustServe(t, base, Env{"RELAY_METRICS_TOKEN": ""})
	if o.MetricsListen != nil || o.MetricsToken != "" {
		t.Fatalf("%+v", o)
	}
	// 命令行优先于环境变量。
	o = mustServe(t, append(append([]string{}, base...), "--port", "9000", "--tls", "plain", "--host", "127.0.0.1"), Env{"RELAY_PORT": "1", "RELAY_TLS": "acme", "RELAY_HOST": "x"})
	if o.Port != 9000 || o.TLS != "plain" || o.Host != "127.0.0.1" {
		t.Fatalf("%+v", o)
	}
	// 环境变量给了空串的 RELAY_PORT 不回退缺省。
	_, err := ParseServe(base, Env{"RELAY_PORT": ""}, "/w")
	wantUsage(t, err, "--port 不是端口：")
	_, err = ParseServe(base, Env{"RELAY_TLS": ""}, "/w")
	wantUsage(t, err, "--tls 只能是 self-signed | acme | file | plain：")
	_, err = ParseServe(base, Env{"RELAY_LOG_LEVEL": ""}, "/w")
	wantUsage(t, err, "--log-level 不合法：")
	_, err = ParseServe(nil, Env{"RELAY_DATA_DIR": "d", "RELAY_HOST": ""}, "/w")
	wantUsage(t, err, "缺少 --host")
}

func TestArgsPortNumbers(t *testing.T) {
	base := args("--data-dir", "d", "--host", "h")
	for text, want := range map[string]int{"1": 1, "65535": 65535, " 80 ": 80, "0x50": 80, "8e3": 8000} {
		if got := mustServe(t, append(append([]string{}, base...), "--port", text), Env{}).Port; got != want {
			t.Errorf("%q -> %d", text, got)
		}
	}
	for _, bad := range []string{"65536", "-1", "1.5", "abc", "Infinity", "NaN", "", "1_0"} {
		_, err := ParseServe(append(append([]string{}, base...), "--port", bad), Env{}, "/w")
		wantUsage(t, err, "--port 不是端口：")
	}
}

func TestArgsParseListen(t *testing.T) {
	good := map[string]ListenAddress{
		"0.0.0.0:80": {"0.0.0.0", 80}, "[::1]:8100": {"::1", 8100}, "localhost:0": {"localhost", 0}, "h:65535": {"h", 65535},
	}
	for in, want := range good {
		if got, err := ParseListen(in); err != nil || got != want {
			t.Errorf("%s: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "h", "h:", ":80", "h:65536", "h:123456", "a:b:80", "[::1]80", "h:-1"} {
		_, err := ParseListen(bad)
		if err == nil || err.Error() != "不是合法的 host:port："+bad {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestArgsResolvePath(t *testing.T) {
	for _, c := range [][3]string{
		{"/w", "d", "/w/d"}, {"/w", "/abs/../x/", "/x"}, {"/w", "", "/w"}, {"/w/", "./a/./b", "/w/a/b"}, {"/w", "../x", "/x"},
	} {
		if got := resolvePath(c[0], c[1]); got != c[2] {
			t.Errorf("%v: %s", c, got)
		}
	}
}

func TestArgsEnvFromOS(t *testing.T) {
	t.Setenv("RELAY_ARGS_TEST", "")
	env := EnvFromOS()
	if v, ok := env.Get("RELAY_ARGS_TEST"); !ok || v != "" {
		t.Fatal("空串也算给了")
	}
	if _, ok := env.NonEmpty("RELAY_ARGS_TEST"); ok {
		t.Fatal("NonEmpty 空串当没给")
	}
}

func TestUsageText(t *testing.T) {
	if !strings.HasPrefix(Usage, "用法: armadra-relay <命令> [选项]\n") || !strings.Contains(Usage, "\n  dev ") || !strings.Contains(Usage, "\n  saas serve ") {
		t.Fatalf("顶层用法：\n%s", Usage)
	}
	if !strings.HasPrefix(PersonalUsage, "用法: armadra-relay personal <子命令> [选项]\n") ||
		!strings.Contains(PersonalUsage, "sqlite | file | postgres（或 RELAY_STORE）；默认按数据目录里已有的，新装 sqlite（file 只能用 migrate 导入）") ||
		!strings.Contains(PersonalUsage, "--to sqlite|postgres [--database-url URL]") ||
		strings.Contains(PersonalUsage, "sqlite|file|postgres") {
		t.Fatalf("personal 用法：\n%s", PersonalUsage)
	}
}
