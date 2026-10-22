package tlsx

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root 不受权限位限制")
	}
}

// 读不到 CA（权限不够）不等于没有 CA：拒绝启动，不重建、不覆盖。
func TestSelfSignedUnreadableCAIsNotRegenerated(t *testing.T) {
	skipIfRoot(t)
	dataDir := t.TempDir()
	names := Names("relay.test")
	if _, err := SelfSignedMaterial(dataDir, names); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, TLSDir)
	before, err := os.ReadFile(filepath.Join(dir, CAKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataDir, 0); err != nil {
		t.Fatal(err)
	}
	_, err = SelfSignedMaterial(dataDir, names)
	_ = os.Chmod(dataDir, 0o700)
	if err == nil {
		t.Fatal("读不到 CA 应拒绝启动")
	}
	after, _ := os.ReadFile(filepath.Join(dir, CAKey))
	if !bytes.Equal(before, after) {
		t.Fatal("CA 私钥被覆盖了")
	}
	// 文件本身读不了也一样。
	if err := os.Chmod(filepath.Join(dir, CAKey), 0); err != nil {
		t.Fatal(err)
	}
	_, err = SelfSignedMaterial(dataDir, names)
	_ = os.Chmod(filepath.Join(dir, CAKey), 0o600)
	if _, ok := err.(*Error); !ok {
		t.Fatalf("私钥读不了应拒绝：%v", err)
	}
}

// 账户密钥读不到不等于没有：不覆盖。
func TestACMEUnreadableAccountKeyIsNotRegenerated(t *testing.T) {
	skipIfRoot(t)
	dir := filepath.Join(t.TempDir(), ACMEDir)
	file := filepath.Join(dir, "account.key")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateAccountKey(file); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	_, err := loadOrCreateAccountKey(file)
	_ = os.Chmod(dir, 0o700)
	if _, ok := err.(*Error); !ok {
		t.Fatalf("应是 *Error：%v", err)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("账户密钥被覆盖了")
	}
}

// 直接测 loadOrCreateCA：stat 报权限不足时答 *Error，而不是当作「没有」去新建。
func TestLoadOrCreateCAPermissionDenied(t *testing.T) {
	skipIfRoot(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, TLSDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(parent, 0o700) }()
	_, err := loadOrCreateCA(dir, time.Now())
	if _, ok := err.(*Error); !ok {
		t.Fatalf("应是 *Error：%v", err)
	}
}
