package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// serveHealth 答 platform.health 的形状 { ok: true, version, mode: "saas" }。
func serveHealth(w http.ResponseWriter, r *http.Request, version string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}
	body, _ := json.Marshal(struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Mode    string `json:"mode"`
	}{true, version, "saas"})
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	h.Set(gen.HeaderProtocol, strconv.Itoa(gen.ProtocolMajor)+"."+strconv.Itoa(gen.ProtocolMinor))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}
