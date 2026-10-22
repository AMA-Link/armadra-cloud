package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{"没有命令", nil, 64, "用法: armadra-relay <命令>", ""},
		{"未知命令", []string{"nope"}, 64, "", "未知命令：nope"},
		{"版本", []string{"version"}, 0, "dev\n", ""},
		{"帮助", []string{"help"}, 0, "用法: armadra-relay", ""},
		{"dev 缺参数", []string{"dev"}, 64, "", "用法: armadra-relay dev --jwks"},
		{"dev 拒绝非回环监听", []string{"dev", "--jwks", "x", "--issuer", "i", "--listen", "0.0.0.0:1"}, 64, "", "明文监听只许回环地址"},
		{"dev 拒绝非回环指标监听", []string{"dev", "--jwks", "x", "--issuer", "i", "--metrics-listen", "10.0.0.1:1"}, 64, "", "明文监听只许回环地址"},
		{"dev 监听地址不合法", []string{"dev", "--jwks", "x", "--issuer", "i", "--listen", "nope"}, 64, "", "监听地址不合法"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(c.args, &out, &errOut)
			if code != c.wantCode || !strings.Contains(out.String(), c.wantOut) || !strings.Contains(errOut.String(), c.wantErr) {
				t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
		})
	}
}

func TestRequireLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:0": true, "[::1]:8443": true, "localhost:80": true,
		"0.0.0.0:80": false, "192.168.1.2:80": false, "example.com:80": false, ":80": false,
	} {
		if err := requireLoopback(addr); (err == nil) != ok {
			t.Errorf("%s：err=%v，应为 ok=%v", addr, err, ok)
		}
	}
}
