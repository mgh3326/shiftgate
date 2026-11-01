package deploy

import (
	"context"

	"github.com/mgh3326/shiftgate/internal/registry"
	"github.com/mgh3326/shiftgate/internal/state"
)

// Resolver turns image:tag into the digest the registry serves.
type Resolver interface {
	Digest(ctx context.Context, image, tag string, cred *registry.Credentials) (string, error)
}

// Follow outcomes.
const (
	FollowUpToDate = "up_to_date" // the tag already points at the active digest
	FollowHeld     = "held"       // the tag points at a held digest
	FollowDeployed = "deployed"
)

// FollowResult is what `shiftgate follow` did.
type FollowResult struct {
	Service string      `json:"service"`
	Tag     string      `json:"tag"`
	Digest  string      `json:"digest"`
	Action  string      `json:"action"`
	Hold    *state.Hold `json:"hold,omitempty"`
	Deploy  *Result     `json:"deploy,omitempty"`
}

// noHoldResults are deploy failures that say nothing about the image (the
// registry or the network failed), so the next follow retries the digest.
var noHoldResults = map[string]bool{"pull_failed": true, "intent_write_failed": true}

// Follow resolves svc's image at tag and deploys the digest when it differs
// from the active one. It is a no-op when the digest is active or held. The
// registry is asked before any lock is taken; the decision and the deploy
// run under the service lock, so a concurrent deploy makes it exit 75.
func (g *Gate) Follow(ctx context.Context, name, tag string) (*FollowResult, error) {
	svc, err := g.blueGreen(name)
	if err != nil {
		return nil, err
	}
	if !registry.ValidTag(tag) {
		return nil, fail(CodeUsage, "--tag %q is not a valid image tag", tag)
	}
	if g.Registry == nil {
		return nil, fail(CodeFailed, "follow: no registry client configured")
	}
	var cred *registry.Credentials
	if svc.RegistryAuthFile != "" {
		if cred, err = registry.LoadCredentials(svc.RegistryAuthFile); err != nil {
			return nil, fail(CodeConfig, "%v", err)
		}
	}
	digest, err := g.Registry.Digest(ctx, svc.Image, tag, cred)
	if err != nil {
		return nil, fail(CodeFailed, "follow %s: resolve %s:%s: %v", name, svc.Image, tag, err)
	}
	if !digestRE.MatchString(digest) {
		return nil, fail(CodeFailed, "follow %s: registry returned %q, not a sha256 digest", name, digest)
	}
	ss, err := g.begin(ctx, name)
	if err != nil {
		return nil, err
	}
	defer ss.release()
	if err := g.requirePending(ss); err != nil {
		return nil, err
	}
	cur := ss.cur
	res := &FollowResult{Service: name, Tag: tag, Digest: digest}
	if cur.Active != nil && cur.Active.Digest == digest {
		res.Action = FollowUpToDate
		return res, nil
	}
	if h := cur.Hold; h != nil && h.Digest == digest {
		res.Action, res.Hold = FollowHeld, h
		return res, nil
	}
	if cur.Hold != nil {
		// A new digest appeared: the hold has done its job. The intent
		// write at the start of the deploy persists the release.
		g.logf("%s: %s:%s moved past held %s; releasing the hold", name, svc.Image, tag, short(cur.Hold.Digest))
		cur.Hold = nil
	}
	dres, err := g.deployLocked(ctx, ss, digest)
	if err != nil {
		if ExitCode(err) == CodeFailed && ss.aborted != "" && !noHoldResults[ss.aborted] {
			// Do not retry the same failing image (and its pre_deploy) on
			// every follow run; a new push or an explicit deploy clears it.
			cur.Hold = &state.Hold{Digest: digest, Reason: "deploy_failed:" + ss.aborted, OpID: ss.abortedOp, At: g.now()}
			if serr := g.Store.Save(cur); serr != nil {
				g.logf("%s: could not record the hold on %s: %v", name, short(digest), serr)
			}
		}
		return nil, err
	}
	res.Action, res.Deploy = FollowDeployed, dres
	return res, nil
}
