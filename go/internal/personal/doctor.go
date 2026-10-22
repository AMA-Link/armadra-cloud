package personal

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/tlsx"
)

// personal doctor（对应 doctor.ts）：只看不改，逐项报「通过 / 警告 / 失败」。
// 输出只有路径、模式位、个数与时间，不打印口令、令牌或私钥。

// Level 是检查结果的级别。
type Level string

// 检查结果的级别。
const (
	LevelPass Level = "pass"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
)

var levelLabels = map[Level]string{LevelPass: "通过", LevelWarn: "警告", LevelFail: "失败"}

// Finding 是一项检查结果。
type Finding struct {
	Level  Level
	Item   string
	Detail string
}

const day = 24 * time.Hour

func octal(mode os.FileMode) string { return fmt.Sprintf("%04o", uint32(mode.Perm())) }

// permission：组与其他用户有任何权限位就算松。不存在（或 Windows）答 nil。
func permission(path, item string, expected os.FileMode) *Finding {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	mode := info.Mode()
	if mode.Perm()&0o077 == 0 {
		return &Finding{LevelPass, item, fmt.Sprintf("%s（%s）", path, octal(mode))}
	}
	return &Finding{LevelWarn, item, fmt.Sprintf("%s（%s，应为 %s）", path, octal(mode), octal(expected))}
}

func keyFiles(dataDir string) []string {
	var out []string
	for _, dir := range []string{tlsx.TLSDir, tlsx.ACMEDir} {
		base := filepath.Join(dataDir, dir)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "key") {
				out = append(out, filepath.Join(base, entry.Name()))
			}
		}
	}
	return out
}

func bindFinding(o DoctorOptions) Finding {
	address := listenString(o.Listen)
	loopback := IsLoopback(o.Listen.Host)
	if o.TLS == "plain" {
		if loopback {
			return Finding{LevelPass, "监听地址", "明文只绑回环（在反向代理之后）：" + address}
		}
		return Finding{LevelFail, "监听地址", "明文监听了非回环地址，serve 会拒绝启动：" + address}
	}
	scope := "对外监听"
	if loopback {
		scope = "只绑回环"
	}
	return Finding{LevelPass, "监听地址", fmt.Sprintf("%s，TLS %s：%s", scope, o.TLS, address)}
}

// listenString 与 Node 一样不给 IPv6 加方括号：`${host}:${port}`。
func listenString(a ListenAddress) string { return a.Host + ":" + strconv.Itoa(a.Port) }

// tooBroad：前缀太短（IPv4 < /8、IPv6 < /16）等于信任任何人伪造客户端 IP。
func tooBroad(entry string) bool {
	address, prefix, found := strings.Cut(entry, "/")
	if !found {
		return false
	}
	n, err := strconv.Atoi(prefix)
	if err != nil {
		return false
	}
	if strings.Contains(address, ":") {
		return n < 16
	}
	return n < 8
}

func storedProxies(tenant *store.Tenant) []string {
	if tenant == nil || len(tenant.TrustedProxy) == 0 {
		return nil
	}
	var cfg struct {
		Proxies []string `json:"proxies"`
	}
	if json.Unmarshal(tenant.TrustedProxy, &cfg) != nil {
		return nil
	}
	return cfg.Proxies
}

func trustedProxyFinding(o DoctorOptions, tenant *store.Tenant) Finding {
	var proxies []string
	if o.TrustedProxy != nil {
		proxies = edge.ParseProxyList(*o.TrustedProxy)
	} else {
		proxies = storedProxies(tenant)
	}
	if len(proxies) == 0 {
		if o.TLS == "plain" {
			return Finding{LevelWarn, "受信代理", "明文模式在反向代理之后却没配受信代理：按 IP 的限流与锁定会合成一个桶"}
		}
		return Finding{LevelPass, "受信代理", "未配置（直连）"}
	}
	if _, err := edge.NewClientIP(&edge.TrustedProxyConfig{Proxies: proxies}); err != nil {
		return Finding{LevelFail, "受信代理", err.Error()}
	}
	var broad []string
	for _, entry := range proxies {
		if tooBroad(entry) {
			broad = append(broad, entry)
		}
	}
	if len(broad) > 0 {
		return Finding{LevelWarn, "受信代理", "网段太宽，任何人都能伪造客户端 IP：" + strings.Join(broad, ", ")}
	}
	return Finding{LevelPass, "受信代理", fmt.Sprintf("%d 条", len(proxies))}
}

// doctorPasswordText：口令文件末尾的一个换行不算口令。
func doctorPasswordText(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	text := string(data)
	if strings.HasSuffix(text, "\r\n") {
		return text[:len(text)-2], nil
	}
	return strings.TrimSuffix(text, "\n"), nil
}

func passwordFindings(ctx context.Context, o DoctorOptions, s Store, now time.Time) []Finding {
	snap, err := s.Snapshot(ctx)
	if err != nil || len(snap.Accounts) == 0 {
		return []Finding{{LevelFail, "口令哈希", "读不出账号"}}
	}
	account := snap.Accounts[0]
	var out []Finding
	weak := account.Password.Cost < gen.IdentityScryptCost ||
		account.Password.Block < gen.IdentityScryptBlock ||
		account.Password.Length < gen.IdentityScryptLength
	if weak {
		out = append(out, Finding{LevelWarn, "口令哈希", "参数低于当前缺省：用 personal passwd 重设一次"})
	} else {
		out = append(out, Finding{LevelPass, "口令哈希", fmt.Sprintf("scrypt N=%d", account.Password.Cost)})
	}
	if account.LockedUntilMs > now.UnixMilli() {
		out = append(out, Finding{LevelWarn, "账号锁定",
			"锁定到：" + time.UnixMilli(account.LockedUntilMs).UTC().Format("2006-01-02T15:04:05.000Z")})
	}
	file := o.PasswordFile
	if file == "" || file == "-" {
		return out
	}
	if _, err := os.Stat(file); err != nil {
		return out
	}
	if mode := permission(file, "口令文件权限", 0o600); mode != nil {
		out = append(out, *mode)
	}
	text, err := doctorPasswordText(file)
	if err != nil {
		return append(out, Finding{LevelFail, "口令文件", "读不出：" + file})
	}
	rule := identity.Violation(text, []string{account.Name}, nil)
	if rule != "" {
		out = append(out, Finding{LevelWarn, "口令强度", fmt.Sprintf("口令文件里的口令不过策略：%s", rule)})
	}
	if identity.VerifyPassword(text, account.Password) {
		level := LevelPass
		if rule != "" {
			level = LevelWarn
		}
		out = append(out, Finding{level, "口令文件", "与当前口令一致，初始化之后不再读取，可以删掉：" + file})
	}
	return out
}

func certificateFinding(dataDir string, now time.Time) *Finding {
	file := filepath.Join(dataDir, tlsx.TLSDir, tlsx.CACert)
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(data)
	var cert *x509.Certificate
	if block != nil {
		cert, err = x509.ParseCertificate(block.Bytes)
	}
	if block == nil || err != nil {
		return &Finding{LevelFail, "自签 CA", "读不出证书：" + file}
	}
	days := int(math.Floor(float64(cert.NotAfter.Sub(now)) / float64(day)))
	switch {
	case days < 0:
		return &Finding{LevelFail, "自签 CA", "已过期"}
	case days < 365:
		return &Finding{LevelWarn, "自签 CA", fmt.Sprintf("剩余 %d 天", days)}
	}
	return &Finding{LevelPass, "自签 CA", fmt.Sprintf("剩余 %d 天", days)}
}

func integrityFinding(ctx context.Context, path string) Finding {
	d, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		return Finding{LevelFail, "数据库完整性", err.Error()}
	}
	defer func() { _ = d.Close() }()
	lines, err := d.IntegrityCheck(ctx)
	if err != nil {
		return Finding{LevelFail, "数据库完整性", err.Error()}
	}
	if len(lines) == 1 && lines[0] == "ok" {
		return Finding{LevelPass, "数据库完整性", "ok"}
	}
	return Finding{LevelFail, "数据库完整性", fmt.Sprintf("%d 处问题", len(lines))}
}

func kindLabel(kind StoreKind) string {
	switch kind {
	case StoreFile:
		return "状态文件"
	case StoreSQLite:
		return "SQLite"
	default:
		return "PostgreSQL"
	}
}

// openForDoctor 打开存储；数据目录里只有 state.json 的老部署报失败并提示迁移。
func openForDoctor(ctx context.Context, o DoctorOptions) (Store, error) {
	_, legacy, err := ResolveKind(o.DataDir, "")
	if err != nil {
		return nil, err
	}
	if legacy {
		return nil, Statef(Invalid, "数据目录里是状态文件，Go 版不再读写它：先运行 armadra-relay personal migrate --to sqlite")
	}
	return OpenStore(ctx, o.DataDir, "", o.Database)
}

// Diagnose 逐项检查；now 用于锁定与证书有效期。
func Diagnose(ctx context.Context, o DoctorOptions, now time.Time) []Finding {
	if _, err := os.Stat(o.DataDir); err != nil {
		return []Finding{{LevelFail, "数据目录", "不存在：" + o.DataDir}}
	}
	var findings []Finding
	add := func(f *Finding) {
		if f != nil {
			findings = append(findings, *f)
		}
	}
	add(permission(o.DataDir, "数据目录权限", 0o700))

	s, err := openForDoctor(ctx, o)
	if err != nil {
		add(&Finding{LevelFail, "存储", err.Error()})
	}
	if s != nil {
		func() {
			defer func() { _ = s.Close() }()
			add(&Finding{LevelPass, "存储", fmt.Sprintf("%s，%s", kindLabel(s.Kind()), s.Path())})
			if s.Kind() == StoreSQLite {
				for _, suffix := range []string{"", "-wal", "-shm"} {
					add(permission(s.Path()+suffix, "存储文件权限", 0o600))
				}
				add(ptr(integrityFinding(ctx, s.Path())))
			}
			findings = append(findings, passwordFindings(ctx, o, s, now)...)
			tenant := s.Tenant()
			add(ptr(trustedProxyFinding(o, &tenant)))
		}()
	} else {
		add(ptr(trustedProxyFinding(o, nil)))
	}
	for _, file := range keyFiles(o.DataDir) {
		add(permission(file, "私钥文件权限", 0o600))
	}
	add(ptr(bindFinding(o)))
	add(certificateFinding(o.DataDir, now))
	return findings
}

func ptr(f Finding) *Finding { return &f }

// RunDoctor 打印并答退出码（有失败为 1）。
func RunDoctor(ctx context.Context, o DoctorOptions, out Print, now time.Time) (int, error) {
	findings := Diagnose(ctx, o, now)
	count := map[Level]int{}
	for _, f := range findings {
		count[f.Level]++
		out(fmt.Sprintf("%s  %s：%s", levelLabels[f.Level], f.Item, f.Detail))
	}
	out(fmt.Sprintf("检查完成：通过 %d，警告 %d，失败 %d", count[LevelPass], count[LevelWarn], count[LevelFail]))
	if count[LevelFail] > 0 {
		return 1, nil
	}
	return 0, nil
}
