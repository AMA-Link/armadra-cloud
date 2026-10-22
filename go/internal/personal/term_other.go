//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package personal

func stdinIsTerminal() bool { return false }

func readSecretTerminal(string) (string, error) {
	return "", Usagef("缺少口令：给 --password-file，或在终端上运行")
}
