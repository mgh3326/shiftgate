#!/bin/sh
# Writes shiftgate status as Prometheus metrics for Alloy's textfile
# collector. Run it every minute (cron, Dagu or a systemd timer).
#   usage: shiftgate-textfile.sh [DIR]   (default /var/lib/alloy/textfile)
#   SHIFTGATE_CONFIG   services.yaml path (default: shiftgate's own default)
#   SHIFTGATE_STATUS_JQ  jq program (default /etc/shiftgate/shiftgate-status.jq)
# On any failure the previous file is left in place and the script exits
# non-zero; alert on stale data with the file's mtime metric.
set -eu
dir=${1:-/var/lib/alloy/textfile}
jqprog=${SHIFTGATE_STATUS_JQ:-/etc/shiftgate/shiftgate-status.jq}
json=$(mktemp "$dir/.shiftgate.json.XXXXXX")
tmp=$(mktemp "$dir/.shiftgate.prom.XXXXXX")
trap 'rm -f "$json" "$tmp"' EXIT
# Not a pipeline: plain sh has no pipefail, and a failed status must not
# produce an empty (all series gone) file.
shiftgate ${SHIFTGATE_CONFIG:+--config "$SHIFTGATE_CONFIG"} status --json > "$json"
jq -r -f "$jqprog" "$json" > "$tmp"
chmod 0644 "$tmp"
mv "$tmp" "$dir/shiftgate.prom"   # atomic: the collector never reads half a file
