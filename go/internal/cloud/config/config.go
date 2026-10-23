// Package config 解析 armadra-cloud 的环境变量（SaaS 实现方案 §2.2，前缀 ARMADRA_CLOUD_）。
//
// 口令、令牌、主密钥、SMTP 口令只从 *_FILE 或环境变量读，Config 的 String / GoString 不打印它们。
// 配置错误一律答 *Error，CLI 打印后退出码 64。
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/clientip"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

var domainPattern = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Prefix 是全部环境变量的前缀。
const Prefix = "ARMADRA_CLOUD_"

// 缺省值。
const (
	DefaultListen               = "0.0.0.0:8100"
	DefaultJobWorkers           = 2
	DefaultTrafficRetentionDays = 400
	DefaultSMTPPort             = 587
	DefaultLogLevel             = "info"
)

// DefaultChallengeScope 是 CHALLENGE_SCOPE 的缺省（§2.2）。
const DefaultChallengeScope = "auth.login,auth.register,auth.passwordResetStart,links.accept"

// Error 是配置错误。
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errorf(format string, args ...any) *Error { return &Error{Msg: fmt.Sprintf(format, args...)} }

// Env 是环境变量的来源。
type Env interface {
	Lookup(name string) (string, bool)
	// Environ 列出全部变量名，用来找出未知的 ARMADRA_CLOUD_*。
	Environ() []string
}

// MapEnv 是测试与装配用的内存环境。
type MapEnv map[string]string

// Lookup 实现 Env。
func (m MapEnv) Lookup(name string) (string, bool) { v, ok := m[name]; return v, ok }

// Environ 实现 Env。
func (m MapEnv) Environ() []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	return out
}

// OSEnv 取进程的环境变量。
type OSEnv struct{}

// Lookup 实现 Env。
func (OSEnv) Lookup(name string) (string, bool) { return os.LookupEnv(name) }

// Environ 实现 Env。
func (OSEnv) Environ() []string {
	var out []string
	for _, item := range os.Environ() {
		if name, _, ok := strings.Cut(item, "="); ok {
			out = append(out, name)
		}
	}
	return out
}

// SMTP 是发信配置（S2 起使用）。
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	// TLS 取 starttls | tls | none；none 只许回环。
	TLS string
}

// Turnstile 是人机挑战配置；两者都配才要求挑战。
type Turnstile struct {
	SiteKey    string
	SecretFile string
	// Scope 是实际要求挑战的 procedure（本版本认识的子集）。
	Scope []core.ChallengeScope
}

// Config 是 armadra-cloud 的全部配置。
type Config struct {
	Issuer    string
	AppOrigin string
	Listen    string

	DatabaseURL string
	RedisURL    string
	// MasterKey 是 32 字节主密钥。
	MasterKey []byte

	RelayDomain          string
	RequireEmailVerified bool
	Name                 string
	WebRoot              string

	SMTP      *SMTP
	Turnstile *Turnstile

	TrustedProxy *clientip.TrustedProxyConfig
	HIBPURL      string

	JobWorkers           int
	TrafficRetentionDays int

	MetricsListen string
	MetricsToken  string
	LogLevel      string

	// Warnings 是不致命的提示（未知变量、挑战范围里本版本还不认识的项），启动时逐条记日志。
	Warnings []string
}

// String 不打印任何秘密。
func (c Config) String() string {
	return fmt.Sprintf("Config{issuer=%s listen=%s relayDomain=%s}", c.Issuer, c.Listen, c.RelayDomain)
}

// GoString 同 String。
func (c Config) GoString() string { return c.String() }

// known 是认识的变量名（不带前缀）。
var known = map[string]bool{
	"ISSUER": true, "APP_ORIGIN": true, "LISTEN": true, "DATABASE_URL": true, "DATABASE_URL_FILE": true, "REDIS_URL": true, "REDIS_URL_FILE": true,
	"MASTER_KEY": true, "MASTER_KEY_FILE": true, "RELAY_DOMAIN": true, "REQUIRE_EMAIL_VERIFIED": true,
	"NAME": true, "WEB_ROOT": true,
	"SMTP_HOST": true, "SMTP_PORT": true, "SMTP_USERNAME": true, "SMTP_PASSWORD_FILE": true, "SMTP_FROM": true, "SMTP_TLS": true,
	"TURNSTILE_SITE_KEY": true, "TURNSTILE_SECRET_FILE": true, "CHALLENGE_SCOPE": true,
	"TRUSTED_PROXY": true, "TRUSTED_PROXY_PREFER": true, "HIBP_URL": true,
	"JOB_WORKERS": true, "TRAFFIC_RETENTION_DAYS": true,
	"METRICS_LISTEN": true, "METRICS_TOKEN_FILE": true, "LOG_LEVEL": true,
}

type reader struct{ env Env }

// get 读 ARMADRA_CLOUD_<name>；空串当没给。
func (r reader) get(name string) string {
	value, _ := r.env.Lookup(Prefix + name)
	return strings.TrimSpace(value)
}

// file 读 ARMADRA_CLOUD_<name> 指向的文件内容（去掉首尾空白）。
func (r reader) file(name string) (string, bool, error) {
	path := r.get(name)
	if path == "" {
		return "", false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, errorf("%s%s 指向的文件读不出来：%s", Prefix, name, path)
	}
	return strings.TrimSpace(string(data)), true, nil
}

// secretValue 读可以用 <name> 或 <name>_FILE 给的秘密（连接串里可能带口令）；两个都给是配错；都没给答空串。
func (r reader) secretValue(name string) (string, error) {
	inline := r.get(name)
	fromFile, hasFile, err := r.file(name + "_FILE")
	if err != nil {
		return "", err
	}
	switch {
	case inline != "" && hasFile:
		return "", errorf("%s%s 与 %s_FILE 只能给一个", Prefix, name, name)
	case hasFile:
		return fromFile, nil
	}
	return inline, nil
}

// redisURLOf 读并校验 REDIS_URL。
func redisURLOf(r reader) (string, error) {
	text, err := r.secretValue("REDIS_URL")
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", errorf("%sREDIS_URL 必填", Prefix)
	}
	if u, err := url.Parse(text); err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Host == "" {
		return "", errorf("%sREDIS_URL 不合法，要 redis:// 或 rediss:// 地址", Prefix)
	}
	return text, nil
}

// databaseURLOf 读并校验 DATABASE_URL。
func databaseURLOf(r reader) (string, error) {
	text, err := r.secretValue("DATABASE_URL")
	if err != nil {
		return "", err
	}
	return databaseURL(text)
}

// Load 解析并校验全部配置。
func Load(env Env) (*Config, error) {
	r := reader{env}
	c := &Config{}
	var err error

	for _, name := range env.Environ() {
		if rest, ok := strings.CutPrefix(name, Prefix); ok && !known[rest] {
			c.Warnings = append(c.Warnings, "未知的环境变量："+name)
		}
	}
	sort.Strings(c.Warnings)

	if c.Issuer, err = origin(r, "ISSUER"); err != nil {
		return nil, err
	}
	if c.AppOrigin, err = origin(r, "APP_ORIGIN"); err != nil {
		return nil, err
	}
	if c.AppOrigin != c.Issuer {
		return nil, errorf("APP_ORIGIN 必须等于 ISSUER：%s%s", Prefix, "APP_ORIGIN")
	}
	c.Listen = r.get("LISTEN")
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if err := checkListen("LISTEN", c.Listen); err != nil {
		return nil, err
	}
	if c.DatabaseURL, err = databaseURLOf(r); err != nil {
		return nil, err
	}
	if c.RedisURL, err = redisURLOf(r); err != nil {
		return nil, err
	}
	if c.MasterKey, err = masterKeyOf(r); err != nil {
		return nil, err
	}
	if c.RelayDomain = strings.ToLower(r.get("RELAY_DOMAIN")); c.RelayDomain == "" {
		return nil, errorf("%sRELAY_DOMAIN 必填", Prefix)
	}
	if !domainPattern.MatchString(c.RelayDomain) {
		return nil, errorf("%sRELAY_DOMAIN 不是合法的域名：%s", Prefix, c.RelayDomain)
	}
	if c.RequireEmailVerified, err = boolean(r, "REQUIRE_EMAIL_VERIFIED", true); err != nil {
		return nil, err
	}
	c.Name, c.WebRoot = r.get("NAME"), r.get("WEB_ROOT")
	if c.SMTP, err = smtpConfig(r); err != nil {
		return nil, err
	}
	if c.Turnstile, err = turnstile(r, c); err != nil {
		return nil, err
	}
	if c.TrustedProxy, err = trustedProxy(r); err != nil {
		return nil, err
	}
	if c.HIBPURL = r.get("HIBP_URL"); c.HIBPURL != "" {
		if u, err := url.Parse(c.HIBPURL); err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, errorf("%sHIBP_URL 要 https 地址：%s", Prefix, c.HIBPURL)
		}
	}
	if c.JobWorkers, err = positive(r, "JOB_WORKERS", DefaultJobWorkers); err != nil {
		return nil, err
	}
	if c.TrafficRetentionDays, err = positive(r, "TRAFFIC_RETENTION_DAYS", DefaultTrafficRetentionDays); err != nil {
		return nil, err
	}
	if c.MetricsListen = r.get("METRICS_LISTEN"); c.MetricsListen != "" {
		if err := checkListen("METRICS_LISTEN", c.MetricsListen); err != nil {
			return nil, err
		}
	}
	if token, ok, err := r.file("METRICS_TOKEN_FILE"); err != nil {
		return nil, err
	} else if ok {
		c.MetricsToken = token
	}
	if c.LogLevel = r.get("LOG_LEVEL"); c.LogLevel == "" {
		c.LogLevel = DefaultLogLevel
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, errorf("%sLOG_LEVEL 要 debug | info | warn | error：%s", Prefix, c.LogLevel)
	}
	return c, nil
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// origin 读一个 https 来源（回环地址允许 http，给开发环境用）：只有 scheme://host[:port]，无路径与参数。
func origin(r reader, name string) (string, error) {
	text := r.get(name)
	if text == "" {
		return "", errorf("%s%s 必填", Prefix, name)
	}
	u, err := url.Parse(text)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errorf("%s%s 要是 https 来源（不带路径）：%s", Prefix, name, text)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return "", errorf("%s%s 要是 https 来源：%s", Prefix, name, text)
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

func checkListen(name, text string) error {
	host, port, err := net.SplitHostPort(text)
	if err != nil {
		return errorf("%s%s 要是 主机:端口：%s", Prefix, name, text)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return errorf("%s%s 的端口不合法：%s", Prefix, name, text)
	}
	if name == "METRICS_LISTEN" && !isLoopback(host) && host != "" {
		return errorf("%s%s 只许回环地址：%s", Prefix, name, text)
	}
	return nil
}

// databaseURL 校验连接串：postgres:// 或 postgresql://；连非回环地址必须 sslmode=verify-full 或 require。
func databaseURL(text string) (string, error) {
	if text == "" {
		return "", errorf("%sDATABASE_URL 必填", Prefix)
	}
	u, err := url.Parse(text)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		// 解析错误可能带连接串片段：不转述。
		return "", errorf("%sDATABASE_URL 不合法，要 postgres:// 地址", Prefix)
	}
	host := u.Hostname()
	local := host == "" || strings.HasPrefix(host, "/") || isLoopback(host)
	if !local {
		switch u.Query().Get("sslmode") {
		case "verify-full", "require":
		default:
			return "", errorf("%sDATABASE_URL 连非回环地址必须 sslmode=verify-full 或 require", Prefix)
		}
	}
	return text, nil
}

func masterKeyOf(r reader) ([]byte, error) {
	inline := r.get("MASTER_KEY")
	fromFile, hasFile, err := r.file("MASTER_KEY_FILE")
	if err != nil {
		return nil, err
	}
	if inline != "" && hasFile {
		return nil, errorf("%sMASTER_KEY 与 MASTER_KEY_FILE 只能给一个", Prefix)
	}
	text := inline
	if hasFile {
		text = fromFile
	}
	if text == "" {
		return nil, errorf("%sMASTER_KEY 或 MASTER_KEY_FILE 必填", Prefix)
	}
	key, err := store.ParseMasterKey(text)
	if err != nil {
		return nil, errorf("%sMASTER_KEY 要是 64 位十六进制（32 字节）", Prefix)
	}
	return key, nil
}

func boolean(r reader, name string, fallback bool) (bool, error) {
	switch strings.ToLower(r.get(name)) {
	case "":
		return fallback, nil
	case "1", "true", "yes":
		return true, nil
	case "0", "false", "no":
		return false, nil
	}
	return false, errorf("%s%s 要是 1 或 0：%s", Prefix, name, r.get(name))
}

func positive(r reader, name string, fallback int) (int, error) {
	text := r.get(name)
	if text == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 {
		return 0, errorf("%s%s 要是正整数：%s", Prefix, name, text)
	}
	return n, nil
}

func smtpConfig(r reader) (*SMTP, error) {
	host := r.get("SMTP_HOST")
	if host == "" {
		return nil, nil
	}
	s := &SMTP{Host: host, Port: DefaultSMTPPort, Username: r.get("SMTP_USERNAME"), From: r.get("SMTP_FROM"), TLS: "starttls"}
	if text := r.get("SMTP_PORT"); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > 65535 {
			return nil, errorf("%sSMTP_PORT 不合法：%s", Prefix, text)
		}
		s.Port = n
	}
	if text := r.get("SMTP_TLS"); text != "" {
		s.TLS = text
	}
	switch s.TLS {
	case "starttls", "tls":
	case "none":
		if !isLoopback(host) {
			return nil, errorf("%sSMTP_TLS=none 只许回环地址：%s", Prefix, host)
		}
	default:
		return nil, errorf("%sSMTP_TLS 要 starttls | tls | none：%s", Prefix, s.TLS)
	}
	if s.From == "" {
		return nil, errorf("%sSMTP_FROM 必填（配了 SMTP_HOST）", Prefix)
	}
	password, ok, err := r.file("SMTP_PASSWORD_FILE")
	if err != nil {
		return nil, err
	}
	if ok {
		s.Password = password
	}
	return s, nil
}

// supportedChallenge 是控制面认识的挑战范围；不在表里的名字是配置错误。
var supportedChallenge = map[string]core.ChallengeScope{
	"auth.login": core.ScopeLogin, "links.accept": core.ScopeLinksAccept, "auth.changePassword": core.ScopeChangePassword,
	"auth.register": core.ScopeRegister, "auth.passwordResetStart": core.ScopeResetStart,
}

func turnstile(r reader, c *Config) (*Turnstile, error) {
	siteKey, secretFile := r.get("TURNSTILE_SITE_KEY"), r.get("TURNSTILE_SECRET_FILE")
	if siteKey == "" && secretFile == "" {
		return nil, nil
	}
	if siteKey == "" || secretFile == "" {
		return nil, errorf("%sTURNSTILE_SITE_KEY 与 TURNSTILE_SECRET_FILE 要两个都配", Prefix)
	}
	if _, err := os.Stat(secretFile); err != nil {
		return nil, errorf("%sTURNSTILE_SECRET_FILE 读不出来：%s", Prefix, secretFile)
	}
	list := r.get("CHALLENGE_SCOPE")
	if list == "" {
		list = DefaultChallengeScope
	}
	t := &Turnstile{SiteKey: siteKey, SecretFile: secretFile}
	seen := map[string]bool{}
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		switch scope, ok := supportedChallenge[item]; {
		case ok:
			t.Scope = append(t.Scope, scope)
		default:
			return nil, errorf("%sCHALLENGE_SCOPE 里有不认识的项：%s", Prefix, item)
		}
	}
	if len(t.Scope) == 0 {
		return nil, errorf("%sCHALLENGE_SCOPE 里没有可用的项", Prefix)
	}
	return t, nil
}

func trustedProxy(r reader) (*clientip.TrustedProxyConfig, error) {
	list := r.get("TRUSTED_PROXY")
	prefer := r.get("TRUSTED_PROXY_PREFER")
	if prefer != "" && prefer != "forwarded-for" && prefer != "real-ip" {
		return nil, errorf("%sTRUSTED_PROXY_PREFER 要 forwarded-for | real-ip：%s", Prefix, prefer)
	}
	if list == "" {
		return nil, nil
	}
	cfg := &clientip.TrustedProxyConfig{Prefer: prefer}
	if cfg.Prefer == "" {
		cfg.Prefer = "forwarded-for"
	}
	for _, item := range strings.Split(list, ",") {
		if item = strings.TrimSpace(item); item != "" {
			cfg.Proxies = append(cfg.Proxies, item)
		}
	}
	if _, err := clientip.NewClientIP(cfg); err != nil {
		return nil, errorf("%sTRUSTED_PROXY 不合法：%v", Prefix, err)
	}
	return cfg, nil
}

// LoadDatabaseURL 只读 DATABASE_URL（migrate 用）。
func LoadDatabaseURL(env Env) (string, error) {
	return databaseURLOf(reader{env})
}

// LoadKeys 读 DATABASE_URL 与主密钥（keys rotate 用）。
func LoadKeys(env Env) (databaseURL string, masterKey []byte, err error) {
	r := reader{env}
	if databaseURL, err = LoadDatabaseURL(env); err != nil {
		return "", nil, err
	}
	if masterKey, err = masterKeyOf(r); err != nil {
		return "", nil, err
	}
	return databaseURL, masterKey, nil
}

// LoadRedis 读 DATABASE_URL 与 REDIS_URL（nodes revoke 用）。
func LoadRedis(env Env) (databaseURL, redisURL string, err error) {
	r := reader{env}
	if databaseURL, err = LoadDatabaseURL(env); err != nil {
		return "", "", err
	}
	if redisURL, err = redisURLOf(r); err != nil {
		return "", "", err
	}
	return databaseURL, redisURL, nil
}

// ListenPort 读 LISTEN 的端口（health 用）；读不出来取缺省 8100。
func ListenPort(env Env) int {
	text := reader{env}.get("LISTEN")
	if text == "" {
		text = DefaultListen
	}
	_, port, err := net.SplitHostPort(text)
	if err != nil {
		return 8100
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 {
		return 8100
	}
	return n
}
