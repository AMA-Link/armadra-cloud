package personal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

const testMasterKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func masterKeyFile(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(file, []byte(text+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, mode); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestArgsMasterKeyFile(t *testing.T) {
	good := masterKeyFile(t, testMasterKeyHex, 0o600)
	// 凡是打开存储的子命令都收：命令行与环境变量。
	for name, parse := range map[string]func(Env, ...string) (Common, error){
		"status": func(env Env, extra ...string) (Common, error) {
			return ParseDataDirOnly(append([]string{"--data-dir", "d"}, extra...), env, "/w")
		},
		"serve": func(env Env, extra ...string) (Common, error) {
			o, err := ParseServe(append([]string{"--data-dir", "d", "--host", "127.0.0.1"}, extra...), env, "/w")
			return o.Common, err
		},
		"doctor": func(env Env, extra ...string) (Common, error) {
			o, err := ParseDoctor(append([]string{"--data-dir", "d"}, extra...), env, "/w")
			return o.Common, err
		},
		"backup": func(env Env, extra ...string) (Common, error) {
			o, err := ParseBackup(append([]string{"--data-dir", "d", "--out", "o"}, extra...), env, "/w")
			return o.Common, err
		},
	} {
		got, err := parse(Env{}, "--master-key-file", good)
		if err != nil || got.Database.masterKey() == nil || got.Database.hasURL() {
			t.Fatalf("%s：命令行：%+v %v", name, got.Database, err)
		}
		got, err = parse(Env{"RELAY_MASTER_KEY_FILE": good})
		if err != nil || len(got.Database.masterKey()) != store.MasterKeySize {
			t.Fatalf("%s：环境变量：%v", name, err)
		}
		// 环境变量为空当作没给。
		got, err = parse(Env{"RELAY_MASTER_KEY_FILE": ""})
		if err != nil || got.Database != nil {
			t.Fatalf("%s：空环境变量：%+v %v", name, got.Database, err)
		}
	}
	// 缺省：不给就没有主密钥，Database 仍是 nil（与之前一致）。
	if c, err := ParseDataDirOnly([]string{"--data-dir", "d"}, Env{}, "/w"); err != nil || c.Database != nil {
		t.Fatalf("缺省：%+v %v", c.Database, err)
	}
	// 权限比 0600 宽、格式不对、读不出来：拒绝，消息不带密钥内容。
	loose := masterKeyFile(t, testMasterKeyHex, 0o640)
	_, err := ParseDataDirOnly([]string{"--data-dir", "d", "--master-key-file", loose}, Env{}, "/w")
	wantUsage(t, err, "权限太松")
	for _, bad := range []string{"zz", strings.Repeat("a", 62), strings.Repeat("a", 66)} {
		file := masterKeyFile(t, bad, 0o600)
		_, err = ParseDataDirOnly([]string{"--data-dir", "d", "--master-key-file", file}, Env{}, "/w")
		wantUsage(t, err, "64 位十六进制")
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("错误消息带了密钥内容：%v", err)
		}
	}
	_, err = ParseDataDirOnly([]string{"--data-dir", "d", "--master-key-file", filepath.Join(t.TempDir(), "none")}, Env{}, "/w")
	wantUsage(t, err, "读不出来")
	// 0400 比 0600 严，允许。
	if _, err := ParseDataDirOnly([]string{"--data-dir", "d", "--master-key-file", masterKeyFile(t, testMasterKeyHex, 0o400)}, Env{}, "/w"); err != nil {
		t.Fatalf("0400 应允许：%v", err)
	}
}

func masterOptions(t *testing.T, dir string) *DatabaseOptions {
	t.Helper()
	key, err := store.ParseMasterKey(testMasterKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	return &DatabaseOptions{MasterKey: key}
}

// 从明文迁到包裹：wrap-keys 要主密钥、可重复执行、包裹后没给主密钥拒绝启动、给了照常读；
// 之后新轮换的钥也是包裹的；doctor 随之从警告变通过。
func TestWrapKeysMigration(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "d")
	createTestStore(t, StoreSQLite, dir, nil).Close()
	db := masterOptions(t, dir)
	out := &collector{}

	stats := func(open *DatabaseOptions) (plain, wrapped int) {
		t.Helper()
		err := WithStore(ctx, dir, open, func(st Store) error {
			var err error
			plain, wrapped, _, err = KeyWrapStats(ctx, st)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return plain, wrapped
	}
	if plain, wrapped := stats(nil); plain != 1 || wrapped != 0 {
		t.Fatalf("开始：%d %d", plain, wrapped)
	}
	// doctor：明文私钥给警告。
	findings := Diagnose(ctx, DoctorOptions{Common: Common{DataDir: dir}, Listen: ListenAddress{"127.0.0.1", 8443}, TLS: "self-signed"}, doctorNow)
	if got := levelsOf(findings)["签名钥包裹"]; got != LevelWarn {
		t.Fatalf("明文应警告：%v", got)
	}

	// 没给主密钥：用法错误。
	if err := RunWrapKeys(ctx, WrapKeysOptions{Common: Common{DataDir: dir}}, out.print); err == nil {
		t.Fatal("没给主密钥应拒绝")
	}
	if _, err := ParseWrapKeys([]string{"--data-dir", dir}, Env{}, "/w"); err == nil {
		t.Fatal("ParseWrapKeys 没给主密钥应拒绝")
	}
	for i, want := range []string{"已包裹 1 把签名钥", "没有需要包裹的签名钥"} {
		if err := RunWrapKeys(ctx, WrapKeysOptions{Common: Common{DataDir: dir, Database: db}}, out.print); err != nil {
			t.Fatal(err)
		}
		if got := out.take(); len(got) != 1 || !strings.Contains(got[0], want) {
			t.Fatalf("第 %d 次：%v", i+1, got)
		}
	}
	if plain, wrapped := stats(db); plain != 0 || wrapped != 1 {
		t.Fatalf("包裹后：%d %d", plain, wrapped)
	}

	// 没给主密钥：拒绝启动，提示给 --master-key-file；doctor 报失败。
	err := WithStore(ctx, dir, nil, func(Store) error { return nil })
	var se *StateError
	if !errors.As(err, &se) || !strings.Contains(se.Msg, "--master-key-file") {
		t.Fatalf("没给主密钥应拒绝：%v", err)
	}
	findings = Diagnose(ctx, DoctorOptions{Common: Common{DataDir: dir}, Listen: ListenAddress{"127.0.0.1", 8443}, TLS: "self-signed"}, doctorNow)
	if got := levelsOf(findings)["存储"]; got != LevelFail {
		t.Fatalf("包裹而没给主密钥 doctor 应失败：%v", got)
	}
	// 给了主密钥：照常读写，轮换出的新钥也包裹；doctor 通过。
	if err := RunRotateKey(ctx, dir, db, out.print); err != nil {
		t.Fatal(err)
	}
	if plain, wrapped := stats(db); plain != 0 || wrapped != 2 {
		t.Fatalf("轮换后：%d %d", plain, wrapped)
	}
	findings = Diagnose(ctx, DoctorOptions{Common: Common{DataDir: dir, Database: db}, Listen: ListenAddress{"127.0.0.1", 8443}, TLS: "self-signed"}, doctorNow)
	if got := levelsOf(findings)["签名钥包裹"]; got != LevelPass {
		t.Fatalf("包裹后 doctor 应通过：%v", findings)
	}
	// backup 不需要主密钥，备份出的库照样要主密钥才能读。
	backup := filepath.Join(t.TempDir(), "b.db")
	if err := RunBackup(ctx, dir, backup, nil, out.print); err != nil {
		t.Fatal(err)
	}
}

// wrap-keys 在 serve 运行时拒绝；PostgreSQL 与没初始化的数据目录拒绝。
func TestWrapKeysRefusals(t *testing.T) {
	ctx := context.Background()
	out := &collector{}
	empty := t.TempDir()
	db := masterOptions(t, empty)
	if err := RunWrapKeys(ctx, WrapKeysOptions{Common: Common{DataDir: empty, Database: db}}, out.print); err == nil {
		t.Fatal("没初始化应拒绝")
	}
	dir := filepath.Join(t.TempDir(), "d")
	createTestStore(t, StoreSQLite, dir, nil).Close()
	if err := os.WriteFile(filepath.Join(dir, RunLockFile), []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
		t.Fatal(err)
	}
	err := RunWrapKeys(ctx, WrapKeysOptions{Common: Common{DataDir: dir, Database: db}}, out.print)
	wantUsage(t, err, "serve 正在运行")
}
