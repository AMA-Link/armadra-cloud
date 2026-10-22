package personal

import "github.com/AMA-Link/armadra-cloud/go/internal/traffic"

type trafficRowForTest struct {
	bucket                  int64
	source, subject, device string
	kind                    string
	conns, up, down         int64
}

func toTrafficRows(rows []trafficRowForTest) []traffic.Row {
	out := make([]traffic.Row, len(rows))
	for i, r := range rows {
		out[i] = traffic.Row{
			BucketStartMs: r.bucket, Node: "n1",
			Key:         traffic.Key{SourceID: r.source, Subject: r.subject, DeviceID: r.device, Kind: traffic.Kind(r.kind)},
			Connections: r.conns, BytesToSource: r.up, BytesToClient: r.down,
		}
	}
	return out
}
