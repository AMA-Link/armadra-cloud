package personal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

func TestReadPasswordFile(t *testing.T) {
	dir := t.TempDir()
	for text, want := range map[string]string{
		"secret\n": "secret", "secret\r\n": "secret", "secret": "secret", "secret\n\n": "secret\n", "": "",
	} {
		file := filepath.Join(dir, "pw")
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadPasswordFile(file)
		if err != nil || got != want {
			t.Fatalf("%q → %q %v，要 %q", text, got, err, want)
		}
	}
	if _, err := ReadPasswordFile(filepath.Join(dir, "none")); err == nil {
		t.Fatal("没有的文件应报错")
	} else if strings.Contains(err.Error(), "no such") {
		t.Fatalf("不转述系统错误：%v", err)
	}
	old := stdin
	defer func() { stdin = old }()
	stdin = strings.NewReader("from stdin\n")
	if got, err := ReadPasswordFile("-"); err != nil || got != "from stdin" {
		t.Fatalf("stdin = %q %v", got, err)
	}
}

func TestObtainPassword(t *testing.T) {
	old := terminalIO
	defer func() { terminalIO = old }()

	t.Run("文件", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "pw")
		_ = os.WriteFile(file, []byte(testPassword+"\n"), 0o600)
		if got, err := ObtainPassword(file, true); err != nil || got != testPassword {
			t.Fatalf("%q %v", got, err)
		}
	})
	t.Run("没有终端", func(t *testing.T) {
		terminalIO.isTerminal = func() bool { return false }
		_, err := ObtainPassword("", true)
		usageErr(t, err, "缺少口令：给 --password-file，或在终端上运行")
	})
	t.Run("终端读两遍", func(t *testing.T) {
		var prompts []string
		answers := []string{"abc", "abc"}
		terminalIO.isTerminal = func() bool { return true }
		terminalIO.readSecret = func(prompt string) (string, error) {
			prompts = append(prompts, prompt)
			answer := answers[0]
			answers = answers[1:]
			return answer, nil
		}
		got, err := ObtainPassword("", true)
		if err != nil || got != "abc" || strings.Join(prompts, "|") != "口令：|再输一次：" {
			t.Fatalf("%q %v %v", got, err, prompts)
		}
	})
	t.Run("两次不一致", func(t *testing.T) {
		answers := []string{"abc", "abd"}
		terminalIO.isTerminal = func() bool { return true }
		terminalIO.readSecret = func(string) (string, error) {
			answer := answers[0]
			answers = answers[1:]
			return answer, nil
		}
		_, err := ObtainPassword("", true)
		usageErr(t, err, "两次输入的口令不一致")
	})
	t.Run("不确认只读一遍", func(t *testing.T) {
		calls := 0
		terminalIO.isTerminal = func() bool { return true }
		terminalIO.readSecret = func(string) (string, error) { calls++; return "once", nil }
		if got, err := ObtainPassword("", false); err != nil || got != "once" || calls != 1 {
			t.Fatalf("%q %v %d", got, err, calls)
		}
	})
}

func TestPasswordUsageError(t *testing.T) {
	want := map[gen.ErrorCode]string{
		gen.ErrPasswordTooShort:     "口令太短：至少 12 个字符",
		gen.ErrPasswordTooLong:      "口令太长：不超过 1024 字节",
		gen.ErrPasswordContainsName: "口令不能包含账号名",
		gen.ErrPasswordTooCommon:    "口令太常见",
	}
	for code, hint := range want {
		usageErr(t, PasswordUsageError(core.Fail(code)), hint)
	}
	other := core.Fail(gen.ErrNotFound)
	if PasswordUsageError(other) != error(other) {
		t.Fatal("别的码原样返回")
	}
	plain := errors.New("x")
	if PasswordUsageError(plain) != plain {
		t.Fatal("别的错误原样返回")
	}
}

func TestCreateStatePasswordRules(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"short":                   "口令太短：至少 12 个字符",
		"my-alice-passphrase-1":   "口令不能包含账号名",
		"000000000000":            "口令太常见",
		strings.Repeat("x", 1025): "口令太长：不超过 1024 字节",
	}
	for password, hint := range cases {
		dir := filepath.Join(t.TempDir(), "d")
		_, err := CreateState(ctx, CreateStateInput{DataDir: dir, Account: "alice", Password: password, Issuer: "https://x"})
		usageErr(t, err, hint)
		if exists(dir) {
			t.Fatal("口令不过不该建数据目录")
		}
	}
}

func TestCreateStateContents(t *testing.T) {
	ctx := context.Background()
	st := createTestStore(t, StoreSQLite, filepath.Join(t.TempDir(), "d"), nil)
	defer func() { _ = st.Close() }()
	snap, err := st.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	account := snap.Accounts[0]
	if !identity.VerifyPassword(testPassword, account.Password) || identity.VerifyPassword("wrong", account.Password) {
		t.Fatal("口令哈希不对")
	}
	if account.Password.KDF != "scrypt" || account.Failures != 0 || account.LockedUntilMs != 0 {
		t.Fatalf("账号 = %+v", account)
	}
	key := snap.SigningKeys[0]
	if key.Kid != "k1" || key.ActiveFromMs != baseTime.UnixMilli() || key.RetiredAtMs != nil {
		t.Fatalf("签名钥 = %+v", key)
	}
	if !snap.Tenant.AllowGuests || len(snap.Tenant.TrustedOrigins) != 0 || snap.Tenant.Revision != 0 {
		t.Fatalf("租户 = %+v", snap.Tenant)
	}
	if len(snap.Sources)+len(snap.Links)+len(snap.Devices)+len(snap.Registrations) != 0 {
		t.Fatal("新装应当是空的")
	}
}
