package deploy_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/deploy"
	"github.com/mgh3326/shiftgate/internal/docker"
	"github.com/mgh3326/shiftgate/internal/haproxy"
	"github.com/mgh3326/shiftgate/internal/haproxy/haproxytest"
	"github.com/mgh3326/shiftgate/internal/state"
)

// fakeDocker is an in-memory docker.Engine.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*fakeCtr
	images     map[string]bool // digest
	calls      []string
	pullGate   chan struct{} // when non-nil, Pull blocks until closed
	pullEnter  chan struct{} // signalled when Pull starts
	failRemove map[string]bool
	pullErr    error
	// exitAfterRun makes a new container stop right after it starts.
	exitAfterRun bool
	// runOnceErr is returned by RunOnce (pre_deploy); runOnceOut is written.
	runOnceErr  error
	runOnceOut  string
	runOnceSeen []docker.RunSpec
	// restartErr fails Restart; restartKeepsHung leaves a hung container hung.
	restartErr       error
	restartKeepsHung bool
	// onInspect, when set, runs (unlocked) before each Inspect answers.
	onInspect func(name string)
}

type fakeCtr struct {
	labels   map[string]string
	running  bool
	port     int
	hung     bool // running but its health endpoint does not answer
	restarts int
	spec     docker.RunSpec
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{containers: map[string]*fakeCtr{}, images: map[string]bool{}, failRemove: map[string]bool{}}
}

func (d *fakeDocker) record(s string) {
	d.mu.Lock()
	d.calls = append(d.calls, s)
	d.mu.Unlock()
}

func (d *fakeDocker) Pull(ctx context.Context, ref string) error {
	d.record("pull " + ref)
	if d.pullEnter != nil {
		d.pullEnter <- struct{}{}
	}
	if d.pullGate != nil {
		<-d.pullGate
	}
	if d.pullErr != nil {
		return d.pullErr
	}
	_, digest, _ := strings.Cut(ref, "@")
	d.mu.Lock()
	d.images[digest] = true
	d.mu.Unlock()
	return nil
}

func (d *fakeDocker) Run(ctx context.Context, spec docker.RunSpec) error {
	d.record("run " + spec.Name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.containers[spec.Name]; ok {
		return fmt.Errorf("name in use")
	}
	hostPort := 0
	if spec.Publish != "" {
		parts := strings.Split(spec.Publish, ":")
		hostPort, _ = strconv.Atoi(parts[1])
	} else {
		hostPort, _ = strconv.Atoi(spec.Env["PORT"])
	}
	d.containers[spec.Name] = &fakeCtr{labels: spec.Labels, running: !d.exitAfterRun, port: hostPort, spec: spec}
	return nil
}

func (d *fakeDocker) RunOnce(ctx context.Context, spec docker.RunSpec, out io.Writer) error {
	d.record("runonce " + spec.Name)
	d.mu.Lock()
	d.runOnceSeen = append(d.runOnceSeen, spec)
	msg, err := d.runOnceOut, d.runOnceErr
	d.mu.Unlock()
	io.WriteString(out, msg)
	return err
}

func (d *fakeDocker) Restart(ctx context.Context, name string, _ time.Duration) error {
	d.record("restart " + name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.restartErr != nil {
		return d.restartErr
	}
	c, ok := d.containers[name]
	if !ok {
		return fmt.Errorf("no such container")
	}
	c.running, c.restarts = true, c.restarts+1
	if !d.restartKeepsHung {
		c.hung = false
	}
	return nil
}

func (d *fakeDocker) set(name string, fn func(*fakeCtr)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d.containers[name])
}

func (d *fakeDocker) callsWith(prefix string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, c := range d.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (d *fakeDocker) Start(ctx context.Context, name string) error {
	d.record("start " + name)
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[name]
	if !ok {
		return fmt.Errorf("no such container")
	}
	c.running = true
	return nil
}

func (d *fakeDocker) Stop(ctx context.Context, name string, _ time.Duration) error {
	d.record("stop " + name)
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[name]
	if !ok {
		return fmt.Errorf("no such container")
	}
	c.running = false
	return nil
}

func (d *fakeDocker) Remove(ctx context.Context, name string) error {
	d.record("rm " + name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failRemove[name] {
		return fmt.Errorf("remove refused")
	}
	delete(d.containers, name)
	return nil
}

func (d *fakeDocker) Inspect(ctx context.Context, name string) (docker.Container, bool, error) {
	if d.onInspect != nil {
		d.onInspect(name)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[name]
	if !ok {
		return docker.Container{}, false, nil
	}
	status := map[bool]string{true: "running", false: "exited"}[c.running]
	return docker.Container{ID: name, Running: c.running, Status: status, Labels: c.labels,
		StartedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), RestartCount: c.restarts}, true, nil
}

func (d *fakeDocker) ImageDigests(ctx context.Context, repo string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for dg := range d.images {
		out = append(out, dg)
	}
	return out, nil
}

func (d *fakeDocker) RemoveImage(ctx context.Context, ref string) error {
	d.record("rmi " + ref)
	_, digest, _ := strings.Cut(ref, "@")
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.containers {
		if c.labels[docker.LabelDigest] == digest {
			return fmt.Errorf("image is in use")
		}
	}
	delete(d.images, digest)
	return nil
}

func (d *fakeDocker) running(name string) (exists, running bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[name]
	if !ok {
		return false, false
	}
	return true, c.running
}

// fakeHealth passes when a running container publishes the port and its
// digest is not marked bad.
type fakeHealth struct {
	d   *fakeDocker
	mu  sync.Mutex
	bad map[string]bool
	// foreign marks ports where some other process answers the probe.
	foreign map[int]bool
}

func (h *fakeHealth) markBad(digest string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bad[digest] = true
}

func (h *fakeHealth) Probe(ctx context.Context, _ config.Health, addr string) error {
	_, p, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(p)
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.foreign[port] {
		return nil
	}
	for _, c := range h.d.containers {
		if c.port == port && c.running {
			if c.hung {
				return fmt.Errorf("context deadline exceeded")
			}
			if h.bad[c.labels[docker.LabelDigest]] {
				return fmt.Errorf("status 503")
			}
			return nil
		}
	}
	return fmt.Errorf("connection refused")
}

type harness struct {
	t      *testing.T
	cfg    *config.File
	gate   *deploy.Gate
	docker *fakeDocker
	health *fakeHealth
	fake   *haproxytest.Fake
	store  state.Store
	marker string // reload marker file; must never exist after deploys
}

const testYAML = `
state_dir: %[1]s/state
keep_digests: 2
haproxy:
  config: %[1]s/haproxy/haproxy.cfg
  socket: %[2]s
  check: ["sh", "-c", "test -s \"$1\"", "check", "{config}"]
  reload: ["sh", "-c", "echo reload >> %[1]s/reload.marker"]
  allowed_binds: ["127.0.0.1"]
services:
  - name: web
    image: registry.example.test/team/web
    binds: ["127.0.0.1:8080"]
    ports: {blue: 18080, green: 18081}
    container_port: 8080
    health: {http: /healthz, timeout: 300ms, interval: 20ms}
    drain_timeout: 200ms
    stop_timeout: 1s
`

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	fake := haproxytest.New(t, map[string][]string{"bk_web": {"blue", "green"}})
	cfg, err := config.Parse([]byte(fmt.Sprintf(testYAML, dir, fake.Path)))
	if err != nil {
		t.Fatal(err)
	}
	d := newFakeDocker()
	h := &fakeHealth{d: d, bad: map[string]bool{}, foreign: map[int]bool{}}
	g := &deploy.Gate{
		Cfg:         cfg,
		Store:       state.Store{Dir: cfg.StateDir},
		Docker:      d,
		Router:      haproxy.Runtime{Socket: fake.Path, Timeout: 300 * time.Millisecond},
		Health:      h,
		Poll:        10 * time.Millisecond,
		FlipTimeout: 300 * time.Millisecond,
		SyncConfig:  true,
	}
	return &harness{t: t, cfg: cfg, gate: g, docker: d, health: h, fake: fake, store: g.Store,
		marker: filepath.Join(dir, "reload.marker")}
}

func digest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

func (h *harness) state() *state.State {
	h.t.Helper()
	s, err := h.store.Load("web")
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) swaps() []string {
	h.t.Helper()
	b, err := os.ReadFile(h.store.LogPath("web"))
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (h *harness) mustDeploy(n int) {
	h.t.Helper()
	if _, err := h.gate.Deploy(context.Background(), "web", digest(n)); err != nil {
		h.t.Fatalf("deploy %d: %v", n, err)
	}
}

func (h *harness) noReload() {
	h.t.Helper()
	if _, err := os.Stat(h.marker); err == nil {
		h.t.Fatal("HAProxy was reloaded during a deploy/rollback; the flip must use the runtime API only")
	}
}

func runSpecFor(h *harness, c config.Color, d string) docker.RunSpec {
	svc, _ := h.cfg.Service("web")
	return deploy.RunSpec(svc, c, d)
}
