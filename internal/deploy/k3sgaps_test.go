package deploy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/deploy"
	"github.com/mgh3326/shiftgate/internal/haproxy/haproxytest"
	"github.com/mgh3326/shiftgate/internal/registry"
	"github.com/mgh3326/shiftgate/internal/state"
)

// --- check -----------------------------------------------------------------

func checkOne(t *testing.T, h *harness, heal bool) (deploy.CheckResult, error) {
	t.Helper()
	res, err := h.gate.Check(context.Background(), "web", heal)
	if len(res) != 1 {
		t.Fatalf("check results: %+v (err %v)", res, err)
	}
	return res[0], err
}

// Acceptance 1: check fails on a stopped container, a failed health check
// and HAProxy DOWN, each with its own reason.
func TestCheckFailures(t *testing.T) {
	cases := map[string]struct {
		break_ func(h *harness)
		kind   string
		reason string
	}{
		"stopped container": {func(h *harness) {
			h.docker.set("shiftgate-web-green", func(c *fakeCtr) { c.running = false })
		}, deploy.KindStopped, "not running"},
		"failed health": {func(h *harness) {
			h.docker.set("shiftgate-web-green", func(c *fakeCtr) { c.hung = true })
		}, deploy.KindUnhealthy, "health check did not pass"},
		"haproxy down": {func(h *harness) {
			h.fake.Set("bk_web", "green", func(s *haproxytest.Server) { s.Op = 0 })
		}, deploy.KindDown, "DOWN"},
		"haproxy maint": {func(h *harness) {
			h.fake.Set("bk_web", "green", func(s *haproxytest.Server) { s.Admin = 0x01 })
		}, deploy.KindRouting, "maint"},
		"container missing": {func(h *harness) {
			h.docker.Remove(context.Background(), "shiftgate-web-green")
		}, deploy.KindMissing, "does not exist"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.mustDeploy(1)
			h.mustDeploy(2) // green active, blue kept stopped
			if r, err := checkOne(t, h, false); err != nil || r.Failed() || r.Color != config.Green {
				t.Fatalf("healthy service should pass: %+v %v", r, err)
			}
			c.break_(h)
			r, err := checkOne(t, h, false)
			if deploy.ExitCode(err) != deploy.CodeFailed {
				t.Fatalf("want exit %d, got %v", deploy.CodeFailed, err)
			}
			if r.Kind != c.kind || !strings.Contains(r.Reason, c.reason) || strings.Contains(r.Reason, "\n") {
				t.Fatalf("result %+v, want kind %s with %q on one line", r, c.kind, c.reason)
			}
			if got := h.docker.callsWith("restart"); len(got) != 0 {
				t.Fatalf("check without --heal restarted: %v", got)
			}
		})
	}
}

// Acceptance 1: --heal restarts only the active container, once, and never
// flips HAProxy or changes the committed state.
func TestCheckHealRestartsActiveOnceNeverFlips(t *testing.T) {
	for _, tc := range []struct {
		name     string
		break_   func(h *harness)
		stillBad bool
	}{
		{"hung container recovers", func(h *harness) {
			h.docker.set("shiftgate-web-green", func(c *fakeCtr) { c.hung = true })
		}, false},
		{"stopped container recovers", func(h *harness) {
			h.docker.set("shiftgate-web-green", func(c *fakeCtr) { c.running = false })
		}, false},
		{"still hung after restart", func(h *harness) {
			h.docker.restartKeepsHung = true
			h.docker.set("shiftgate-web-green", func(c *fakeCtr) { c.hung = true })
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustDeploy(1)
			h.mustDeploy(2)
			before := h.state()
			setBefore := setCommands(h.fake.Commands())
			tc.break_(h)
			r, err := checkOne(t, h, true)
			if got := h.docker.callsWith("restart"); !slices.Equal(got, []string{"restart shiftgate-web-green"}) {
				t.Fatalf("restarts = %v, want exactly the active container once", got)
			}
			if got := h.docker.callsWith("start "); len(got) != 0 {
				t.Fatalf("heal must not start other containers: %v", got)
			}
			if exists, running := h.docker.running("shiftgate-web-blue"); !exists || running {
				t.Fatalf("the idle colour must stay stopped: exists=%v running=%v", exists, running)
			}
			if after := setCommands(h.fake.Commands()); !slices.Equal(setBefore, after) {
				t.Fatalf("heal changed HAProxy: %v", after[len(setBefore):])
			}
			after := h.state()
			if *after.Active != *before.Active || after.Generation != before.Generation || after.Pending != nil {
				t.Fatalf("heal changed state: before %+v after %+v", before, after)
			}
			last := h.swaps()[len(h.swaps())-1]
			if !strings.Contains(last, `"op":"heal"`) {
				t.Fatalf("heal not audited: %s", last)
			}
			if tc.stillBad {
				if deploy.ExitCode(err) != deploy.CodeFailed || r.Healed || !strings.Contains(r.Reason, "restarted shiftgate-web-green once") {
					t.Fatalf("want a failure after one restart: %+v %v", r, err)
				}
				if !strings.Contains(last, `"result":"failed"`) {
					t.Fatalf("audit: %s", last)
				}
				return
			}
			if err != nil || !r.Healed || r.Failed() || r.HealedFrom == "" {
				t.Fatalf("want healed: %+v %v", r, err)
			}
		})
	}
}

func TestCheckHealDoesNotTouchMaintOrForeign(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	h.fake.Set("bk_web", "blue", func(s *haproxytest.Server) { s.Admin = 0x01 })
	if r, _ := checkOne(t, h, true); r.Kind != deploy.KindRouting {
		t.Fatalf("maint: %+v", r)
	}
	h.fake.Set("bk_web", "blue", func(s *haproxytest.Server) { s.Admin = 0 })
	h.docker.set("shiftgate-web-blue", func(c *fakeCtr) {
		c.labels = map[string]string{"shiftgate.service": "web", "shiftgate.color": "blue", "shiftgate.digest": digest(5)}
		c.running = false
	})
	if r, _ := checkOne(t, h, true); r.Kind != deploy.KindForeign {
		t.Fatalf("foreign: %+v", r)
	}
	if got := h.docker.callsWith("restart"); len(got) != 0 {
		t.Fatalf("restarted a container check must not touch: %v", got)
	}
}

func TestCheckHealSkipsWhileLocked(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	h.docker.set("shiftgate-web-blue", func(c *fakeCtr) { c.hung = true })
	lk, err := h.store.LockService("web")
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Unlock()
	r, err := checkOne(t, h, true)
	if deploy.ExitCode(err) != deploy.CodeFailed || !strings.Contains(r.Reason, "not healed") {
		t.Fatalf("heal under a held lock: %+v %v", r, err)
	}
	if got := h.docker.callsWith("restart"); len(got) != 0 {
		t.Fatalf("restarted while another operation held the lock: %v", got)
	}
}

func TestCheckSkipsAndPending(t *testing.T) {
	h := newHarness(t)
	if r, err := checkOne(t, h, false); err != nil || r.Skipped == "" {
		t.Fatalf("never deployed should be skipped: %+v %v", r, err)
	}
	h.mustDeploy(1)
	st := h.state()
	st.Pending = &state.Intent{OpID: "x", Op: "deploy", Phase: state.PhaseFlipping, From: st.Active,
		To: state.Slot{Color: config.Green, Digest: digest(2)}}
	if err := h.store.Save(st); err != nil {
		t.Fatal(err)
	}
	if r, _ := checkOne(t, h, false); r.Kind != deploy.KindPending || !strings.Contains(r.Reason, "recover") {
		t.Fatalf("stale pending intent: %+v", r)
	}
	lk, _ := h.store.LockService("web")
	defer lk.Unlock()
	if r, err := checkOne(t, h, false); err != nil || r.Skipped == "" {
		t.Fatalf("pending with the lock held is an operation in progress: %+v %v", r, err)
	}
}

// --- pre_deploy ------------------------------------------------------------

// Acceptance 2: a failing pre_deploy aborts with the old colour serving, no
// flip, the target slot untouched and the output captured to the op log.
func TestPreDeployFailureAbortsWithoutFlip(t *testing.T) {
	h := newHarness(t)
	svc, _ := h.cfg.Service("web")
	svc.PreDeploy = &config.PreDeploy{Args: []string{"migrate", "--color", "{color}"}, Timeout: config.Duration(1e9)}
	h.mustDeploy(1)
	h.mustDeploy(2) // green active, blue (digest 1) kept for rollback
	setBefore := setCommands(h.fake.Commands())
	h.docker.runOnceOut = "applying 0042_add_column\nERROR: relation exists\n"
	h.docker.runOnceErr = errors.New("exit status 3")
	_, err := h.gate.Deploy(context.Background(), "web", digest(3))
	if deploy.ExitCode(err) != deploy.CodeFailed || !strings.Contains(err.Error(), "pre_deploy_failed") {
		t.Fatalf("want pre_deploy abort, got %v", err)
	}
	if after := setCommands(h.fake.Commands()); !slices.Equal(setBefore, after) {
		t.Fatalf("HAProxy changed: %v", after[len(setBefore):])
	}
	if g := h.fake.Get("bk_web", "green"); g.Admin&^0x04 != 0 || g.Op != 2 {
		t.Fatalf("green must keep serving: %+v", g)
	}
	st := h.state()
	if st.Active.Digest != digest(2) || st.LastResult != "pre_deploy_failed" || st.Pending != nil || !st.RollbackAvailable {
		t.Fatalf("state: %+v", st)
	}
	// The target slot (blue, the rollback target) was never replaced.
	if c := h.docker.containers["shiftgate-web-blue"]; c == nil || c.labels["shiftgate.digest"] != digest(1) {
		t.Fatalf("rollback container replaced: %+v", c)
	}
	if got := h.docker.callsWith("run "); len(got) != 2 {
		t.Fatalf("the new colour must not start: %v", got)
	}
	spec := h.docker.runOnceSeen[len(h.docker.runOnceSeen)-1]
	if !spec.OneShot || spec.Publish != "" || spec.Image != "registry.example.test/team/web@"+digest(3) ||
		!slices.Equal(spec.Args, []string{"migrate", "--color", "blue"}) {
		t.Fatalf("pre_deploy spec: %+v", spec)
	}
	// Output captured to the op log named in the swap record.
	last := h.swaps()[len(h.swaps())-1]
	if !strings.Contains(last, `"result":"pre_deploy_failed"`) || !strings.Contains(last, `"op_log":`) {
		t.Fatalf("swap record: %s", last)
	}
	rec, err := h.store.LastSwap("web")
	if err != nil || rec.OpLog == "" || filepath.Dir(rec.OpLog) != filepath.Join(h.cfg.StateDir, "web", "ops") {
		t.Fatalf("last swap: %+v %v", rec, err)
	}
	if b, _ := os.ReadFile(rec.OpLog); !strings.Contains(string(b), "ERROR: relation exists") {
		t.Fatalf("op log content: %q", b)
	}
	// Rollback still works.
	if _, err := h.gate.Rollback(context.Background(), "web"); err != nil {
		t.Fatalf("rollback after pre_deploy failure: %v", err)
	}
}

func TestPreDeployRunsBeforeTargetStarts(t *testing.T) {
	h := newHarness(t)
	svc, _ := h.cfg.Service("web")
	svc.PreDeploy = &config.PreDeploy{Args: []string{"migrate"}, Timeout: config.Duration(1e9)}
	h.mustDeploy(1)
	calls := h.docker.calls
	i, j := slices.Index(calls, "runonce shiftgate-web-pre-deploy"), slices.Index(calls, "run shiftgate-web-blue")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("pre_deploy must run before the colour starts: %v", calls)
	}
}

// --- log options -----------------------------------------------------------

// Acceptance 4: docker run args carry the log options from the top-level
// default and from a per-service override.
func TestRunArgsCarryLogOptions(t *testing.T) {
	cfg, err := config.Parse([]byte(`
log: {max_size: 20m}
haproxy:
  allowed_binds: ["127.0.0.1"]
services:
  - name: web
    image: ghcr.io/example/web
    binds: ["127.0.0.1:8080"]
    ports: {blue: 18080, green: 18081}
    health: {http: /healthz}
    pre_deploy: {args: [migrate]}
  - name: api
    image: ghcr.io/example/api
    binds: ["127.0.0.1:8081"]
    ports: {blue: 18090, green: 18091}
    health: {http: /healthz}
    log: {max_file: 5}
`))
	if err != nil {
		t.Fatal(err)
	}
	pairs := func(args []string) []string {
		var out []string
		for i, a := range args {
			if (a == "--log-driver" || a == "--log-opt") && i+1 < len(args) {
				out = append(out, a+" "+args[i+1])
			}
		}
		return out
	}
	web, _ := cfg.Service("web")
	api, _ := cfg.Service("api")
	for _, c := range []struct {
		name string
		args []string
		want []string
	}{
		{"default+top-level", deploy.RunSpec(web, config.Blue, digest(1)).ArgList(),
			[]string{"--log-driver json-file", "--log-opt max-size=20m", "--log-opt max-file=3"}},
		{"service override", deploy.RunSpec(api, config.Green, digest(1)).ArgList(),
			[]string{"--log-driver json-file", "--log-opt max-size=20m", "--log-opt max-file=5"}},
		{"pre_deploy", deploy.PreDeploySpec(web, config.Blue, digest(1)).ArgList(),
			[]string{"--log-driver json-file", "--log-opt max-size=20m", "--log-opt max-file=3"}},
	} {
		if got := pairs(c.args); !slices.Equal(got, c.want) {
			t.Errorf("%s: log args %v, want %v (full %v)", c.name, got, c.want, c.args)
		}
	}
	// The documented default is 10m x 3.
	def, err := config.Parse([]byte(strings.Replace(`
haproxy:
  allowed_binds: ["127.0.0.1"]
services:
  - name: web
    image: ghcr.io/example/web
    binds: ["127.0.0.1:8080"]
    ports: {blue: 18080, green: 18081}
    health: {http: /healthz}
`, "\t", "", -1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := pairs(deploy.RunSpec(def.Services[0], config.Blue, digest(1)).ArgList()); !slices.Equal(got,
		[]string{"--log-driver json-file", "--log-opt max-size=10m", "--log-opt max-file=3"}) {
		t.Fatalf("default log args: %v", got)
	}
	// driver: daemon renders no log flags at all.
	web.Log.Driver = config.LogDaemon
	if got := pairs(deploy.RunSpec(web, config.Blue, digest(1)).ArgList()); len(got) != 0 {
		t.Fatalf("daemon driver must not pin log flags: %v", got)
	}
	web.Log.Driver = config.LogJSONFile
	// pre_deploy is one-shot: removed on exit, no restart policy, no publish.
	pd := deploy.PreDeploySpec(web, config.Blue, digest(1)).ArgList()
	if !slices.Contains(pd, "--rm") || slices.Contains(pd, "--restart") || slices.Contains(pd, "-d") || slices.Contains(pd, "-p") {
		t.Fatalf("pre_deploy args: %v", pd)
	}
}

// --- follow ----------------------------------------------------------------

// fakeRegistry is an OCI distribution endpoint with a bearer token realm.
type fakeRegistry struct {
	srv *httptest.Server
	mu  sync.Mutex
	tag map[string]string // tag → digest
	// wantBasic, when set, is required on token requests.
	wantUser, wantPass string
	tokenCalls         int
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	r := &fakeRegistry{tag: map[string]string{}}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch {
		case req.URL.Path == "/token":
			r.tokenCalls++
			if req.URL.Query().Get("scope") != "repository:team/web:pull" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			if r.wantUser != "" {
				if u, p, ok := req.BasicAuth(); !ok || u != r.wantUser || p != r.wantPass {
					http.Error(w, "denied", http.StatusUnauthorized)
					return
				}
			}
			w.Write([]byte(`{"token":"tok123"}`))
		case strings.HasPrefix(req.URL.Path, "/v2/team/web/manifests/"):
			if req.Header.Get("Authorization") != "Bearer tok123" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.srv.URL+`/token",service="fake"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			d, ok := r.tag[strings.TrimPrefix(req.URL.Path, "/v2/team/web/manifests/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Docker-Content-Digest", d)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRegistry) push(tag, d string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tag[tag] = d
}

func withRegistry(t *testing.T, h *harness) *fakeRegistry {
	reg := newFakeRegistry(t)
	h.gate.Registry = registry.Client{HTTP: reg.srv.Client(), Endpoint: func(string) string { return reg.srv.URL }}
	return reg
}

func follow(t *testing.T, h *harness) (*deploy.FollowResult, error) {
	t.Helper()
	return h.gate.Follow(context.Background(), "web", "main")
}

// Acceptance 3: follow deploys a new digest, is a no-op on an equal digest,
// refuses a rolled-back digest until a newer one appears.
func TestFollowDeploysNoopsAndHoldsRolledBack(t *testing.T) {
	h := newHarness(t)
	reg := withRegistry(t, h)

	reg.push("main", digest(1))
	if r, err := follow(t, h); err != nil || r.Action != deploy.FollowDeployed || h.state().Active.Digest != digest(1) {
		t.Fatalf("first follow: %+v %v", r, err)
	}
	before := len(h.docker.callsWith("pull"))
	if r, err := follow(t, h); err != nil || r.Action != deploy.FollowUpToDate {
		t.Fatalf("equal digest: %+v %v", r, err)
	}
	if got := len(h.docker.callsWith("pull")); got != before {
		t.Fatal("a no-op follow must not pull or deploy")
	}

	reg.push("main", digest(2))
	if r, err := follow(t, h); err != nil || r.Action != deploy.FollowDeployed || r.Deploy.To.Digest != digest(2) {
		t.Fatalf("new digest: %+v %v", r, err)
	}

	// The operator rolls back: digest 2 is held even though main still points at it.
	if _, err := h.gate.Rollback(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	st := h.state()
	if st.Active.Digest != digest(1) || st.Hold == nil || st.Hold.Digest != digest(2) || st.Hold.Reason != "rolled_back" {
		t.Fatalf("rollback must persist a hold: %+v", st)
	}
	gen := st.Generation
	for range 2 {
		r, err := follow(t, h)
		if err != nil || r.Action != deploy.FollowHeld {
			t.Fatalf("held digest: %+v %v", r, err)
		}
	}
	if st := h.state(); st.Active.Digest != digest(1) || st.Generation != gen {
		t.Fatalf("follow redeployed a rolled-back digest: %+v", st)
	}

	// A newer push releases the hold and deploys.
	reg.push("main", digest(3))
	if r, err := follow(t, h); err != nil || r.Action != deploy.FollowDeployed || r.Deploy.To.Digest != digest(3) {
		t.Fatalf("newer digest: %+v %v", r, err)
	}
	if st := h.state(); st.Hold != nil || st.Active.Digest != digest(3) {
		t.Fatalf("hold should be released: %+v", st)
	}
}

// Acceptance 3: follow returns 75 when a deploy holds the service lock.
func TestFollowLockedByDeploy(t *testing.T) {
	h := newHarness(t)
	reg := withRegistry(t, h)
	reg.push("main", digest(2))
	h.docker.pullGate = make(chan struct{})
	h.docker.pullEnter = make(chan struct{}, 1)
	first := make(chan error, 1)
	go func() {
		_, err := h.gate.Deploy(context.Background(), "web", digest(1))
		first <- err
	}()
	<-h.docker.pullEnter
	_, err := follow(t, h)
	if !errors.Is(err, state.ErrLocked) || deploy.ExitCode(err) != deploy.CodeLocked {
		t.Fatalf("follow during a deploy: want exit %d, got %v", deploy.CodeLocked, err)
	}
	close(h.docker.pullGate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	h.docker.pullGate, h.docker.pullEnter = nil, nil
	if r, err := follow(t, h); err != nil || r.Action != deploy.FollowDeployed {
		t.Fatalf("follow after the deploy: %+v %v", r, err)
	}
}

// A followed digest whose deploy fails is held too, so a bad push (and its
// pre_deploy) does not run on every follow; a pull failure is retried.
func TestFollowHoldsFailedDeployButRetriesPullFailure(t *testing.T) {
	h := newHarness(t)
	reg := withRegistry(t, h)
	reg.push("main", digest(1))
	if _, err := follow(t, h); err != nil {
		t.Fatal(err)
	}
	reg.push("main", digest(2))
	h.docker.pullErr = errors.New("registry unreachable")
	if _, err := follow(t, h); deploy.ExitCode(err) != deploy.CodeFailed {
		t.Fatalf("pull failure: %v", err)
	}
	if st := h.state(); st.Hold != nil {
		t.Fatalf("a pull failure must not hold: %+v", st.Hold)
	}
	h.docker.pullErr = nil
	h.health.markBad(digest(2))
	if _, err := follow(t, h); deploy.ExitCode(err) != deploy.CodeFailed {
		t.Fatalf("unhealthy: %v", err)
	}
	if st := h.state(); st.Hold == nil || st.Hold.Digest != digest(2) || st.Hold.Reason != "deploy_failed:unhealthy" {
		t.Fatalf("hold after failed deploy: %+v", st.Hold)
	}
	runs := len(h.docker.callsWith("run "))
	if r, err := follow(t, h); err != nil || r.Action != deploy.FollowHeld {
		t.Fatalf("failed digest should be held: %+v %v", r, err)
	}
	if len(h.docker.callsWith("run ")) != runs {
		t.Fatal("held digest was started again")
	}
	// An explicit deploy of the held digest clears the hold once it succeeds.
	h.health.mu.Lock()
	delete(h.health.bad, digest(2))
	h.health.mu.Unlock()
	h.mustDeploy(2)
	if st := h.state(); st.Hold != nil {
		t.Fatalf("explicit deploy should clear the hold: %+v", st.Hold)
	}
}

func TestFollowCredentialsAndErrors(t *testing.T) {
	h := newHarness(t)
	reg := withRegistry(t, h)
	reg.wantUser, reg.wantPass = "robot", "s3cret-token"
	reg.push("main", digest(1))
	if _, err := follow(t, h); deploy.ExitCode(err) != deploy.CodeFailed || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("anonymous against a private repo: %v", err)
	}
	auth := filepath.Join(t.TempDir(), "registry.env")
	os.WriteFile(auth, []byte("# test\nREGISTRY_USERNAME=robot\nREGISTRY_PASSWORD='s3cret-token'\n"), 0o600)
	svc, _ := h.cfg.Service("web")
	svc.RegistryAuthFile = auth
	if r, err := follow(t, h); err != nil || r.Action != deploy.FollowDeployed {
		t.Fatalf("with credentials: %+v %v", r, err)
	}
	if _, err := h.gate.Follow(context.Background(), "web", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing tag: %v", err)
	}
	if _, err := h.gate.Follow(context.Background(), "web", "bad tag"); deploy.ExitCode(err) != deploy.CodeUsage {
		t.Fatalf("invalid tag: %v", err)
	}
}

// --- status ----------------------------------------------------------------

func TestStatusReportsColoursAndLastOp(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	h.mustDeploy(2)
	h.docker.set("shiftgate-web-green", func(c *fakeCtr) { c.restarts = 4 })
	st := h.gate.Status(context.Background())[0]
	g, b := st.Colors["green"], st.Colors["blue"]
	if g.State != "running" || g.RestartCount != 4 || g.UptimeSeconds <= 0 || g.Digest != digest(2) || g.StartedAt == nil {
		t.Fatalf("green: %+v", g)
	}
	if b.State != "exited" || b.UptimeSeconds != 0 || b.Digest != digest(1) {
		t.Fatalf("blue: %+v", b)
	}
	if st.LastOp == nil || st.LastOp.Op != "deploy" || st.LastOp.Result != "ok" || st.LastOp.ToDigest != digest(2) {
		t.Fatalf("last op: %+v", st.LastOp)
	}
}

// Review fix: a host-side failure (pre_deploy could not even start, or the
// run was interrupted) must not hold a good digest.
func TestFollowDoesNotHoldHostFailures(t *testing.T) {
	h := newHarness(t)
	reg := withRegistry(t, h)
	svc, _ := h.cfg.Service("web")
	svc.PreDeploy = &config.PreDeploy{Args: []string{"migrate"}, Timeout: config.Duration(1e9)}
	reg.push("main", digest(1))
	if _, err := follow(t, h); err != nil {
		t.Fatal(err)
	}
	reg.push("main", digest(2))
	// A pre_deploy container left running by a killed earlier run.
	h.docker.containers["shiftgate-web-pre-deploy"] = &fakeCtr{running: true,
		labels: map[string]string{"shiftgate.service": "web", "shiftgate.role": "pre_deploy"}}
	if _, err := follow(t, h); deploy.ExitCode(err) != deploy.CodeFailed || !strings.Contains(err.Error(), "pre_deploy_refused") {
		t.Fatalf("leftover pre_deploy container: %v", err)
	}
	if st := h.state(); st.Hold != nil {
		t.Fatalf("a refused pre_deploy must not hold: %+v", st.Hold)
	}
	delete(h.docker.containers, "shiftgate-web-pre-deploy")
	// An interrupted run (SIGTERM) that aborts as unhealthy is not held either.
	ctx, cancel := context.WithCancel(context.Background())
	h.docker.pullEnter, h.docker.pullGate = make(chan struct{}, 1), make(chan struct{})
	go func() { <-h.docker.pullEnter; cancel(); close(h.docker.pullGate) }()
	h.health.markBad(digest(2)) // so the cancelled run aborts in the health wait
	if _, err := h.gate.Follow(ctx, "web", "main"); deploy.ExitCode(err) != deploy.CodeFailed {
		t.Fatalf("cancelled follow: %v", err)
	}
	if st := h.state(); st.Hold != nil {
		t.Fatalf("an interrupted run must not hold: %+v", st.Hold)
	}
	h.docker.pullEnter, h.docker.pullGate = nil, nil
	h.health.mu.Lock()
	delete(h.health.bad, digest(2))
	h.health.mu.Unlock()
	if r, err := follow(t, h); err != nil || r.Action != deploy.FollowDeployed {
		t.Fatalf("follow after the host was fixed: %+v %v", r, err)
	}
}

// Review fix: while a deploy is before its flip (pull, pre_deploy), the old
// colour still serves, so check keeps probing it instead of skipping.
func TestCheckWatchesActiveDuringDeployStartingPhase(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	h.docker.pullEnter, h.docker.pullGate = make(chan struct{}, 1), make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := h.gate.Deploy(context.Background(), "web", digest(2)); done <- err }()
	<-h.docker.pullEnter
	if r, err := checkOne(t, h, false); err != nil || r.Failed() || r.Skipped != "" {
		t.Fatalf("serving colour during a deploy should be checked and pass: %+v %v", r, err)
	}
	h.docker.set("shiftgate-web-blue", func(c *fakeCtr) { c.running = false })
	r, err := checkOne(t, h, true)
	if deploy.ExitCode(err) != deploy.CodeFailed || r.Kind != deploy.KindStopped || !strings.Contains(r.Reason, "not healed") {
		t.Fatalf("dead serving colour during a deploy must fail (unhealed, lock busy): %+v %v", r, err)
	}
	if got := h.docker.callsWith("restart"); len(got) != 0 {
		t.Fatalf("no restart while the deploy holds the lock: %v", got)
	}
	close(h.docker.pullGate)
	<-done
}

// Review fix (#3 thread on check.go): an intent that appears while check
// probes is an operation in progress only if someone holds the lock; an
// intent left by a process that died is recovery_required, not a skip.
func TestCheckIntentWrittenDuringProbe(t *testing.T) {
	for _, held := range []bool{false, true} {
		h := newHarness(t)
		h.mustDeploy(1)
		h.docker.set("shiftgate-web-blue", func(c *fakeCtr) { c.running = false })
		var lk *state.Lock
		h.docker.onInspect = func(string) {
			h.docker.onInspect = nil
			st := h.state()
			st.Pending = &state.Intent{OpID: "dead", Op: "deploy", Phase: state.PhaseFlipping, From: st.Active,
				To: state.Slot{Color: config.Green, Digest: digest(2)}}
			if err := h.store.Save(st); err != nil {
				t.Fatal(err)
			}
			if held {
				var err error
				if lk, err = h.store.LockService("web"); err != nil {
					t.Fatal(err)
				}
			}
		}
		r, err := checkOne(t, h, false)
		lk.Unlock()
		if held {
			if err != nil || r.Skipped == "" {
				t.Fatalf("live flip during the probe should skip: %+v %v", r, err)
			}
			continue
		}
		if deploy.ExitCode(err) != deploy.CodeFailed || r.Kind != deploy.KindPending {
			t.Fatalf("stale intent written during the probe must fail as %s: %+v %v", deploy.KindPending, r, err)
		}
	}
}
