package obs

// 黄金输出由 TS 的 RelayMetrics.render() 对同一场景实际跑出。
const goldenMetrics = `# HELP relay_tunnels Open tunnels on this node.
# TYPE relay_tunnels gauge
relay_tunnels 1
# HELP relay_streams Open streams on this node.
# TYPE relay_streams gauge
relay_streams 2
# HELP relay_bytes_total Payload bytes relayed.
# TYPE relay_bytes_total counter
relay_bytes_total{direction="to_source"} 7
relay_bytes_total{direction="to_client"} 15
# HELP relay_edge_connections_total Client connections by result.
# TYPE relay_edge_connections_total counter
relay_edge_connections_total{result="ok"} 2
relay_edge_connections_total{result="source_offline"} 1
# HELP relay_edge_streams_total Streams opened at the edge by kind.
# TYPE relay_edge_streams_total counter
relay_edge_streams_total{kind="http"} 2
relay_edge_streams_total{kind="ws"} 1
# HELP relay_handshake_total Tunnel handshakes by result.
# TYPE relay_handshake_total counter
relay_handshake_total{result="bad_token"} 1
relay_handshake_total{result="ok"} 2
# HELP relay_heartbeat_miss_total Tunnels closed after missed heartbeats.
# TYPE relay_heartbeat_miss_total counter
relay_heartbeat_miss_total 1
# HELP relay_handshake_seconds Tunnel handshake duration.
# TYPE relay_handshake_seconds histogram
relay_handshake_seconds_bucket{le="0.01"} 0
relay_handshake_seconds_bucket{le="0.05"} 1
relay_handshake_seconds_bucket{le="0.1"} 1
relay_handshake_seconds_bucket{le="0.25"} 1
relay_handshake_seconds_bucket{le="0.5"} 1
relay_handshake_seconds_bucket{le="1"} 1
relay_handshake_seconds_bucket{le="2.5"} 1
relay_handshake_seconds_bucket{le="5"} 2
relay_handshake_seconds_bucket{le="10"} 2
relay_handshake_seconds_bucket{le="+Inf"} 2
relay_handshake_seconds_sum 3.02
relay_handshake_seconds_count 2
`
