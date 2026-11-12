# shiftgate

Blue-green deploys behind one shared HAProxy per host. shiftgate starts the
idle colour, health-checks it, flips traffic through the HAProxy runtime API
(no reload), drains and stops the old colour, and keeps that container so
you can roll back.

Every service on the host shares one HAProxy instance, and shiftgate
generates its configuration from `services.yaml`. shiftgate is a single
static Go binary with no daemon. You run it by hand, a scheduler on the host
runs `shiftgate follow` to deploy whatever CI last pushed (pull-based: CI
never reaches the host), and `shiftgate check --heal` restarts a hung
container.

```text
                      ┌──────────── one HAProxy per host ────────────┐
client ─▶ bind 127.0.0.1:8080 ─▶ backend bk_web ─┬─ server blue  127.0.0.1:18080  (ready)
                                                 └─ server green 127.0.0.1:18081  (maint)
shiftgate deploy web --digest sha256:… ─▶ start green ─▶ health ─▶ set green ready ─▶ set blue drain ─▶ stop blue
```

## Install

You need Go 1.27 or newer to build, plus Docker and HAProxy 2.8 or newer on
the host.

```sh
go install github.com/mgh3326/shiftgate/cmd/shiftgate@latest
# or build a static binary for the host
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o shiftgate ./cmd/shiftgate
sudo install -m 0755 shiftgate /usr/local/bin/shiftgate
```

Host setup, once:

1. Write `/etc/shiftgate/services.yaml`. Start from
   [`examples/services.yaml`](examples/services.yaml).
2. Let shiftgate own the HAProxy configuration file named in
   `haproxy.config`. The generated file declares the admin socket
   (`stats socket … level admin`) that shiftgate uses for flips.
3. If the images are private, log the host in to the registry once as the
   user that runs shiftgate (for GHCR: `docker login ghcr.io` with a token
   that has only `read:packages`). shiftgate pulls `image@digest` with
   Docker's stored credentials. Without them, a deploy aborts with
   `pull_failed` and the old colour keeps serving.
4. Run `sudo shiftgate render`. It generates the file, validates it with
   `haproxy -c`, installs it and reloads HAProxy. Every colour starts
   disabled until its first deploy.

shiftgate needs permission to use Docker, to write `state_dir` and the
HAProxy config file, and to open the admin socket. In practice it runs as
root or as a dedicated `deploy` user in the `docker` and `haproxy` groups.

## Commands

| Command | What it does |
|---|---|
| `shiftgate deploy <svc> --digest sha256:…` | Pull `image@digest`, start the idle colour, wait for health (bounded), set it `ready` and wait until HAProxy sees it UP, set the old colour to `drain`, commit state, wait for sessions to reach zero (or `drain_timeout`), set the old colour to `maint`, and `docker stop` it (the container is kept). |
| `shiftgate rollback <svc>` | Start the kept previous container if it is stopped, health-check it, and flip back. Refused if that container is gone. Running it again rolls forward. |
| `shiftgate recover <svc>` | Resolve an operation that was interrupted mid-flip (exit code 3): finish it or revert it after reading HAProxy's live state, or say why only a person can decide. |
| `shiftgate follow <svc> --tag <tag>` | Resolve `image:tag` to a digest through the registry HTTP API. If it differs from the active digest, run the normal deploy. A no-op when it is equal, or when the digest is **held** (see below). Exits 75 if another operation holds the service lock. |
| `shiftgate check [<svc>] [--heal] [--ping-url-env NAME]` | For each blue-green service: the active colour's container runs, its health endpoint answers within `health.timeout`, and HAProxy has it ready and UP. Prints one line per service and exits 1 if any fails. `--heal` restarts the failing active container once and re-checks; it never flips colours. `--ping-url-env` pings the URL in that variable (healthchecks style). |
| `shiftgate status [--json]` | Show committed state, HAProxy server states and container states per service. `--json` adds per-colour `state`, `digest`, `started_at`, `uptime_seconds` and `restart_count`, and `last_op` (the newest `swaps.log` record) for exporters; see [`docs/monitoring.md`](docs/monitoring.md). |
| `shiftgate render [--no-reload] [--dry-run] [--allow-wildcard-bind]` | Generate the HAProxy config from `services.yaml` and the committed state, validate it with `haproxy -c`, install it (the previous file is kept as `.previous`) and reload. |
| `shiftgate prune [<svc>]` | Remove local images of each service's repository, keeping active, previous, pending, anything a colour container uses, and the newest `keep_digests`. |

Global flag: `--config FILE` (default `/etc/shiftgate/services.yaml`).

Exit codes:

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | failed; the old colour is still serving |
| 2 | usage error |
| 3 | recovery required: run `shiftgate recover <svc>` |
| 75 | another operation holds the lock |
| 78 | configuration refused |

### Safety rules

- **A failed health check never flips traffic.** The new colour is stopped
  (kept for `docker logs`) and HAProxy is not touched.
- **A failed flip leaves the old colour serving.** The target is put back in
  maintenance only after a read-back proves the old colour serves again. If
  that cannot be proven, both containers are kept, the intent stays in
  `state.json`, and the command exits 3.
- **One deploy per service at a time.** A second deploy of the same service
  exits 75. `render` and `prune` take a host-wide lock, so a reload never
  interleaves with a flip.
- **Config is validated before every reload.** `render` runs
  `haproxy.check` (default `haproxy -c -f {config}`) on a candidate file
  first. Deploys never reload.
- **Secrets are referenced, never read.** `env_files` paths go to
  `docker run --env-file`. shiftgate never opens or prints them.
- **`pre_deploy` runs before the new colour starts.** A non-zero exit or a
  timeout aborts the deploy (`pre_deploy_failed`): the old colour keeps
  serving and the rollback container is untouched. The output goes to
  `<state_dir>/<svc>/ops/<op_id>.log` (mode 0600), named in `swaps.log`.
- **`check` never changes routing.** `--heal` only restarts the active
  container, once per run, under the service lock. It does not touch the
  other colour, HAProxy or `state.json`; each heal is recorded in
  `swaps.log` as `op: heal`. A container that is missing, not shiftgate's,
  or in HAProxy maintenance is reported, not restarted.
- **`follow` does not undo a rollback.** `rollback` holds the digest it
  moved away from, and a followed deploy that fails because of the image
  (`unhealthy`, `target_not_running`, or a `pre_deploy` command that ran
  and failed) holds its digest, so a bad push is not retried every minute.
  Host-side failures (pull, Docker, HAProxy, a `pre_deploy` that could not
  start, an interrupted run) are retried on the next run. `follow`
  deploys again only when the tag points at a new digest. An explicit
  `deploy` of the held digest clears the hold.
- **Binds are allowlisted.** Every `bind` address must appear in
  `haproxy.allowed_binds`. A wildcard (`0.0.0.0`, `::`, `*`, empty host) is
  refused unless you pass `render --allow-wildcard-bind` or set
  `haproxy.allow_wildcard_bind: true`.

## services.yaml reference

```yaml
state_dir: /var/lib/shiftgate      # state.json, swaps.log and locks per service
keep_digests: 3                    # prune keeps this many recent promoted digests per service
log: {max_size: 10m, max_file: 3}  # json-file log limits of every container (default 10m x 3)
haproxy:
  config: /etc/haproxy/haproxy.cfg # generated file (shiftgate owns it)
  socket: /run/haproxy/admin.sock  # admin socket shiftgate dials
  socket_in_config: ""             # socket path as HAProxy sees it (container); default = socket
  check: ["haproxy", "-c", "-f", "{config}"]   # validation command; {config} = candidate path
  reload: ["systemctl", "reload", "haproxy"]   # reload command (render only)
  allowed_binds: ["127.0.0.1", "192.0.2.10"]   # every address a frontend may bind
  allow_wildcard_bind: false
  log: stdout format raw local0
  timeouts: {connect: 5s, client: 1m, server: 1m, tunnel: 0s, http_request: 10s}
services:
  - name: web                      # [a-z][a-z0-9-]*; containers are shiftgate-web-blue / -green
    mode: http                     # http | tcp
    binds: ["127.0.0.1:8080"]      # frontend listeners (must be in allowed_binds)
    frontend: ft_web               # default ft_<name>
    backend: bk_web                # default bk_<name>; servers are named blue and green
    image: ghcr.io/example/web     # repository only; deploys pin @sha256
    ports: {blue: 18080, green: 18081}  # host ports, one per colour
    container_port: 8080           # published as 127.0.0.1:<colour port>:<container_port>
    network: ""                    # or "host": no publish; the app listens on the colour port
    env_files: [/etc/shiftgate/web.env]  # passed by path
    env: {LOG_FORMAT: json, APP_COLOR: "{color}"}   # {port} and {color} are expanded; PORT is always set
    run_args: ["--memory=512m", "-v", "/srv/web:/data"]  # extra docker run flags (managed ones refused)
    args: ["serve", "--port", "{port}"]  # command after the image
    health: {http: /healthz, expect_status: 200, timeout: 60s, interval: 1s}  # or {tcp: true}
    check: {inter: 2s, fall: 2, rise: 1}  # HAProxy's own server checks
    drain_timeout: 30s             # wait this long for sessions to finish before stopping the old colour
    stop_timeout: 10s              # docker stop grace period
    log: {max_file: 5}             # per-service override of the top-level log block, field by field; driver: daemon opts out
    pre_deploy:                    # optional one-shot command before the new colour starts
      args: ["alembic", "upgrade", "head"]   # replaces the image CMD; ENTRYPOINT is kept
      timeout: 10m                 # default 10m; on timeout the container is removed and the deploy aborts
      env_files:                   # optional; appended after service env_files (never given to colour containers)
        - /etc/web/migration.env
    registry_auth_file: /etc/shiftgate/registry.env  # optional; for `follow` on a private image

  - name: legacy-unit              # a static route: rendered, never deployed
    binds: ["192.0.2.10:8768"]
    upstream: 127.0.0.1:8768
    health: {http: /health}
```

`run_args` may not set flags that shiftgate manages: `--name`, `--label`,
`--network`, `-p`, `--env-file`, `-e`, `--restart`, `-d`, `--rm`,
`--privileged`, `--entrypoint`, `--cap-add`, `--log-driver`, `--log-opt` and
similar.

**Logs.** Every container shiftgate runs gets `--log-driver json-file
--log-opt max-size=<max_size> --log-opt max-file=<max_file>`. The driver is
pinned so the limits apply even when the Docker daemon's default driver is
different. The default is 10m x 3 (at most about 30 MB per container). The
limits apply when a container is created, so a running colour keeps its old
settings until its next deploy. To keep the Docker daemon's own logging
(journald, Loki, …), set `log: {driver: daemon}` at the top level or per
service: shiftgate then sets no log flags, and `run_args` may set
`--log-driver`/`--log-opt` again. A static upstream runs no container and
may not set `log`.

**`pre_deploy`.** The command runs as `docker run --rm` with the new image
(`image@digest`), the colour's `env_files` followed by any
`pre_deploy.env_files` (so migration-only env files never reach the serving
colour containers), `env` (with `{color}` and `{port}` of the colour being
deployed), `network` and `run_args` as the colour, no published port and no
restart policy. It runs after the pull and before anything else changes, **while the old colour still serves traffic**. A
database migration in `pre_deploy` must therefore be backward compatible
with the running version (expand, deploy, then contract in a later release).
That is the same rule a Kubernetes rolling update imposes. Move the
migration out of the container's start command, otherwise the new colour
would run it again on start.

**`registry_auth_file`.** An env-style file (mode 0600) with two keys,
`REGISTRY_USERNAME` and `REGISTRY_PASSWORD` (a read-only token is enough).
Only `follow` reads it, to resolve tags. `docker pull` still uses Docker's
own stored login, so a private image also needs `docker login` on the host.
Public images need neither.

## Walkthrough

```console
$ sudo shiftgate render
rendered /etc/haproxy/haproxy.cfg (validated) and reloaded

$ shiftgate deploy web --digest sha256:1111…
shiftgate: web: deploy sha256:1111… → blue (op 3f1c…)
shiftgate: web: blue now serves sha256:1111…
deploy web: nothing → blue@sha256:1111… (op 3f1c…)

$ shiftgate deploy web --digest sha256:2222…
shiftgate: web: deploy sha256:2222… → green (op 9a0e…)
shiftgate: web: green now serves sha256:2222…
deploy web: blue@sha256:1111… → green@sha256:2222… (op 9a0e…)

$ shiftgate status
SERVICE  ACTIVE  DIGEST               PREVIOUS                  LAST SWAP             RESULT  HAPROXY                              CONTAINERS
web      green   sha256:2222…         blue@sha256:1111…         2026-11-01T07:00:00Z  ok      blue=maint/down,green=ready/up       blue=stopped,green=running

$ shiftgate rollback web          # green → blue (starts the kept blue container)
$ shiftgate deploy web --digest sha256:bad…
shiftgate: web: green is unhealthy; HAProxy untouched, stopping it
shiftgate: deploy web: unhealthy: health check did not pass within 1m0s: … (the previous colour keeps serving)
$ echo $?
1
```

`swaps.log` keeps one JSON line per operation (`op_id`, from and to colour
and digest, `result`, `error`, `warnings`).

## Pull-based deploys

With `follow`, CI only builds and pushes. A scheduler on the host asks the
registry which digest a tag points at and deploys it through the normal
deploy path (pull, `pre_deploy`, health, flip, drain). Nothing that runs in
CI can reach the host: no SSH key, no tailnet access, no deploy credential.
The host needs only read access to the registry.

```console
$ shiftgate follow web --tag main          # main points at a new digest
deploy web: blue@sha256:1111… → green@sha256:2222… (op 9a0e…)
$ shiftgate follow web --tag main          # nothing new
follow web: main is sha256:2222…, already active
$ shiftgate rollback web                   # 2222 is bad; back to 1111
$ shiftgate follow web --tag main          # main still points at 2222
follow web: main is sha256:2222…, held (rolled_back, op 4c1d…); waiting for a new digest
```

[`examples/github-actions/build-push.yml`](examples/github-actions/build-push.yml)
is a push-only workflow for this setup.

`follow` resolves the tag with the OCI distribution API: a `HEAD` of
`/v2/<repo>/manifests/<tag>`, an anonymous bearer token for public images
(or `registry_auth_file` credentials for private ones), and the
`Docker-Content-Digest` header (or a hash of the manifest when a registry
omits it). For a multi-arch image that is the index digest, the same one
`docker pull image@digest` and `docker/build-push-action` report.

The hold lives in `state.json` (`hold: {digest, reason, op_id, at}`). It
is released when the tag resolves to any other digest, or when the held
digest is deployed explicitly with `shiftgate deploy`.

`follow` converges on the tag. If you `deploy` a digest by hand that the
tag does not point at, the next `follow` run replaces it with the tag's
digest (unless that digest is held). Pause the follow job while you run a
hand-picked version.

### Push-based CI over SSH (alternative)

[`examples/github-actions/shiftgate-deploy.yml`](examples/github-actions/shiftgate-deploy.yml)
is a reusable workflow. It builds and pushes to GHCR, joins the tailnet and
runs `shiftgate deploy <svc> --digest <pushed digest>` over SSH.
[`caller.yml`](examples/github-actions/caller.yml) shows how a repository
calls it. Restrict the deploy key with the forced command in
[`examples/ssh-forced-command.sh`](examples/ssh-forced-command.sh), which
accepts only `shiftgate deploy <svc> --digest sha256:<64 hex>`. Neither file
is wired to any repository here.

## Scheduling check, follow and prune

shiftgate has no daemon; a scheduler runs it. Three jobs cover what a
cluster would do continuously:

| Job | Schedule | What it replaces |
|---|---|---|
| `shiftgate follow <svc> --tag main` | every minute, one per followed service | CI pushing a deploy over SSH; image automation in a cluster |
| `shiftgate check --heal --ping-url-env HC_PING_URL` | every minute | liveness probes, and an external dead-man's switch |
| `shiftgate prune` | daily | kubelet image garbage collection |

Exit code 75 means another operation holds the lock; the next run retries,
so treat it as success. Run `check` independently of `follow`: a failed
deploy must not stop the monitor. While a deploy is pulling or running
`pre_deploy`, `check` keeps probing the colour that still serves (it reports
but cannot heal it, because the deploy holds the lock); during a flip it
skips the service.

**Dagu.** [`examples/dagu/`](examples/dagu/) has one DAG per job
(`shiftgate-follow-web.yaml`, `shiftgate-check.yaml`,
`shiftgate-prune.yaml`), written for Dagu 2.x field names and validated with
Dagu 2.21. The check DAG loads the ping URL from
`/etc/shiftgate/dagu.env` with `dotenv`.

**cron.** The same with `/etc/cron.d/shiftgate` (the user `deploy` is the
one that runs shiftgate; `logger` sends the output to the journal, which
rotates it):

```cron
SHELL=/bin/sh
PATH=/usr/local/bin:/usr/bin:/bin
# /etc/shiftgate/cron.env (mode 0600): HC_PING_URL=https://hc-ping.example/<uuid>
* * * * *  deploy  shiftgate follow web --tag main 2>&1 | logger -t shiftgate-follow
* * * * *  deploy  set -a; . /etc/shiftgate/cron.env; set +a; shiftgate check --heal --ping-url-env HC_PING_URL 2>&1 | logger -t shiftgate-check
17 4 * * * deploy  shiftgate prune 2>&1 | logger -t shiftgate-prune
```

`set -a` exports the sourced variable to `shiftgate`; without it, `check`
would not see the URL. The ping URL is a secret: `check` reads it from the
variable named by `--ping-url-env` and never prints it. It POSTs the
report to the URL on success and to `<url>/fail` when any service is still
failing, so the ping service alerts both on failures and when checks stop
arriving.

## k3s → shiftgate

For one service on one host, shiftgate replaces a single-node k3s cluster
with Docker, HAProxy and a scheduler. What changes:

| Need | k3s | shiftgate |
|---|---|---|
| Zero-downtime release | Deployment rolling update gated by readiness probes | `deploy`: start the idle colour, health gate, runtime-API flip, drain |
| Rollback | `kubectl rollout undo` (scales the previous ReplicaSet back up) | `rollback`: starts the kept previous container, no pull; one step back only |
| Restart a crashed container | kubelet, CrashLoopBackOff | Docker `--restart unless-stopped`; `restart_count` in `status --json` |
| Restart a hung container | liveness probe, every few seconds | `check --heal` from the scheduler: once per run (per minute), no backoff |
| Memory and CPU limits | `resources.limits` | `run_args: ["--memory=512m", "--cpus=1"]` |
| Migrations before the new version | Helm `pre-upgrade` hook, Job or init container | `pre_deploy`; it runs while the old colour serves, so it must be backward compatible |
| Log rotation | kubelet (`containerLogMaxSize` 10Mi, 5 files) | `log: {max_size, max_file}`, default 10m x 3 |
| Image garbage collection | kubelet, at disk-usage thresholds | `prune` on a schedule; keeps `keep_digests`, never disk-triggered |
| Deploy on image push | Flux or Argo CD image automation, Keel | `follow --tag` on a schedule; holds rolled-back and failed digests |
| Metrics | kube-state-metrics, metrics-server | `status --json` → textfile → Alloy ([`docs/monitoring.md`](docs/monitoring.md)); container CPU and memory need a Docker integration such as cAdvisor |
| Ingress and service routing | Ingress controller, Service, kube-proxy | one generated HAProxy, colour ports on loopback |
| Secrets | Secret objects, RBAC | `env_files` by path; Unix permissions only |

**You gain:** no control plane, datastore or CNI to run and upgrade on a
small (ARM) host; an explicit health gate and drain per release; rollback
without a pull; an audit line per operation in `swaps.log`; deploys that
need no inbound access from CI.

**You give up:** more than one node, scheduling and failover between nodes;
probes evaluated by an always-on agent (check runs per minute instead);
declarative desired state reconciled continuously; `kubectl` and the
Kubernetes API, Helm charts and operators; CronJob, DaemonSet and
StatefulSet semantics (use the scheduler and Docker volumes); network
policies.

## Migrating a service that runs as a plain systemd binary

Say `web` runs today as `web.service` (`ExecStart=/opt/web/web
--port 8080`), and HAProxy, or nothing, forwards `:80` to it.

1. **Containerise it.** Build an image whose process listens on `$PORT`
   (or on `{port}` passed through `args`), with an HTTP health endpoint.
   Push it and note the digest.
2. **Move the environment.** Copy the unit's `Environment=` and
   `EnvironmentFile=` contents into an env file such as
   `/etc/shiftgate/web.env` (mode 0600) and list it under `env_files`. If
   the binary reads files or sockets on the host, mount them with
   `run_args: ["-v", "/srv/web:/srv/web"]`. For host networking (the binary
   expected `127.0.0.1` services), set `network: host` and pass the port
   through `args`.
3. **Pick ports.** Keep the public listener as the HAProxy `bind`
   (`127.0.0.1:8080`). Choose two free colour ports (`18080`, `18081`).
   They must not equal the bind port on loopback, so the old binary and the
   new colours can coexist during cut-over.
4. **Keep the routes you already have, then render.** `render` replaces
   the whole HAProxy file with what `services.yaml` declares, so first
   declare every route HAProxy must keep serving. For example, the frontend
   that forwards traffic to the old binary today becomes a static service:
   `{name: web-legacy, binds: ["127.0.0.1:8080"], upstream: "127.0.0.1:8081", health: {http: /healthz}}`.
   Add `web` itself on a temporary bind (for example `127.0.0.1:8090`),
   check the result with `shiftgate render --dry-run`, and then run
   `shiftgate render`. Until you are sure, you can also leave your
   hand-written config in place and point `haproxy.config` at a separate
   file.
5. **First deploy, then cut over.** `shiftgate deploy web --digest sha256:…`
   starts blue, health-checks it, and enables it in HAProxy on the temporary
   bind. Check it there. Then remove `web-legacy`, move `web`'s bind to the
   real address (`127.0.0.1:8080`), run `shiftgate render`, and stop the old
   unit (`systemctl disable --now web.service`).
6. **Rollback during migration.** Until a second shiftgate deploy exists,
   `shiftgate rollback` has nothing to go back to. To go back, re-enable
   `web.service`, restore the `web-legacy` static route on the real bind,
   and run `render`.
7. **Clean up.** After a few deploys, `shiftgate prune web` removes old
   images. Delete the old binary and unit when you no longer need them.

## Development

```sh
go vet ./...
go test -p 2 ./...     # Docker and HAProxy tests skip when missing locally
SHIFTGATE_REQUIRE_DOCKER=1 SHIFTGATE_REQUIRE_HAPROXY=1 go test ./...   # what CI runs
```

[`docs/design.md`](docs/design.md) describes the data shape, the state
machine, the design consult and the first-adopter mapping.
[`docs/monitoring.md`](docs/monitoring.md) shows how to scrape
`status --json` with Grafana Alloy.

## License

MIT
