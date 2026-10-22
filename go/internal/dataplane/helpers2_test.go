package dataplane_test

import (
	"net/http/httptest"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
)

func edgeFilter(sourceID string) edge.ConnectionFilter {
	return edge.ConnectionFilter{SourceID: sourceID}
}

func newMetricsServer(t *testing.T, render func() string, token string) string {
	srv := httptest.NewServer(obs.MetricsHandler(render, token))
	t.Cleanup(srv.Close)
	return srv.URL
}
