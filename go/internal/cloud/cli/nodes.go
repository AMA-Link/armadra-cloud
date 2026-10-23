package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/server"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	controlredis "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

// 节点令牌（设计 §2.4，契约 §30）。
const (
	// DefaultNodeTokenDays 是节点令牌的缺省寿命；MaxNodeTokenDays 是上限（撤销记录按它保留，见 nodesRevoke）。
	DefaultNodeTokenDays = 30
	MaxNodeTokenDays     = 365
	// revokeKeep 是 nodes revoke 写撤销记录的保留期：命令只知道 jti、不知道令牌何时到期，所以保留到任何节点令牌都已
	// 过期之后（上限寿命再加一天余量），到期后记录自然消失。
	revokeKeep = (MaxNodeTokenDays + 1) * 24 * time.Hour
)

var (
	// nodeIDPattern 是节点名（nodeTokenClaimsSchema 的 sub，relayNodeIdSchema）。
	nodeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// nodeJTIPattern 是节点令牌的 jti：n.<节点名>.<24 位 hex>；nodes revoke 只认这个形状，不拿它撤别的令牌。
	nodeJTIPattern = regexp.MustCompile(`^n\.[a-z0-9][a-z0-9-]{0,62}\.[0-9a-f]{24}$`)
)

func flags(name string, stderr io.Writer) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}

// nodesToken 是 nodes token：用租户签名钥签一枚节点令牌，令牌打印到 stdout（一行），jti 与到期打印到 stderr。
func nodesToken(args []string, env config.Env, stdout, stderr io.Writer, options Options) int {
	set := flags("nodes token", stderr)
	node := set.String("node", "", "")
	days := set.Int("days", DefaultNodeTokenDays, "")
	if err := set.Parse(args); err != nil || set.NArg() > 0 {
		fmt.Fprintf(stderr, "用法: armadra-cloud nodes token --node <节点名> [--days <1-%d>]\n", MaxNodeTokenDays)
		return ExitUsage
	}
	if !nodeIDPattern.MatchString(*node) {
		fmt.Fprintln(stderr, "--node 要小写字母、数字与连字符，1 到 63 个字符，以字母或数字开头")
		return ExitUsage
	}
	if *days < 1 || *days > MaxNodeTokenDays {
		fmt.Fprintf(stderr, "--days 要在 1 到 %d 之间\n", MaxNodeTokenDays)
		return ExitUsage
	}
	databaseURL, masterKey, err := config.LoadKeys(env)
	if err != nil {
		return fail(err, stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := postgres.Open(ctx, postgres.Options{
		URL: databaseURL, ApplicationName: "armadra-cloud-nodes", NoMigrate: true, MasterKey: masterKey,
	})
	if err != nil {
		return fail(err, stderr)
	}
	defer func() { _ = db.Close() }()
	tenant, err := db.ReadTenant(ctx, server.TenantID)
	if err != nil {
		return fail(err, stderr)
	}
	if tenant == nil {
		fmt.Fprintln(stderr, "还没有存储租户：先运行 armadra-cloud serve 完成引导")
		return ExitFail
	}
	signer := core.NewSigner(db.Keys(server.TenantID), options.Now)
	signed, err := signer.Sign(ctx, gen.TokenTypeNode, core.Claims{
		{Key: "sub", Value: *node}, {Key: "aud", Value: gen.TokenAudienceRelayNode},
	}, core.SignOptions{TTL: time.Duration(*days) * 24 * time.Hour, JTIPrefix: "n." + *node})
	if err != nil {
		return fail(err, stderr)
	}
	fmt.Fprintln(stdout, signed.Token)
	fmt.Fprintf(stderr, "节点 %s 的令牌 jti：%s，到期：%s\n", *node, signed.JTI, time.UnixMilli(signed.ExpiresAtMs).UTC().Format(time.RFC3339))
	return ExitOK
}

// nodesRevoke 是 nodes revoke：走普通撤销（设计 §7.2）——先写 PostgreSQL 的撤销表，再写 Redis 并广播，
// 持有节点已建立的节点间链路在下一次查撤销表时断开。
func nodesRevoke(args []string, env config.Env, stdout, stderr io.Writer, options Options) int {
	set := flags("nodes revoke", stderr)
	jti := set.String("jti", "", "")
	if err := set.Parse(args); err != nil || set.NArg() > 0 {
		fmt.Fprintln(stderr, "用法: armadra-cloud nodes revoke --jti <jti>")
		return ExitUsage
	}
	if !nodeJTIPattern.MatchString(*jti) {
		fmt.Fprintln(stderr, "--jti 不是节点令牌的 jti（形如 n.<节点名>.<24 位十六进制>）")
		return ExitUsage
	}
	databaseURL, redisURL, err := config.LoadRedis(env)
	if err != nil {
		return fail(err, stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := postgres.Open(ctx, postgres.Options{URL: databaseURL, ApplicationName: "armadra-cloud-nodes", NoMigrate: true})
	if err != nil {
		return fail(err, stderr)
	}
	defer func() { _ = db.Close() }()
	client, err := server.OpenRedis(ctx, redisURL)
	if err != nil {
		return fail(err, stderr)
	}
	defer func() { _ = client.Close() }()
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	origin := "cloud/cli-" + hex.EncodeToString(raw[:])
	plane, err := controlredis.New(ctx, controlredis.Options{
		Options: cluster.Options{Issuer: "cli", Mode: "saas", Origin: origin, NodeID: origin, Now: options.Now},
		Client:  client, Prefix: orDefault(options.RelayPrefix, server.RelayPrefix),
	})
	if err != nil {
		return fail(err, stderr)
	}
	defer func() { _ = plane.Close() }()
	redis := relayctl.NewRedis(relayctl.RedisOptions{Plane: plane, Now: options.Now})
	expMs := options.Now().Add(revokeKeep).UnixMilli()
	if err := relayctl.RevokeToken(ctx, db.SaaS(), server.TenantID, redis, *jti, expMs); err != nil {
		fmt.Fprintf(stderr, "撤销已写入 PostgreSQL 并排了 relay.revoke 作业，但这次没能写进 Redis（%v）；运行中的控制面会重试，启动与重新连上 Redis 时也会重放\n", err)
		return ExitFail
	}
	fmt.Fprintf(stdout, "已作废节点令牌：%s\n", *jti)
	return ExitOK
}

func orDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
