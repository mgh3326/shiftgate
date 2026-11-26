package docker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The real `docker image inspect` has no --size flag (only `docker inspect`
// does) and exits 125 on it. This fake behaves the same way, so the test fails
// if ImageInfo ever passes an option image inspect does not accept.
func TestImageInfoUsesOnlyImageInspectFlags(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    --size|-s) echo "unknown flag: $a" >&2; exit 125 ;;
  esac
done
if [ "$1 $2" != "image inspect" ]; then echo "unexpected: $*" >&2; exit 2; fi
echo '{"Size":1234,"RootFS":{"Layers":["sha256:a","sha256:b"]}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := CLI{Bin: bin}.ImageInfo(context.Background(), "example/app:1")
	if err != nil {
		t.Fatalf("ImageInfo: %v", err)
	}
	if info.Bytes != 1234 || len(info.Layers) != 2 {
		t.Fatalf("ImageInfo = %+v, want size 1234 and 2 layers", info)
	}
}
