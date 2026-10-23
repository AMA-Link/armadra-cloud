package personal

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
)

// 参数解析：命令行优先，其次环境变量（RELAY_*），最后缺省值。对应 已删除的 TypeScript 版 apps/relay/src/personal/args.ts。

// TLS 模式。
const (
	TLSSelfSigned = "self-signed"
	TLSACME       = "acme"
	TLSFile       = "file"
	TLSPlain      = "plain"
)

// TLSModes 是 --tls 的全部取值（按提示顺序）。
var TLSModes = []string{TLSSelfSigned, TLSACME, TLSFile, TLSPlain}

// StoreKinds 是 --store / --to 的全部取值（按提示顺序）。
var StoreKinds = []StoreKind{StoreFile, StoreSQLite, StorePostgres}

// ControlKinds 是 --control 的全部取值（按提示顺序）。
var ControlKinds = []string{"memory", "redis", "postgres"}

// PlatformNameMax 是服务端名称的最大长度（UTF-16 单元，同 JS 的 length）。
const PlatformNameMax = 128

const trafficRetentionMaxDays = 3650

var (
	// AccountName 是账号名的合法形状。
	AccountName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	schemaName    = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	loopbackV4    = regexp.MustCompile(`^127\.\d+\.\d+\.\d+$`)
	postgresURL   = regexp.MustCompile(`^postgres(?:ql)?://`)
	redisURL      = regexp.MustCompile(`^rediss?://`)
	listenPattern = regexp.MustCompile(`^(?:\[([^\]]+)\]|([^:]+)):(\d{1,5})$`)
	linkIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
	sourceIDRe    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	durationRe    = regexp.MustCompile(`^(\d{1,5})([dhm])$`)
	digitsRe      = regexp.MustCompile(`^\d+$`)
	decimalRe     = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	prefixedRe    = regexp.MustCompile(`^0[xXoObB][0-9a-zA-Z]+$`)
)

// Env 是环境变量表。键的有无有意义：TS 的 `env.X ?? d`（给了空串也算给了）与
// `env.X || undefined`（空串当没给）在这里分别是 Get 与 NonEmpty。
type Env map[string]string

// EnvFromOS 取进程的环境变量。
func EnvFromOS() Env {
	env := Env{}
	for _, item := range os.Environ() {
		if key, value, ok := strings.Cut(item, "="); ok && key != "" {
			env[key] = value
		}
	}
	return env
}

// Get 是 `env.X`：键在就算给了（含空串）。
func (e Env) Get(key string) (string, bool) {
	v, ok := e[key]
	return v, ok
}

// NonEmpty 是 `env.X || undefined`：空串当没给。
func (e Env) NonEmpty(key string) (string, bool) {
	v := e[key]
	return v, v != ""
}

// Values 是解析出的 `--name value` 对。
type Values map[string]string

// pick 是 `values.get(flag) ?? env.X`。
func (v Values) pick(flag string, env Env, variable string) (string, bool) {
	if s, ok := v[flag]; ok {
		return s, true
	}
	return env.Get(variable)
}

// pickNonEmpty 是 `values.get(flag) ?? (env.X || undefined)`。
func (v Values) pickNonEmpty(flag string, env Env, variable string) (string, bool) {
	if s, ok := v[flag]; ok {
		return s, true
	}
	return env.NonEmpty(variable)
}

// FlagSet 是认识的选项名（不带 --）。
type FlagSet map[string]bool

// FlagOptions 是 ParseFlags 的附加参数。
type FlagOptions struct {
	// Positional 是收几个不带 -- 的参数（links revoke <id>）。
	Positional int
	// Booleans 是不带取值的选项（出现即为 "1"）。
	Booleans FlagSet
}

// ParseFlags 解 `--name value` 对。
func ParseFlags(argv []string, known FlagSet, options FlagOptions) (Values, []string, error) {
	values := Values{}
	var positional []string
	for i := 0; i < len(argv); i++ {
		flag := argv[i]
		if !strings.HasPrefix(flag, "--") {
			if len(positional) >= options.Positional {
				return nil, nil, Usagef("多余的参数：%s", flag)
			}
			positional = append(positional, flag)
			continue
		}
		name := flag[2:]
		if !known[name] {
			return nil, nil, Usagef("未知选项：%s", flag)
		}
		if options.Booleans[name] {
			values[name] = "1"
			continue
		}
		if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
			return nil, nil, Usagef("%s 缺少取值", flag)
		}
		values[name] = argv[i+1]
		i++
	}
	return values, positional, nil
}

// jsNumber 近似 JS 的 Number(text)；不是有限数答 ok=false（调用方都要求整数，NaN / Infinity 都不合格）。
func jsNumber(text string) (float64, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return 0, true
	}
	if prefixedRe.MatchString(t) {
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[t[1]|0x20]
		n, err := strconv.ParseUint(t[2:], base, 64)
		if err != nil {
			return 0, false
		}
		return float64(n), true
	}
	if !decimalRe.MatchString(t) {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// jsInteger 是 Number.isInteger(Number(text)) 为真时的值。
func jsInteger(text string) (int, bool) {
	f, ok := jsNumber(text)
	if !ok || f != float64(int64(f)) || f > 1e15 || f < -1e15 {
		return 0, false
	}
	return int(f), true
}

// ParseListen 解 `host:port`，IPv6 写成 `[::1]:8100`。
func ParseListen(text string) (ListenAddress, error) {
	m := listenPattern.FindStringSubmatch(text)
	if m != nil {
		if port, err := strconv.Atoi(m[3]); err == nil && port <= 65535 {
			host := m[1]
			if host == "" {
				host = m[2]
			}
			return ListenAddress{Host: host, Port: port}, nil
		}
	}
	return ListenAddress{}, Usagef("不是合法的 host:port：%s", text)
}

func listenOf(text, flag string) (ListenAddress, error) {
	addr, err := ParseListen(text)
	if err != nil {
		return ListenAddress{}, Usagef("%s 不是一个地址：%s", flag, text)
	}
	return addr, nil
}

func originOf(text string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(text))
	if err != nil || u.Scheme == "" {
		return "", Usagef("--public-origin 不是一个地址：%s", text)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", Usagef("--public-origin 只能是 https 或 http：%s", text)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", Usagef("--public-origin 不是一个地址：%s", text)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

// DefaultIssuer 是 personal 的 issuer：`https://<host>:<port>`（443 时省略端口）。
func DefaultIssuer(host string, port int) string {
	name := host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		name = "[" + host + "]"
	}
	if port == 443 {
		return "https://" + name
	}
	return "https://" + name + ":" + strconv.Itoa(port)
}

// IsLoopback 是 host 是否回环地址。
func IsLoopback(host string) bool {
	literal := host
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		literal = host[1 : len(host)-1]
	}
	return literal == "localhost" || literal == "::1" || loopbackV4.MatchString(literal)
}

// resolvePath 等同 node:path.resolve(cwd, p)。
func resolvePath(cwd, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	return filepath.Clean(p)
}

// ------------------------------------------------------------ 公共

var databaseFlags = []string{"database-url", "database-url-file", "database-schema"}

func withDatabase(flags ...string) FlagSet {
	set := FlagSet{}
	for _, f := range append(append([]string{}, flags...), databaseFlags...) {
		set[f] = true
	}
	return set
}

func dataDirOf(values Values, env Env, cwd string) (string, error) {
	dir, _ := values.pick("data-dir", env, "RELAY_DATA_DIR")
	if dir == "" {
		return "", Usagef("缺少 --data-dir（或 RELAY_DATA_DIR）")
	}
	return resolvePath(cwd, dir), nil
}

// databaseOf 是 --database-url / RELAY_DATABASE_URL，或从文件读；连接串里有口令：错误消息不带它。
func databaseOf(values Values, env Env, cwd string) (*DatabaseOptions, error) {
	url, haveURL := values.pickNonEmpty("database-url", env, "RELAY_DATABASE_URL")
	file, haveFile := values.pickNonEmpty("database-url-file", env, "RELAY_DATABASE_URL_FILE")
	if !haveURL && haveFile {
		data, err := os.ReadFile(resolvePath(cwd, file))
		if err != nil {
			return nil, Usagef("--database-url-file 读不出来：%s", file)
		}
		url, haveURL = strings.TrimSpace(string(data)), true
	}
	schema, haveSchema := values.pickNonEmpty("database-schema", env, "RELAY_DATABASE_SCHEMA")
	if !haveURL {
		if haveSchema {
			return nil, Usagef("--database-schema 要和 --database-url 一起给")
		}
		return nil, nil
	}
	if !postgresURL.MatchString(url) {
		return nil, Usagef("--database-url 要是 postgres:// 开头的连接串")
	}
	if haveSchema && !schemaName.MatchString(schema) {
		return nil, Usagef("--database-schema 只能是小写字母、数字与下划线：%s", schema)
	}
	return &DatabaseOptions{URL: url, Schema: schema}, nil
}

// commonOf 是数据目录 + PostgreSQL 连接（每个子命令都要）。
func commonOf(values Values, env Env, cwd string) (Common, error) {
	database, err := databaseOf(values, env, cwd)
	if err != nil {
		return Common{}, err
	}
	dir, err := dataDirOf(values, env, cwd)
	if err != nil {
		return Common{}, err
	}
	return Common{DataDir: dir, Database: database}, nil
}

func requirePostgres(store StoreKind, database *DatabaseOptions) error {
	if store == StorePostgres && database == nil {
		return Usagef("--store postgres 要给 --database-url（或 RELAY_DATABASE_URL / RELAY_DATABASE_URL_FILE）")
	}
	return nil
}

// accountOf 答账号与是否给了；给了就要合法。
func accountOf(values Values, env Env) (string, bool, error) {
	account, ok := values.pick("account", env, "RELAY_PERSONAL_ACCOUNT")
	if ok && !AccountName.MatchString(account) {
		return "", false, Usagef("--account 只能是字母、数字与 . _ -，1–64 个字符：%s", account)
	}
	return account, ok, nil
}

// passwordFileOf 答口令文件与是否给了；"-" 保持原样，其余相对 cwd 解析。
func passwordFileOf(values Values, env Env, cwd string) (string, bool) {
	file, ok := values.pick("password-file", env, "RELAY_PERSONAL_PASSWORD_FILE")
	if !ok || file == "-" {
		return file, ok
	}
	return resolvePath(cwd, file), true
}

func portOf(values Values, env Env) (int, error) {
	text, ok := values.pick("port", env, "RELAY_PORT")
	if !ok {
		text = "8443"
	}
	port, isInt := jsInteger(text)
	if !isInt || port < 1 || port > 65535 {
		return 0, Usagef("--port 不是端口：%s", text)
	}
	return port, nil
}

func tlsOf(values Values, env Env) (string, error) {
	tls, ok := values.pick("tls", env, "RELAY_TLS")
	if !ok {
		tls = TLSSelfSigned
	}
	for _, mode := range TLSModes {
		if mode == tls {
			return tls, nil
		}
	}
	return "", Usagef("--tls 只能是 %s：%s", strings.Join(TLSModes, " | "), tls)
}

func storeKindNames() string {
	names := make([]string, len(StoreKinds))
	for i, k := range StoreKinds {
		names[i] = string(k)
	}
	return strings.Join(names, " | ")
}

func validStoreKind(text string) bool {
	for _, k := range StoreKinds {
		if string(k) == text {
			return true
		}
	}
	return false
}

// storeOf 是 --store / RELAY_STORE；不给时空（由数据目录里已有的文件决定，新装 sqlite）。
func storeOf(values Values, env Env) (StoreKind, error) {
	text, ok := values.pick("store", env, "RELAY_STORE")
	if !ok {
		return "", nil
	}
	if !validStoreKind(text) {
		return "", Usagef("--store 只能是 %s：%s", storeKindNames(), text)
	}
	return StoreKind(text), nil
}

// ------------------------------------------------------------ init

var initFlags = withDatabase("data-dir", "store", "account", "password-file", "host", "port", "tls", "public-origin")

// ParseInit 解 personal init 的参数。
func ParseInit(argv []string, env Env, cwd string) (InitOptions, error) {
	values, _, err := ParseFlags(argv, initFlags, FlagOptions{})
	if err != nil {
		return InitOptions{}, err
	}
	account, _, err := accountOf(values, env)
	if err != nil {
		return InitOptions{}, err
	}
	if account == "" {
		return InitOptions{}, Usagef("缺少 --account（或 RELAY_PERSONAL_ACCOUNT）")
	}
	host, _ := values.pick("host", env, "RELAY_HOST")
	if host == "" {
		return InitOptions{}, Usagef("缺少 --host：公网 IP 或域名（或 RELAY_HOST）")
	}
	publicText, _ := values.pick("public-origin", env, "RELAY_PUBLIC_ORIGIN")
	passwordFile, _ := passwordFileOf(values, env, cwd)
	store, err := storeOf(values, env)
	if err != nil {
		return InitOptions{}, err
	}
	common, err := commonOf(values, env, cwd)
	if err != nil {
		return InitOptions{}, err
	}
	if err := requirePostgres(store, common.Database); err != nil {
		return InitOptions{}, err
	}
	port, err := portOf(values, env)
	if err != nil {
		return InitOptions{}, err
	}
	tls, err := tlsOf(values, env)
	if err != nil {
		return InitOptions{}, err
	}
	options := InitOptions{Common: common, Account: account, PasswordFile: passwordFile, Host: host, Port: port, TLS: tls, Store: store}
	if publicText != "" {
		if options.PublicOrigin, err = originOf(publicText); err != nil {
			return InitOptions{}, err
		}
	}
	return options, nil
}

// ------------------------------------------------------------ serve

func trafficRetentionOf(values Values, env Env) (int, error) {
	text, ok := values.pickNonEmpty("traffic-retention-days", env, "RELAY_TRAFFIC_RETENTION_DAYS")
	if !ok {
		return DefaultTrafficRetentionDays, nil
	}
	days, isInt := jsInteger(text)
	if !digitsRe.MatchString(text) || !isInt || days > trafficRetentionMaxDays {
		return 0, Usagef("--traffic-retention-days 要是 0 到 %d 的整数：%s", trafficRetentionMaxDays, text)
	}
	return days, nil
}

var rateLimitFlags = []struct {
	flag, variable string
	field          func(*RateLimits) *int
}{
	{"limit-anonymous", "RELAY_LIMIT_ANONYMOUS", func(r *RateLimits) *int { return &r.Anonymous }},
	{"limit-preflight", "RELAY_LIMIT_PREFLIGHT", func(r *RateLimits) *int { return &r.Preflight }},
	{"limit-token", "RELAY_LIMIT_TOKEN", func(r *RateLimits) *int { return &r.Token }},
	{"limit-account", "RELAY_LIMIT_ACCOUNT", func(r *RateLimits) *int { return &r.Account }},
}

func rateLimitsOf(values Values, env Env) (*RateLimits, error) {
	limits := &RateLimits{}
	given := false
	for _, item := range rateLimitFlags {
		text, ok := values.pick(item.flag, env, item.variable)
		if !ok {
			continue
		}
		value, isInt := jsInteger(text)
		if !isInt || value < 1 || value > 1_000_000 {
			return nil, Usagef("--%s 要是 1 到 1000000 的整数：%s", item.flag, text)
		}
		*item.field(limits) = value
		given = true
	}
	if !given {
		return nil, nil
	}
	return limits, nil
}

func trustedProxyOf(values Values, env Env) (*edge.TrustedProxyConfig, error) {
	text, haveText := values.pick("trusted-proxy", env, "RELAY_TRUSTED_PROXY")
	prefer, havePrefer := values.pick("trusted-proxy-prefer", env, "RELAY_TRUSTED_PROXY_PREFER")
	if havePrefer && prefer != "forwarded-for" && prefer != "real-ip" {
		return nil, Usagef("--trusted-proxy-prefer 只能是 forwarded-for 或 real-ip：%s", prefer)
	}
	if !haveText {
		if havePrefer {
			return nil, Usagef("--trusted-proxy-prefer 要和 --trusted-proxy 一起给")
		}
		return nil, nil
	}
	proxies := edge.ParseProxyList(text)
	if _, err := edge.NewClientIP(&edge.TrustedProxyConfig{Proxies: proxies}); err != nil {
		return nil, Usagef("--trusted-proxy %s", err.Error())
	}
	config := &edge.TrustedProxyConfig{Proxies: proxies}
	if prefer != "" {
		config.Prefer = prefer
	}
	return config, nil
}

func challengeOf(values Values, env Env, cwd string) (*ChallengeOptions, error) {
	// 环境变量为空当作没给（compose 缺省把它们传成空串）。
	provider, ok := values.pickNonEmpty("challenge", env, "RELAY_CHALLENGE")
	if !ok {
		provider = "off"
	}
	siteKey, _ := values.pickNonEmpty("turnstile-site-key", env, "RELAY_TURNSTILE_SITE_KEY")
	secretFile, _ := values.pickNonEmpty("turnstile-secret-file", env, "RELAY_TURNSTILE_SECRET_FILE")
	scopeText, haveScope := values.pickNonEmpty("challenge-scope", env, "RELAY_CHALLENGE_SCOPE")
	hostnamesText, _ := values.pickNonEmpty("turnstile-hostnames", env, "RELAY_TURNSTILE_HOSTNAMES")
	if provider == "off" {
		if siteKey != "" || secretFile != "" || scopeText != "" || hostnamesText != "" {
			return nil, Usagef("--turnstile-site-key、--turnstile-secret-file、--turnstile-hostnames 与 --challenge-scope 要和 --challenge turnstile 一起给")
		}
		return nil, nil
	}
	if provider != "turnstile" {
		return nil, Usagef("--challenge 只能是 turnstile 或 off：%s", provider)
	}
	if strings.TrimSpace(siteKey) == "" || secretFile == "" {
		return nil, Usagef("--challenge turnstile 要同时给 --turnstile-site-key 与 --turnstile-secret-file")
	}
	scope := []core.ChallengeScope{core.ScopeLogin}
	if haveScope {
		parsed, err := core.ParseChallengeScope(scopeText)
		if err != nil {
			return nil, Usagef("--challenge-scope 只能是 auth.login、links.accept、auth.changePassword，逗号分隔：%s", scopeText)
		}
		scope = parsed
	}
	publicText, _ := values.pick("public-origin", env, "RELAY_PUBLIC_ORIGIN")
	allowLocal := true
	if publicText != "" {
		origin, err := originOf(publicText)
		if err != nil {
			return nil, err
		}
		allowLocal = !strings.HasPrefix(origin, "https://")
	}
	hostnames, err := core.ParseChallengeHostnames(hostnamesText, allowLocal)
	if err != nil {
		return nil, Usagef("--turnstile-hostnames %s", err.Error())
	}
	return &ChallengeOptions{
		SiteKey:    strings.TrimSpace(siteKey),
		SecretFile: resolvePath(cwd, secretFile),
		Scope:      scope,
		Hostnames:  hostnames,
	}, nil
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func nameOf(values Values, env Env) (string, error) {
	// 环境变量为空当作没给；命令行给了空串则拒绝。
	text, ok := values.pickNonEmpty("name", env, "RELAY_NAME")
	if !ok {
		return "", nil
	}
	name := strings.TrimSpace(text)
	if n := utf16Len(name); n < 1 || n > PlatformNameMax {
		return "", Usagef("--name 要是 1 到 %d 个字符：%s", PlatformNameMax, text)
	}
	return name, nil
}

var serveFlags = withDatabase(
	"data-dir", "store", "control", "redis-url", "node-id", "host", "port", "tls", "listen", "log-level",
	"metrics-listen", "public-origin", "account", "password-file", "web-root", "tls-cert", "tls-key",
	"acme-email", "acme-directory", "acme-ca-bundle", "acme-http-listen",
	"limit-anonymous", "limit-preflight", "limit-token", "limit-account",
	"trusted-proxy", "trusted-proxy-prefer", "name",
	"challenge", "turnstile-site-key", "turnstile-secret-file", "turnstile-hostnames", "challenge-scope",
	"traffic-retention-days",
)

// controlOf 是 --control、--redis-url 与 --node-id。
func controlOf(values Values, env Env, host string, database *DatabaseOptions) (control, redis, nodeID string, err error) {
	control, ok := values.pickNonEmpty("control", env, "RELAY_CONTROL")
	if !ok {
		control = "memory"
	}
	valid := false
	for _, kind := range ControlKinds {
		valid = valid || kind == control
	}
	if !valid {
		return "", "", "", Usagef("--control 只能是 %s：%s", strings.Join(ControlKinds, " | "), control)
	}
	redisText, haveRedis := values.pickNonEmpty("redis-url", env, "RELAY_REDIS_URL")
	if control == "redis" && redisText == "" {
		return "", "", "", Usagef("--control redis 要给 --redis-url（或 RELAY_REDIS_URL）")
	}
	if haveRedis && !redisURL.MatchString(redisText) {
		return "", "", "", Usagef("--redis-url 要是 redis:// 或 rediss:// 开头的地址")
	}
	if control == "postgres" && database == nil {
		return "", "", "", Usagef("--control postgres 要给 --database-url（或 RELAY_DATABASE_URL / RELAY_DATABASE_URL_FILE）")
	}
	given, haveGiven := values.pickNonEmpty("node-id", env, "RELAY_NODE_ID")
	if haveGiven && !nodeIDPattern.MatchString(given) {
		return "", "", "", Usagef("--node-id 只能是字母、数字与 . _ : -，1–64 个字符：%s", given)
	}
	if haveGiven {
		nodeID = given
	} else {
		nodeID = sliceUTF16(host, 64)
	}
	if control == "redis" {
		redis = redisText
	}
	return control, redis, nodeID, nil
}

// sliceUTF16 等同 JS 的 s.slice(0, n)（按 UTF-16 单元）。
func sliceUTF16(s string, n int) string {
	units := utf16.Encode([]rune(s))
	if len(units) <= n {
		return s
	}
	return string(utf16.Decode(units[:n]))
}

// ParseServe 解 personal serve 的参数。
func ParseServe(argv []string, env Env, cwd string) (ServeOptions, error) {
	values, _, err := ParseFlags(argv, serveFlags, FlagOptions{})
	if err != nil {
		return ServeOptions{}, err
	}
	common, err := commonOf(values, env, cwd)
	if err != nil {
		return ServeOptions{}, err
	}
	host, _ := values.pick("host", env, "RELAY_HOST")
	if host == "" {
		return ServeOptions{}, Usagef("缺少 --host：公网 IP 或域名（或 RELAY_HOST）")
	}
	port, err := portOf(values, env)
	if err != nil {
		return ServeOptions{}, err
	}
	tls, err := tlsOf(values, env)
	if err != nil {
		return ServeOptions{}, err
	}
	listenText, ok := values.pick("listen", env, "RELAY_LISTEN")
	if !ok {
		bind := "0.0.0.0"
		if IsLoopback(host) {
			bind = "127.0.0.1"
		}
		listenText = bind + ":" + strconv.Itoa(port)
	}
	listen, err := listenOf(listenText, "--listen")
	if err != nil {
		return ServeOptions{}, err
	}
	if tls == TLSPlain && !IsLoopback(listen.Host) {
		return ServeOptions{}, Usagef("--tls plain 只允许监听回环地址（给同机的反向代理用）：%s", listen.Host)
	}
	logLevel, ok := values.pick("log-level", env, "RELAY_LOG_LEVEL")
	if !ok {
		logLevel = "info"
	}
	switch logLevel {
	case "debug", "info", "warn", "error":
	default:
		return ServeOptions{}, Usagef("--log-level 不合法：%s", logLevel)
	}
	metricsText, _ := values.pick("metrics-listen", env, "RELAY_METRICS_LISTEN")
	publicText, _ := values.pick("public-origin", env, "RELAY_PUBLIC_ORIGIN")
	tlsCert, _ := values.pick("tls-cert", env, "RELAY_TLS_CERT")
	tlsKey, _ := values.pick("tls-key", env, "RELAY_TLS_KEY")
	if tls == TLSFile && (tlsCert == "" || tlsKey == "") {
		return ServeOptions{}, Usagef("--tls file 要同时给 --tls-cert 与 --tls-key")
	}
	var acme *ACMEArgs
	if tls == TLSACME {
		email, _ := values.pick("acme-email", env, "RELAY_ACME_EMAIL")
		if email == "" {
			return ServeOptions{}, Usagef("--tls acme 要给 --acme-email")
		}
		directory, _ := values.pick("acme-directory", env, "RELAY_ACME_DIRECTORY")
		caBundle, _ := values.pick("acme-ca-bundle", env, "RELAY_ACME_CA_BUNDLE")
		httpText, ok := values.pick("acme-http-listen", env, "RELAY_ACME_HTTP_LISTEN")
		if !ok {
			httpText = "0.0.0.0:80"
		}
		httpListen, err := listenOf(httpText, "--acme-http-listen")
		if err != nil {
			return ServeOptions{}, err
		}
		acme = &ACMEArgs{Email: email, Directory: directory, HTTPListen: httpListen}
		if caBundle != "" {
			acme.CABundle = resolvePath(cwd, caBundle)
		}
	}
	account, _, err := accountOf(values, env)
	if err != nil {
		return ServeOptions{}, err
	}
	passwordFile, _ := passwordFileOf(values, env, cwd)
	webRoot, _ := values.pick("web-root", env, "RELAY_WEB_ROOT")
	rateLimits, err := rateLimitsOf(values, env)
	if err != nil {
		return ServeOptions{}, err
	}
	trustedProxy, err := trustedProxyOf(values, env)
	if err != nil {
		return ServeOptions{}, err
	}
	name, err := nameOf(values, env)
	if err != nil {
		return ServeOptions{}, err
	}
	store, err := storeOf(values, env)
	if err != nil {
		return ServeOptions{}, err
	}
	if err := requirePostgres(store, common.Database); err != nil {
		return ServeOptions{}, err
	}
	challenge, err := challengeOf(values, env, cwd)
	if err != nil {
		return ServeOptions{}, err
	}
	retention, err := trafficRetentionOf(values, env)
	if err != nil {
		return ServeOptions{}, err
	}
	control, redis, nodeID, err := controlOf(values, env, host, common.Database)
	if err != nil {
		return ServeOptions{}, err
	}
	options := ServeOptions{
		Common: common, Store: store, Control: control, RedisURL: redis, NodeID: nodeID,
		Host: host, Port: port, Listen: listen, TLS: tls, LogLevel: logLevel,
		Account: account, PasswordFile: passwordFile, ACME: acme,
		RateLimits: rateLimits, TrustedProxy: trustedProxy, Name: name, Challenge: challenge,
		TrafficRetentionDays: retention,
	}
	if publicText != "" {
		if options.PublicOrigin, err = originOf(publicText); err != nil {
			return ServeOptions{}, err
		}
	}
	if webRoot != "" {
		options.WebRoot = resolvePath(cwd, webRoot)
	}
	if tls == TLSFile {
		options.TLSCert, options.TLSKey = resolvePath(cwd, tlsCert), resolvePath(cwd, tlsKey)
	}
	if metricsText != "" {
		metrics, err := listenOf(metricsText, "--metrics-listen")
		if err != nil {
			return ServeOptions{}, err
		}
		options.MetricsListen = &metrics
	}
	options.MetricsToken, _ = env.NonEmpty("RELAY_METRICS_TOKEN")
	return options, nil
}

// ------------------------------------------------------------ 其余子命令

// ParseDataDirOnly 解只收 --data-dir（与数据库连接）的子命令（status、rotate-key）。
func ParseDataDirOnly(argv []string, env Env, cwd string) (Common, error) {
	values, _, err := ParseFlags(argv, withDatabase("data-dir"), FlagOptions{})
	if err != nil {
		return Common{}, err
	}
	return commonOf(values, env, cwd)
}

// PasswdOptions 是 personal passwd 的参数。
type PasswdOptions struct {
	Common
	// PasswordFile 空 = 终端交互；"-" 是标准输入。
	PasswordFile string
}

// ParsePasswd 解 personal passwd 的参数。
func ParsePasswd(argv []string, env Env, cwd string) (PasswdOptions, error) {
	values, _, err := ParseFlags(argv, withDatabase("data-dir", "password-file"), FlagOptions{})
	if err != nil {
		return PasswdOptions{}, err
	}
	common, err := commonOf(values, env, cwd)
	if err != nil {
		return PasswdOptions{}, err
	}
	options := PasswdOptions{Common: common}
	if file := values["password-file"]; file == "-" {
		options.PasswordFile = "-"
	} else if file != "" {
		options.PasswordFile = resolvePath(cwd, file)
	}
	return options, nil
}

// LinksOptions 是 personal links 的参数。
type LinksOptions struct {
	Common
	// Action 是 "list" 或 "revoke"。
	Action string
	// LinkID 只在 revoke 时有。
	LinkID string
}

// ParseLinks 解 personal links list | revoke <id> 的参数。
func ParseLinks(argv []string, env Env, cwd string) (LinksOptions, error) {
	action := ""
	var rest []string
	if len(argv) > 0 {
		action, rest = argv[0], argv[1:]
	}
	switch action {
	case "list":
		values, _, err := ParseFlags(rest, withDatabase("data-dir"), FlagOptions{})
		if err != nil {
			return LinksOptions{}, err
		}
		common, err := commonOf(values, env, cwd)
		return LinksOptions{Common: common, Action: action}, err
	case "revoke":
		values, positional, err := ParseFlags(rest, withDatabase("data-dir"), FlagOptions{Positional: 1})
		if err != nil {
			return LinksOptions{}, err
		}
		linkID := ""
		if len(positional) > 0 {
			linkID = positional[0]
		}
		if linkID == "" || !linkIDPattern.MatchString(linkID) {
			return LinksOptions{}, Usagef("links revoke 要一个链接 id：%s", linkID)
		}
		common, err := commonOf(values, env, cwd)
		return LinksOptions{Common: common, Action: action, LinkID: linkID}, err
	}
	return LinksOptions{}, Usagef("links 只有 list 与 revoke：%s", action)
}

// ------------------------------------------------------------ 流量统计

// ParseDuration 解 `7d`、`24h`、`30m`：时长（毫秒）；不认识 ok=false。
func ParseDuration(text string) (int64, bool) {
	m := durationRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	unit := map[string]int64{"d": 86_400_000, "h": 3_600_000, "m": 60_000}[m[2]]
	n, _ := strconv.ParseInt(m[1], 10, 64)
	if n*unit <= 0 {
		return 0, false
	}
	return n * unit, true
}

// ParseTraffic 解 personal traffic 的参数。
func ParseTraffic(argv []string, env Env, cwd string) (TrafficOptions, error) {
	values, _, err := ParseFlags(argv, withDatabase("data-dir", "since", "source"), FlagOptions{})
	if err != nil {
		return TrafficOptions{}, err
	}
	sinceText, ok := values["since"]
	if !ok {
		sinceText = "7d"
	}
	sinceMs, ok := ParseDuration(sinceText)
	if !ok {
		return TrafficOptions{}, Usagef("--since 要是天、小时或分钟，例如 7d、24h：%s", sinceText)
	}
	sourceID, haveSource := values["source"]
	if haveSource && !sourceIDRe.MatchString(sourceID) {
		return TrafficOptions{}, Usagef("--source 要是一个源 id：%s", sourceID)
	}
	common, err := commonOf(values, env, cwd)
	if err != nil {
		return TrafficOptions{}, err
	}
	return TrafficOptions{Common: common, SinceMs: sinceMs, SourceID: sourceID}, nil
}

// ------------------------------------------------------------ 存储

// MigrateOptions 是 personal migrate 的参数。
type MigrateOptions struct {
	Common
	To StoreKind
}

// ParseMigrate 解 personal migrate 的参数。
func ParseMigrate(argv []string, env Env, cwd string) (MigrateOptions, error) {
	values, _, err := ParseFlags(argv, withDatabase("data-dir", "to"), FlagOptions{})
	if err != nil {
		return MigrateOptions{}, err
	}
	to, ok := values["to"]
	if !ok {
		return MigrateOptions{}, Usagef("缺少 --to：%s", storeKindNames())
	}
	if !validStoreKind(to) {
		return MigrateOptions{}, Usagef("--to 只能是 %s：%s", storeKindNames(), to)
	}
	common, err := commonOf(values, env, cwd)
	if err != nil {
		return MigrateOptions{}, err
	}
	if err := requirePostgres(StoreKind(to), common.Database); err != nil {
		return MigrateOptions{}, err
	}
	return MigrateOptions{Common: common, To: StoreKind(to)}, nil
}

// BackupOptions 是 personal backup 的参数。
type BackupOptions struct {
	Common
	Out string
}

// ParseBackup 解 personal backup 的参数。
func ParseBackup(argv []string, env Env, cwd string) (BackupOptions, error) {
	values, _, err := ParseFlags(argv, withDatabase("data-dir", "out"), FlagOptions{})
	if err != nil {
		return BackupOptions{}, err
	}
	out := values["out"]
	if out == "" {
		return BackupOptions{}, Usagef("缺少 --out：备份文件的路径")
	}
	common, err := commonOf(values, env, cwd)
	if err != nil {
		return BackupOptions{}, err
	}
	return BackupOptions{Common: common, Out: resolvePath(cwd, out)}, nil
}

var doctorFlags = withDatabase("data-dir", "host", "port", "listen", "tls", "trusted-proxy", "password-file")

// ParseDoctor 解 personal doctor 的参数：收 serve 的同名参数与环境变量，只看不改；
// 明文监听公网之类的问题报出来而不是拒绝。
func ParseDoctor(argv []string, env Env, cwd string) (DoctorOptions, error) {
	values, _, err := ParseFlags(argv, doctorFlags, FlagOptions{})
	if err != nil {
		return DoctorOptions{}, err
	}
	host, _ := values.pick("host", env, "RELAY_HOST")
	port, err := portOf(values, env)
	if err != nil {
		return DoctorOptions{}, err
	}
	listenText, ok := values.pick("listen", env, "RELAY_LISTEN")
	if !ok {
		bind := "0.0.0.0"
		if host != "" && IsLoopback(host) {
			bind = "127.0.0.1"
		}
		listenText = bind + ":" + strconv.Itoa(port)
	}
	listen, err := listenOf(listenText, "--listen")
	if err != nil {
		return DoctorOptions{}, err
	}
	var trustedProxy *string
	if text, ok := values.pick("trusted-proxy", env, "RELAY_TRUSTED_PROXY"); ok {
		trustedProxy = &text
	}
	passwordFile, _ := passwordFileOf(values, env, cwd)
	common, err := commonOf(values, env, cwd)
	if err != nil {
		return DoctorOptions{}, err
	}
	tls, err := tlsOf(values, env)
	if err != nil {
		return DoctorOptions{}, err
	}
	options := DoctorOptions{Common: common, Listen: listen, TLS: tls, TrustedProxy: trustedProxy}
	if passwordFile != "" && passwordFile != "-" {
		options.PasswordFile = passwordFile
	}
	return options, nil
}
