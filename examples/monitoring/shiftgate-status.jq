# Turns `shiftgate status --json` into Prometheus text exposition format for
# a node-exporter style textfile collector. See docs/monitoring.md.
def esc: tostring | gsub("\\\\"; "\\\\\\\\") | gsub("\""; "\\\"") | gsub("\n"; "\\n");
def labels($o): "{" + ([$o | to_entries[] | "\(.key)=\"\(.value | esc)\""] | join(",")) + "}";
def family($name; $type; $help; samples):
  "# HELP \($name) \($help)", "# TYPE \($name) \($type)",
  (samples | "\($name)\(labels(.[0])) \(.[1])");
def bg: .[] | select(.static | not);

family("shiftgate_active"; "gauge"; "1 for the colour that serves traffic.";
  bg | .name as $s | select(.state.active) | [{service: $s, color: .state.active.color}, 1]),
family("shiftgate_container_running"; "gauge"; "1 when the colour container is running.";
  bg | .name as $s | (.colors // {}) | to_entries[] | [{service: $s, color: .key}, (if .value.state == "running" then 1 else 0 end)]),
family("shiftgate_container_uptime_seconds"; "gauge"; "Seconds since the colour container started (0 unless running).";
  bg | .name as $s | (.colors // {}) | to_entries[] | [{service: $s, color: .key}, .value.uptime_seconds]),
family("shiftgate_container_restart_count"; "gauge"; "Restarts by the Docker restart policy since the container was created.";
  bg | .name as $s | (.colors // {}) | to_entries[] | [{service: $s, color: .key}, .value.restart_count]),
family("shiftgate_pending"; "gauge"; "1 while an operation is uncommitted (exit 3 needs `shiftgate recover`).";
  bg | [{service: .name}, (if .state.pending then 1 else 0 end)]),
family("shiftgate_last_op_ok"; "gauge"; "1 when the newest swaps.log record succeeded.";
  bg | select(.last_op) | [{service: .name, op: .last_op.op, result: .last_op.result}, (if .last_op.result == "ok" then 1 else 0 end)]),
family("shiftgate_last_op_timestamp_seconds"; "gauge"; "Unix time of the newest swaps.log record.";
  bg | select(.last_op) | [{service: .name}, (.last_op.time | sub("\\.[0-9]+"; "") | fromdateiso8601)]),
family("shiftgate_haproxy_server_up"; "gauge"; "1 when HAProxy has the server ready and UP.";
  .[] | .name as $s | (.servers // {}) | to_entries[] | [{service: $s, server: .key}, (if .value.admin == "ready" and .value.op == "up" then 1 else 0 end)]),
family("shiftgate_status_errors"; "gauge"; "Errors shiftgate hit while reading state, Docker or HAProxy.";
  .[] | [{service: .name}, ((.errors // []) | length)])
