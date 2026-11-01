// Package docker drives containers through the docker CLI. Shelling out keeps
// the binary small and static; the Engine interface lets tests substitute it.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

// Container is what Inspect reports.
type Container struct {
	ID      string
	Running bool
	Labels  map[string]string
}

// Engine is the container runtime surface the deploy logic needs.
type Engine interface {
	Pull(ctx context.Context, ref string) error
	Run(ctx context.Context, spec RunSpec) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string, timeout time.Duration) error
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

// Args builds the docker run argument list (exported for tests and docs).
func (s RunSpec) ArgList() []string {
	args := []string{"run", "-d", "--name", s.Name, "--restart", "unless-stopped"}
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
	args = append(args, s.RunArgs...)
	args = append(args, s.Image)
	return append(args, s.Args...)
}

func (c CLI) Run(ctx context.Context, spec RunSpec) error {
	_, err := c.run(ctx, spec.ArgList()...)
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
	var raw struct {
		ID     string `json:"Id"`
		State  struct{ Running bool }
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return Container{}, false, fmt.Errorf("docker inspect %s: %w", name, err)
	}
	return Container{ID: raw.ID, Running: raw.State.Running, Labels: raw.Config.Labels}, true, nil
}

func (c CLI) ImageDigests(ctx context.Context, repo string) ([]string, error) {
	out, err := c.run(ctx, "image", "ls", "--digests", "--format", "{{.Repository}} {{.Digest}}", repo)
	if err != nil {
		return nil, err
	}
	var ds []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		r, d, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || r != repo || !strings.HasPrefix(d, "sha256:") || seen[d] {
			continue
		}
		seen[d] = true
		ds = append(ds, d)
	}
	return ds, nil
}

// RemoveImage removes one image reference without force: Docker refuses when
// a container still uses it, which is the behaviour prune relies on.
func (c CLI) RemoveImage(ctx context.Context, ref string) error {
	_, err := c.run(ctx, "image", "rm", ref)
	return err
}

// ErrNotOwned means a container with a managed name was not created by
// shiftgate for this service.
var ErrNotOwned = errors.New("container is not owned by shiftgate for this service")

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
