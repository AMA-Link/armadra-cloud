// armadra-cloud 是 SaaS 控制面的命令行：serve、migrate、keys rotate、health、version。
// 其余子命令随后续批次加入。
package main

import (
	"os"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/cli"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
)

// version 由构建注入（-ldflags "-X main.version=…"）。
var version = "dev"

func main() {
	os.Exit(cli.Run(os.Args[1:], config.OSEnv{}, os.Stdout, os.Stderr, cli.Options{Version: version}))
}
