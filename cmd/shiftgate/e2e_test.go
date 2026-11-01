package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/haproxy"
	"github.com/mgh3326/shiftgate/internal/haproxy/haproxytest"
	"github.com/mgh3326/shiftgate/internal/state"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "shiftgate-bin")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "shiftgate")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type cli struct {
	t      *testing.T
	config string
}

// run executes the shiftgate binary and returns its exit code and output.
func (c cli) run(args ...string) (int, string) {
	c.t.Helper()
	cmd := exec.Command(binary, append([]string{"--config", c.config}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String()
	} else if err != nil {
		c.t.Fatal(err)
	}
	return 0, out.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func needDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").Run(); err != nil {
		if os.Getenv("SHIFTGATE_REQUIRE_DOCKER") == "1" {
			t.Fatalf("SHIFTGATE_REQUIRE_DOCKER=1 but docker is unavailable: %v", err)
		}
		t.Skip("docker unavailable (CI runs this test)")
	}
}

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// testImages starts a throwaway registry, builds three busybox images (two
// healthy versions and one whose /healthz is missing), pushes them and
// returns the repository and their digests.
func testImages(t *testing.T, tag string) (repo string, digests map[string]string) {
	t.Helper()
	sh(t, "docker", "pull", "-q", "registry:2")
	sh(t, "docker", "pull", "-q", "busybox:1.36")
	runOut := strings.Split(sh(t, "docker", "run", "-d", "--rm", "-p", "127.0.0.1::5000", "--label", "shiftgate.test="+tag, "registry:2"), "\n")
	reg := runOut[len(runOut)-1]
	t.Cleanup(func() { exec.Command("docker", "stop", reg).Run() })
	hostPort := sh(t, "docker", "port", reg, "5000/tcp")
	_, port, _ := net.SplitHostPort(strings.Split(hostPort, "\n")[0])
	repo = "localhost:" + port + "/shiftgate-e2e/" + tag
	dir := t.TempDir()
	dockerfile := `FROM busybox:1.36
ARG VERSION
ARG HEALTHY=1
RUN mkdir -p /www && echo "$VERSION" > /www/index.html && if [ "$HEALTHY" = 1 ]; then echo ok > /www/healthz; fi
CMD ["sh", "-c", "exec httpd -f -p ${PORT:-8080} -h /www"]
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	digests = map[string]string{}
	deadline := time.Now().Add(30 * time.Second)
	for _, v := range []struct{ name, healthy string }{{"v1", "1"}, {"v2", "1"}, {"bad", "0"}} {
		ref := repo + ":" + v.name
		sh(t, "docker", "build", "-q", "--build-arg", "VERSION="+v.name, "--build-arg", "HEALTHY="+v.healthy, "-t", ref, dir)
		t.Cleanup(func() { exec.Command("docker", "image", "rm", "-f", ref).Run() })
		var out []byte
		var err error
		for { // the registry may need a moment to accept connections
			out, err = exec.Command("docker", "push", ref).CombinedOutput()
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil {
			t.Fatalf("push %s: %v\n%s", ref, err, out)
		}
		i := strings.Index(string(out), "digest: sha256:")
		if i < 0 {
			t.Fatalf("no digest in push output:\n%s", out)
		}
		digests[v.name] = strings.Fields(string(out)[i+len("digest: "):])[0]
	}
	return repo, digests
}

type e2eEnv struct {
	cli
	svc      string
	frontend string // non-empty with a real HAProxy
	router   haproxy.Runtime
	store    state.Store
	marker   string
}

// newE2E writes services.yaml for one service and starts the router side:
// a real HAProxy when installed (CI), otherwise the fake admin socket.
func newE2E(t *testing.T, repo, tag string) *e2eEnv {
	t.Helper()
	dir := t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "sge2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "admin.sock")
	svc := "e2e-" + tag
	fe := freePort(t)
	blue, green := freePort(t), freePort(t)
	for green == blue {
		green = freePort(t)
	}
	haproxyBin, _ := exec.LookPath("haproxy")
	if haproxyBin == "" && os.Getenv("SHIFTGATE_REQUIRE_HAPROXY") == "1" {
		t.Fatal("SHIFTGATE_REQUIRE_HAPROXY=1 but haproxy is not on PATH")
	}
	if haproxyBin == "" {
		fake := haproxytest.New(t, map[string][]string{"bk_" + strings.ReplaceAll(svc, "-", "_"): {"blue", "green"}})
		sock = fake.Path
	}
	check := `["sh", "-c", "test -s \"$1\"", "check", "{config}"]`
	if haproxyBin != "" {
		check = fmt.Sprintf(`[%q, "-c", "-f", "{config}"]`, haproxyBin)
	}
	marker := filepath.Join(dir, "reload.marker")
	yaml := fmt.Sprintf(`
state_dir: %[1]s/state
keep_digests: 1
haproxy:
  config: %[1]s/haproxy.cfg
  socket: %[2]s
  check: %[3]s
  reload: ["sh", "-c", "echo reload >> %[4]s"]
  allowed_binds: ["127.0.0.1"]
  timeouts: {client: 10s, server: 10s}
services:
  - name: %[5]s
    image: %[6]s
    binds: ["127.0.0.1:%[7]d"]
    ports: {blue: %[8]d, green: %[9]d}
    container_port: 8080
    health: {http: /healthz, timeout: 20s, interval: 250ms}
    check: {inter: 200ms, fall: 1, rise: 1}
    drain_timeout: 2s
    stop_timeout: 1s
`, dir, sock, check, marker, svc, repo, fe, blue, green)
	cfgPath := filepath.Join(dir, "services.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	e := &e2eEnv{cli: cli{t: t, config: cfgPath}, svc: svc, router: haproxy.Runtime{Socket: sock},
		store: state.Store{Dir: cfg.StateDir}, marker: marker}
	t.Cleanup(func() {
		for _, c := range []string{"blue", "green"} {
			exec.Command("docker", "rm", "-f", "shiftgate-"+svc+"-"+c).Run()
		}
	})
	if haproxyBin == "" {
		t.Log("haproxy not installed: routing via the fake admin socket (CI uses a real HAProxy)")
		return e
	}
	// Real HAProxy: install the generated config without reload, then start it.
	if code, out := e.run("render", "--no-reload"); code != 0 {
		t.Fatalf("render: %d %s", code, out)
	}
	cmd := exec.Command(haproxyBin, "-db", "-f", cfg.HAProxy.Config)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	e.frontend = fmt.Sprintf("http://127.0.0.1:%d/", fe)
	for i := 0; i < 50; i++ {
		if _, err := e.router.Servers(context.Background(), cfg.Services[0].Backend); err == nil {
			return e
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("HAProxy admin socket did not come up")
	return nil
}

func (e *e2eEnv) state() *state.State {
	e.t.Helper()
	s, err := e.store.Load(e.svc)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *e2eEnv) servers() map[string]haproxy.ServerState {
	e.t.Helper()
	st, err := e.router.Servers(context.Background(), "bk_"+strings.ReplaceAll(e.svc, "-", "_"))
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *e2eEnv) container(color string) (exists, running bool) {
	out, err := exec.Command("docker", "container", "inspect", "--format", "{{.State.Running}}", "shiftgate-"+e.svc+"-"+color).Output()
	if err != nil {
		return false, false
	}
	return true, strings.TrimSpace(string(out)) == "true"
}

// served asserts what the HAProxy frontend returns (real HAProxy only).
func (e *e2eEnv) served(want string) {
	e.t.Helper()
	if e.frontend == "" {
		return
	}
	var body string
	for i := 0; i < 20; i++ {
		resp, err := http.Get(e.frontend)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = strings.TrimSpace(string(b))
			if body == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("frontend serves %q, want %q", body, want)
}

func (e *e2eEnv) noReload() {
	e.t.Helper()
	if _, err := os.Stat(e.marker); err == nil {
		e.t.Fatal("HAProxy was reloaded during deploy/rollback")
	}
}

func (e *e2eEnv) swapResults() []string {
	b, err := os.ReadFile(e.store.LogPath(e.svc))
	if err != nil {
		e.t.Fatal(err)
	}
	var res []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r state.SwapRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			e.t.Fatal(err)
		}
		res = append(res, r.Op+":"+string(r.ToColor)+":"+r.Result)
	}
	return res
}

// Acceptance 1, 2 and 3 against real Docker (and real HAProxy in CI).
func TestDockerDeployRollbackUnhealthy(t *testing.T) {
	needDocker(t)
	tag := "t" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
	repo, d := testImages(t, tag)
	e := newE2E(t, repo, tag)

	if code, out := e.run("deploy", e.svc, "--digest", d["v1"]); code != 0 {
		t.Fatalf("bootstrap deploy v1: exit %d\n%s", code, out)
	}
	e.served("v1")

	ok := t.Run("acceptance1_healthy_deploy_flips_drains_keeps_old", func(t *testing.T) {
		code, out := e.run("deploy", e.svc, "--digest", d["v2"])
		if code != 0 {
			t.Fatalf("deploy v2: exit %d\n%s", code, out)
		}
		st := e.state()
		if st.Active.Color != config.Green || st.Active.Digest != d["v2"] || st.Previous.Digest != d["v1"] || st.LastResult != "ok" || st.LastSwapAt.IsZero() {
			t.Fatalf("state: %+v", st)
		}
		if exists, running := e.container("blue"); !exists || running {
			t.Fatalf("old colour must be stopped but kept: exists=%v running=%v", exists, running)
		}
		srv := e.servers()
		if !srv["green"].Serving() || !srv["blue"].Maint() {
			t.Fatalf("HAProxy: %v", srv)
		}
		if got := e.swapResults(); strings.Join(got, ",") != "deploy:blue:ok,deploy:green:ok" {
			t.Fatalf("swap log: %v", got)
		}
		e.served("v2")
		e.noReload()
	})
	if !ok {
		return
	}
	ok = t.Run("acceptance3_rollback_restores_previous", func(t *testing.T) {
		code, out := e.run("rollback", e.svc)
		if code != 0 {
			t.Fatalf("rollback: exit %d\n%s", code, out)
		}
		st := e.state()
		if st.Active.Color != config.Blue || st.Active.Digest != d["v1"] {
			t.Fatalf("state after rollback: %+v", st)
		}
		if _, running := e.container("blue"); !running {
			t.Fatal("blue must be running again")
		}
		if !e.servers()["blue"].Serving() {
			t.Fatalf("HAProxy: %v", e.servers())
		}
		e.served("v1")
		e.noReload()
	})
	if !ok {
		return
	}
	t.Run("acceptance2_unhealthy_never_flips", func(t *testing.T) {
		code, out := e.run("deploy", e.svc, "--digest", d["bad"])
		if code == 0 {
			t.Fatalf("unhealthy deploy must exit non-zero:\n%s", out)
		}
		st := e.state()
		if st.Active.Color != config.Blue || st.Active.Digest != d["v1"] || st.LastResult != "unhealthy" || st.Pending != nil {
			t.Fatalf("state: %+v", st)
		}
		srv := e.servers()
		if !srv["blue"].Serving() || !srv["green"].Maint() {
			t.Fatalf("old colour must keep serving, new one untouched: %v", srv)
		}
		if exists, running := e.container("green"); !exists || running {
			t.Fatalf("unhealthy container must be stopped: exists=%v running=%v", exists, running)
		}
		e.served("v1")
		e.noReload()
		if code, out := e.run("status", "--json"); code != 0 || !strings.Contains(out, `"last_result": "unhealthy"`) {
			t.Fatalf("status: %d %s", code, out)
		}
	})
}

// Acceptance 4 across processes: a held service lock refuses a second deploy.
func TestCLIConcurrentDeployRefusedByLock(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.yaml")
	yaml := fmt.Sprintf(`
state_dir: %s/state
haproxy:
  socket: /nonexistent/admin.sock
  allowed_binds: ["127.0.0.1"]
services:
  - name: web
    image: ghcr.io/example/web
    binds: ["127.0.0.1:8080"]
    ports: {blue: 18080, green: 18081}
    health: {http: /healthz}
`, dir)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	lk, err := state.Store{Dir: filepath.Join(dir, "state")}.LockService("web") // the "first deploy"
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Unlock()
	code, out := cli{t: t, config: cfgPath}.run("deploy", "web", "--digest", "sha256:"+strings.Repeat("a", 64))
	if code != 75 || !strings.Contains(out, "locked") {
		t.Fatalf("want exit 75 (locked), got %d: %s", code, out)
	}
}

// Acceptance 5 at the CLI: a wildcard bind is refused without the flag.
func TestCLIRenderRefusesWildcard(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.yaml")
	yaml := fmt.Sprintf(`
state_dir: %[1]s/state
haproxy:
  config: %[1]s/haproxy.cfg
  check: ["sh", "-c", "test -s \"$1\"", "check", "{config}"]
  reload: ["true"]
  allowed_binds: ["127.0.0.1"]
services:
  - name: web
    image: ghcr.io/example/web
    binds: ["0.0.0.0:8080"]
    ports: {blue: 18080, green: 18081}
    health: {http: /healthz}
`, dir)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c := cli{t: t, config: cfgPath}
	if code, out := c.run("render", "--no-reload"); code != 78 || !strings.Contains(out, "wildcard") {
		t.Fatalf("want exit 78 wildcard refusal, got %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "haproxy.cfg")); err == nil {
		t.Fatal("refused render wrote the config")
	}
	if code, out := c.run("render", "--no-reload", "--allow-wildcard-bind"); code != 0 {
		t.Fatalf("explicit flag: %d %s", code, out)
	}
}
