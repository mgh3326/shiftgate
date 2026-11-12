// Package deploy is shiftgate's state machine: deploy, rollback, recover,
// status, prune and render, built on the config, state, docker, health and
// haproxy packages.
//
// Ordering rule (see docs/design.md): durable intent → container work →
// health → runtime-API flip with read-back → durable commit → audit log →
// drain and stop. A failed health check never touches HAProxy; a failed flip
// is compensated only when read-back proves the old colour serves again,
// otherwise the intent stays and the exit code says "recovery required".
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/docker"
	"github.com/mgh3326/shiftgate/internal/haproxy"
	"github.com/mgh3326/shiftgate/internal/health"
	"github.com/mgh3326/shiftgate/internal/state"
)

// Exit codes.
const (
	CodeFailed   = 1  // operation failed; the old colour keeps serving
	CodeUsage    = 2  // bad command line
	CodeRecovery = 3  // routing may have changed; run `shiftgate recover`
	CodeLocked   = 75 // another operation holds the lock (EX_TEMPFAIL)
	CodeConfig   = 78 // configuration refused (EX_CONFIG)
)

// Error carries an exit code.
type Error struct {
	Code int
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func fail(code int, format string, a ...any) *Error {
	return &Error{Code: code, Err: fmt.Errorf(format, a...)}
}

// ExitCode maps any error to a process exit code.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, state.ErrLocked) {
		return CodeLocked
	}
	return CodeFailed
}

// lockErr maps a lock failure: contention is CodeLocked, anything else
// (permissions, missing directory) is an ordinary failure.
func lockErr(err error) error {
	if errors.Is(err, state.ErrLocked) {
		return &Error{Code: CodeLocked, Err: err}
	}
	return &Error{Code: CodeFailed, Err: err}
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Gate runs operations against one host.
type Gate struct {
	Cfg    *config.File
	Store  state.Store
	Docker docker.Engine
	Router haproxy.Router
	Health health.Checker
	// Registry resolves tags for `follow`; nil disables follow.
	Registry Resolver
	Log      io.Writer
	Now      func() time.Time

	// Poll is the runtime-API read-back interval (default 250ms).
	Poll time.Duration
	// FlipTimeout bounds waiting for HAProxy to see the target UP and the
	// old colour serving again during compensation (default 30s).
	FlipTimeout time.Duration
	// HostLockWait bounds waiting for a render to finish (default 30s).
	HostLockWait time.Duration
	// SyncConfig rewrites (validated, never reloaded) the generated HAProxy
	// file after each commit so an external reload keeps the new colour.
	SyncConfig bool
}

func (g *Gate) logf(format string, a ...any) {
	if g.Log != nil {
		fmt.Fprintf(g.Log, "shiftgate: "+format+"\n", a...)
	}
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

func (g *Gate) poll() time.Duration {
	if g.Poll > 0 {
		return g.Poll
	}
	return 250 * time.Millisecond
}

func (g *Gate) flipTimeout() time.Duration {
	if g.FlipTimeout > 0 {
		return g.FlipTimeout
	}
	return 30 * time.Second
}

func (g *Gate) hostWait() time.Duration {
	if g.HostLockWait > 0 {
		return g.HostLockWait
	}
	return 30 * time.Second
}

// Result describes a committed promotion.
type Result struct {
	Service  string      `json:"service"`
	Op       string      `json:"op"`
	OpID     string      `json:"op_id"`
	From     *state.Slot `json:"from,omitempty"`
	To       state.Slot  `json:"to"`
	Warnings []string    `json:"warnings,omitempty"`
}

func (g *Gate) blueGreen(name string) (*config.Service, error) {
	s, err := g.Cfg.Service(name)
	if err != nil {
		return nil, &Error{Code: CodeConfig, Err: err}
	}
	if s.Static() {
		return nil, fail(CodeConfig, "service %s is a static upstream; there is nothing to deploy", name)
	}
	return s, nil
}

// session holds the locks and state of one operation.
type session struct {
	svc   *config.Service
	cur   *state.State
	locks []*state.Lock
	// aborted is the recorded result of an operation abort ended (and its
	// op id), so callers can tell an aborted operation from a refusal.
	aborted, abortedOp string
}

func (s *session) release() {
	for i := len(s.locks) - 1; i >= 0; i-- {
		_ = s.locks[i].Unlock()
	}
}

// begin takes the host lock (shared) and the service lock (exclusive, no
// wait), then loads state. A missing state file is a bootstrap only when no
// managed container of the service exists.
func (g *Gate) begin(ctx context.Context, name string) (*session, error) {
	svc, err := g.blueGreen(name)
	if err != nil {
		return nil, err
	}
	ss := &session{svc: svc}
	host, err := g.Store.LockHost(false, g.hostWait())
	if err != nil {
		return nil, lockErr(err)
	}
	ss.locks = append(ss.locks, host)
	lk, err := g.Store.LockService(name)
	if err != nil {
		ss.release()
		return nil, lockErr(err)
	}
	ss.locks = append(ss.locks, lk)
	cur, err := g.Store.Load(name)
	switch {
	case errors.Is(err, state.ErrNoState):
		for _, c := range []config.Color{config.Blue, config.Green} {
			if _, ok, ierr := g.Docker.Inspect(ctx, svc.ContainerName(c)); ierr != nil {
				ss.release()
				return nil, fail(CodeFailed, "inspect %s: %v", svc.ContainerName(c), ierr)
			} else if ok {
				ss.release()
				return nil, fail(CodeRecovery, "service %s has no state file but container %s exists; refusing to guess (restore %s or remove the container)",
					name, svc.ContainerName(c), g.Store.Path(name))
			}
		}
		cur = &state.State{SchemaVersion: state.SchemaVersion, Service: name}
	case err != nil:
		ss.release()
		return nil, fail(CodeRecovery, "%v", err)
	}
	ss.cur = cur
	return ss, nil
}

func (g *Gate) requirePending(ss *session) error {
	if p := ss.cur.Pending; p != nil {
		return fail(CodeRecovery, "service %s has an unresolved %s (op %s, phase %s, started %s); run `shiftgate recover %s`",
			ss.svc.Name, p.Op, p.OpID, p.Phase, p.StartedAt.Format(time.RFC3339), ss.svc.Name)
	}
	return nil
}

// precheck verifies HAProxy knows both colour servers before anything runs.
func (g *Gate) precheck(ctx context.Context, svc *config.Service) error {
	st, err := g.Router.Servers(ctx, svc.Backend)
	if err != nil {
		return fail(CodeFailed, "HAProxy runtime API: %v", err)
	}
	for _, c := range []config.Color{config.Blue, config.Green} {
		if _, ok := st[string(c)]; !ok {
			return fail(CodeFailed, "HAProxy backend %s has no server %q; run `shiftgate render` first", svc.Backend, c)
		}
	}
	return nil
}

// owned checks that a container with a managed name belongs to svc.
func owned(c docker.Container, svc *config.Service, color config.Color) bool {
	return c.Labels[docker.LabelService] == svc.Name && c.Labels[docker.LabelColor] == string(color)
}

// Deploy promotes image@digest to the inactive colour.
func (g *Gate) Deploy(ctx context.Context, name, digest string) (*Result, error) {
	if !digestRE.MatchString(digest) {
		return nil, fail(CodeUsage, "--digest must be sha256:<64 hex>, got %q", digest)
	}
	ss, err := g.begin(ctx, name)
	if err != nil {
		return nil, err
	}
	defer ss.release()
	if err := g.requirePending(ss); err != nil {
		return nil, err
	}
	return g.deployLocked(ctx, ss, digest)
}

// deployLocked runs a deploy inside a session that holds the locks and has
// no pending intent.
func (g *Gate) deployLocked(ctx context.Context, ss *session, digest string) (*Result, error) {
	svc, cur, name := ss.svc, ss.cur, ss.svc.Name
	if err := g.precheck(ctx, svc); err != nil {
		return nil, err
	}
	target := config.Blue
	if cur.Active != nil {
		target = cur.Active.Color.Other()
	}
	tname := svc.ContainerName(target)
	if c, ok, err := g.Docker.Inspect(ctx, tname); err != nil {
		return nil, fail(CodeFailed, "inspect %s: %v", tname, err)
	} else if ok && !owned(c, svc, target) {
		return nil, fail(CodeFailed, "container %s exists but is not labelled as %s/%s by shiftgate; refusing to replace it", tname, svc.Name, target)
	}

	op := &state.Intent{OpID: state.NewOpID(), Op: "deploy", Phase: state.PhaseStarting,
		From: cur.Active, To: state.Slot{Color: target, Digest: digest}, StartedAt: g.now()}
	cur.Pending = op
	if err := g.Store.Save(cur); err != nil {
		return nil, fail(CodeFailed, "write intent: %v", err)
	}
	g.logf("%s: deploy %s → %s (op %s)", name, short(digest), target, op.OpID)

	ref := svc.Image + "@" + digest
	if err := g.Docker.Pull(ctx, ref); err != nil {
		return nil, g.abort(ss, "pull_failed", err, false)
	}
	// pre_deploy runs while the old colour serves and before the target
	// slot is touched, so a failure leaves routing and rollback intact.
	if svc.PreDeploy != nil {
		if err := g.preDeploy(ctx, ss, digest); err != nil {
			result := "pre_deploy_failed"
			var refused *preDeployRefused
			if errors.As(err, &refused) {
				result = "pre_deploy_refused" // the command never ran
			}
			return nil, g.abort(ss, result, err, false)
		}
	}
	// The target slot is the previous colour: once its container is
	// replaced, rollback to it is impossible. Record that durably right
	// before the removal, not earlier, so a failed pull keeps rollback.
	if cur.Previous != nil && cur.Previous.Color == target && cur.RollbackAvailable {
		cur.RollbackAvailable = false
		if err := g.Store.Save(cur); err != nil {
			cur.RollbackAvailable = true
			return nil, g.abort(ss, "intent_write_failed", err, false)
		}
	}
	_ = g.Docker.Remove(ctx, tname) // absent is fine; ownership checked above
	if err := g.Docker.Run(ctx, RunSpec(svc, target, digest)); err != nil {
		_ = g.Docker.Remove(ctx, tname)
		return nil, g.abort(ss, "start_failed", err, false)
	}
	if err := health.Wait(ctx, g.Health, svc.Health, svc.Port(target)); err != nil {
		g.logf("%s: %s is unhealthy; HAProxy untouched, stopping it", name, target)
		return nil, g.abort(ss, "unhealthy", err, true)
	}
	if err := g.verifyTarget(ctx, svc, op.To); err != nil {
		return nil, g.abort(ss, "target_not_running", err, true)
	}
	return g.flipAndCommit(ctx, ss)
}

// Rollback promotes the previous colour's existing container again.
func (g *Gate) Rollback(ctx context.Context, name string) (*Result, error) {
	ss, err := g.begin(ctx, name)
	if err != nil {
		return nil, err
	}
	defer ss.release()
	if err := g.requirePending(ss); err != nil {
		return nil, err
	}
	svc, cur := ss.svc, ss.cur
	if cur.Active == nil || cur.Previous == nil || !cur.RollbackAvailable {
		return nil, fail(CodeFailed, "service %s has no rollback target (the previous colour was never promoted or has been replaced)", name)
	}
	prev := *cur.Previous
	pname := svc.ContainerName(prev.Color)
	c, ok, err := g.Docker.Inspect(ctx, pname)
	switch {
	case err != nil:
		return nil, fail(CodeFailed, "inspect %s: %v", pname, err)
	case !ok:
		return nil, fail(CodeFailed, "rollback refused: previous container %s no longer exists", pname)
	case !owned(c, svc, prev.Color) || c.Labels[docker.LabelDigest] != prev.Digest:
		return nil, fail(CodeFailed, "rollback refused: container %s does not carry %s/%s@%s", pname, svc.Name, prev.Color, short(prev.Digest))
	}
	if err := g.precheck(ctx, svc); err != nil {
		return nil, err
	}
	op := &state.Intent{OpID: state.NewOpID(), Op: "rollback", Phase: state.PhaseStarting,
		From: cur.Active, To: prev, StartedAt: g.now()}
	cur.Pending = op
	if err := g.Store.Save(cur); err != nil {
		return nil, fail(CodeFailed, "write intent: %v", err)
	}
	g.logf("%s: rollback %s → %s@%s (op %s)", name, cur.Active.Color, prev.Color, short(prev.Digest), op.OpID)
	if !c.Running {
		if err := g.Docker.Start(ctx, pname); err != nil {
			return nil, g.abort(ss, "start_failed", err, true)
		}
	}
	if err := health.Wait(ctx, g.Health, svc.Health, svc.Port(prev.Color)); err != nil {
		return nil, g.abort(ss, "unhealthy", err, true)
	}
	if err := g.verifyTarget(ctx, svc, prev); err != nil {
		return nil, g.abort(ss, "target_not_running", err, true)
	}
	return g.flipAndCommit(ctx, ss)
}

// verifyTarget re-inspects the target after the health check: the probe
// only proves that something answers on the colour port (with host
// networking that could be another process), so the container itself must
// still be running, owned by this service and colour, and at the digest.
func (g *Gate) verifyTarget(ctx context.Context, svc *config.Service, to state.Slot) error {
	name := svc.ContainerName(to.Color)
	c, ok, err := g.Docker.Inspect(ctx, name)
	switch {
	case err != nil:
		return fmt.Errorf("inspect %s: %w", name, err)
	case !ok || !c.Running:
		return fmt.Errorf("container %s is not running after the health check (something else may answer on port %d)", name, svc.Port(to.Color))
	case !owned(c, svc, to.Color) || c.Labels[docker.LabelDigest] != to.Digest:
		return fmt.Errorf("container %s does not carry %s/%s@%s", name, svc.Name, to.Color, short(to.Digest))
	}
	return nil
}

// abort ends an operation that never changed routing: the target container
// is stopped (kept for inspection) when stopTarget, the intent is cleared
// and the failure recorded.
func (g *Gate) abort(ss *session, result string, cause error, stopTarget bool) error {
	ctx := context.WithoutCancel(context.Background())
	svc, cur, op := ss.svc, ss.cur, ss.cur.Pending
	if stopTarget {
		if err := g.Docker.Stop(ctx, svc.ContainerName(op.To.Color), svc.StopTimeout.D()); err != nil {
			g.logf("%s: stop %s: %v", svc.Name, svc.ContainerName(op.To.Color), err)
		}
	}
	cur.Pending = nil
	cur.LastOp, cur.LastOpID, cur.LastResult, cur.LastError = op.Op, op.OpID, result, cause.Error()
	ss.aborted, ss.abortedOp = result, op.OpID
	if err := g.Store.Save(cur); err != nil {
		return fail(CodeRecovery, "%s: %v; and clearing the intent failed: %v", result, cause, err)
	}
	g.audit(svc, op, result, cause, nil)
	return fail(CodeFailed, "%s %s: %s: %v (the previous colour keeps serving)", op.Op, svc.Name, result, cause)
}

func (g *Gate) audit(svc *config.Service, op *state.Intent, result string, cause error, warnings []string) {
	r := state.SwapRecord{Time: g.now(), OpID: op.OpID, Service: svc.Name, Op: op.Op,
		ToColor: op.To.Color, ToDigest: op.To.Digest, Result: result, Warnings: warnings, OpLog: op.OpLog}
	if op.From != nil {
		r.FromColor, r.FromDigest = op.From.Color, op.From.Digest
	}
	if cause != nil {
		r.Error = cause.Error()
	}
	if err := g.Store.AppendSwap(r); err != nil {
		g.logf("%s: swap log append failed (the operation result stands): %v", svc.Name, err)
	}
}

// waitState polls the runtime API until cond holds for server or the flip
// timeout passes. It returns the last observed state.
func (g *Gate) waitState(ctx context.Context, backend, server string, cond func(haproxy.ServerState) bool) (haproxy.ServerState, bool) {
	deadline := time.Now().Add(g.flipTimeout())
	var last haproxy.ServerState
	for {
		st, err := g.Router.Servers(ctx, backend)
		if err == nil {
			last = st[server]
			if cond(last) {
				return last, true
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return last, false
		}
		time.Sleep(g.poll())
	}
}

func (g *Gate) flipAndCommit(ctx context.Context, ss *session) (*Result, error) {
	svc, cur, op := ss.svc, ss.cur, ss.cur.Pending
	be, target := svc.Backend, string(op.To.Color)
	op.Phase = state.PhaseFlipping
	if err := g.Store.Save(cur); err != nil {
		return nil, g.abort(ss, "intent_write_failed", err, true)
	}
	// 1. Target ready, then wait until HAProxy's own checks see it UP.
	err := g.Router.SetState(ctx, be, target, "ready")
	if err == nil {
		if st, ok := g.waitState(ctx, be, target, haproxy.ServerState.Serving); !ok {
			err = fmt.Errorf("HAProxy did not report %s/%s ready and UP within %s (last %s)", be, target, g.flipTimeout(), st)
		}
	}
	if err != nil {
		return nil, g.compensate(ctx, ss, err)
	}
	// 2. Old colour to drain; a lost reply is checked by read-back.
	if op.From != nil {
		old := string(op.From.Color)
		if err := g.Router.SetState(ctx, be, old, "drain"); err != nil {
			st, rerr := g.Router.Servers(ctx, be)
			if rerr != nil || st[old].Ready() {
				return nil, g.compensate(ctx, ss, err)
			}
			g.logf("%s: drain reply lost but read-back shows %s; continuing", svc.Name, st[old])
		}
	}
	return g.commit(ctx, ss)
}

// compensate puts routing back on the old colour after a failed flip. It
// disables the target only once read-back proves the old colour serves; if
// that cannot be proven the intent stays and the caller must recover.
func (g *Gate) compensate(ctx context.Context, ss *session, cause error) error {
	svc, op := ss.svc, ss.cur.Pending
	be, target := svc.Backend, string(op.To.Color)
	ctx = context.WithoutCancel(ctx)
	if op.From != nil {
		old := string(op.From.Color)
		_ = g.Router.SetState(ctx, be, old, "ready")
		if st, ok := g.waitState(ctx, be, old, haproxy.ServerState.Serving); !ok {
			g.audit(svc, op, "recovery_required", cause, []string{"old colour not proven serving: " + st.String()})
			return fail(CodeRecovery, "%s %s: flip failed (%v) and %s is not proven serving (%s); both containers kept, run `shiftgate recover %s`",
				op.Op, svc.Name, cause, old, st, svc.Name)
		}
	}
	_ = g.Router.SetState(ctx, be, target, "maint")
	if st, ok := g.waitState(ctx, be, target, haproxy.ServerState.Maint); !ok {
		g.audit(svc, op, "recovery_required", cause, []string{"target not proven in maintenance: " + st.String()})
		return fail(CodeRecovery, "%s %s: flip failed (%v) and %s could not be put back in maintenance (%s); run `shiftgate recover %s`",
			op.Op, svc.Name, cause, target, st, svc.Name)
	}
	return g.abort(ss, "flip_failed", cause, true)
}

// commit records the promotion, then drains and stops the old colour.
func (g *Gate) commit(ctx context.Context, ss *session) (*Result, error) {
	svc, cur, op := ss.svc, ss.cur, ss.cur.Pending
	to := op.To
	cur.Previous = op.From
	cur.Active = &to
	cur.RollbackAvailable = op.From != nil
	cur.PushHistory(to.Digest)
	cur.Generation++
	cur.LastSwapAt = g.now()
	cur.LastOp, cur.LastOpID, cur.LastResult, cur.LastError = op.Op, op.OpID, "ok", ""
	switch {
	case op.Op == "rollback" && op.From != nil:
		// follow must not bring back the digest that was just rolled back.
		cur.Hold = &state.Hold{Digest: op.From.Digest, Reason: "rolled_back", OpID: op.OpID, At: g.now()}
	case cur.Hold != nil && cur.Hold.Digest == to.Digest:
		cur.Hold = nil // the held digest was deployed on purpose
	}
	cur.Pending = nil
	if err := g.Store.Save(cur); err != nil {
		return nil, fail(CodeRecovery, "%s %s: traffic moved to %s but the commit write failed: %v; run `shiftgate recover %s`",
			op.Op, svc.Name, to.Color, err, svc.Name)
	}
	g.logf("%s: %s now serves %s", svc.Name, to.Color, short(to.Digest))
	res := &Result{Service: svc.Name, Op: op.Op, OpID: op.OpID, From: op.From, To: to}
	if g.SyncConfig {
		if err := g.syncConfig(ctx); err != nil {
			res.Warnings = append(res.Warnings, "config file not refreshed (runtime routing is correct; run `shiftgate render --no-reload`): "+err.Error())
		}
	}
	if op.From != nil {
		res.Warnings = append(res.Warnings, g.drainAndStop(ctx, svc, op.From.Color)...)
	}
	for _, w := range res.Warnings {
		g.logf("%s: warning: %s", svc.Name, w)
	}
	g.audit(svc, op, "ok", nil, res.Warnings)
	return res, nil
}

// drainAndStop waits for the old colour's sessions to reach zero (bounded by
// drain_timeout), puts it in maintenance and stops (does not remove) it.
func (g *Gate) drainAndStop(ctx context.Context, svc *config.Service, old config.Color) []string {
	var warns []string
	be := svc.Backend
	deadline := time.Now().Add(svc.DrainTimeout.D())
	for {
		if ctx.Err() != nil {
			return append(warns, fmt.Sprintf("drain of %s canceled", old))
		}
		n, err := g.Router.Sessions(ctx, be, string(old))
		if ctx.Err() != nil {
			return append(warns, fmt.Sprintf("drain of %s canceled", old))
		}
		if err == nil && n == 0 {
			break
		}
		if time.Now().After(deadline) {
			if err != nil {
				warns = append(warns, fmt.Sprintf("drain of %s not observable: %v", old, err))
			} else {
				warns = append(warns, fmt.Sprintf("drain timeout %s: %d session(s) on %s were cut", svc.DrainTimeout.D(), n, old))
			}
			break
		}
		select {
		case <-ctx.Done():
			return append(warns, fmt.Sprintf("drain of %s canceled", old))
		case <-time.After(g.poll()):
		}
	}
	if err := g.Router.SetState(ctx, be, string(old), "maint"); err != nil {
		warns = append(warns, fmt.Sprintf("set %s maint: %v", old, err))
	}
	if err := g.Docker.Stop(ctx, svc.ContainerName(old), svc.StopTimeout.D()); err != nil {
		warns = append(warns, fmt.Sprintf("stop %s: %v", svc.ContainerName(old), err))
	}
	return warns
}

func (g *Gate) syncConfig(ctx context.Context) error {
	lk, err := g.Store.LockConfigWrite(g.hostWait())
	if err != nil {
		return err
	}
	defer lk.Unlock()
	enabled, err := haproxy.Enabled(g.Cfg, g.Store)
	if err != nil {
		return err
	}
	// A wildcard bind allowed only by an explicit `render --allow-wildcard-bind`
	// stays allowed here when the installed file already carries exactly
	// that bind line: the operator approved it, and refusing would leave the
	// file naming the old colour as active.
	allow, err := haproxy.WildcardsApproved(g.Cfg)
	if err != nil {
		return err
	}
	data, err := haproxy.Render(g.Cfg, enabled, allow)
	if err != nil {
		return err
	}
	return haproxy.Install(ctx, g.Cfg, data)
}

// Recover resolves an operation interrupted after its intent was written.
func (g *Gate) Recover(ctx context.Context, name string) (string, error) {
	ss, err := g.begin(ctx, name)
	if err != nil {
		return "", err
	}
	defer ss.release()
	svc, op := ss.svc, ss.cur.Pending
	if op == nil {
		return "nothing to recover", nil
	}
	if op.Phase == state.PhaseStarting {
		// Routing was never touched: stop the half-started target.
		if err := g.abort(ss, "recovered_abort", errors.New("interrupted before the flip"), true); ExitCode(err) == CodeRecovery {
			return "", err
		}
		return fmt.Sprintf("aborted %s %s before the flip; %s keeps serving", op.Op, op.OpID, slotColor(op.From)), nil
	}
	st, err := g.Router.Servers(ctx, svc.Backend)
	if err != nil {
		return "", fail(CodeRecovery, "cannot read HAProxy state: %v", err)
	}
	tgt := st[string(op.To.Color)]
	var old haproxy.ServerState
	if op.From != nil {
		old = st[string(op.From.Color)]
	}
	c, ok, err := g.Docker.Inspect(ctx, svc.ContainerName(op.To.Color))
	if err != nil {
		return "", fail(CodeRecovery, "inspect target: %v", err)
	}
	targetOK := ok && c.Running && owned(c, svc, op.To.Color) && c.Labels[docker.LabelDigest] == op.To.Digest
	switch {
	case targetOK && tgt.Serving() && (op.From == nil || !old.Ready()):
		res, err := g.commit(ctx, ss)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("completed %s %s: %s serves %s", op.Op, op.OpID, res.To.Color, short(res.To.Digest)), nil
	case op.From != nil && old.Serving(), op.From == nil && tgt.Maint():
		if err := g.compensate(ctx, ss, errors.New("interrupted during the flip")); ExitCode(err) == CodeRecovery {
			return "", err
		}
		return fmt.Sprintf("rolled back %s %s: %s keeps serving", op.Op, op.OpID, slotColor(op.From)), nil
	}
	return "", fail(CodeRecovery, "cannot decide safely: target %s (container ok=%v), old %s; fix HAProxy by hand (set server … state), then rerun recover",
		tgt, targetOK, old)
}

func slotColor(s *state.Slot) string {
	if s == nil {
		return "nothing (first deploy)"
	}
	return string(s.Color)
}

func short(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

// RunSpec is the docker run specification of colour c at digest.
func RunSpec(svc *config.Service, c config.Color, digest string) docker.RunSpec {
	port := svc.ContainerPort
	spec := docker.RunSpec{
		Name:  svc.ContainerName(c),
		Image: svc.Image + "@" + digest,
		Labels: map[string]string{
			docker.LabelService: svc.Name,
			docker.LabelColor:   string(c),
			docker.LabelDigest:  digest,
		},
		EnvFiles: svc.EnvFiles,
		Env:      map[string]string{},
		RunArgs:  svc.RunArgs,
		LogOpts:  logOpts(svc.Log),
	}
	if svc.Network == "host" {
		port = svc.Port(c)
		spec.Network = "host"
	} else {
		spec.Publish = fmt.Sprintf("127.0.0.1:%d:%d", svc.Port(c), port)
	}
	expand := strings.NewReplacer("{port}", strconv.Itoa(port), "{color}", string(c)).Replace
	for k, v := range svc.Env {
		spec.Env[k] = expand(v)
	}
	spec.Env["PORT"] = strconv.Itoa(port)
	for _, a := range svc.Args {
		spec.Args = append(spec.Args, expand(a))
	}
	return spec
}

func logOpts(l config.LogOptions) []string {
	if l.Driver == config.LogDaemon {
		return nil
	}
	return []string{"max-size=" + l.MaxSize, "max-file=" + strconv.Itoa(l.MaxFile)}
}

// PreDeploySpec is the one-shot pre_deploy container for a deploy of colour
// c at digest: the same image, env files, env and network as the colour,
// no published port, no restart policy, removed when it exits.
func PreDeploySpec(svc *config.Service, c config.Color, digest string) docker.RunSpec {
	spec := RunSpec(svc, c, digest)
	spec.Name = svc.PreDeployName()
	spec.OneShot = true
	spec.Publish = ""
	spec.Labels = map[string]string{
		docker.LabelService: svc.Name,
		docker.LabelRole:    "pre_deploy",
		docker.LabelDigest:  digest,
	}
	if svc.PreDeploy != nil && len(svc.PreDeploy.EnvFiles) > 0 {
		envFiles := make([]string, 0, len(svc.EnvFiles)+len(svc.PreDeploy.EnvFiles))
		envFiles = append(envFiles, svc.EnvFiles...)
		envFiles = append(envFiles, svc.PreDeploy.EnvFiles...)
		spec.EnvFiles = envFiles
	}
	port := spec.Env["PORT"]
	expand := strings.NewReplacer("{port}", port, "{color}", string(c)).Replace
	spec.Args = nil
	for _, a := range svc.PreDeploy.Args {
		spec.Args = append(spec.Args, expand(a))
	}
	return spec
}

// maxOpLog caps the captured pre_deploy output.
const maxOpLog = 1 << 20

// preDeploy runs svc.PreDeploy to completion with the new image, writing its
// output to the operation's log file.
func (g *Gate) preDeploy(ctx context.Context, ss *session, digest string) error {
	svc, op := ss.svc, ss.cur.Pending
	spec := PreDeploySpec(svc, op.To.Color, digest)
	if c, ok, err := g.Docker.Inspect(ctx, spec.Name); err != nil {
		return refuse("inspect %s: %v", spec.Name, err)
	} else if ok {
		if c.Labels[docker.LabelService] != svc.Name || c.Labels[docker.LabelRole] != "pre_deploy" {
			return refuse("container %s exists but is not shiftgate's pre_deploy container of %s; refusing to replace it", spec.Name, svc.Name)
		}
		if c.Running {
			return refuse("pre_deploy container %s is still running from an earlier operation; stop it first", spec.Name)
		}
		_ = g.Docker.Remove(ctx, spec.Name) // leftover of an interrupted run
	}
	f, err := g.Store.CreateOpLog(svc.Name, op.OpID)
	if err != nil {
		return refuse("create op log: %v", err)
	}
	defer f.Close()
	op.OpLog = f.Name()
	g.logf("%s: pre_deploy %s (output in %s)", svc.Name, strings.Join(svc.PreDeploy.Args, " "), op.OpLog)
	rctx, cancel := context.WithTimeout(ctx, svc.PreDeploy.Timeout.D())
	defer cancel()
	w := &capWriter{w: f, left: maxOpLog}
	err = g.Docker.RunOnce(rctx, spec, w)
	if w.cut {
		fmt.Fprintf(f, "\n[shiftgate: output truncated at %d bytes]\n", maxOpLog)
	}
	if err != nil {
		if rctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("pre_deploy did not finish within %s (container removed; output in %s)", svc.PreDeploy.Timeout.D(), op.OpLog)
		}
		return fmt.Errorf("pre_deploy: %v (output in %s)", err, op.OpLog)
	}
	return nil
}

// preDeployRefused means the pre_deploy command never ran (the host was
// not ready for it), as opposed to running and failing.
type preDeployRefused struct{ err error }

func (e *preDeployRefused) Error() string { return e.err.Error() }

func refuse(format string, a ...any) error {
	return &preDeployRefused{fmt.Errorf(format, a...)}
}

// capWriter writes at most left bytes and drops the rest.
type capWriter struct {
	w    io.Writer
	left int
	cut  bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > c.left {
		p, c.cut = p[:c.left], true
	}
	if len(p) > 0 {
		if _, err := c.w.Write(p); err != nil {
			return 0, err
		}
		c.left -= len(p)
	}
	return n, nil
}
