package deploy_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mgh3326/shiftgate/internal/docker"
)

func TestPruneDigestOnlyImages(t *testing.T) {
	for _, use := range []string{"unused", "created", "docker-conflict"} {
		t.Run(use, func(t *testing.T) {
			h := newHarness(t)
			h.mustDeploy(1)
			service, _ := h.cfg.Service("web")
			protected, candidate := digest(1), digest(2)
			protectedID, candidateID := digest(101), digest(102)
			ref := service.Image + "@" + candidate
			dir := t.TempDir()
			bin := filepath.Join(dir, "docker")
			removed := filepath.Join(dir, "removed")
			t.Setenv("PRUNE_REMOVED", removed)

			// Classic Docker omits both pulls from the default listing. Their
			// inspection data has RepoTags: [] and a matching RepoDigests entry.
			script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  "container inspect --format {{json .}} shiftgate-web-"*)
    echo 'No such container' >&2
    exit 1 ;;
  "image ls --digests --format {{.Repository}} {{.Digest}} %s") ;;
  "image ls -a -q --no-trunc") printf '%%s\n' '%s' '%s' '%s' ;;
  "image inspect --format {{json .RepoDigests}} %s") echo '["%s@%s"]' ;;
  "image inspect --format {{json .RepoDigests}} %s") echo '["%s@%s","%s-other@%s"]' ;;
  "container ls --all --quiet --filter ancestor=%s")
    if [ '%s' = created ]; then echo created-container; fi ;;
  "image inspect --format {{.Id}} %s") echo '%s' ;;
  "container inspect --format {{.Image}} created-container") echo '%s' ;;
  "image rm %s")
    if [ '%s' = docker-conflict ]; then
      echo 'conflict: container is using its referenced image' >&2
      exit 1
    fi
    printf '%%s\n' "$3" > "$PRUNE_REMOVED" ;;
  *) echo "unexpected docker command: $*" >&2; exit 1 ;;
esac
`, service.Image, protectedID, candidateID, protectedID,
				protectedID, service.Image, protected,
				candidateID, service.Image, candidate, service.Image, digest(3),
				ref, use, ref, candidateID, candidateID, ref, use)
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			h.gate.Docker = docker.CLI{Bin: bin}

			res, err := h.gate.Prune(context.Background(), "web")
			if err != nil {
				t.Fatal(err)
			}
			got := res[0]
			if !slices.Equal(got.Kept, []string{protected}) {
				t.Fatalf("kept = %v, want %s", got.Kept, protected)
			}
			if use == "unused" {
				out, err := os.ReadFile(removed)
				if err != nil || strings.TrimSpace(string(out)) != ref ||
					!slices.Equal(got.Removed, []string{candidate}) || len(got.Warnings) != 0 {
					t.Fatalf("prune = %+v, recorded removal = %q (%v), want removal of %s", got, out, err, ref)
				}
				return
			}
			if _, err := os.Stat(removed); !os.IsNotExist(err) {
				t.Fatalf("unexpected removal record: %v", err)
			}
			wantWarning := "in use by a container"
			if use == "docker-conflict" {
				wantWarning = "container is using its referenced image"
			}
			if len(got.Removed) != 0 || len(got.Warnings) != 2 ||
				!strings.Contains(got.Warnings[0], wantWarning) ||
				!strings.Contains(got.Warnings[1], "found 2 local image digests") {
				t.Fatalf("prune = %+v, want a container-use warning and a no-removal warning", got)
			}
		})
	}
}
