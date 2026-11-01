package deploy_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/deploy"
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

func setCommands(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if strings.HasPrefix(c, "set server") {
			out = append(out, c)
		}
	}
	return out
}
