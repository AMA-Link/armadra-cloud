package personal

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// stdin 是 "-" 口令文件读的输入（测试里换掉）。
var stdin io.Reader = os.Stdin

// ReadPasswordFile 读口令文件（"-" 是标准输入）；末尾的一个换行不算口令（echo 与编辑器都会加）。
func ReadPasswordFile(file string) (string, error) {
	var data []byte
	var err error
	if file == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return "", Usagef("口令文件读不出来：%s", file)
	}
	text := string(data)
	if strings.HasSuffix(text, "\r\n") {
		return text[:len(text)-2], nil
	}
	return strings.TrimSuffix(text, "\n"), nil
}

// terminalIO 是终端交互口令的两个动作（测试里换掉）。
var terminalIO = struct {
	isTerminal func() bool
	readSecret func(prompt string) (string, error)
}{isTerminal: stdinIsTerminal, readSecret: readSecretTerminal}

// ObtainPassword 取口令：文件 / 标准输入 / 终端交互（confirm 时读两遍，提示写在标准错误）。
func ObtainPassword(passwordFile string, confirm bool) (string, error) {
	if passwordFile != "" {
		return ReadPasswordFile(passwordFile)
	}
	if !terminalIO.isTerminal() {
		return "", Usagef("缺少口令：给 --password-file，或在终端上运行")
	}
	first, err := terminalIO.readSecret("口令：")
	if err != nil {
		return "", err
	}
	if confirm {
		second, err := terminalIO.readSecret("再输一次：")
		if err != nil {
			return "", err
		}
		if first != second {
			return "", Usagef("两次输入的口令不一致")
		}
	}
	return first, nil
}

// PasswordRuleHints 是口令规则的中文提示（线上只发码）。
func PasswordRuleHints() map[gen.ErrorCode]string {
	return map[gen.ErrorCode]string{
		gen.ErrPasswordTooShort:     "口令太短：至少 " + strconv.Itoa(gen.IdentityPasswordPolicyMinLengthDefault) + " 个字符",
		gen.ErrPasswordTooLong:      "口令太长：不超过 " + strconv.Itoa(gen.IdentityPasswordPolicyMaxBytes) + " 字节",
		gen.ErrPasswordContainsName: "口令不能包含账号名",
		gen.ErrPasswordTooCommon:    "口令太常见",
	}
}

// PasswordUsageError 口令策略不过 → 带中文提示的 UsageError；别的错误原样。
func PasswordUsageError(err error) error {
	if code, ok := core.CodeOf(err); ok {
		if hint, found := PasswordRuleHints()[code]; found {
			return Usagef("%s", hint)
		}
	}
	return err
}

// CreateStateInput 是 CreateState 的入参。
type CreateStateInput struct {
	DataDir  string
	Account  string
	Password string
	Issuer   string
	// Store 缺省 sqlite（新装）。
	Store    StoreKind
	Database *DatabaseOptions
	// Now 缺省 time.Now。
	Now time.Time
}

// CreateState 建账号（口令过策略、scrypt 哈希）、生成第一把签名钥并写存储。
func CreateState(ctx context.Context, in CreateStateInput) (Store, error) {
	policy := &core.Context{PasswordPolicy: core.DefaultPasswordPolicy}
	if err := core.CheckPassword(policy, in.Password, in.Account); err != nil {
		return nil, PasswordUsageError(err)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	nowMs := now.UnixMilli()
	record, err := identity.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	key, err := store.GenerateSigningKey("k1", nowMs)
	if err != nil {
		return nil, err
	}
	kind := in.Store
	if kind == "" {
		kind = StoreSQLite
	}
	snap := store.TenantSnapshot{
		Tenant: store.Tenant{TenantSettings: store.TenantSettings{
			Issuer: in.Issuer, AllowGuests: true, TrustedOrigins: []string{},
		}},
		Accounts: []store.AccountRecord{{
			AccountID: core.OwnerSubject(in.Account), Name: in.Account, Password: record,
			CreatedAtMs: nowMs, UpdatedAtMs: nowMs,
		}},
		SigningKeys: []store.SigningKeyRecord{key},
	}
	return CreateStore(ctx, in.DataDir, kind, snap, in.Database)
}
