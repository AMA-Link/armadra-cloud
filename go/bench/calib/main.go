// calib：本机回环 TCP 往返基线（两个进程，1 KiB 回显），用来判断延迟数字里有多少是环境本身。
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		fmt.Println(ln.Addr())
		c, _ := ln.Accept()
		io.Copy(c, c)
		return
	}
	cmd := exec.Command(os.Args[0], "serve")
	out, _ := cmd.StdoutPipe()
	cmd.Start()
	buf := make([]byte, 64)
	n, _ := out.Read(buf)
	addr := string(buf[:n-1])
	c, err := net.Dial("tcp", addr)
	if err != nil {
		panic(err)
	}
	msg := make([]byte, 1024)
	var us []int64
	for i := 0; i < 2000; i++ {
		t := time.Now()
		c.Write(msg)
		io.ReadFull(c, msg)
		us = append(us, time.Since(t).Microseconds())
		if i%2 == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	slices.Sort(us)
	fmt.Printf("回环往返 µs：p50 %d p90 %d p99 %d max %d\n", us[len(us)/2], us[len(us)*9/10], us[len(us)*99/100], us[len(us)-1])
	cmd.Process.Kill()
}
