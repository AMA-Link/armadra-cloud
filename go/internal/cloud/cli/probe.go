package cli

import (
	"io"
	"net"
	"net/http"
	"time"
)

// probeHealth 打本机监听端口的 /health，200 就是活着（镜像的 HEALTHCHECK 用）。
func probeHealth(port int) bool {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", portText(port)) + "/health")
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
