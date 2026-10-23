package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/admin"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/server"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

// otpIssuer 是认证器 App 里显示的发行方。
const otpIssuer = "Armadra"

// operatorArgs 解 --email <地址>（或 --email=<地址>）与 --reset；别的参数一律用法错误。
func operatorArgs(args []string, allowReset bool) (email string, reset bool, ok bool) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--email" && i+1 < len(args):
			i++
			email = args[i]
		case strings.HasPrefix(arg, "--email="):
			email = strings.TrimPrefix(arg, "--email=")
		case arg == "--reset" && allowReset:
			reset = true
		default:
			return "", false, false
		}
	}
	return strings.TrimSpace(email), reset, strings.TrimSpace(email) != ""
}

// operators 是 operators add / totp：运营身份只经命令行设置（接口改不了），直接对库，需要 DATABASE_URL 与主密钥。
func operators(sub string, args []string, env config.Env, stdout, stderr io.Writer, options Options) int {
	allowReset := sub == "totp"
	email, reset, ok := operatorArgs(args, allowReset)
	if !ok {
		usage := "用法: armadra-cloud operators add --email <地址>"
		if allowReset {
			usage = "用法: armadra-cloud operators totp --email <地址> [--reset]"
		}
		fmt.Fprintln(stderr, usage)
		return ExitUsage
	}
	databaseURL, masterKey, err := config.LoadKeys(env)
	if err != nil {
		return fail(err, stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := postgres.Open(ctx, postgres.Options{
		URL: databaseURL, ApplicationName: "armadra-cloud-operators", NoMigrate: true, MasterKey: masterKey,
	})
	if err != nil {
		return fail(err, stderr)
	}
	defer func() { _ = db.Close() }()
	nowMs := options.Now().UnixMilli()
	switch sub {
	case "add":
		if _, err := admin.AddOperator(ctx, db.SaaS(), server.TenantID, email, nowMs); err != nil {
			return operatorFail(err, stderr)
		}
		fmt.Fprintf(stdout, "已给 %s 加上运营范围；下一步运行 armadra-cloud operators totp --email %s\n", email, email)
	case "totp":
		uri, err := admin.EnrollTOTP(ctx, db.SaaS(), db.Wrapper(), server.TenantID, email, otpIssuer, reset, nowMs)
		if err != nil {
			return operatorFail(err, stderr)
		}
		// 种子只在这里出现一次：不进日志、不再能读出来，丢了只能 --reset。
		fmt.Fprintf(stdout, "已登记两步验证。用认证器 App 扫描或导入下面的地址（只显示这一次）：\n%s\n", uri)
	}
	return ExitOK
}

// operatorFail 打印运营命令的业务错误：账号不存在 / 不是运营 / 已登记是运行出错（1），不是用法错误。
func operatorFail(err error, stderr io.Writer) int {
	switch {
	case errors.Is(err, admin.ErrNoAccount), errors.Is(err, admin.ErrNotOperator), errors.Is(err, admin.ErrTOTPExists):
		fmt.Fprintln(stderr, err.Error())
		return ExitFail
	}
	return fail(err, stderr)
}
