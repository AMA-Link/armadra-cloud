package personal

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// serve 的运行标记：<data-dir>/serve.pid（0600，只有进程号）。personal migrate 看到活着的
// serve 就拒绝——换存储要先停服。serve 正常退出时删掉；崩溃留下的标记按进程是否还活着判断。

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	// EPERM：进程在，只是不归我们管。
	return err == nil || errors.Is(err, syscall.EPERM)
}

// HoldRunLock 写下本进程号；答一个释放函数（只删自己写的那份）。写不出来时不拦 serve（标记只是
// 给 migrate 看的），释放函数照样可用。
func HoldRunLock(dataDir string) (release func()) {
	file := filepath.Join(dataDir, RunLockFile)
	mine := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(file, []byte(mine+"\n"), 0o600); err != nil {
		return func() {}
	}
	_ = os.Chmod(file, 0o600)
	return func() {
		data, err := os.ReadFile(file)
		if err == nil && strings.TrimSpace(string(data)) == mine {
			_ = os.Remove(file)
		}
	}
}

// RunningServe 是正在运行的 serve 的进程号；没有、已退出或就是本进程答 ok=false。
func RunningServe(dataDir string) (pid int, ok bool) {
	data, err := os.ReadFile(filepath.Join(dataDir, RunLockFile))
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return 0, false
	}
	if !pidAlive(pid) {
		return 0, false
	}
	return pid, true
}
