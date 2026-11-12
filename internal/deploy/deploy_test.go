package deploy_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/deploy"
	"github.com/mgh3326/shiftgate/internal/haproxy"
	"github.com/mgh3326/shiftgate/internal/haproxy/haproxytest"
	"github.com/mgh3326/shiftgate/internal/state"
)

// Acceptance 1 (logic level; the Docker-backed run is in e2e_test.go).
func TestDeployHealthyFlipsDrainsAndKeepsOld(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	if a := h.state().Active; a == nil || a.Color != config.Blue {
		t.Fatalf("first deploy should activate blue, got %+v", a)
	}
	h.fake.Set("bk_web", "blue", func(s *haproxytest.Server) { s.Scur = 3 })
	go func() { // sessions finish during the drain window
		time.Sleep(50 * time.Millisecond)
		h.fake.Set("bk_web", "blue", func(s *haproxytest.Server) { s.Scur = 0 })
	}()
	res, err := h.gate.Deploy(context.Background(), "web", digest(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	st := h.state()
	if st.Active.Color != config.Green || st.Active.Digest != digest(2) {
		t.Fatalf("active = %+v, want green %s", st.Active, digest(2))
	}
	if st.Previous.Color != config.Blue || st.Previous.Digest != digest(1) || !st.RollbackAvailable {
		t.Fatalf("previous = %+v rollback=%v", st.Previous, st.RollbackAvailable)
	}
	if st.Pending != nil || st.LastResult != "ok" || st.LastSwapAt.IsZero() || st.Generation != 2 {
		t.Fatalf("state not committed cleanly: %+v", st)
	}
	if exists, running := h.docker.running("shiftgate-web-blue"); !exists || running {
		t.Fatalf("old colour should be stopped but kept: exists=%v running=%v", exists, running)
	}
	if g := h.fake.Get("bk_web", "green"); g.Admin&^0x04 != 0 || g.Op != 2 {
		t.Fatalf("green not serving in HAProxy: %+v", g)
	}
	if b := h.fake.Get("bk_web", "blue"); b.Admin&0x01 == 0 {
		t.Fatalf("blue should be in maintenance after drain: %+v", b)
	}
	want := []string{"set server bk_web/green state ready", "set server bk_web/blue state drain", "set server bk_web/blue state maint"}
	if got := setCommands(h.fake.Commands()); !slices.Equal(got[len(got)-3:], want) {
		t.Fatalf("flip commands = %v, want suffix %v", got, want)
	}
	logs := h.swaps()
	if len(logs) != 2 || !strings.Contains(logs[1], `"result":"ok"`) || !strings.Contains(logs[1], `"to_color":"green"`) {
		t.Fatalf("swap log = %v", logs)
	}
	h.noReload()
	// The generated file was refreshed (validated, not reloaded) with green enabled.
	cfgData, err := os.ReadFile(h.cfg.HAProxy.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfgData), "server green 127.0.0.1:18081 check\n") ||
		!strings.Contains(string(cfgData), "server blue 127.0.0.1:18080 check disabled") {
		t.Fatalf("config file not synced to committed state:\n%s", cfgData)
	}
}

// Acceptance 2 (logic level).
func TestDeployUnhealthyNeverFlips(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	before := setCommands(h.fake.Commands())
	h.health.markBad(digest(9))
	_, err := h.gate.Deploy(context.Background(), "web", digest(9))
	if err == nil || deploy.ExitCode(err) != deploy.CodeFailed {
		t.Fatalf("want exit %d, got %v (code %d)", deploy.CodeFailed, err, deploy.ExitCode(err))
	}
	if after := setCommands(h.fake.Commands()); !slices.Equal(before, after) {
		t.Fatalf("HAProxy runtime state changed on an unhealthy deploy: %v", after[len(before):])
	}
	if b := h.fake.Get("bk_web", "blue"); b.Admin&^0x04 != 0 || b.Op != 2 {
		t.Fatalf("blue must keep serving: %+v", b)
	}
	if exists, running := h.docker.running("shiftgate-web-green"); !exists || running {
		t.Fatalf("unhealthy container should be stopped (kept for logs): exists=%v running=%v", exists, running)
	}
	st := h.state()
	if st.Active.Color != config.Blue || st.Active.Digest != digest(1) || st.Pending != nil || st.LastResult != "unhealthy" {
		t.Fatalf("state after unhealthy deploy: %+v", st)
	}
	if l := h.swaps(); !strings.Contains(l[len(l)-1], `"result":"unhealthy"`) {
		t.Fatalf("swap log missing unhealthy entry: %v", l)
	}
	h.noReload()
}

// Acceptance 3 (logic level).
func TestRollbackRestoresPrevious(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	h.mustDeploy(2)
	res, err := h.gate.Rollback(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if res.To.Color != config.Blue || res.To.Digest != digest(1) {
		t.Fatalf("rollback promoted %+v", res.To)
	}
	st := h.state()
	if st.Active.Color != config.Blue || st.Active.Digest != digest(1) || st.Previous.Digest != digest(2) || !st.RollbackAvailable {
		t.Fatalf("state after rollback: %+v", st)
	}
	if !slices.Contains(h.docker.calls, "start shiftgate-web-blue") {
		t.Fatalf("stopped previous container should be started: %v", h.docker.calls)
	}
	if _, running := h.docker.running("shiftgate-web-green"); running {
		t.Fatal("rolled-back-from colour should be stopped")
	}
	// Rolling back again returns to the new version (roll forward).
	if _, err := h.gate.Rollback(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if st := h.state(); st.Active.Digest != digest(2) {
		t.Fatalf("second rollback should roll forward, got %+v", st.Active)
	}
	h.noReload()
}

func TestRollbackRefusedWhenPreviousGoneOrUnhealthy(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	if _, err := h.gate.Rollback(context.Background(), "web"); deploy.ExitCode(err) != deploy.CodeFailed {
		t.Fatalf("rollback with no previous: %v", err)
	}
	h.mustDeploy(2)
	h.health.markBad(digest(1))
	if _, err := h.gate.Rollback(context.Background(), "web"); deploy.ExitCode(err) != deploy.CodeFailed {
		t.Fatalf("unhealthy rollback should fail: %v", err)
	}
	if st := h.state(); st.Active.Digest != digest(2) || !st.RollbackAvailable {
		t.Fatalf("failed rollback must leave state: %+v", st)
	}
	if g := h.fake.Get("bk_web", "green"); g.Admin&^0x04 != 0 {
		t.Fatalf("green must keep serving: %+v", g)
	}
	delete(h.docker.containers, "shiftgate-web-blue")
	if _, err := h.gate.Rollback(context.Background(), "web"); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("rollback with missing container: %v", err)
	}
}

// A pull failure must not cost the rollback target: the previous colour's
// container is untouched until the new image is on the host.
func TestPullFailureKeepsRollback(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	h.mustDeploy(2) // active green@2, previous blue@1
	h.docker.pullErr = errors.New("registry unreachable")
	if _, err := h.gate.Deploy(context.Background(), "web", digest(3)); deploy.ExitCode(err) != deploy.CodeFailed {
		t.Fatalf("pull failure: %v", err)
	}
	st := h.state()
	if !st.RollbackAvailable || st.Previous.Digest != digest(1) || st.LastResult != "pull_failed" || st.Pending != nil {
		t.Fatalf("state after pull failure: %+v", st)
	}
	if exists, _ := h.docker.running("shiftgate-web-blue"); !exists {
		t.Fatal("previous container must be untouched")
	}
	h.docker.pullErr = nil
	if _, err := h.gate.Rollback(context.Background(), "web"); err != nil {
		t.Fatalf("rollback after a failed pull: %v", err)
	}
	// Once the previous colour is actually replaced, rollback is gone.
	h.health.markBad(digest(4))
	h.mustDeploy(2) // active green@2 again, previous blue@1
	if _, err := h.gate.Deploy(context.Background(), "web", digest(4)); err == nil {
		t.Fatal("unhealthy deploy should fail")
	}
	if h.state().RollbackAvailable {
		t.Fatal("rollback must be unavailable after its container was replaced")
	}
}

// The health probe only proves that something answers on the colour port.
// With the new container gone, a foreign listener must never get traffic.
func TestTargetMustBeRunningBeforeFlip(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	before := setCommands(h.fake.Commands())
	h.docker.exitAfterRun = true
	h.health.foreign[18081] = true // green's port answers, but not from green
	_, err := h.gate.Deploy(context.Background(), "web", digest(2))
	if deploy.ExitCode(err) != deploy.CodeFailed || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("want refusal before the flip, got %v", err)
	}
	if after := setCommands(h.fake.Commands()); !slices.Equal(before, after) {
		t.Fatalf("HAProxy changed: %v", after[len(before):])
	}
	if st := h.state(); st.Active.Digest != digest(1) || st.LastResult != "target_not_running" {
		t.Fatalf("state: %+v", st)
	}
}

// A wildcard bind approved by an explicit `render --allow-wildcard-bind`
// must not make the post-commit config refresh fail (the file would keep
// naming the old colour active for the next outside reload).
func TestSyncKeepsExplicitlyApprovedWildcard(t *testing.T) {
	h := newHarness(t)
	h.cfg.Services[0].Binds = []string{"0.0.0.0:8080"}
	if _, err := h.gate.Render(context.Background(), deploy.RenderOptions{NoReload: true, AllowWildcard: true}); err != nil {
		t.Fatal(err)
	}
	h.mustDeploy(1)
	res, err := h.gate.Deploy(context.Background(), "web", digest(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("sync should succeed for an approved wildcard: %v", res.Warnings)
	}
	b, _ := os.ReadFile(h.cfg.HAProxy.Config)
	if !strings.Contains(string(b), "server green 127.0.0.1:18081 check\n") {
		t.Fatalf("config not refreshed:\n%s", b)
	}
	// A wildcard that no explicit render approved is still refused.
	h.cfg.Services[0].Binds = []string{"0.0.0.0:9090"}
	res, err = h.gate.Deploy(context.Background(), "web", digest(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, " "), "wildcard") {
		t.Fatalf("unapproved wildcard must not be written: %v", res.Warnings)
	}
}

// Acceptance 4.
func TestConcurrentDeploySecondRefusedByLock(t *testing.T) {
	h := newHarness(t)
	h.docker.pullGate = make(chan struct{})
	h.docker.pullEnter = make(chan struct{}, 1)
	first := make(chan error, 1)
	go func() {
		_, err := h.gate.Deploy(context.Background(), "web", digest(1))
		first <- err
	}()
	<-h.docker.pullEnter // the first deploy holds the lock and is mid-pull
	_, err := h.gate.Deploy(context.Background(), "web", digest(2))
	if !errors.Is(err, state.ErrLocked) || deploy.ExitCode(err) != deploy.CodeLocked {
		t.Fatalf("second deploy: want lock refusal (exit %d), got %v", deploy.CodeLocked, err)
	}
	close(h.docker.pullGate)
	if err := <-first; err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	if st := h.state(); st.Active.Digest != digest(1) {
		t.Fatalf("first deploy should win: %+v", st.Active)
	}
}

func TestFlipFailureCompensatesToOld(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	// The target never becomes UP in HAProxy (its checks fail).
	h.fake.Set("bk_web", "green", func(s *haproxytest.Server) { s.Healthy = false })
	_, err := h.gate.Deploy(context.Background(), "web", digest(2))
	if deploy.ExitCode(err) != deploy.CodeFailed {
		t.Fatalf("want flip failure exit 1, got %v", err)
	}
	if b := h.fake.Get("bk_web", "blue"); b.Admin&^0x04 != 0 || b.Op != 2 {
		t.Fatalf("old colour must serve: %+v", b)
	}
	if g := h.fake.Get("bk_web", "green"); g.Admin&0x01 == 0 {
		t.Fatalf("target must be back in maintenance: %+v", g)
	}
	st := h.state()
	if st.Active.Digest != digest(1) || st.Pending != nil || st.LastResult != "flip_failed" {
		t.Fatalf("state: %+v", st)
	}
	if _, running := h.docker.running("shiftgate-web-green"); running {
		t.Fatal("target container should be stopped after a failed flip")
	}
}

func TestDrainReplyLostButAppliedContinues(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	h.fake.Hook(func(cmd string) *haproxytest.Action {
		if cmd == "set server bk_web/blue state drain" {
			return &haproxytest.Action{Apply: true, Hang: true}
		}
		return nil
	})
	if _, err := h.gate.Deploy(context.Background(), "web", digest(2)); err != nil {
		t.Fatalf("read-back shows drain applied; deploy should commit: %v", err)
	}
	if h.state().Active.Color != config.Green {
		t.Fatal("green should be active")
	}
}

func TestUncertainCompensationRequiresRecovery(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	// Drain is applied but its reply is lost and every later command fails:
	// shiftgate cannot prove blue serves again, so it must not disable green.
	drained := false
	h.fake.Hook(func(cmd string) *haproxytest.Action {
		switch {
		case cmd == "set server bk_web/blue state drain":
			drained = true
			return &haproxytest.Action{Apply: true, Hang: true}
		case !drained:
			return nil
		case strings.HasPrefix(cmd, "show servers state"):
			return &haproxytest.Action{Reply: "Permission denied\n"}
		case strings.HasPrefix(cmd, "set server bk_web/blue state ready"):
			return &haproxytest.Action{Reply: "Permission denied\n"}
		}
		return nil
	})
	_, err := h.gate.Deploy(context.Background(), "web", digest(2))
	if deploy.ExitCode(err) != deploy.CodeRecovery {
		t.Fatalf("want recovery-required exit %d, got %v", deploy.CodeRecovery, err)
	}
	if g := h.fake.Get("bk_web", "green"); g.Admin&^0x04 != 0 {
		t.Fatalf("green must not be disabled while blue is unproven: %+v", g)
	}
	if _, running := h.docker.running("shiftgate-web-green"); !running {
		t.Fatal("both containers must be kept running")
	}
	st := h.state()
	if st.Pending == nil || st.Pending.Phase != state.PhaseFlipping {
		t.Fatalf("intent must stay for recovery: %+v", st.Pending)
	}
	// Further deploys are refused until recovery.
	if _, err := h.gate.Deploy(context.Background(), "web", digest(3)); deploy.ExitCode(err) != deploy.CodeRecovery {
		t.Fatalf("deploy with pending intent: %v", err)
	}
	// HAProxy answers again: green serves and blue drains → recover completes forward.
	h.fake.Hook(nil)
	msg, err := h.gate.Recover(context.Background(), "web")
	if err != nil || !strings.Contains(msg, "completed") {
		t.Fatalf("recover: %q %v", msg, err)
	}
	st = h.state()
	if st.Pending != nil || st.Active.Color != config.Green || st.Previous.Color != config.Blue {
		t.Fatalf("state after recover: %+v", st)
	}
}

func TestRecoverAbortsWhenOldStillServing(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	// Simulate a crash right after "green ready": write the flipping intent by hand.
	st := h.state()
	h.docker.Run(context.Background(), runSpecFor(h, config.Green, digest(2)))
	st.Pending = &state.Intent{OpID: "crash", Op: "deploy", Phase: state.PhaseFlipping, From: st.Active,
		To: state.Slot{Color: config.Green, Digest: digest(2)}, StartedAt: time.Now()}
	if err := h.store.Save(st); err != nil {
		t.Fatal(err)
	}
	h.fake.Set("bk_web", "green", func(s *haproxytest.Server) { s.Admin, s.Op = 0, 2 })
	msg, err := h.gate.Recover(context.Background(), "web")
	if err != nil || !strings.Contains(msg, "rolled back") {
		t.Fatalf("recover: %q %v", msg, err)
	}
	if g := h.fake.Get("bk_web", "green"); g.Admin&0x01 == 0 {
		t.Fatalf("green must be back in maintenance: %+v", g)
	}
	if st := h.state(); st.Pending != nil || st.Active.Color != config.Blue {
		t.Fatalf("state after recover: %+v", st)
	}
}

func TestMissingStateWithContainersRefuses(t *testing.T) {
	h := newHarness(t)
	h.docker.Run(context.Background(), runSpecFor(h, config.Blue, digest(1)))
	_, err := h.gate.Deploy(context.Background(), "web", digest(2))
	if deploy.ExitCode(err) != deploy.CodeRecovery {
		t.Fatalf("want refusal without state, got %v", err)
	}
}

func TestForeignContainerNotReplaced(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	h.docker.containers["shiftgate-web-green"] = &fakeCtr{labels: map[string]string{"owner": "someone-else"}}
	_, err := h.gate.Deploy(context.Background(), "web", digest(2))
	if err == nil || !strings.Contains(err.Error(), "not labelled") {
		t.Fatalf("want ownership refusal, got %v", err)
	}
	if st := h.state(); st.Pending != nil || st.Active.Digest != digest(1) {
		t.Fatalf("refusal must happen before any intent: %+v", st)
	}
}

func TestBadDigestAndUnknownService(t *testing.T) {
	h := newHarness(t)
	if _, err := h.gate.Deploy(context.Background(), "web", "latest"); deploy.ExitCode(err) != deploy.CodeUsage {
		t.Fatalf("tag instead of digest: %v", err)
	}
	if _, err := h.gate.Deploy(context.Background(), "nope", digest(1)); deploy.ExitCode(err) != deploy.CodeConfig {
		t.Fatalf("unknown service: %v", err)
	}
}

func TestPruneKeepsProtectedDigests(t *testing.T) {
	h := newHarness(t)
	for i := 1; i <= 4; i++ {
		h.mustDeploy(i)
	}
	h.docker.images[digest(77)] = true // an old image never promoted
	res, err := h.gate.Prune(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	// keep_digests=2 → history 4,3 kept; active 4 and previous 3 protected;
	// containers hold 4 and 3. Digests 1, 2 and 77 go.
	removed := res[0].Removed
	slices.Sort(removed)
	if want := []string{digest(1), digest(2), digest(77)}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	for _, d := range []string{digest(3), digest(4)} {
		if !h.docker.images[d] {
			t.Fatalf("protected digest %s was removed", d)
		}
	}
}

const testSharedYAML = `
state_dir: %[1]s/state
keep_digests: 2
haproxy:
  config: %[1]s/haproxy/haproxy.cfg
  socket: %[2]s
  check: ["sh", "-c", "test -s \"$1\"", "check", "{config}"]
  reload: ["sh", "-c", "echo reload >> %[1]s/reload.marker"]
  allowed_binds: ["127.0.0.1"]
services:
  - name: app1
    image: registry.example.test/team/shared-app
    binds: ["127.0.0.1:8080"]
    ports: {blue: 18080, green: 18081}
    container_port: 8080
    health: {http: /healthz, timeout: 300ms, interval: 20ms}
    drain_timeout: 200ms
    stop_timeout: 1s
  - name: app2
    image: registry.example.test/team/shared-app
    binds: ["127.0.0.1:8082"]
    ports: {blue: 18082, green: 18083}
    container_port: 8080
    health: {http: /healthz, timeout: 300ms, interval: 20ms}
    drain_timeout: 200ms
    stop_timeout: 1s
`

func newSharedRepoHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	fake := haproxytest.New(t, map[string][]string{
		"bk_app1": {"blue", "green"},
		"bk_app2": {"blue", "green"},
	})
	cfg, err := config.Parse([]byte(fmt.Sprintf(testSharedYAML, dir, fake.Path)))
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

func TestPruneSharedRepoKeepsProtectedUnion(t *testing.T) {
	// (a) two services sharing a repo, each protecting a different digest →
	// pruning either (and pruning all) removes neither protected digest.
	setup := func(t *testing.T) *harness {
		h := newSharedRepoHarness(t)
		if _, err := h.gate.Deploy(context.Background(), "app1", digest(1)); err != nil {
			t.Fatalf("deploy app1: %v", err)
		}
		if _, err := h.gate.Deploy(context.Background(), "app2", digest(2)); err != nil {
			t.Fatalf("deploy app2: %v", err)
		}
		h.docker.images[digest(77)] = true // unprotected digest
		return h
	}

	t.Run("prune app1 only", func(t *testing.T) {
		h := setup(t)
		res, err := h.gate.Prune(context.Background(), "app1")
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].Service != "app1" {
			t.Fatalf("unexpected results: %+v", res)
		}
		if !h.docker.images[digest(1)] {
			t.Fatalf("app1 active digest %s was removed", digest(1))
		}
		if !h.docker.images[digest(2)] {
			t.Fatalf("app2 active digest %s was removed by app1 prune", digest(2))
		}
		if h.docker.images[digest(77)] {
			t.Fatalf("unprotected digest %s was not removed", digest(77))
		}
		if !slices.Contains(res[0].Kept, digest(1)) || !slices.Contains(res[0].Kept, digest(2)) {
			t.Fatalf("expected both digests in Kept, got %v", res[0].Kept)
		}
		if want := []string{digest(77)}; !slices.Equal(res[0].Removed, want) {
			t.Fatalf("removed %v, want %v", res[0].Removed, want)
		}
	})

	t.Run("prune app2 only", func(t *testing.T) {
		h := setup(t)
		res, err := h.gate.Prune(context.Background(), "app2")
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].Service != "app2" {
			t.Fatalf("unexpected results: %+v", res)
		}
		if !h.docker.images[digest(1)] {
			t.Fatalf("app1 active digest %s was removed by app2 prune", digest(1))
		}
		if !h.docker.images[digest(2)] {
			t.Fatalf("app2 active digest %s was removed", digest(2))
		}
		if h.docker.images[digest(77)] {
			t.Fatalf("unprotected digest %s was not removed", digest(77))
		}
		if !slices.Contains(res[0].Kept, digest(1)) || !slices.Contains(res[0].Kept, digest(2)) {
			t.Fatalf("expected both digests in Kept, got %v", res[0].Kept)
		}
		if want := []string{digest(77)}; !slices.Equal(res[0].Removed, want) {
			t.Fatalf("removed %v, want %v", res[0].Removed, want)
		}
	})

	t.Run("prune all", func(t *testing.T) {
		h := setup(t)
		res, err := h.gate.Prune(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("want 2 results, got %d", len(res))
		}
		if !h.docker.images[digest(1)] {
			t.Fatalf("app1 active digest %s was removed", digest(1))
		}
		if !h.docker.images[digest(2)] {
			t.Fatalf("app2 active digest %s was removed", digest(2))
		}
		if h.docker.images[digest(77)] {
			t.Fatalf("unprotected digest %s was not removed", digest(77))
		}
		// First service removed 77; both report kept union.
		if !slices.Contains(res[0].Kept, digest(1)) || !slices.Contains(res[0].Kept, digest(2)) {
			t.Fatalf("res[0] expected kept union, got %v", res[0].Kept)
		}
		if !slices.Contains(res[1].Kept, digest(1)) || !slices.Contains(res[1].Kept, digest(2)) {
			t.Fatalf("res[1] expected kept union, got %v", res[1].Kept)
		}
	})
}

func TestPruneOnlyRespectsOtherServiceKeepSet(t *testing.T) {
	// (b) prune <only> still respects the other service's keep set.
	h := newSharedRepoHarness(t)
	if _, err := h.gate.Deploy(context.Background(), "app1", digest(10)); err != nil {
		t.Fatalf("deploy app1: %v", err)
	}
	if _, err := h.gate.Deploy(context.Background(), "app2", digest(20)); err != nil {
		t.Fatalf("deploy app2: %v", err)
	}
	h.docker.images[digest(30)] = true

	res, err := h.gate.Prune(context.Background(), "app1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Service != "app1" {
		t.Fatalf("unexpected results: %+v", res)
	}
	if !slices.Contains(res[0].Kept, digest(20)) {
		t.Fatalf("digest 20 protected by app2 was not reported in Kept: %v", res[0].Kept)
	}
	if !h.docker.images[digest(20)] {
		t.Fatalf("digest 20 protected by app2 was removed by app1 prune")
	}
	if !h.docker.images[digest(10)] {
		t.Fatalf("digest 10 protected by app1 was removed")
	}
	if h.docker.images[digest(30)] {
		t.Fatalf("unprotected digest 30 was not removed")
	}
}

func TestPruneSharedRepoStateLoadErrorFailsClosed(t *testing.T) {
	// (c) state load error for a sharing service → nothing removed for that repo, error returned.
	setup := func(t *testing.T) *harness {
		h := newSharedRepoHarness(t)
		if _, err := h.gate.Deploy(context.Background(), "app1", digest(1)); err != nil {
			t.Fatalf("deploy app1: %v", err)
		}
		if _, err := h.gate.Deploy(context.Background(), "app2", digest(2)); err != nil {
			t.Fatalf("deploy app2: %v", err)
		}
		h.docker.images[digest(77)] = true

		// Corrupt app2 state file to trigger a load error (not ErrNoState).
		if err := os.WriteFile(h.store.Path("app2"), []byte("corrupt-state"), 0644); err != nil {
			t.Fatal(err)
		}
		return h
	}

	t.Run("prune only fails closed", func(t *testing.T) {
		h := setup(t)
		_, err := h.gate.Prune(context.Background(), "app1")
		if err == nil {
			t.Fatal("expected prune to fail when sharing service state is corrupt")
		}
		if deploy.ExitCode(err) != deploy.CodeRecovery {
			t.Fatalf("expected CodeRecovery, got: %v", err)
		}
		// Nothing must be removed from the shared repository.
		if !h.docker.images[digest(77)] {
			t.Fatalf("digest 77 was removed despite fail-closed policy")
		}
		if !h.docker.images[digest(1)] || !h.docker.images[digest(2)] {
			t.Fatal("active digests were unexpectedly removed")
		}
	})

	t.Run("prune all fails closed", func(t *testing.T) {
		h := setup(t)
		_, err := h.gate.Prune(context.Background(), "")
		if err == nil {
			t.Fatal("expected prune to fail when sharing service state is corrupt")
		}
		if deploy.ExitCode(err) != deploy.CodeRecovery {
			t.Fatalf("expected CodeRecovery, got: %v", err)
		}
		if !h.docker.images[digest(77)] {
			t.Fatalf("digest 77 was removed despite fail-closed policy")
		}
	})
}

func TestPruneSingleServiceBehaviorUnchanged(t *testing.T) {
	// (d) single-service repo behaviour unchanged.
	t.Run("prune only", func(t *testing.T) {
		h := newHarness(t)
		for i := 1; i <= 4; i++ {
			h.mustDeploy(i)
		}
		h.docker.images[digest(77)] = true
		res, err := h.gate.Prune(context.Background(), "web")
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].Service != "web" {
			t.Fatalf("unexpected result: %+v", res)
		}
		removed := res[0].Removed
		slices.Sort(removed)
		if want := []string{digest(1), digest(2), digest(77)}; !slices.Equal(removed, want) {
			t.Fatalf("removed %v, want %v", removed, want)
		}
		for _, d := range []string{digest(3), digest(4)} {
			if !h.docker.images[d] {
				t.Fatalf("protected digest %s was removed", d)
			}
		}
	})
}

func TestRenderRefusesWildcardAndRequiresFlag(t *testing.T) {
	h := newHarness(t)
	h.cfg.HAProxy.AllowedBinds = append(h.cfg.HAProxy.AllowedBinds, "0.0.0.0")
	h.cfg.Services[0].Binds = []string{"0.0.0.0:8080"}
	_, err := h.gate.Render(context.Background(), deploy.RenderOptions{NoReload: true})
	if deploy.ExitCode(err) != deploy.CodeConfig || !strings.Contains(err.Error(), "wildcard") {
		t.Fatalf("want wildcard refusal, got %v", err)
	}
	if _, err := os.Stat(h.cfg.HAProxy.Config); err == nil {
		t.Fatal("a refused render must not write the config")
	}
	if _, err := h.gate.Render(context.Background(), deploy.RenderOptions{NoReload: true, AllowWildcard: true}); err != nil {
		t.Fatalf("explicit flag should allow: %v", err)
	}
}

func TestRenderRefusedWhilePending(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy(1)
	st := h.state()
	st.Pending = &state.Intent{OpID: "x", Op: "deploy", Phase: state.PhaseFlipping, To: state.Slot{Color: config.Green, Digest: digest(2)}}
	if err := h.store.Save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := h.gate.Render(context.Background(), deploy.RenderOptions{}); deploy.ExitCode(err) != deploy.CodeRecovery {
		t.Fatalf("render with pending flip: %v", err)
	}
	h.noReload()
}

func TestDeployCommittedAndFailedRecordLastOpID(t *testing.T) {
	h := newHarness(t)
	res, err := h.gate.Deploy(context.Background(), "web", digest(1))
	if err != nil {
		t.Fatal(err)
	}
	st := h.state()
	if st.LastOpID == "" || st.LastOpID != res.OpID {
		t.Fatalf("committed deploy: want LastOpID %q, got %q", res.OpID, st.LastOpID)
	}
	if st.LastOp != "deploy" || st.LastResult != "ok" {
		t.Fatalf("committed deploy state: %+v", st)
	}
	// status --json exposes state.last_op_id
	stat := h.gate.Status(context.Background())[0]
	if stat.State == nil || stat.State.LastOpID != res.OpID {
		t.Fatalf("status state.last_op_id = %v, want %q", stat.State, res.OpID)
	}
	committedOpID := res.OpID

	// Failed deploy also records its op_id in LastOpID
	h.health.markBad(digest(2))
	_, err = h.gate.Deploy(context.Background(), "web", digest(2))
	if err == nil || deploy.ExitCode(err) != deploy.CodeFailed {
		t.Fatalf("want exit %d, got %v", deploy.CodeFailed, err)
	}
	st = h.state()
	swaps := h.swaps()
	lastSwap := swaps[len(swaps)-1]
	if st.LastOpID == "" || !strings.Contains(lastSwap, fmt.Sprintf(`"op_id":%q`, st.LastOpID)) {
		t.Fatalf("failed deploy LastOpID %q not in swap log: %s", st.LastOpID, lastSwap)
	}
	if st.LastOpID == committedOpID {
		t.Fatalf("failed deploy LastOpID must differ from previous committed op_id")
	}
	if st.LastOp != "deploy" || st.LastResult != "unhealthy" {
		t.Fatalf("failed deploy state: %+v", st)
	}
}

func TestDeployLoadsOldStateWithoutLastOpID(t *testing.T) {
	h := newHarness(t)
	// Seed an old state.json that has no last_op_id field.
	oldJSON := `{
  "schema_version": 1,
  "service": "web",
  "generation": 1,
  "active": {
    "color": "blue",
    "digest": "` + digest(1) + `"
  },
  "rollback_available": false,
  "last_op": "deploy",
  "last_result": "ok"
}
`
	if err := os.MkdirAll(h.store.Dir+"/web", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.store.Path("web"), []byte(oldJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	st := h.state()
	if st.LastOpID != "" {
		t.Fatalf("expected empty LastOpID, got %q", st.LastOpID)
	}
	if st.Active == nil || st.Active.Digest != digest(1) {
		t.Fatalf("unexpected active slot: %+v", st.Active)
	}
	if st.LastResult != "ok" {
		t.Fatalf("unexpected last result: %q", st.LastResult)
	}
}

func TestDrainAndStopHonorsCancellation(t *testing.T) {
	h := newHarness(t)
	svc, _ := h.cfg.Service("web")
	if err := h.docker.Run(context.Background(), runSpecFor(h, config.Blue, digest(1))); err != nil {
		t.Fatal(err)
	}
	// Router fake reports sessions > 0 forever and server is in drain.
	h.fake.Set("bk_web", "blue", func(s *haproxytest.Server) {
		s.Admin = 0x08 // drain
		s.Op = 2
		s.Scur = 10
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstPoll := make(chan struct{})
	h.fake.Hook(func(cmd string) *haproxytest.Action {
		if strings.HasPrefix(cmd, "show stat") {
			select {
			case <-firstPoll:
			default:
				close(firstPoll)
				cancel()
			}
		}
		return nil
	})

	start := time.Now()
	warns := h.gate.DrainAndStop(ctx, svc, config.Blue)
	elapsed := time.Since(start)

	// drainAndStop returns within one poll interval (with margin for test scheduling).
	// Poll is 10ms, DrainTimeout is 200ms.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("drainAndStop took %v, want return within one poll interval", elapsed)
	}

	// Warning contains "canceled"
	if len(warns) != 1 || !strings.Contains(warns[0], "canceled") {
		t.Fatalf("want warning containing 'canceled', got: %v", warns)
	}

	// SetState maint was NOT called
	for _, cmd := range h.fake.Commands() {
		if strings.Contains(cmd, "state maint") {
			t.Fatalf("SetState maint was called: %v", cmd)
		}
	}
	if b := h.fake.Get("bk_web", "blue"); b.Admin&0x01 != 0 {
		t.Fatalf("blue server admin has maint bit set: %+v", b)
	}

	// Docker.Stop was NOT called
	h.docker.mu.Lock()
	for _, call := range h.docker.calls {
		if strings.HasPrefix(call, "stop ") {
			t.Fatalf("Docker.Stop was called: %v", call)
		}
	}
	h.docker.mu.Unlock()

	exists, running := h.docker.running("shiftgate-web-blue")
	if !exists || !running {
		t.Fatalf("shiftgate-web-blue should remain running: exists=%v running=%v", exists, running)
	}
}

func TestDrainAndStopWithoutCancellation(t *testing.T) {
	h := newHarness(t)
	svc, _ := h.cfg.Service("web")
	if err := h.docker.Run(context.Background(), runSpecFor(h, config.Blue, digest(1))); err != nil {
		t.Fatal(err)
	}
	h.fake.Set("bk_web", "blue", func(s *haproxytest.Server) {
		s.Admin = 0x08 // drain
		s.Op = 2
		s.Scur = 3
	})
	go func() {
		time.Sleep(30 * time.Millisecond)
		h.fake.Set("bk_web", "blue", func(s *haproxytest.Server) {
			s.Scur = 0
		})
	}()

	warns := h.gate.DrainAndStop(context.Background(), svc, config.Blue)
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}

	// SetState maint was called
	if b := h.fake.Get("bk_web", "blue"); b.Admin&0x01 == 0 {
		t.Fatalf("blue server should have maint bit set: %+v", b)
	}

	// Docker.Stop was called
	exists, running := h.docker.running("shiftgate-web-blue")
	if !exists || running {
		t.Fatalf("shiftgate-web-blue should be stopped: exists=%v running=%v", exists, running)
	}
}

func setCommands(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if strings.HasPrefix(c, "set server") {
			out = append(out, c)
		}
	}
	return out
}

func TestPreDeployEnvFilesSpec(t *testing.T) {
	svc := &config.Service{
		Name:          "web",
		Image:         "ghcr.io/example/web",
		ContainerPort: 8080,
		Ports:         config.Ports{Blue: 18080, Green: 18081},
		EnvFiles:      []string{"/etc/web/common.env", "/etc/web/base.env"},
		PreDeploy: &config.PreDeploy{
			Args:     []string{"alembic", "upgrade", "head"},
			EnvFiles: []string{"/etc/web/migration.env", "/etc/web/override.env"},
		},
	}

	t.Run("b_pre_deploy_spec_env_files_order", func(t *testing.T) {
		// (b) pre_deploy spec env files = colour files + pre_deploy files in that order
		spec := deploy.PreDeploySpec(svc, config.Blue, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
		want := []string{
			"/etc/web/common.env",
			"/etc/web/base.env",
			"/etc/web/migration.env",
			"/etc/web/override.env",
		}
		if !slices.Equal(spec.EnvFiles, want) {
			t.Fatalf("PreDeploySpec EnvFiles = %v, want %v", spec.EnvFiles, want)
		}
	})

	t.Run("c_colour_container_spec_has_no_pre_deploy_files", func(t *testing.T) {
		// (c) colour container spec has no pre_deploy files
		colourSpec := deploy.RunSpec(svc, config.Blue, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
		want := []string{"/etc/web/common.env", "/etc/web/base.env"}
		if !slices.Equal(colourSpec.EnvFiles, want) {
			t.Fatalf("RunSpec EnvFiles = %v, want %v", colourSpec.EnvFiles, want)
		}
		for _, pdFile := range svc.PreDeploy.EnvFiles {
			if slices.Contains(colourSpec.EnvFiles, pdFile) {
				t.Fatalf("RunSpec EnvFiles should not contain pre_deploy env file %q", pdFile)
			}
		}
	})

	t.Run("d_config_without_the_key_identical_to_base", func(t *testing.T) {
		// (d) config without the key → pre_deploy spec identical to base
		svcNoKey := &config.Service{
			Name:          "web",
			Image:         "ghcr.io/example/web",
			ContainerPort: 8080,
			Ports:         config.Ports{Blue: 18080, Green: 18081},
			EnvFiles:      []string{"/etc/web/common.env", "/etc/web/base.env"},
			PreDeploy: &config.PreDeploy{
				Args: []string{"alembic", "upgrade", "head"},
			},
		}
		spec := deploy.PreDeploySpec(svcNoKey, config.Blue, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
		colourSpec := deploy.RunSpec(svcNoKey, config.Blue, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
		if !slices.Equal(spec.EnvFiles, colourSpec.EnvFiles) {
			t.Fatalf("PreDeploySpec EnvFiles = %v without key, want identical to colour EnvFiles %v", spec.EnvFiles, colourSpec.EnvFiles)
		}
		if !slices.Equal(spec.EnvFiles, svcNoKey.EnvFiles) {
			t.Fatalf("PreDeploySpec EnvFiles = %v, want %v", spec.EnvFiles, svcNoKey.EnvFiles)
		}
	})
}
