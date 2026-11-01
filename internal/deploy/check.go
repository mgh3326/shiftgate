package deploy

import (
	"context"
	"errors"
	"fmt"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/docker"
	"github.com/mgh3326/shiftgate/internal/haproxy"
	"github.com/mgh3326/shiftgate/internal/health"
	"github.com/mgh3326/shiftgate/internal/state"
)

// Failure kinds of `shiftgate check`. Only the healable ones make --heal
// restart the active container; the others need a person or a deploy.
const (
	KindStopped   = "container_stopped"  // healable
	KindUnhealthy = "unhealthy"          // healable
	KindDown      = "haproxy_down"       // healable: HAProxy's checks fail
	KindMissing   = "container_missing"  // deploy again
	KindForeign   = "container_mismatch" // not ours, or another digest
	KindRouting   = "haproxy_not_ready"  // admin state maint/drain: not a container problem
	KindPending   = "recovery_required"  // an interrupted operation
	KindError     = "error"              // Docker, HAProxy or state unreadable
	healAuditOp   = "heal"
	checkSkipLock = "an operation is in progress"
)

func healable(kind string) bool {
	return kind == KindStopped || kind == KindUnhealthy || kind == KindDown
}

// CheckResult is one service in `shiftgate check`.
type CheckResult struct {
	Service string       `json:"service"`
	Color   config.Color `json:"color,omitempty"`
	Digest  string       `json:"digest,omitempty"`
	// Skipped says why the service was not checked (never deployed, or an
	// operation is running); a skip is not a failure.
	Skipped string `json:"skipped,omitempty"`
	// Kind and Reason describe the failure still present at the end ("" ok).
	Kind   string `json:"kind,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Healed is set when --heal restarted the active container and the
	// re-check passed; HealedFrom is the reason that triggered it.
	Healed     bool   `json:"healed,omitempty"`
	HealedFrom string `json:"healed_from,omitempty"`
}

// Failed reports whether the service is failing after check (and heal).
func (r CheckResult) Failed() bool { return r.Kind != "" }

// Check verifies, for each blue-green service (or only), that the active
// colour's container is running, its health endpoint answers within
// health.timeout and HAProxy sees it ready and UP. With heal, a healable
// failure restarts the active container once, under the service lock, and
// re-checks; check never flips colours or changes state.json. The error is
// non-nil (exit 1) when any service is still failing.
func (g *Gate) Check(ctx context.Context, only string, heal bool) ([]CheckResult, error) {
	if only != "" {
		if _, err := g.blueGreen(only); err != nil {
			return nil, err
		}
	}
	var out []CheckResult
	failed := 0
	for _, s := range g.Cfg.Services {
		if s.Static() || (only != "" && s.Name != only) {
			continue
		}
		r := g.checkOne(ctx, s, heal)
		if r.Failed() {
			failed++
		}
		out = append(out, r)
	}
	if failed > 0 {
		return out, fail(CodeFailed, "check: %d of %d service(s) failing", failed, len(out))
	}
	return out, nil
}

func (g *Gate) checkOne(ctx context.Context, svc *config.Service, heal bool) CheckResult {
	r := CheckResult{Service: svc.Name}
	cur, skip, kind, reason := g.checkState(svc)
	if skip != "" || kind != "" {
		r.Skipped, r.Kind, r.Reason = skip, kind, reason
		return r
	}
	act := *cur.Active
	r.Color, r.Digest = act.Color, act.Digest
	kind, reason = g.probeActive(ctx, svc, act, false)
	if kind == "" {
		return r
	}
	// A deploy may have committed while we probed (the old colour is being
	// drained and stopped): that is not a failure of the active colour.
	if now, err := g.Store.Load(svc.Name); err == nil && (now.Generation != cur.Generation || now.Pending != nil) {
		r.Skipped = "state changed during the check (a deploy or rollback ran)"
		return r
	}
	r.Kind, r.Reason = kind, reason
	if !heal || !healable(kind) {
		return r
	}
	return g.heal(ctx, svc, cur, r)
}

// checkState reads committed state without holding a lock. A pending intent
// is a failure only when no operation holds the service lock.
func (g *Gate) checkState(svc *config.Service) (cur *state.State, skip, kind, reason string) {
	cur, err := g.Store.Load(svc.Name)
	switch {
	case errors.Is(err, state.ErrNoState):
		return nil, "not deployed yet", "", ""
	case err != nil:
		return nil, "", KindError, err.Error()
	}
	if p := cur.Pending; p != nil {
		lk, err := g.Store.LockService(svc.Name)
		if errors.Is(err, state.ErrLocked) {
			return nil, checkSkipLock, "", ""
		}
		if err != nil {
			return nil, "", KindError, err.Error()
		}
		// Re-read under the lock: the operation may have just finished.
		cur, err = g.Store.Load(svc.Name)
		lk.Unlock()
		if err != nil {
			return nil, "", KindError, err.Error()
		}
		if p := cur.Pending; p != nil {
			return nil, "", KindPending, fmt.Sprintf("unresolved %s (op %s, phase %s); run `shiftgate recover %s`", p.Op, p.OpID, p.Phase, svc.Name)
		}
	}
	if cur.Active == nil {
		return nil, "not deployed yet", "", ""
	}
	return cur, "", "", ""
}

// probeActive checks container, health and HAProxy for the active slot.
// After a heal (wait) it gives HAProxy up to the flip timeout to see the
// server UP again; otherwise it reads HAProxy once.
func (g *Gate) probeActive(ctx context.Context, svc *config.Service, act state.Slot, wait bool) (kind, reason string) {
	name := svc.ContainerName(act.Color)
	c, ok, err := g.Docker.Inspect(ctx, name)
	switch {
	case err != nil:
		return KindError, fmt.Sprintf("inspect %s: %v", name, err)
	case !ok:
		return KindMissing, fmt.Sprintf("active container %s does not exist; deploy again", name)
	case !owned(c, svc, act.Color) || c.Labels[docker.LabelDigest] != act.Digest:
		return KindForeign, fmt.Sprintf("container %s does not carry %s/%s@%s", name, svc.Name, act.Color, short(act.Digest))
	case !c.Running:
		return KindStopped, fmt.Sprintf("active container %s is not running (%s)", name, c.Status)
	}
	if err := health.Wait(ctx, g.Health, svc.Health, svc.Port(act.Color)); err != nil {
		return KindUnhealthy, fmt.Sprintf("%s: %v", act.Color, err)
	}
	server := string(act.Color)
	var st haproxy.ServerState
	if wait {
		var ok bool
		if st, ok = g.waitState(ctx, svc.Backend, server, haproxy.ServerState.Serving); !ok && st.Name == "" {
			return KindError, fmt.Sprintf("HAProxy runtime API: no answer for %s/%s", svc.Backend, server)
		}
	} else {
		all, err := g.Router.Servers(ctx, svc.Backend)
		if err != nil {
			return KindError, fmt.Sprintf("HAProxy runtime API: %v", err)
		}
		if st, ok = all[server]; !ok {
			return KindError, fmt.Sprintf("HAProxy backend %s has no server %s", svc.Backend, server)
		}
	}
	switch {
	case !st.Ready():
		return KindRouting, fmt.Sprintf("HAProxy has %s/%s in %s; check does not change routing", svc.Backend, server, st)
	case !st.Up():
		return KindDown, fmt.Sprintf("HAProxy reports %s/%s DOWN (%s)", svc.Backend, server, st)
	}
	return "", ""
}

// heal restarts the active container once and re-checks it, holding the
// service lock so no deploy or rollback interleaves. It never touches the
// other colour or HAProxy.
func (g *Gate) heal(ctx context.Context, svc *config.Service, seen *state.State, r CheckResult) CheckResult {
	ss, err := g.begin(ctx, svc.Name)
	if err != nil {
		if errors.Is(err, state.ErrLocked) {
			r.Reason += "; not healed: " + checkSkipLock
		} else {
			r.Reason += "; not healed: " + err.Error()
		}
		return r
	}
	defer ss.release()
	cur := ss.cur
	if cur.Pending != nil || cur.Generation != seen.Generation || cur.Active == nil || *cur.Active != *seen.Active {
		r.Reason += "; not healed: state changed since the check"
		return r
	}
	act := *cur.Active
	name := svc.ContainerName(act.Color)
	op := &state.Intent{OpID: state.NewOpID(), Op: healAuditOp, To: act, StartedAt: g.now()}
	g.logf("%s: heal: restarting %s (%s)", svc.Name, name, r.Reason)
	if err := g.Docker.Restart(ctx, name, svc.StopTimeout.D()); err != nil {
		cause := fmt.Errorf("restart %s: %w", name, err)
		g.audit(svc, op, "failed", cause, []string{r.Reason})
		r.Reason += "; heal failed: " + cause.Error()
		return r
	}
	kind, reason := g.probeActive(ctx, svc, act, true)
	if kind != "" {
		cause := errors.New(reason)
		g.audit(svc, op, "failed", cause, []string{r.Reason})
		r.Kind, r.Reason = kind, fmt.Sprintf("%s; restarted %s once, still failing: %s", r.Reason, name, reason)
		return r
	}
	g.audit(svc, op, "ok", nil, []string{r.Reason})
	r.Healed, r.HealedFrom, r.Kind, r.Reason = true, r.Reason, "", ""
	return r
}
