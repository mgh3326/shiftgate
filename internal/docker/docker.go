// Package docker drives containers through the docker CLI. Shelling out keeps
// the binary small and static; the Engine interface lets tests substitute it.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Labels shiftgate puts on every container it creates.
const (
	LabelService = "shiftgate.service"
	LabelColor   = "shiftgate.color"
	LabelDigest  = "shiftgate.digest"
	// LabelRole marks a container that is not a colour (pre_deploy).
	LabelRole = "shiftgate.role"
)

// RunSpec is everything `docker run` needs for one colour.
type RunSpec struct {
	Name     string
	Image    string // repo@sha256:…
	Labels   map[string]string
	EnvFiles []string // passed by path; never opened by shiftgate
	Env      map[string]string
	Publish  string // "127.0.0.1:18080:8080", empty with host networking
	Network  string // "" or "host"
	RunArgs  []string
	Args     []string
	// LogOpts are json-file --log-opt values ("max-size=10m"); when set the
	// log driver is pinned to json-file so the options always apply.
	LogOpts []string
	// OneShot runs in the foreground with --rm and no restart policy.
	OneShot bool
}

// Container is what Inspect reports.
type Container struct {
	ID           string
	Running      bool
	Status       string // created | running | paused | restarting | exited | dead
	StartedAt    time.Time
	RestartCount int // restarts by Docker's restart policy
	Labels       map[string]string
}

// ImageInfo contains Docker's image size and uncompressed layer identities.
// Docker does not expose per-pull network bytes through `docker image inspect`.
type ImageInfo struct {
	Layers []string `json:"-"`
	Bytes  int64    `json:"bytes"`
}

// PullReport compares the pulled image with the previous active image. BytesPulled
// stays nil because Docker's inspect API reports image size, not transfer bytes.
type PullReport struct {
	Layers             *int   `json:"layers"`
	LayersAlreadyThere *int   `json:"layers_already_present"`
	LayersPulled       *int   `json:"layers_pulled"`
	ImageBytes         *int64 `json:"image_bytes"`
	BytesPulled        *int64 `json:"bytes_pulled"`
	PreviousDigest     string `json:"previous_digest,omitempty"`
	PreviousLayers     *int   `json:"previous_layers"`
	PreviousImageBytes *int64 `json:"previous_image_bytes"`
	PreviousShared     *int   `json:"previous_shared_layers"`
	Warning            string `json:"warning,omitempty"`
}

// Engine is the container runtime surface the deploy logic needs.
type Engine interface {
	Pull(ctx context.Context, ref string) error
	ImageInfo(ctx context.Context, ref string) (ImageInfo, error)
	Run(ctx context.Context, spec RunSpec) error
	// RunOnce runs a OneShot spec to completion, writing its combined
	// output to out. A non-zero exit is an error. When ctx ends first the
	// container is removed.
	RunOnce(ctx context.Context, spec RunSpec, out io.Writer) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string, timeout time.Duration) error
	// Restart stops (with timeout) and starts the container again.
	Restart(ctx context.Context, name string, timeout time.Duration) error
	Remove(ctx context.Context, name string) error
	// Inspect returns ok=false when the container does not exist.
	Inspect(ctx context.Context, name string) (c Container, ok bool, err error)
	// ImageDigests lists the repo digests of local images of repo.
	ImageDigests(ctx context.Context, repo string) ([]string, error)
	RemoveImage(ctx context.Context, ref string) error
}

// CLI is the Engine backed by the docker binary.
type CLI struct{ Bin string }

func (c CLI) run(ctx context.Context, args ...string) (string, error) {
	bin := c.Bin
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (c CLI) Pull(ctx context.Context, ref string) error {
	_, err := c.run(ctx, "pull", "--quiet", ref)
	return err
}

func (c CLI) ImageInfo(ctx context.Context, ref string) (ImageInfo, error) {
	out, err := c.run(ctx, "image", "inspect", "--format", "{{json .}}", ref)
	if err != nil {
		return ImageInfo{}, err
	}
	var raw struct {
		Size   int64 `json:"Size"`
		RootFS struct {
			Layers []string `json:"Layers"`
		} `json:"RootFS"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return ImageInfo{}, fmt.Errorf("docker image inspect %s: %w", ref, err)
	}
	return ImageInfo{Layers: raw.RootFS.Layers, Bytes: raw.Size}, nil
}

// ComparePull reports the new image's layer count, layers shared with the
// previous active image, and Docker's total image sizes. Transfer bytes are
// intentionally unknown: Docker's CLI does not report them after a quiet pull.
func ComparePull(current ImageInfo, previous *ImageInfo) PullReport {
	layers := len(current.Layers)
	r := PullReport{Layers: &layers}
	sz := current.Bytes
	r.ImageBytes = &sz
	if previous == nil {
		return r
	}
	prevCount, prevSize := len(previous.Layers), previous.Bytes
	r.PreviousLayers, r.PreviousImageBytes = &prevCount, &prevSize
	prev := make(map[string]bool, len(previous.Layers))
	for _, layer := range previous.Layers {
		prev[layer] = true
	}
	shared := 0
	for _, layer := range current.Layers {
		if prev[layer] {
			shared++
		}
	}
	pulled := len(current.Layers) - shared
	r.LayersAlreadyThere = &shared
	r.LayersPulled = &pulled
	r.PreviousShared = &shared
	return r
}

// ArgList builds the docker run argument list (exported for tests and docs).
func (s RunSpec) ArgList() []string {
	args := []string{"run", "-d", "--name", s.Name, "--restart", "unless-stopped"}
	if s.OneShot {
		args = []string{"run", "--rm", "--name", s.Name}
	}
	for _, k := range sortedKeys(s.Labels) {
		args = append(args, "--label", k+"="+s.Labels[k])
	}
	for _, f := range s.EnvFiles {
		args = append(args, "--env-file", f)
	}
	for _, k := range sortedKeys(s.Env) {
		args = append(args, "-e", k+"="+s.Env[k])
	}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	if s.Publish != "" {
		args = append(args, "-p", s.Publish)
	}
	if len(s.LogOpts) > 0 {
		args = append(args, "--log-driver", "json-file")
		for _, o := range s.LogOpts {
			args = append(args, "--log-opt", o)
		}
	}
	args = append(args, s.RunArgs...)
	args = append(args, s.Image)
	return append(args, s.Args...)
}

func (c CLI) Run(ctx context.Context, spec RunSpec) error {
	_, err := c.run(ctx, spec.ArgList()...)
	return err
}

func (c CLI) RunOnce(ctx context.Context, spec RunSpec, out io.Writer) error {
	if !spec.OneShot {
		return errors.New("RunOnce needs a OneShot spec")
	}
	bin := c.Bin
	if bin == "" {
		bin = "docker"
	}
	args := spec.ArgList()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if ctx.Err() != nil {
		// Killing the CLI does not stop the container; remove it.
		_, _ = c.run(context.WithoutCancel(ctx), "rm", "-f", spec.Name)
		return fmt.Errorf("docker run %s: %w", spec.Name, ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("docker run %s: %w", spec.Name, err)
	}
	return nil
}

func (c CLI) Restart(ctx context.Context, name string, timeout time.Duration) error {
	_, err := c.run(ctx, "restart", "--time", strconv.Itoa(int(timeout.Seconds())), name)
	return err
}

func (c CLI) Start(ctx context.Context, name string) error {
	_, err := c.run(ctx, "start", name)
	return err
}

func (c CLI) Stop(ctx context.Context, name string, timeout time.Duration) error {
	_, err := c.run(ctx, "stop", "--time", strconv.Itoa(int(timeout.Seconds())), name)
	return err
}

func (c CLI) Remove(ctx context.Context, name string) error {
	_, err := c.run(ctx, "rm", "-f", name)
	return err
}

func (c CLI) Inspect(ctx context.Context, name string) (Container, bool, error) {
	out, err := c.run(ctx, "container", "inspect", "--format", "{{json .}}", name)
	if err != nil {
		if strings.Contains(err.Error(), "No such container") || strings.Contains(err.Error(), "no such container") {
			return Container{}, false, nil
		}
		return Container{}, false, err
	}
	return parseInspect(name, out)
}

func parseInspect(name, out string) (Container, bool, error) {
	var raw struct {
		ID           string `json:"Id"`
		RestartCount int
		State        struct {
			Running   bool
			Status    string
			StartedAt string
		}
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return Container{}, false, fmt.Errorf("docker inspect %s: %w", name, err)
	}
	c := Container{ID: raw.ID, Running: raw.State.Running, Status: raw.State.Status,
		RestartCount: raw.RestartCount, Labels: raw.Config.Labels}
	// Docker reports "0001-01-01T00:00:00Z" for a never-started container.
	if t, err := time.Parse(time.RFC3339Nano, raw.State.StartedAt); err == nil && t.Year() > 1 {
		c.StartedAt = t
	}
	return c, true, nil
}

func (c CLI) ImageDigests(ctx context.Context, repo string) ([]string, error) {
	// The classic image store omits digest-only pulls from the default list.
	// Include all images and read repository membership from RepoDigests.
	out, err := c.run(ctx, "image", "ls", "-a", "-q", "--no-trunc")
	if err != nil {
		return nil, err
	}
	var ds []string
	seenIDs, seenDigests := map[string]bool{}, map[string]bool{}
	for _, id := range strings.Fields(out) {
		if seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		refsJSON, err := c.run(ctx, "image", "inspect", "--format", "{{json .RepoDigests}}", id)
		if err != nil {
			return nil, fmt.Errorf("inspect image %s: %w", id, err)
		}
		var refs []string
		if err := json.Unmarshal([]byte(refsJSON), &refs); err != nil {
			return nil, fmt.Errorf("decode repository digests for image %s: %w", id, err)
		}
		for _, ref := range refs {
			r, d, ok := strings.Cut(ref, "@")
			if !ok || r != repo || !strings.HasPrefix(d, "sha256:") || seenDigests[d] {
				continue
			}
			seenDigests[d] = true
			ds = append(ds, d)
		}
	}
	return ds, nil
}

// RemoveImage removes one image reference without force.
func (c CLI) RemoveImage(ctx context.Context, ref string) error {
	// Docker can untag a reference even when a container uses the same image
	// through another reference. Check all container states before untagging.
	out, err := c.run(ctx, "container", "ls", "--all", "--quiet", "--filter", "ancestor="+ref)
	if err != nil {
		return err
	}
	if containers := strings.Fields(out); len(containers) != 0 {
		id, err := c.run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
		if err != nil {
			return err
		}
		// The ancestor filter also matches descendant images.
		args := append([]string{"container", "inspect", "--format", "{{.Image}}"}, containers...)
		images, err := c.run(ctx, args...)
		if err != nil {
			return err
		}
		if id := strings.TrimSpace(id); slices.Contains(strings.Fields(images), id) {
			return fmt.Errorf("image %s is in use by a container", id)
		}
	}
	_, err = c.run(ctx, "image", "rm", ref)
	return err
}

// ErrNotOwned means a container with a managed name was not created by
// shiftgate for this service.
var ErrNotOwned = errors.New("container is not owned by shiftgate for this service")

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
