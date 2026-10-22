package personal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRunLock(t *testing.T) {
	dir := t.TempDir()
	if _, ok := RunningServe(dir); ok {
		t.Fatal("没有标记不该有 serve")
	}
	release := HoldRunLock(dir)
	file := filepath.Join(dir, RunLockFile)
	mustMode(t, file, 0o600)
	data, _ := os.ReadFile(file)
	if string(data) != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("标记 = %q", data)
	}
	// 就是本进程：不算在运行。
	if _, ok := RunningServe(dir); ok {
		t.Fatal("本进程不算")
	}
	// 别人改写了标记：释放时不删。
	if err := os.WriteFile(file, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	if !exists(file) {
		t.Fatal("别人的标记被删了")
	}
	// pid 1 活着（EPERM 也算活着）。
	if pid, ok := RunningServe(dir); !ok || pid != 1 {
		t.Fatalf("RunningServe = %d %v", pid, ok)
	}
	release2 := HoldRunLock(dir)
	release2()
	if exists(file) {
		t.Fatal("自己的标记没删")
	}
}

func TestRunningServeStalePid(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("没有 true 命令")
	}
	// 已退出的子进程号：标记作废。
	if err := os.WriteFile(filepath.Join(dir, RunLockFile), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := RunningServe(dir); ok {
		t.Fatal("已退出的进程不该算在运行")
	}
	for _, bad := range []string{"", "abc\n", "-3\n", "0\n"} {
		if err := os.WriteFile(filepath.Join(dir, RunLockFile), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := RunningServe(dir); ok {
			t.Fatalf("%q 不该算", bad)
		}
	}
}
