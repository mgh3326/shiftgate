# shiftgate

Blue-green deploys behind one shared HAProxy per host. shiftgate starts the
idle colour, health-checks it, flips traffic through the HAProxy runtime API
(no reload), drains and stops the old colour, and keeps that container so
you can roll back.

Every service on the host shares one HAProxy instance, and shiftgate
generates its configuration from `services.yaml`. shiftgate is a single
static Go binary with no daemon. You run it by hand, or CI runs it over SSH
with the digest it just pushed.

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
| `shiftgate status [--json]` | Show committed state, HAProxy server states and container states per service. |
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
- **Binds are allowlisted.** Every `bind` address must appear in
  `haproxy.allowed_binds`. A wildcard (`0.0.0.0`, `::`, `*`, empty host) is
  refused unless you pass `render --allow-wildcard-bind` or set
  `haproxy.allow_wildcard_bind: true`.

## services.yaml reference

```yaml
state_dir: /var/lib/shiftgate      # state.json, swaps.log and locks per service
keep_digests: 3                    # prune keeps this many recent promoted digests per service
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

  - name: legacy-unit              # a static route: rendered, never deployed
    binds: ["192.0.2.10:8768"]
    upstream: 127.0.0.1:8768
    health: {http: /health}
```

`run_args` may not set flags that shiftgate manages: `--name`, `--label`,
`--network`, `-p`, `--env-file`, `-e`, `--restart`, `-d`, `--rm`,
`--privileged`, `--entrypoint`, `--cap-add` and similar.

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

## CI deploys

[`examples/github-actions/shiftgate-deploy.yml`](examples/github-actions/shiftgate-deploy.yml)
is a reusable workflow. It builds and pushes to GHCR, joins the tailnet and
runs `shiftgate deploy <svc> --digest <pushed digest>` over SSH.
[`caller.yml`](examples/github-actions/caller.yml) shows how a repository
calls it. Restrict the deploy key with the forced command in
[`examples/ssh-forced-command.sh`](examples/ssh-forced-command.sh), which
accepts only `shiftgate deploy <svc> --digest sha256:<64 hex>`. Neither file
is wired to any repository here.

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

## License

MIT
