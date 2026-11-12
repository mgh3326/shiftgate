package deploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/docker"
	"github.com/mgh3326/shiftgate/internal/haproxy"
	"github.com/mgh3326/shiftgate/internal/state"
)

// ServiceStatus is one service in `shiftgate status`.
type ServiceStatus struct {
	Name       string                  `json:"name"`
	Static     bool                    `json:"static,omitempty"`
	Backend    string                  `json:"backend"`
	State      *state.State            `json:"state,omitempty"`
	Servers    map[string]ServerStatus `json:"servers,omitempty"`
	Containers map[string]string       `json:"containers,omitempty"`
	// Colors is the per-colour container detail for exporters.
	Colors map[string]ColorStatus `json:"colors,omitempty"`
	// LastOp is the newest swaps.log record: deploy, rollback, recover or
	// heal, with its result.
	LastOp *state.SwapRecord `json:"last_op,omitempty"`
	Errors []string          `json:"errors,omitempty"`
}

// ColorStatus is one colour's container as Docker reports it.
type ColorStatus struct {
	State         string     `json:"state"` // absent | running | exited | restarting | …
	Digest        string     `json:"digest,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	UptimeSeconds int64      `json:"uptime_seconds"` // 0 unless running
	RestartCount  int        `json:"restart_count"`  // restarts by Docker's restart policy
}

// ServerStatus is HAProxy's live view of one server.
type ServerStatus struct {
	Admin string `json:"admin"` // ready | drain | maint
	Op    string `json:"op"`    // up | down | starting | stopping
}

// Status reads state, HAProxy and Docker without taking locks (read-only).
func (g *Gate) Status(ctx context.Context) []ServiceStatus {
	var out []ServiceStatus
	for _, s := range g.Cfg.Services {
		ss := ServiceStatus{Name: s.Name, Static: s.Static(), Backend: s.Backend}
		if !s.Static() {
			cur, err := g.Store.Load(s.Name)
			switch {
			case err == nil:
				ss.State = cur
			case !errors.Is(err, state.ErrNoState):
				ss.Errors = append(ss.Errors, err.Error())
			}
			if r, err := g.Store.LastSwap(s.Name); err != nil {
				ss.Errors = append(ss.Errors, err.Error())
			} else {
				ss.LastOp = r
			}
			ss.Containers = map[string]string{}
			ss.Colors = map[string]ColorStatus{}
			for _, c := range []config.Color{config.Blue, config.Green} {
				ct, ok, err := g.Docker.Inspect(ctx, s.ContainerName(c))
				switch {
				case err != nil:
					ss.Errors = append(ss.Errors, err.Error())
				case !ok:
					ss.Containers[string(c)] = "absent"
					ss.Colors[string(c)] = ColorStatus{State: "absent"}
				default:
					ss.Containers[string(c)] = map[bool]string{true: "running", false: "stopped"}[ct.Running]
					cs := ColorStatus{State: ct.Status, Digest: ct.Labels[docker.LabelDigest], RestartCount: ct.RestartCount}
					if cs.State == "" {
						cs.State = ss.Containers[string(c)]
					}
					if !ct.StartedAt.IsZero() {
						t := ct.StartedAt.UTC()
						cs.StartedAt = &t
						if ct.Running {
							cs.UptimeSeconds = max(int64(g.now().Sub(t)/time.Second), 0)
						}
					}
					ss.Colors[string(c)] = cs
				}
			}
		}
		if g.Router != nil {
			st, err := g.Router.Servers(ctx, s.Backend)
			if err != nil {
				ss.Errors = append(ss.Errors, "haproxy: "+err.Error())
			} else {
				ss.Servers = map[string]ServerStatus{}
				for name, v := range st {
					admin := "ready"
					if v.Maint() {
						admin = "maint"
					} else if v.Draining() {
						admin = "drain"
					}
					op := map[int]string{0: "down", 1: "starting", 2: "up", 3: "stopping"}[v.OpState]
					ss.Servers[name] = ServerStatus{Admin: admin, Op: op}
				}
			}
		}
		out = append(out, ss)
	}
	return out
}

// PruneResult lists what prune removed and kept.
type PruneResult struct {
	Service  string   `json:"service"`
	Removed  []string `json:"removed,omitempty"`
	Kept     []string `json:"kept,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Prune removes local images of each service's repository except the protected
// ones: active, previous, pending, the image of each colour container, and
// the newest keep_digests entries of the promotion history. When multiple
// non-static services share an image repository, the protected set is the union
// of their keep sets so pruning one service never removes images another protects.
// It holds the host lock exclusively so no pull or run interleaves.
func (g *Gate) Prune(ctx context.Context, only string) ([]PruneResult, error) {
	host, err := g.Store.LockHost(true, g.hostWait())
	if err != nil {
		return nil, lockErr(err)
	}
	defer host.Unlock()

	if only != "" {
		if _, err := g.blueGreen(only); err != nil {
			return nil, err
		}
	}

	var targets []*config.Service
	for _, s := range g.Cfg.Services {
		if s.Static() || (only != "" && s.Name != only) {
			continue
		}
		targets = append(targets, s)
	}

	// Before removing anything, compute the protected set per image repository
	// as the union of the keep sets of every non-static configured service with
	// that image (regardless of the only filter). Removal for a service then
	// skips any digest in its repository's union set.
	//
	// Locking order:
	// 1. Host lock (exclusive) is held above for the duration of Prune to prevent
	//    concurrent deploys, rollbacks, or renders.
	// 2. Service locks needed to read each sharing service's state are acquired
	//    in deterministic alphabetical (lexicographical) order by service name
	//    to guarantee deadlock-free execution.
	// 3. During image removal in pruneOne, each service lock is acquired
	//    individually in target order.
	unionKeep := map[string]map[string]bool{}
	for _, target := range targets {
		if unionKeep[target.Image] != nil {
			continue
		}
		repoKeep := map[string]bool{}
		sharers := g.sharingServices(target.Image)
		for _, s := range sharers {
			keep, err := g.serviceKeepSet(ctx, s)
			if err != nil {
				return nil, err
			}
			for d := range keep {
				repoKeep[d] = true
			}
		}
		unionKeep[target.Image] = repoKeep
	}

	var out []PruneResult
	for _, s := range targets {
		res, err := g.pruneOne(ctx, s, unionKeep[s.Image])
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// sharingServices returns all non-static configured services that share the
// given image repository, sorted alphabetically by service name to ensure
// deterministic lock acquisition without deadlock.
func (g *Gate) sharingServices(image string) []*config.Service {
	var svcs []*config.Service
	for _, s := range g.Cfg.Services {
		if !s.Static() && s.Image == image {
			svcs = append(svcs, s)
		}
	}
	slices.SortFunc(svcs, func(a, b *config.Service) int {
		return strings.Compare(a.Name, b.Name)
	})
	return svcs
}

func (g *Gate) serviceKeepSet(ctx context.Context, s *config.Service) (map[string]bool, error) {
	lk, err := g.Store.LockService(s.Name)
	if err != nil {
		return nil, lockErr(err)
	}
	defer lk.Unlock()

	cur, err := g.Store.Load(s.Name)
	if errors.Is(err, state.ErrNoState) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, fail(CodeRecovery, "%v", err)
	}

	keep := map[string]bool{}
	for _, sl := range []*state.Slot{cur.Active, cur.Previous} {
		if sl != nil {
			keep[sl.Digest] = true
		}
	}
	if p := cur.Pending; p != nil {
		keep[p.To.Digest] = true
		if p.From != nil {
			keep[p.From.Digest] = true
		}
	}
	for i, d := range cur.History {
		if i < g.Cfg.KeepDigests {
			keep[d] = true
		}
	}
	for _, c := range []config.Color{config.Blue, config.Green} {
		ct, ok, err := g.Docker.Inspect(ctx, s.ContainerName(c))
		if err != nil {
			return nil, fail(CodeFailed, "inspect %s: %v", s.ContainerName(c), err)
		}
		if ok && ct.Labels[docker.LabelDigest] != "" {
			keep[ct.Labels[docker.LabelDigest]] = true
		}
	}
	return keep, nil
}

func (g *Gate) pruneOne(ctx context.Context, s *config.Service, unionKeep map[string]bool) (PruneResult, error) {
	res := PruneResult{Service: s.Name}
	lk, err := g.Store.LockService(s.Name)
	if err != nil {
		return res, lockErr(err)
	}
	defer lk.Unlock()

	_, err = g.Store.Load(s.Name)
	if errors.Is(err, state.ErrNoState) {
		return res, nil
	}
	if err != nil {
		return res, fail(CodeRecovery, "%v", err)
	}

	digests, err := g.Docker.ImageDigests(ctx, s.Image)
	if err != nil {
		return res, fail(CodeFailed, "list images of %s: %v", s.Image, err)
	}
	for _, d := range digests {
		if unionKeep[d] {
			res.Kept = append(res.Kept, d)
			continue
		}
		if err := g.Docker.RemoveImage(ctx, s.Image+"@"+d); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("kept %s: %v", short(d), err))
			continue
		}
		res.Removed = append(res.Removed, d)
	}
	return res, nil
}

// RenderOptions controls `shiftgate render`.
type RenderOptions struct {
	AllowWildcard bool
	NoReload      bool
	DryRun        bool // print only, write nothing
}

// Render writes the generated config (validated before install) and reloads
// HAProxy. It holds the host lock exclusively, so no flip is in progress.
func (g *Gate) Render(ctx context.Context, o RenderOptions) ([]byte, error) {
	host, err := g.Store.LockHost(true, g.hostWait())
	if err != nil {
		return nil, lockErr(err)
	}
	defer host.Unlock()
	enabled, err := haproxy.Enabled(g.Cfg, g.Store)
	if err != nil {
		return nil, &Error{Code: CodeRecovery, Err: err}
	}
	data, err := haproxy.Render(g.Cfg, enabled, o.AllowWildcard)
	if err != nil {
		return nil, &Error{Code: CodeConfig, Err: err}
	}
	if o.DryRun {
		return data, nil
	}
	if err := haproxy.Install(ctx, g.Cfg, data); err != nil {
		return nil, &Error{Code: CodeConfig, Err: err}
	}
	if o.NoReload {
		return data, nil
	}
	if err := haproxy.Reload(ctx, g.Cfg); err != nil {
		return nil, fail(CodeFailed, "%v", err)
	}
	return data, nil
}
