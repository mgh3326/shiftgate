# Scraping shiftgate status with Grafana Alloy

shiftgate has no daemon, so nothing serves `/metrics`. Instead, a scheduled
job turns `shiftgate status --json` into Prometheus text format in a
directory that Alloy's textfile collector reads. This uses three files from
[`examples/monitoring/`](../examples/monitoring/):

| File | Install as | Role |
|---|---|---|
| `shiftgate-status.jq` | `/etc/shiftgate/shiftgate-status.jq` | maps the JSON to metrics |
| `shiftgate-textfile.sh` | `/usr/local/bin/shiftgate-textfile` | runs status and jq, replaces the `.prom` file atomically |
| `config.alloy` | merge into the Alloy configuration | textfile collector, scrape, remote write |

## What `status --json` reports

For every blue-green service:

- `state`: the committed `state.json` (active and previous slot,
  `last_op`, `last_result`, `last_error`, `pending`, `hold`).
- `colors.<blue|green>`: `state` (Docker's: `running`, `exited`,
  `restarting`, … or `absent`), `digest`, `started_at`, `uptime_seconds`
  (0 unless running) and `restart_count` (restarts by Docker's restart
  policy; `check --heal` restarts are not counted here, they appear in
  `swaps.log` as `op: heal`).
- `last_op`: the newest `swaps.log` record (`op` is `deploy`, `rollback`
  or `heal`; `result`, `time`, `error`).
- `servers`: HAProxy's view of each server (`admin`, `op`).
- `errors`: anything shiftgate could not read.

`status` takes no lock and changes nothing, so it is safe to run every
minute.

## Generate the textfile

Run the script every minute from the same scheduler that runs `check`
(see the README, "Scheduling check, follow and prune"), as a user that may
run `shiftgate status`, for example in cron:

```cron
* * * * *  deploy  /usr/local/bin/shiftgate-textfile /var/lib/alloy/textfile 2>&1 | logger -t shiftgate-textfile
```

The directory must be writable by that user and readable by Alloy. If
`shiftgate status` or `jq` fails, the script exits non-zero and leaves the
previous file in place. Alert on its age (below), not on missing series.
Set `SHIFTGATE_CONFIG` when `services.yaml` is not at the default path.

The file looks like this (excerpt):

```text
# HELP shiftgate_active 1 for the colour that serves traffic.
# TYPE shiftgate_active gauge
shiftgate_active{service="web",color="green"} 1
# TYPE shiftgate_container_running gauge
shiftgate_container_running{service="web",color="blue"} 0
shiftgate_container_running{service="web",color="green"} 1
# TYPE shiftgate_container_uptime_seconds gauge
shiftgate_container_uptime_seconds{service="web",color="green"} 15147
# TYPE shiftgate_container_restart_count gauge
shiftgate_container_restart_count{service="web",color="green"} 2
# TYPE shiftgate_last_op_ok gauge
shiftgate_last_op_ok{service="web",op="deploy",result="ok"} 1
…
```

| Metric | Labels | Meaning |
|---|---|---|
| `shiftgate_active` | service, color | 1 for the colour that serves traffic |
| `shiftgate_container_running` | service, color | 1 when the container is running |
| `shiftgate_container_uptime_seconds` | service, color | seconds since start; 0 unless running |
| `shiftgate_container_restart_count` | service, color | Docker restart-policy restarts of this container |
| `shiftgate_pending` | service | 1 while an operation is uncommitted |
| `shiftgate_last_op_ok` | service, op, result | 1 when the newest `swaps.log` record succeeded |
| `shiftgate_last_op_timestamp_seconds` | service | time of that record |
| `shiftgate_haproxy_server_up` | service, server | 1 when HAProxy has the server ready and UP |
| `shiftgate_status_errors` | service | errors while reading state, Docker or HAProxy |

## Alloy

[`config.alloy`](../examples/monitoring/config.alloy) enables only the
`textfile` collector of `prometheus.exporter.unix`, scrapes it every 60
seconds and forwards to `prometheus.remote_write`. Replace the remote-write
URL with your own endpoint. If Alloy already runs a `prometheus.exporter.unix`
component for host metrics, add the `textfile` block to that component
instead of declaring a second one.

The configuration passes `alloy validate` (Alloy 1.19). Run against the
file above, the exporter served every `shiftgate_*` series with
`node_textfile_scrape_error 0`.

## Alerts

PromQL starting points:

```promql
# The serving colour is not running.
shiftgate_active * on (service, color) shiftgate_container_running == 0

# A container keeps crashing.
increase(shiftgate_container_restart_count[15m]) > 3

# An operation was interrupted; run `shiftgate recover <svc>`.
shiftgate_pending == 1

# The last deploy, rollback or heal failed.
shiftgate_last_op_ok == 0

# The textfile is stale: the generating job stopped.
time() - node_textfile_mtime_seconds{file=~".*/shiftgate.prom"} > 300
```

`shiftgate check --ping-url-env` covers the "is it up right now" question
from outside the host; these metrics add history and the restart and
deploy trail.
