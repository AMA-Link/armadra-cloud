//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package personal

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), ioctlGetTermios)
	return err == nil
}

// readSecretTerminal 在标准错误上提示，不回显地从终端读一行（读完补一个换行）。
func readSecretTerminal(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return "", Usagef("缺少口令：给 --password-file，或在终端上运行")
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return "", Usagef("终端不能关回显")
	}
	restore := func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }
	// 中断时先把回显还给终端。
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	finished := make(chan struct{})
	go func() {
		select {
		case <-interrupts:
			restore()
			_, _ = os.Stderr.WriteString("\n")
			os.Exit(130)
		case <-finished:
		}
	}()
	defer func() {
		close(finished)
		signal.Stop(interrupts)
		restore()
		_, _ = os.Stderr.WriteString("\n")
	}()
	_, _ = os.Stderr.WriteString(prompt)
	var line []byte
	one := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				break
			}
			if one[0] != '\r' {
				line = append(line, one[0])
			}
		}
		if err != nil {
			if len(line) == 0 {
				return "", Usagef("缺少口令：给 --password-file，或在终端上运行")
			}
			break
		}
	}
	return string(line), nil
}
